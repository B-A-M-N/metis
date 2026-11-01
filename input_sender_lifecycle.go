package main

// input_sender_lifecycle.go: lifecycle-managed registry for terminal
// input senders (F-11).
//
// The sender registry used to be a bare process-global
// map[string]*inputSender with no close path: every distinct session
// name leaked one goroutine plus one channel forever, and a session
// stopped and recreated under the same name reused the dead lane.
// This file moves that map behind one small service:
//
//   - Senders are keyed by backend-qualified identity+generation
//     (backend | session | generation). The tmux backend fences on
//     the registry Owner token (fresh per start/adopt, so a
//     replacement session never collides with its predecessor);
//     the PTY backend fences on the supervisor identity plus its
//     persisted generation. Unknown sessions resolve to the empty
//     generation and still retire explicitly.
//   - Retirement is explicit and idempotent: retireInputSenders(name)
//     closes every lane for a session name across backends and
//     generations. The session stop path calls it once the kill is
//     confirmed; replacement needs no hook because a lookup that
//     observes a new generation retires the predecessor's lanes
//     before creating the successor.
//   - A retired lane drains instead of delivering: queued jobs get
//     rejected receipts, the lane's goroutine exits, and new work is
//     refused. Stale input can never reach a successor session.
//
// The service never executes input itself; delivery stays in
// terminal_input_queue.go. No Bubble Tea here: terminal delivery
// works without the Tea runtime (see boundary_test.go).

import (
	"fmt"
	"sync"
)

// inputSenderKey is one delivery lane's identity: the execution
// backend plus the session plus the fencing generation. Two
// generations of the same session name are two different lanes.
type inputSenderKey struct {
	backend    string
	session    string
	generation string
}

// senderBackendName maps the job's lane flag to the dispatch backend
// identity (exec_target.go owns the constants; this only reads them).
func senderBackendName(pty bool) string {
	if pty {
		return TargetPTY
	}
	return TargetTMUX
}

// senderGeneration resolves the fencing generation for one lane.
// Tmux lanes fence on the registry Owner token: startSession and
// adoptSession mint a fresh token, so a replaced session resolves
// to a new generation while an unknown or unowned name resolves to
// "". PTY lanes fence on the supervisor identity (pid+starttime,
// which survives pid reuse) plus the persisted generation. Any
// store error resolves to "" — an unfenced lane that still retires
// explicitly — never to a guess.
func senderGeneration(session, backend string) string {
	if backend == TargetPTY {
		m, err := ptyGet(session)
		if err != nil {
			return ""
		}
		return fmt.Sprintf("%d|%s|%d|%d", m.Generation, m.Owner, m.SupervisorPid, m.SupervisorStart)
	}
	return loadRegistry()[session].Owner
}

// inputSenderService owns every registered delivery lane. The mutex
// is the only registry lock; it always nests outside a sender's own
// lock (sender methods never call back into the service), so retire
// and lookup cannot deadlock.
type inputSenderService struct {
	mu sync.Mutex
	m  map[inputSenderKey]*inputSender
}

func newInputSenderService() *inputSenderService {
	return &inputSenderService{m: map[inputSenderKey]*inputSender{}}
}

// inputSenderRegistry is the process-wide lane registry. Reach it
// through senderForQualified / retireInputSenders, never directly.
var inputSenderRegistry = newInputSenderService()

// senderForQualified returns the live lane for one backend-qualified
// identity+generation, creating it once. A lane registered for the
// same backend+session at an older generation is a replaced
// predecessor: it is retired (queued work rejected, goroutine
// released) before the successor is created, so replacement needs
// no separate hook.
func senderForQualified(session string, pty bool) *inputSender {
	backend := senderBackendName(pty)
	return inputSenderRegistry.forKey(inputSenderKey{
		backend:    backend,
		session:    session,
		generation: senderGeneration(session, backend),
	})
}

// forKey is the single get-or-create path with the default buffer.
// Direct constructors in terminal_input_queue.go register through
// here (or forKeyWithCap) too, so every lane is lifecycle-managed
// and no orphan channel can outlive its key.
func (s *inputSenderService) forKey(key inputSenderKey) *inputSender {
	return s.forKeyWithCap(key, 64)
}

// forKeyWithCap is get-or-create with an explicit buffer for the
// fresh lane; a live lane at the same key keeps its own buffer.
func (s *inputSenderService) forKeyWithCap(key inputSenderKey, cap int) *inputSender {
	s.mu.Lock()
	defer s.mu.Unlock()
	if live, ok := s.m[key]; ok {
		return live
	}
	for k, old := range s.m {
		if k.backend == key.backend && k.session == key.session {
			old.closeLocked()
			delete(s.m, k)
		}
	}
	lane := newBareInputSenderWithCap(key, cap)
	s.m[key] = lane
	return lane
}

// replaceKeyWithCap always builds a fresh lane: every registered
// lane for the same backend+session is retired first. Direct
// constructors use this so a constructor constructs (never aliases
// a live lane built with another buffer), while the service lookup
// above singleflights a live lane at the same key.
func (s *inputSenderService) replaceKeyWithCap(key inputSenderKey, cap int) *inputSender {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, old := range s.m {
		if k.backend == key.backend && k.session == key.session {
			old.closeLocked()
			delete(s.m, k)
		}
	}
	lane := newBareInputSenderWithCap(key, cap)
	s.m[key] = lane
	return lane
}

// retireLocked closes and drops every lane matching keep==false.
// Callers hold the service lock and do no work while holding it:
// close is non-blocking (the lane's goroutine drains itself).
func (s *inputSenderService) retireLocked(keep func(inputSenderKey) bool) int {
	n := 0
	for k, lane := range s.m {
		if keep(k) {
			continue
		}
		lane.closeLocked()
		delete(s.m, k)
		n++
	}
	return n
}

// retireInputSenders closes every lane for a session name across
// backends and generations: queued work is rejected, lane
// goroutines exit, and new work gets a fresh lane. The session stop
// path calls this once the kill is confirmed. Idempotent: retiring
// an unknown or already-retired session retires zero lanes.
func retireInputSenders(session string) int {
	inputSenderRegistry.mu.Lock()
	defer inputSenderRegistry.mu.Unlock()
	return inputSenderRegistry.retireLocked(func(k inputSenderKey) bool {
		return k.session != session
	})
}

// deregisterLane closes one lane and drops its registry entry when
// the entry still points at it (a replaced lane is already gone, so
// this never removes a successor). Direct Close() funnels through
// here; service-held paths use closeLocked plus their own delete.
func (s *inputSenderService) deregisterLane(lane *inputSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := inputSenderKey{backend: lane.backend, session: lane.session, generation: lane.generation}
	if cur, ok := s.m[key]; ok && cur == lane {
		delete(s.m, key)
	}
	lane.closeLocked()
}

// retireAllInputSenders closes every registered lane. Tests use it
// to return the process to baseline; production code prefers the
// scoped retireInputSenders.
func retireAllInputSenders() int {
	inputSenderRegistry.mu.Lock()
	defer inputSenderRegistry.mu.Unlock()
	return inputSenderRegistry.retireLocked(func(inputSenderKey) bool { return false })
}

// liveInputSenderCount reports the registered lane count. Tests use
// it to assert the registry drains; it is not a control signal.
func liveInputSenderCount() int {
	inputSenderRegistry.mu.Lock()
	defer inputSenderRegistry.mu.Unlock()
	return len(inputSenderRegistry.m)
}

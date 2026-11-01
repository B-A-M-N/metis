package main

// terminal_input_queue.go: terminal input delivery boundary.
//
// Queueing, execution, per-session serialization, and receipt
// handling live here, free of Bubble Tea: the terminal boundary
// returns a typed result {session, opID, disposition} and never
// produces a UI-framework command nor mutates the model.
// Presentation adapters (waitInputResultCmd, applyInputResult)
// live in tui_input.go.
//
// One bounded FIFO sender per session: keypresses enqueue in
// arrival order, a single goroutine executes them one at a time,
// and each job's typed result returns on its own reply channel.
// A full queue refuses immediately on the update path — the UI
// never blocks and keystrokes never reorder.
//
// Results carry session identity + operation ID. The model shows
// a result on the status line only while still attached to that
// session; anything else is parked per-session, never applied to
// another session's UI state.
//
// Serialization is local ordering only: the queue does NOT grant
// control permission. Every job still arbitrates through the SQL
// input-lease authority and generation fencing inside ptyInput /
// sendTerminalInput (see pty_lease.go); no mutex replaces them.

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ensureMcpTerminalTable owns the request_id -> outcome store for
// the terminal lifecycle tools: one row per key, claimed before the
// pipeline runs and filled with the typed outcome after it settles.
// It lives in the terminal domain (not the MCP adapter) because the
// transport boundary must never open persistence directly.
func ensureMcpTerminalTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS mcp_terminal_ops (
		request_id TEXT PRIMARY KEY,
		tool TEXT NOT NULL,
		session TEXT NOT NULL,
		payload TEXT NOT NULL,
		result TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`)
	return err
}

// claimTerminalOp claims request_id for one terminal op. A fresh
// key inserts a started row and returns replay=false. A recorded key
// with the identical payload returns the stored result with
// replay=true; a reused key with a different payload is a conflict;
// a claimed-but-unsettled key refuses single-flight instead of
// executing twice.
func claimTerminalOp(requestID, tool, session, payload string) (stored string, replay bool, err error) {
	db, err := openTasksDB()
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	if err := ensureMcpTerminalTable(db); err != nil {
		return "", false, err
	}
	if _, err := db.Exec(`INSERT INTO mcp_terminal_ops (request_id, tool, session, payload, result, created)
		VALUES (?, ?, ?, ?, '', ?)`, requestID, tool, session, payload, nowUTC()); err == nil {
		return "", false, nil
	} else if !strings.Contains(err.Error(), "UNIQUE constraint") && !strings.Contains(strings.ToLower(err.Error()), "unique") {
		return "", false, err
	}
	var t, s, p, r string
	if qerr := db.QueryRow(`SELECT tool, session, payload, result FROM mcp_terminal_ops WHERE request_id = ?`,
		requestID).Scan(&t, &s, &p, &r); qerr != nil {
		return "", false, qerr
	}
	if t != tool || s != session || p != payload {
		return "", false, &mcpTerminalConflict{RequestID: requestID, Tool: tool}
	}
	if r == "" {
		return "", false, fmt.Errorf("terminal: request_id %q is already in flight; refusing duplicate execution (retry with a new request_id once the first settles)", requestID)
	}
	return r, true, nil
}

// recordTerminalOp fills the claimed key with the settled typed
// outcome. Only settled pipeline verdicts are recorded.
func recordTerminalOp(requestID, result string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`UPDATE mcp_terminal_ops SET result = ? WHERE request_id = ?`, result, requestID)
}

// releaseTerminalOp drops a claimed-but-unsettled key after a
// transient transport failure (held lease, down supervisor) so an
// identical retry starts fresh instead of replaying the failure or
// wedging on single-flight.
func releaseTerminalOp(requestID string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`DELETE FROM mcp_terminal_ops WHERE request_id = ? AND result = ''`, requestID)
}

// DispositionBlocked marks input the transport accepted structurally
// but refused to deliver: the session's harness does not consume
// live input (one-shot exec), so keystrokes would vanish into a
// process that cannot read them. Blocked is a typed, recorded
// outcome — never silent success, never a send.
const DispositionBlocked = "blocked"

// terminalBlockedError carries a harness-blocked input refusal from
// the domain transport to the MCP layer, where it becomes a typed
// blocked disposition instead of an isError refusal.
type terminalBlockedError struct{ reason string }

func (e *terminalBlockedError) Error() string { return e.reason }

// isTerminalBlocked reports whether err is a harness-blocked refusal.
func isTerminalBlocked(err error) bool {
	_, ok := err.(*terminalBlockedError)
	return ok
}

// sendTerminalInputAsOwner delivers keystrokes to an owned, running
// session under an explicit lease owner — the channel the MCP
// terminal tools use so a remote writer's authority is fenced to
// the lease it actually holds. There is no unrestricted send-keys
// path here: ownership resolves to the immutable session ID (a
// replaced session never receives a stale send), the single-writer
// lease arbitrates rivals, and harnesses without a live-input
// channel are blocked, never typed into.
func sendTerminalInputAsOwner(name, owner, literal string, keys ...string) error {
	reg := loadRegistry()
	// resolveOwnedTarget verifies ownership and binds to the live
	// session ID; a dead session fails resolution, so no separate
	// liveness check is needed.
	_, target, err := resolveOwnedTarget(reg, name)
	if err != nil {
		return err
	}
	// A session running a known one-shot harness has no live-input
	// channel; keystrokes would vanish into a process that cannot
	// consume them. Block explicitly instead of reporting the input
	// as delivered. Unknown harnesses (custom commands) keep the
	// legacy attempt path.
	if h, herr := sessionHarness(name); herr == nil && h != "" {
		if a, aerr := lookupAdapter(h); aerr == nil && !a.AcceptsLiveInput {
			if a.OneShot {
				return &terminalBlockedError{fmt.Sprintf("harness %q is one-shot and does not consume live input", h)}
			}
			return &terminalBlockedError{fmt.Sprintf("harness %q does not consume live input and cannot continue a conversation", h)}
		}
	}
	// Single input writer (TD-061): MCP keystrokes need the same
	// lease as interactive typing, or two controllers interleave on
	// one terminal. A rival holder's refusal names the holder.
	if err := tmuxAcquireLease(name, owner); err != nil {
		return fmt.Errorf("direct input refused: %w", err)
	}
	if literal != "" {
		if _, err := tmux("send-keys", "-t", target, "-l", literal); err != nil {
			return fmt.Errorf("send literal: %w", err)
		}
	}
	if len(keys) > 0 {
		args := append([]string{"send-keys", "-t", target}, keys...)
		if _, err := tmux(args...); err != nil {
			return fmt.Errorf("send keys: %w", err)
		}
	}
	return nil
}

// tmuxResizePaneAsOwner resizes the bound session's active pane
// under an explicit lease owner: ownership resolves to the
// immutable session ID and the single-writer lease arbitrates
// rivals, exactly like input delivery.
func tmuxResizePaneAsOwner(name, owner string, rows, cols int) error {
	reg := loadRegistry()
	_, target, err := resolveOwnedTarget(reg, name)
	if err != nil {
		return err
	}
	if err := tmuxAcquireLease(name, owner); err != nil {
		return fmt.Errorf("resize refused: %w", err)
	}
	if _, err := tmux("resize-pane", "-t", target, "-y", fmt.Sprintf("%d", rows), "-x", fmt.Sprintf("%d", cols)); err != nil {
		return fmt.Errorf("resize pane: %w", err)
	}
	return nil
}

// Input dispositions: rejected (never sent), queued (accepted by
// transport, outcome not yet observed), confirmed (bytes handed
// to tmux/PTY master — transport delivery, not worker
// comprehension).
const (
	DispositionRejected  = "rejected"
	DispositionQueued    = "queued"
	DispositionConfirmed = "confirmed"
)

// inputResultMsg carries one job's outcome back to the model.
type inputResultMsg struct {
	session     string
	opID        string
	disposition string
	detail      string
}

// inputJob is one ordered keypress delivery. backend+generation
// stamp the lane the job was accepted on: a lane that observes a
// job from another generation rejects it instead of delivering
// stale input to a successor session.
type inputJob struct {
	session    string
	opID       string
	pty        bool
	backend    string
	generation string
	literal    string
	keys       []string
	text       string
	owner      string
	reply      chan inputResultMsg
}

// inputSender is one backend-qualified session generation's single
// ordered delivery lane. The zero value is unusable: build lanes
// through newInputSender, newInputSenderWithCap, or senderFor so
// the lane is registered with the lifecycle service and its run
// goroutine is tracked. Close retires the lane; every lane must be
// closed (directly, via retireInputSenders, or by generation
// replacement) or its goroutine leaks.
type inputSender struct {
	backend    string
	session    string
	generation string
	jobs       chan inputJob
	mu         sync.Mutex
	retired    bool
	done       chan struct{}
}

// execInputJob executes one job; replaceable in tests. Production
// sends through the real tmux/FIFO path.
var execInputJob = execInputJobReal

func execInputJobReal(job inputJob) (string, string) {
	if job.pty {
		id, err := ptyInput(job.session, job.owner, job.text)
		if err != nil {
			return DispositionRejected, err.Error()
		}
		// Transport accepted; observe the supervisor's verdict
		// briefly before reporting back.
		for i := 0; i < 10; i++ {
			status, detail, serr := ptyInputStatus(id)
			if serr != nil {
				return DispositionQueued, fmt.Sprintf("op %s accepted; status unreadable: %v", id, serr)
			}
			switch status {
			case "written_to_pty":
				return DispositionConfirmed, fmt.Sprintf("op %s on PTY master", id)
			case "error":
				return DispositionRejected, fmt.Sprintf("op %s refused: %s", id, detail)
			}
			time.Sleep(100 * time.Millisecond)
		}
		return DispositionQueued, fmt.Sprintf("op %s accepted; supervisor has not settled it yet", id)
	}
	if err := sendTerminalInput(job.session, job.literal, job.keys...); err != nil {
		return DispositionRejected, err.Error()
	}
	return DispositionConfirmed, "bytes handed to tmux (worker consumption not tracked)"
}

// newBareInputSender builds an unregistered lane for key. Only the
// lifecycle service calls it (via forKey); the lane starts draining
// immediately so registration can never strand a live goroutine.
func newBareInputSender(key inputSenderKey) *inputSender {
	return newBareInputSenderWithCap(key, 64)
}

func newBareInputSenderWithCap(key inputSenderKey, cap int) *inputSender {
	if cap < 1 {
		cap = 1
	}
	s := &inputSender{backend: key.backend, session: key.session, generation: key.generation, jobs: make(chan inputJob, cap), done: make(chan struct{})}
	go s.run()
	return s
}

func newInputSender(session string) *inputSender {
	return newInputSenderWithCap(session, 64)
}

// newInputSenderWithCap builds a fresh tmux lane with an explicit
// buffer, registered with the lifecycle service: any lane already
// registered for the same backend+session is retired first, so a
// constructor always constructs and never aliases another buffer.
func newInputSenderWithCap(session string, cap int) *inputSender {
	return inputSenderRegistry.replaceKeyWithCap(inputSenderKey{
		backend:    TargetTMUX,
		session:    session,
		generation: senderGeneration(session, TargetTMUX),
	}, cap)
}

// isRetired reports whether the lane is drained or draining: no new
// work is accepted and queued work is rejected, never delivered.
func (s *inputSender) isRetired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retired
}

// retireHoldingLock marks the lane retired and closes its channel.
// The sender mutex must be held. Closing lets the run loop reject
// every still-buffered job and then exit, so retirement releases
// the goroutine without executing stale work.
func (s *inputSender) retireHoldingLock() {
	if s.retired {
		return
	}
	s.retired = true
	close(s.jobs)
}

// closeLocked retires the lane while the service lock is held (the
// service always nests outside the sender lock). Non-blocking: the
// lane's goroutine performs the drain.
func (s *inputSender) closeLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retireHoldingLock()
}

// Close retires the lane: new work is refused, queued work is
// rejected with a typed receipt, the lane leaves the registry, and
// its goroutine exits. Idempotent; done closes once the goroutine
// has exited.
func (s *inputSender) Close() {
	inputSenderRegistry.deregisterLane(s)
}

// Retire is the lifecycle-service name for Close: drain queued work
// with rejected receipts and release the lane's goroutine.
func (s *inputSender) Retire() {
	s.Close()
}

// tryEnqueue places one stamped job on the lane without blocking.
// False means the lane is full or retired — the caller must refuse
// immediately on the update path, never block. The send holds the
// sender lock, as does retirement's close, so a retired lane can
// never panic on send-to-closed.
func (s *inputSender) tryEnqueue(job inputJob) (inputJob, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return inputJob{}, false
	}
	select {
	case s.jobs <- job:
		return job, true
	default:
		return inputJob{}, false
	}
}

// stamp binds a job to this lane's backend-qualified generation.
func (s *inputSender) stamp(opID string, pty bool, literal string, keys []string, text, owner string) inputJob {
	return inputJob{session: s.session, opID: opID, pty: pty, backend: s.backend, generation: s.generation,
		literal: literal, keys: keys, text: text, owner: owner, reply: make(chan inputResultMsg, 1)}
}

// retiredReceipt is the typed refusal for work a retired lane will
// not deliver: rejected, never silent, never sent.
func retiredReceipt(job inputJob) inputResultMsg {
	return inputResultMsg{session: job.session, opID: job.opID, disposition: DispositionRejected,
		detail: "input sender retired (session stopped or replaced); keypress refused, retry on the live session"}
}

func (s *inputSender) run() {
	defer close(s.done)
	for job := range s.jobs {
		// Drain, don't deliver: anything still buffered after
		// retirement — or stamped for another generation — is
		// stale by definition and must never reach a session.
		if s.isRetired() || job.backend != s.backend || job.generation != s.generation {
			job.reply <- retiredReceipt(job)
			continue
		}
		disp, detail := execInputJob(job)
		job.reply <- inputResultMsg{session: job.session, opID: job.opID, disposition: disp, detail: detail}
	}
}

// senderFor returns the session's tmux lane, creating it once per
// generation. A replaced session (new Owner token) resolves to a
// new generation and a fresh lane; the predecessor is retired.
func senderFor(session string) *inputSender {
	return senderForQualified(session, false)
}

var inputOpCounter atomic.Uint64

// enqueueInputJob places one keypress on its backend-qualified
// session lane. Returns the job and true, or false when the lane
// is full or retired.
func enqueueInputJob(session string, pty bool, literal string, keys []string, text, owner string) (inputJob, bool) {
	op := fmt.Sprintf("in-%d", inputOpCounter.Add(1))
	lane := senderForQualified(session, pty)
	return lane.tryEnqueue(lane.stamp(op, pty, literal, keys, text, owner))
}

// enqueue is the test-facing form with an explicit op ID.
// Non-blocking like the production path: false means full.
func (s *inputSender) enqueue(opID string, pty bool, literal string, keys []string, text, owner string) (inputJob, bool) {
	return s.tryEnqueue(s.stamp(opID, pty, literal, keys, text, owner))
}

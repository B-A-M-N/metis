package main

// orch_visibility.go: orchestration visibility producers.
//
// Defined-but-never-emitted event types are invisible
// orchestration: the CONTROLLER tab can only show what the ledger
// holds. This file owns the producers for the previously missing
// actions — delegation requests, terminal resizes, and transport
// disconnects — reusing recordOrchEvent (idempotent by
// request_id+type, bounded payloads, durable rows). It also tracks
// the last authenticated MCP principal per process so a disconnect
// is attributable without inventing an identity.

import (
	"strconv"
	"sync"
)

// stdioSess is the authenticated identity of ONE stdio transport
// session (one `mcp serve` process/connection). Disconnect
// attribution is bound to this session object — never to a
// process-global last principal — so interleaved controllers in
// one process cannot cross-attribute, and HTTP (stateless,
// per-request) never infers disconnects at all: its disconnect
// signal is presence-lease expiry only.
type stdioSess struct {
	mu        sync.Mutex
	id        string
	principal string
}

// newStdioSession mints one transport session. The session ID is
// distinct from the authenticated principal: several sessions may
// share one principal (e.g. local-operator), and each session's
// lifecycle (register on auth, deregister on disconnect) is
// tracked independently in the durable session registry.
func newStdioSession() *stdioSess { return &stdioSess{id: newRequestID()} }

// recordPrincipal stores the session's authenticated principal
// (first authenticated identity wins for the session; an
// unauthenticated session records nothing and emits nothing) and
// registers/refreshes this session's durable row so a peer
// session sharing the principal is not marked disconnected by
// this session's later disconnect.
func (s *stdioSess) recordPrincipal(principal string) {
	if s == nil || principal == "" {
		return
	}
	s.mu.Lock()
	if s.principal == "" {
		s.principal = principal
	}
	established := s.principal
	id := s.id
	s.mu.Unlock()
	if principal == established {
		registerStdioSession(id, established, musePresenceLeaseTTL)
	}
}

func (s *stdioSess) getPrincipal() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principal
}

// emitDisconnect records this session's disconnect attributed to
// its own authenticated principal. Unauthenticated sessions emit
// nothing: there is no identity to attribute.
func (s *stdioSess) emitDisconnect() {
	principal := s.getPrincipal()
	if principal == "" {
		return
	}
	// Deregister THIS session first: peers sharing the principal
	// keep their own rows. A crash before this point leaves a
	// row that expires within the lease TTL (bounded stale
	// window, explicit absent afterwards).
	deregisterStdioSession(s.id)
	emitTransportDisconnectedFor(principal, "mcp-local")
}

// emitTransportDisconnectedFor records one authenticated
// controller's transport ending under its verified principal.
// Presence logic projects a fresh disconnect to the
// disconnected/error state only when no other live lease and no
// newer same-principal activity recover it (see collectMuseFacts).
func emitTransportDisconnectedFor(principal, source string) {
	if principal == "" {
		return
	}
	e := OrchEvent{
		OriginPrincipal: principal,
		EventType:       OrchTransportDisconnected,
		Payload:         source,
		Disposition:     "disconnected",
	}
	if _, err := recordOrchEvent(e); err != nil {
		recordReceipt("", "orch_gap", "transport disconnect feed write failed: "+err.Error())
	}
}

// emitDelegationRequested records one delegation ask: who
// (verified transport principal), which run/worker, and the exact
// instruction, with disposition pending (accepted for dispatch) or
// refused (capability/validation refusal — the attempt stays
// visible without granting anything). Replays converge on the
// original row via (request_id, event_type) idempotency.
func emitDelegationRequested(req OperationRequest, run, session, objective, disposition string) {
	e := OrchEvent{
		RunID:           run,
		OriginPrincipal: req.Principal,
		TargetAgent:     session,
		EventType:       OrchDelegationRequested,
		RequestID:       req.RequestID,
		Payload:         objective,
		Disposition:     disposition,
	}
	if _, err := recordOrchEvent(e); err != nil {
		recordReceipt("", "orch_gap", "delegation requested feed write failed: "+err.Error())
	}
}

// emitTerminalResizeOutcome records one terminal-resize attempt
// the same way input/stop outcomes are recorded: verified
// principal, worker session, request ID, dimensions, and the
// actual outcome — refused or resized. Payload carries geometry
// only, never caller content.
func emitTerminalResizeOutcome(req OperationRequest, session, detail string, resized bool, rerr error) {
	e := OrchEvent{
		OriginPrincipal: req.Principal,
		TargetAgent:     session,
		EventType:       OrchTerminalResize,
		RequestID:       req.RequestID,
	}
	if rerr != nil {
		e.Disposition = "refused"
		e.Payload = rerr.Error()
	} else if resized {
		e.Disposition = "resized"
		e.Payload = detail
	} else {
		e.Disposition = "admitted"
		e.Payload = detail
	}
	if _, err := recordOrchEvent(e); err != nil {
		recordReceipt("", "orch_gap", "terminal resize feed write failed: "+err.Error())
	}
}

// emitWorkerClaimed records one worker's successful claim of an
// exact attempt, after the claim's ownership checks passed.
// Payload carries the attempt only — never execution content.
func emitWorkerClaimed(session string, tk Task) {
	e := OrchEvent{
		RunID:           tk.Run,
		OriginPrincipal: session,
		TargetAgent:     session,
		TaskID:          tk.ID,
		Attempt:         tk.Attempt,
		EventType:       OrchWorkerClaimed,
		Payload:         "attempt=" + strconv.Itoa(tk.Attempt),
		Disposition:     "claimed",
	}
	if _, err := recordOrchEvent(e); err != nil {
		recordReceipt(tk.ID, "orch_gap", "worker claimed feed write failed: "+err.Error())
	}
}

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
	"sync"
)

// lastMCPPrincipal is the most recent authenticated principal on
// this process's MCP transports (stdio auth or HTTP JWT subject).
// A disconnect with no authenticated principal never happened for
// an authenticated controller, so it is not recorded.
var (
	lastPrincipalMu sync.Mutex
	lastPrincipal   string
)

func setLastMCPPrincipal(principal string) {
	if principal == "" {
		return
	}
	lastPrincipalMu.Lock()
	lastPrincipal = principal
	lastPrincipalMu.Unlock()
}

func getLastMCPPrincipal() string {
	lastPrincipalMu.Lock()
	defer lastPrincipalMu.Unlock()
	return lastPrincipal
}

// emitTransportDisconnected records one authenticated controller's
// transport ending. Presence logic (scanMuseEventFacts) projects a
// fresh disconnect with no newer traffic to the disconnected/error
// state; newer traffic recovers it — so this event is the real
// disconnect signal that was previously impossible to produce.
func emitTransportDisconnected(source string) {
	principal := getLastMCPPrincipal()
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

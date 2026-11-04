package main

// orch_visibility_test.go: orchestration visibility (failing-first).
//
// Every Metis-mediated controller action must leave a durable,
// attributable ledger event the CONTROLLER tab can show: delegation
// (requested + dispatched), terminal resize, and transport
// disconnect. Defined-but-never-emitted event types are invisible
// orchestration — these tests pin the producers.

import (
	"strings"
	"testing"
)

func orchEventsOfType(t *testing.T, typ string) []OrchEvent {
	t.Helper()
	evs, err := listLatestOrchEvents(maxOrchPage)
	if err != nil {
		t.Fatal(err)
	}
	var out []OrchEvent
	for _, e := range evs {
		if e.EventType == typ {
			out = append(out, e)
		}
	}
	return out
}

// Delegation through the MCP tool must record BOTH the request
// (who asked, for which worker, with what instruction) and the
// dispatch commit. Previously only dispatched was recorded.
func TestDelegateRecordsRequestedAndDispatched(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "del-owner")
	seedOwnedSession(t, "wv", "del-owner")

	res, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "run-vis", "session": "wv", "objective": "visible work",
		"actor": "ctl-op", "request_id": "vis-del-1",
	}))
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if res.IsError {
		t.Fatalf("delegate isError: %s", res.Content[0].Text)
	}
	req := orchEventsOfType(t, OrchDelegationRequested)
	if len(req) != 1 {
		t.Fatalf("delegation.requested events = %d, want 1", len(req))
	}
	e := req[0]
	// Origin is the server-established principal (verified
	// transport identity), never the asserted actor label —
	// matching provision/message/input emitters.
	if e.OriginPrincipal != "local-operator" {
		t.Fatalf("requested origin must be the verified principal, got %q", e.OriginPrincipal)
	}
	if e.TargetAgent != "wv" || e.RunID != "run-vis" {
		t.Fatalf("requested target/run wrong: %+v", e)
	}
	if e.Disposition == "" {
		t.Fatalf("requested disposition must be recorded: %+v", e)
	}
	if !strings.Contains(e.Payload, "visible work") {
		t.Fatalf("requested payload must carry the instruction: %+v", e)
	}
	dis := orchEventsOfType(t, OrchDelegationDispatched)
	if len(dis) != 1 {
		t.Fatalf("delegation.dispatched events = %d, want 1", len(dis))
	}
	// Replay converges: same request_id must not duplicate either event.
	res2, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "run-vis", "session": "wv", "objective": "visible work",
		"actor": "ctl-op", "request_id": "vis-del-1",
	}))
	if err != nil || res2.IsError {
		t.Fatalf("delegate replay: %v %v", err, res2)
	}
	if n := len(orchEventsOfType(t, OrchDelegationRequested)); n != 1 {
		t.Fatalf("replay duplicated requested events: %d", n)
	}
}

// Terminal resize through the MCP tool must land in the ledger
// with the controller identity and target session.
func TestTerminalResizeRecordsEvent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "rz1")
	openFifoReader(t, "rz1")

	res, err := mcpCallTool("metis_terminal_resize", mustJSON(t, map[string]any{
		"session": "pty:rz1", "rows": 30, "cols": 100,
		"actor": "ctl-rz", "request_id": "vis-rz-1",
	}))
	if err != nil {
		t.Fatalf("resize: %v", err)
	}
	if res.IsError {
		t.Fatalf("resize isError (seeded self-supervisor must accept): %s", res.Content[0].Text)
	}
	evs := orchEventsOfType(t, OrchTerminalResize)
	if len(evs) != 1 {
		t.Fatalf("terminal.resized events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.TargetAgent != "pty:rz1" {
		t.Fatalf("resize target wrong: %+v", e)
	}
	if e.Disposition == "" {
		t.Fatalf("resize disposition must be recorded: %+v", e)
	}
}

// A transport disconnect for an authenticated controller must be
// durable and attributable, and must drive the presence model to
// the disconnected/error projection (never silent).
func TestTransportDisconnectedRecordedAndProjected(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	sess := newStdioSession()
	sess.recordPrincipal("jwt:ctl-x")
	sess.emitDisconnect()
	evs := orchEventsOfType(t, OrchTransportDisconnected)
	if len(evs) != 1 {
		t.Fatalf("transport.disconnected events = %d, want 1", len(evs))
	}
	if evs[0].OriginPrincipal != "jwt:ctl-x" {
		t.Fatalf("disconnect origin must name the authenticated principal, got %q", evs[0].OriginPrincipal)
	}
	// Fresh disconnect with no newer traffic projects to Error.
	if state, _, _ := museActivity(); state != MuseError {
		t.Fatalf("fresh disconnect must project MuseError, got %q", state)
	}
	// Newer traffic recovers the connection projection.
	if _, err := recordOrchEvent(OrchEvent{
		OriginPrincipal: "jwt:ctl-x", TargetAgent: "w",
		EventType: OrchMessageSent, Disposition: "accepted",
	}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state == MuseError {
		t.Fatalf("recovered traffic must clear the disconnect projection, got %q", state)
	}
}

// The CONTROLLER pane must show the separated presence facts
// (connection / lease / operation), not just the derived state word.
func TestControllerPaneShowsPresenceFacts(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = true
	m.width, m.height = 120, 30
	m.museState = MuseConnected
	m.museFacts = MuseFacts{Connection: MuseConnUp, Lease: MuseLeaseLive, Operation: MuseOpNone, Progress: MuseProgressStale, Blocking: MuseBlockingNone}
	v := m.View()
	for _, want := range []string{"connection=", "lease=", "operation="} {
		if !strings.Contains(v, want) {
			t.Fatalf("controller pane must show presence facts (%q missing):\n%s", want, v)
		}
	}
}

// Dispatched events name the verified principal (authenticated
// callers), falling back to the actor label only for internal
// constructions without a principal.
func TestDispatchedOriginIsVerifiedPrincipal(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "org-owner")
	seedOwnedSession(t, "wv2", "org-owner")
	seedOwnedSession(t, "wv3", "org-owner")
	tk, err := createTask("run-org", "wv2", "do it", "done")
	if err != nil {
		t.Fatal(err)
	}
	req := OperationRequest{Actor: "asserted-label", Principal: "jwt:verified-1", RequestID: "org-req-1"}
	if err := dispatchTaskWithRequest(tk.ID, "wv2", req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	dis := orchEventsOfType(t, OrchDelegationDispatched)
	if len(dis) != 1 || dis[0].OriginPrincipal != "jwt:verified-1" {
		t.Fatalf("dispatched origin must be the verified principal, got %+v", dis)
	}
	// Internal actor-only construction keeps the actor fallback.
	tk2, err := createTask("run-org2", "wv3", "do it too", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk2.ID, "wv3", testReq("internal-actor")); err != nil {
		t.Fatalf("dispatch2: %v", err)
	}
	dis = orchEventsOfType(t, OrchDelegationDispatched)
	if len(dis) != 2 || dis[1].OriginPrincipal != "internal-actor" {
		t.Fatalf("actor-only fallback must record the actor, got %+v", dis)
	}
}

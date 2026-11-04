package main

// read_scope_test.go: remote read authorization (failing-first).
//
// Two JWT subjects must not read each other's scoped work through
// the HTTP MCP. Fleet-wide visibility requires the explicit
// metis:fleet grant. Worker reports for authorized operations
// remain visible to their owner.

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func mcpHTTPText(out map[string]any) string {
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		raw, _ := json.Marshal(out)
		return string(raw)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

// provisionOneHTTP provisions a muse-code echo worker for sub via
// the HTTP transport (owner = jwt:sub). Real worker, zero-cost echo.
func provisionOneHTTP(t *testing.T, f *mcpHTTPFixture, sub, session, run string) {
	t.Helper()
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	tok := f.mint(sub, f.res, "metis:message metis:provision metis:input metis:stop", time.Now().Add(time.Hour), nil)
	code, out := f.call(tok, "2024-11-05", mcpCallBody(1, "metis_provision", map[string]any{
		"run": run, "session": session, "workdir": work,
		"harness": "muse-code", "provider": "echo", "backend": "pty",
		"actor": "op", "request_id": "scope-prov-" + session,
	}))
	if code != http.StatusOK {
		t.Fatalf("provision %s: %d %v", session, code, out)
	}
	if _, hasErr := out["error"]; hasErr {
		t.Fatalf("provision %s refused: %v", session, out["error"])
	}
	t.Cleanup(func() { _ = ptyStop(session) })
}

func TestMCPHTTPReadScopeTwoControllers(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	provisionOneHTTP(t, f, "controller-a", "scopeA", "run-a")
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	tokB := f.mint("controller-b", f.res, "metis:message", time.Now().Add(time.Hour), nil)

	// B must not see A's session in agents.
	code, out := f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_agents", map[string]any{}))
	if code != http.StatusOK {
		t.Fatalf("agents B: %d", code)
	}
	if strings.Contains(mcpHTTPText(out), "scopeA") {
		t.Fatalf("controller B must not see controller A's session: %s", mcpHTTPText(out))
	}
	// A sees its own session.
	_, out = f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_agents", map[string]any{}))
	if !strings.Contains(mcpHTTPText(out), "scopeA") {
		t.Fatalf("controller A must see its own session: %s", mcpHTTPText(out))
	}
	// Events: B sees no events attributed to A's session.
	_, out = f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 50}))
	if strings.Contains(mcpHTTPText(out), "scopeA") {
		t.Fatalf("controller B must not see A's events: %s", mcpHTTPText(out))
	}
	_, out = f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 50}))
	if !strings.Contains(mcpHTTPText(out), "scopeA") {
		t.Fatalf("controller A must see its own events: %s", mcpHTTPText(out))
	}
}

func TestMCPHTTPEventsPreserveAuthorizedWorkerReports(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	provisionOneHTTP(t, f, "controller-a", "scopeR", "run-r")
	// A worker report event for A's session must remain visible to A.
	if _, err := recordOrchEvent(OrchEvent{
		OriginPrincipal: "scopeR", TargetAgent: "metis",
		EventType: OrchWorkerReported, Payload: "done work", Disposition: "filed",
	}); err != nil {
		t.Fatal(err)
	}
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	tokB := f.mint("controller-b", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	_, out := f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 50}))
	if !strings.Contains(mcpHTTPText(out), "done work") {
		t.Fatalf("owner must see its worker report event: %s", mcpHTTPText(out))
	}
	_, out = f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 50}))
	if strings.Contains(mcpHTTPText(out), "done work") {
		t.Fatalf("non-owner must not see the worker report event: %s", mcpHTTPText(out))
	}
}

func TestMCPHTTPFleetObserverGrant(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	provisionOneHTTP(t, f, "controller-a", "scopeF", "run-f")
	// Without the fleet grant, B sees nothing of A's.
	tokB := f.mint("observer-b", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	_, out := f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_agents", map[string]any{}))
	if strings.Contains(mcpHTTPText(out), "scopeF") {
		t.Fatalf("without fleet grant, B must not see A's session")
	}
	// With the explicit metis:fleet grant, fleet visibility is retained.
	tokFleet := f.mint("observer-b", f.res, "metis:message metis:fleet", time.Now().Add(time.Hour), nil)
	code, out := f.call(tokFleet, "2024-11-05", mcpCallBody(1, "metis_agents", map[string]any{}))
	if code != http.StatusOK || !strings.Contains(mcpHTTPText(out), "scopeF") {
		t.Fatalf("fleet observer must see fleet sessions: %d %s", code, mcpHTTPText(out))
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Remote provision must be scope-gated: a message-scoped token (or no
// token) cannot reach metis_provision over the HTTP transport.
func TestMCPHTTPProvisionNeedsProvisionScope(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	weak := f.mint("op-1", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	_, out := f.call(weak, "2024-11-05", mcpCallBody(1, "metis_provision", map[string]any{
		"session": "rp1", "actor": "cloud", "request_id": "rp-1",
		"run": "rr", "harness": "muse-code", "backend": "tmux",
		"workdir": t.TempDir()}))
	errObj, hasErr := out["error"]
	if !hasErr {
		t.Fatalf("message-scoped provision admitted: %v", out)
	}
	raw, _ := json.Marshal(errObj)
	if !strings.Contains(string(raw), "metis:provision") {
		t.Fatalf("refusal must name the missing provision scope: %s", raw)
	}
}

// A cloud controller holding the provision scope and the admin
// capability provisions a REAL muse-code echo worker through the
// HTTP transport: the worker appears in metis_agents, replays
// converge, conflicts refuse, empty/unsupported harnesses,
// explicit commands, and unregistered roots are refused — all
// without restarting anything. A parked sleeper or fake tmux
// session does NOT qualify this path.
func TestMCPHTTPProvisionRoundTrip(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	tok := f.mint("op-1", f.res, "metis:provision", time.Now().Add(time.Hour), nil)
	prov := func(id any, args map[string]any) map[string]any {
		_, out := f.call(tok, "2024-11-05", mcpCallBody(id, "metis_provision", args))
		return out
	}
	// Real curated launch: muse-code on the echo provider (no
	// inference spend) under the managed PTY supervisor.
	base := map[string]any{
		"session": "rprov", "actor": "cloud", "request_id": "rprov-1",
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "model": "spark", "backend": "pty"}
	out := prov(1, base)
	if _, hasErr := out["error"]; hasErr {
		t.Fatalf("provision refused: %v", out["error"])
	}
	res, _ := out["result"].(map[string]any)
	inner := map[string]any{}
	if content, _ := res["content"].([]any); len(content) > 0 {
		if text, _ := content[0].(map[string]any)["text"].(string); text != "" {
			_ = json.Unmarshal([]byte(text), &inner)
		}
	}
	if inner["state"] != "running" || inner["session"] != "rprov" {
		t.Fatalf("provision must admit running with the session identity: %v", res)
	}
	// Hygiene: the supervisor daemonizes, so a failure after this
	// point must still stop what was started — otherwise the
	// orphan breaks later runs by holding the session name.
	t.Cleanup(func() { _ = ptyStop("rprov") })
	// The worker is the curated muse echo launcher, alive now.
	meta, merr := ptyGet("rprov")
	if merr != nil {
		t.Fatalf("pty get: %v", merr)
	}
	if !ptySupervisorAlive(meta) || !ptyWorkerAlive(meta) {
		t.Fatalf("remote-provisioned worker must be alive: %+v", meta.Status)
	}
	if !strings.Contains(meta.Command, "muse") || !strings.Contains(meta.Command, "echo") {
		t.Fatalf("worker must run the curated muse echo launcher, got %q", meta.Command)
	}
	// Visible through the same transport: no restart needed.
	_, agents := f.call(tok, "2024-11-05", mcpCallBody(2, "metis_agents", map[string]any{}))
	araw, _ := json.Marshal(agents)
	if !strings.Contains(string(araw), "rprov") {
		t.Fatalf("provisioned worker missing from metis_agents: %s", araw)
	}
	// Identical replay converges on the original: no second worker.
	out = prov(3, base)
	if _, hasErr := out["error"]; hasErr {
		t.Fatalf("replay refused: %v", out["error"])
	}
	// Same key, different session: conflict, never the original.
	conflict := map[string]any{
		"session": "rprov2", "actor": "cloud", "request_id": "rprov-1",
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "backend": "pty"}
	out = prov(4, conflict)
	if _, hasErr := out["error"]; hasErr {
		t.Fatalf("conflict must arrive as domain isError, not RPC error: %v", out)
	}
	raw, _ := json.Marshal(out["result"])
	if !strings.Contains(string(raw), "conflict") {
		t.Fatalf("conflicting reuse admitted: %s", raw)
	}
	// Remote arbitrary commands stay prohibited even with scope+cap.
	cmd := map[string]any{
		"session": "rcmd", "actor": "cloud", "request_id": "rcmd-1",
		"run": "rr", "backend": "pty", "workdir": work,
		"harness": "muse-code", "command": "sleep 60"}
	out = prov(5, cmd)
	raw, _ = json.Marshal(out["result"])
	if !strings.Contains(string(raw), "explicit commands are refused") {
		t.Fatalf("remote explicit command admitted: %s", raw)
	}
	// An empty harness would admit a parked sleeper locally; the
	// remote boundary refuses it.
	noharness := map[string]any{
		"session": "rpark", "actor": "cloud", "request_id": "rpark-1",
		"run": "rr", "workdir": work, "backend": "pty"}
	out = prov(6, noharness)
	raw, _ = json.Marshal(out["result"])
	if !strings.Contains(string(raw), "explicit harness") {
		t.Fatalf("remote empty harness admitted: %s", raw)
	}
	// Unqualified harnesses stay refused on this transport.
	badharness := map[string]any{
		"session": "rone", "actor": "cloud", "request_id": "rone-1",
		"run": "rr", "workdir": work, "backend": "pty",
		"harness": "claude"}
	out = prov(7, badharness)
	raw, _ = json.Marshal(out["result"])
	if !strings.Contains(string(raw), "not qualified") {
		t.Fatalf("remote unqualified harness admitted: %s", raw)
	}
	// Unregistered roots stay refused.
	bad := map[string]any{
		"session": "rbad", "actor": "cloud", "request_id": "rbad-1",
		"run": "rr", "workdir": t.TempDir(), "harness": "muse-code",
		"provider": "echo", "backend": "pty"}
	out = prov(8, bad)
	raw, _ = json.Marshal(out["result"])
	if !strings.Contains(string(raw), "registered project root") {
		t.Fatalf("unregistered root admitted: %s", raw)
	}
	if code, _ := f.call(tok, "2024-11-05", mcpCallBody(9, "metis_agents", map[string]any{})); code != http.StatusOK {
		t.Fatalf("transport unhealthy after refusals: %d", code)
	}
	// The provision appears in the controller timeline under the
	// verified JWT identity — origin, target, request ID, and
	// outcome all recorded, nothing invented.
	feed, ferr := listLatestOrchEvents(500)
	if ferr != nil {
		t.Fatalf("feed read: %v", ferr)
	}
	seen := map[string]string{}
	for _, e := range feed {
		if e.EventType == OrchProvisionRequested && (e.RequestID == "rprov-1" || e.RequestID == "rcmd-1" || e.RequestID == "rbad-1") {
			seen[e.RequestID] = e.OriginPrincipal + "|" + e.TargetAgent + "|" + e.Disposition
		}
	}
	if got := seen["rprov-1"]; got != "jwt:op-1|rprov|running" && got != "jwt:op-1|rprov|admitted" {
		t.Fatalf("admitted provision timeline entry wrong: %q (all: %v)", got, seen)
	}
	if got := seen["rcmd-1"]; !strings.Contains(got, "jwt:op-1") || !strings.Contains(got, "refused") {
		t.Fatalf("refused explicit-command timeline entry wrong: %q", got)
	}
	if got := seen["rbad-1"]; !strings.Contains(got, "jwt:op-1") || !strings.Contains(got, "refused") {
		t.Fatalf("refused root timeline entry wrong: %q", got)
	}
	// Replay convergence does not duplicate the timeline entry:
	// (request_id, event_type) idempotency returns the original.
	n := 0
	for _, e := range feed {
		if e.EventType == OrchProvisionRequested && e.RequestID == "rprov-1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("replay duplicated the timeline entry %d times", n)
	}
}

package main

// provision_p0_test.go: P0-A/B/C failing-first.
//
// P0-A: metis_provision with harness=muse-native must return an
// explicit capability refusal, never panic on a nil BuildCommand.
// P0-B: a successful metis_bind must not print to stdout (the
// JSON-RPC stream); protocol operations return typed results.
// P0-C: provision replay with a changed payload is a conflict, and
// the durable intent precedes the launch (Reserved→Starting→
// Running) so a crash cannot orphan or duplicate a session.

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestResolveWorkerCommandNativeRefuses(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	_, err := resolveWorkerCommand("muse-native", "", work, "do thing", "")
	if err == nil {
		t.Fatal("muse-native has no shell command builder; must refuse explicitly, not panic")
	}
	if !strings.Contains(err.Error(), "muse-native") && !strings.Contains(err.Error(), "native") {
		t.Fatalf("refusal must name the native path: %v", err)
	}
	// Shell adapters keep working.
	cmd, err := resolveWorkerCommand("demo", "", work, "do thing", "")
	if err != nil || cmd == "" {
		t.Fatalf("shell adapter must still resolve: %q %v", cmd, err)
	}
}

func TestMCPBindKeepsStdoutClean(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	installFakeTmux(t, "bind-clean")
	markFakeTmuxDead(t)
	seedOwnedSession(t, "wb", "bind-clean")

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	res, callErr := mcpCallTool("metis_bind", mustJSON(t, map[string]any{
		"session": "wb", "profile": "", "provider": "echo",
		"actor": "orch", "request_id": "clean-1",
	}))
	_ = w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	if callErr != nil {
		t.Fatalf("bind: %v", callErr)
	}
	if res.IsError {
		t.Fatalf("bind isError: %s", res.Content[0].Text)
	}
	if out := buf.String(); out != "" {
		t.Fatalf("bind contaminated stdout with %q; protocol ops must not print", out)
	}
	if got := mcpTaskText(t, res)["bound"]; got != true {
		t.Fatalf("bind must report bound:true, got %v", got)
	}
}

func TestMCPProvisionReplayConflict(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-conflict")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	args := map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "pc1",
		"workdir": work, "command": "sleep 60",
	}
	if _, err := mcpCallTool("metis_provision", mustJSON(t, args)); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Exact replay converges on the original.
	same, err := mcpCallTool("metis_provision", mustJSON(t, args))
	if err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	if same.IsError {
		t.Fatalf("exact replay isError: %s", same.Content[0].Text)
	}
	if got := mcpTaskText(t, same)["session"]; got != "w1" {
		t.Fatalf("exact replay must return the original session, got %v", got)
	}
	// Same key, different session: conflict, not the original.
	diff := map[string]any{
		"run": "r1", "session": "w2", "objective": "do thing",
		"actor": "orch", "request_id": "pc1",
		"workdir": work, "command": "sleep 60",
	}
	res, err := mcpCallTool("metis_provision", mustJSON(t, diff))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused request ID with a different session must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if _, dup := loadRegistry()["w2"]; dup {
		t.Fatal("conflicting replay must not create a second session")
	}
}

func TestMCPProvisionResultStates(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-states")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	base := map[string]any{
		"run": "rs", "session": "ws", "objective": "do thing",
		"actor": "orch", "request_id": "ps1",
		"workdir": work, "command": "sleep 60",
	}
	first, err := mcpCallTool("metis_provision", mustJSON(t, base))
	if err != nil || first.IsError {
		t.Fatalf("provision: %v %v", err, first)
	}
	got := mcpTaskText(t, first)
	if got["state"] != "running" || got["replay"] != false {
		t.Fatalf("fresh provision must report state=running replay=false, got %v", got)
	}
	again, err := mcpCallTool("metis_provision", mustJSON(t, base))
	if err != nil || again.IsError {
		t.Fatalf("replay: %v %v", err, again)
	}
	rg := mcpTaskText(t, again)
	if rg["state"] != "running" || rg["replay"] != true {
		t.Fatalf("replay must report state=running replay=true, got %v", rg)
	}
	if _, dup := rg["reconciled"]; dup {
		t.Fatalf("clean replay must not claim reconciliation: %v", rg)
	}
}

func TestMCPProvisionIntentPrecedesLaunch(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-intent")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r2", "session": "wi", "objective": "do thing",
		"actor": "orch", "request_id": "pi1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil || res.IsError {
		t.Fatalf("provision: %v %v", err, res)
	}
	var state string
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT state FROM mcp_provision WHERE request_id = 'pi1'`).Scan(&state); err != nil {
		t.Fatalf("intent row missing: %v", err)
	}
	if state != "running" {
		t.Fatalf("terminal provision state = %q, want running", state)
	}
	// A reserved intent plus a live session reconciles to the
	// original without launching a duplicate.
	if _, err := db.Exec(`INSERT INTO mcp_provision (request_id, session, run, objective, workdir, harness, command, model, state, created)
		VALUES ('pi2','wr','r2','do','` + work + `','','sleep 60','','reserved',?)`, nowUTC()); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	if err := startSession("wr", work, "sleep 60", "custom", ""); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	res2, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r2", "session": "wr", "objective": "do",
		"actor": "orch", "request_id": "pi2",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res2.IsError {
		t.Fatalf("reconcile isError: %s", res2.Content[0].Text)
	}
	out2 := mcpTaskText(t, res2)
	if got := out2["session"]; got != "wr" {
		t.Fatalf("reconcile must return the original session, got %v", got)
	}
	if out2["reconciled"] != true || out2["state"] != "running" {
		t.Fatalf("crash reconcile must report reconciled=true state=running, got %v", out2)
	}
	if err := db.QueryRow(`SELECT state FROM mcp_provision WHERE request_id = 'pi2'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("reconciled state = %q err %v, want running", state, err)
	}
	if n := len(loadRegistry()); n != 2 {
		t.Fatalf("reconcile must not duplicate the session, got %d sessions", n)
	}
}

func TestBindRetryConvergesSameRequestID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	defer h.Close()
	work := t.TempDir()
	// The server requires UUIDv7 command IDs; mint a real one so
	// the retry exercises dedup, not input validation.
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatalf("command id: %v", err)
	}
	first, _, err := museStartSessionWithID(ctx, h, "retry-me", work, museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, _, err := museStartSessionWithID(ctx, h, "retry-me", work, museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if first != second {
		t.Fatalf("bind retry minted a duplicate native session: %q vs %q", first, second)
	}
}

package main

// provision_backend_test.go: package 3 failing-first.
//
// metis_provision must route by explicit backend (tmux|pty), with
// muse-native refused toward metis_bind. A replayed request_id with
// a different backend is an exact-payload conflict, never a second
// session. PTY launches reuse ptyStart's own generation fencing;
// reconcile converges on a live supervisor instead of duplicating.

import (
	"strings"
	"testing"
)

func ptyRowCount(t *testing.T, name string) int {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pty_sessions WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func provisionState(t *testing.T, requestID string) string {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var s string
	if err := db.QueryRow(`SELECT state FROM mcp_provision WHERE request_id = ?`, requestID).Scan(&s); err != nil {
		t.Fatalf("provision row missing: %v", err)
	}
	return s
}

func TestMCPProvisionPtyBackend(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pty-prov")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rpty", "session": "pp1", "objective": "do thing",
		"actor": "orch", "request_id": "pty-1", "backend": "pty",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("pty provision: %v", err)
	}
	if res.IsError {
		t.Fatalf("pty provision isError: %s", res.Content[0].Text)
	}
	if got := mcpTaskText(t, res)["session"]; got != "pp1" {
		t.Fatalf("pty provision must return the session, got %v", got)
	}
	if provisionState(t, "pty-1") != "running" {
		t.Fatal("pty provision must reach running")
	}
	// Observable through the unified listing under its backend name.
	agents, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents.Content[0].Text, `"pty:pp1"`) {
		t.Fatalf("pty session missing from unified listing: %s", agents.Content[0].Text)
	}
	// Exact replay converges without a second supervisor row.
	same, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rpty", "session": "pp1", "objective": "do thing",
		"actor": "orch", "request_id": "pty-1", "backend": "pty",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil || same.IsError {
		t.Fatalf("pty replay: %v %v", err, same)
	}
	if ptyRowCount(t, "pp1") != 1 {
		t.Fatal("pty replay must not duplicate the supervisor row")
	}
	// Hygiene (F-17): a provisioned PTY supervisor is daemonized by
	// design, so the test must stop what it started — otherwise the
	// daemon leaks past the test binary.
	meta, err := ptyGet("pp1")
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyStop("pp1"); err != nil {
		t.Fatalf("cleanup stop pp1: %v", err)
	}
	requireSupervisorReaped(t, meta)
}

func TestMCPProvisionBackendConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pty-conflict")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	if _, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rc", "session": "c1", "objective": "do thing",
		"actor": "orch", "request_id": "bc1", "backend": "tmux",
		"workdir": work, "command": "sleep 60",
	})); err != nil {
		t.Fatalf("tmux provision: %v", err)
	}
	// Same key, same name, different backend: conflict.
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rc", "session": "c1", "objective": "do thing",
		"actor": "orch", "request_id": "bc1", "backend": "pty",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused request ID with a different backend must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if ptyRowCount(t, "c1") != 0 {
		t.Fatal("conflicting replay must not start a PTY supervisor")
	}
}

func TestMCPProvisionNativeRefused(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	for _, args := range []map[string]any{
		{"run": "rn", "session": "n1", "objective": "do", "actor": "orch",
			"request_id": "bn1", "backend": "muse-native", "workdir": work, "command": "sleep 60"},
		{"run": "rn", "session": "n2", "objective": "do", "actor": "orch",
			"request_id": "bn2", "backend": "tmux", "harness": "muse-native",
			"workdir": work, "command": "sleep 60"},
	} {
		res, err := mcpCallTool("metis_provision", mustJSON(t, args))
		if err != nil {
			t.Fatalf("native refusal must arrive as isError, not RPC error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("native provisioning must refuse, got: %s", res.Content[0].Text)
		}
		if text := res.Content[0].Text; !strings.Contains(text, "bind") {
			t.Fatalf("native refusal must point at the bind path: %s", text)
		}
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMcpProvisionTable(db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bn1", "bn2"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_provision WHERE request_id = ?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("refused provision %s must leave no intent row", id)
		}
	}
}

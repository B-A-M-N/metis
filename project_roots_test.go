package main

// project_roots_test.go: P1-2 failing-first.
//
// A model caller must not gain arbitrary command execution by
// passing a workdir and a token. metis_provision enforces two
// gates: the workdir must sit under a locally registered project
// root, and an explicit custom command additionally requires that
// root's custom-command capability. Registration itself stays
// CLI-only: no MCP tool can authorize its own target.

import (
	"strings"
	"testing"
)

func mustRegisterRoot(t *testing.T, dir string, custom bool) string {
	t.Helper()
	root, err := registerProjectRoot(dir, custom)
	if err != nil {
		t.Fatalf("register root: %v", err)
	}
	return root
}

func TestMCPProvisionRefusesUnregisteredRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "noroot")
	markFakeTmuxDead(t)
	work := t.TempDir()
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "nr1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("provision into an unregistered root must refuse")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "project root") {
		t.Fatalf("refusal must name project-root registration: %s", text)
	}
	if _, ok := loadRegistry()["w1"]; ok {
		t.Fatal("refused provision must start no session")
	}
}

func TestMCPProvisionCustomCommandNeedsCapability(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "cap")
	markFakeTmuxDead(t)
	plain := t.TempDir()
	mustRegisterRoot(t, plain, false)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "nc1",
		"workdir": plain, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("explicit custom command on a plain root must refuse")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "custom") {
		t.Fatalf("refusal must name the custom-command capability: %s", text)
	}
	if _, ok := loadRegistry()["w1"]; ok {
		t.Fatal("refused provision must start no session")
	}
}

func TestMCPProvisionCustomCommandAllowedOnCapableRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "capok")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "nc2",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil || res.IsError {
		t.Fatalf("capable root must admit custom commands: %v %v", err, res)
	}
}

func TestMCPProvisionHarnessCommandAllowedOnPlainRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "hroot")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, false)
	// Adapter-built commands are curated invocations, not custom
	// commands: a plain root admits them.
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "nh1",
		"workdir": work, "harness": "demo",
	}))
	if err != nil || res.IsError {
		t.Fatalf("plain root must admit harness-built commands: %v %v", err, res)
	}
}

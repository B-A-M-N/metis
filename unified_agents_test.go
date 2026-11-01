package main

// unified_agents_test.go: package 3 failing-first.
//
// metis_agents must enumerate the unified session view (tmux
// registry + managed PTY supervisors), not the tmux registry alone.
// A PTY-backed worker invisible to the model-facing listing is a
// split-brain observability boundary: the controller can act on a
// session the model cannot see.

import (
	"strings"
	"testing"
)

func seedPtyRow(t *testing.T, name, workdir string) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, logpath, owner, status, heartbeat, generation, harness, model)
		VALUES (?, ?, 'sleep 60', ?, 'test-owner', 'starting', ?, 0, 'demo', '')`,
		name, workdir, ptyLogPath(name), nowUTC()); err != nil {
		t.Fatal(err)
	}
}

func TestMCPAgentsListsUnifiedSessions(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "unified-owner")
	markFakeTmuxDead(t)
	seedOwnedSession(t, "tm", "unified-owner")
	seedPtyRow(t, "pw", t.TempDir())

	res, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	if res.IsError {
		t.Fatalf("agents isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	agents, _ := out["agents"].([]any)
	byName := map[string]map[string]any{}
	for _, a := range agents {
		m, _ := a.(map[string]any)
		byName[m["name"].(string)] = m
	}
	tm, ok := byName["tm"]
	if !ok {
		t.Fatalf("tmux session missing from unified listing: %v", out)
	}
	if tm["backend"] != "tmux" {
		t.Fatalf("tmux session backend = %v, want tmux", tm["backend"])
	}
	pw, ok := byName["pty:pw"]
	if !ok {
		t.Fatalf("PTY session missing from unified listing (tmux-only view): %v", out)
	}
	if pw["backend"] != "pty" {
		t.Fatalf("PTY session backend = %v, want pty", pw["backend"])
	}
	if pw["workdir"] == "" {
		t.Fatal("PTY session must project its workdir")
	}
	if total, _ := out["total"].(float64); int(total) != 2 {
		t.Fatalf("total = %v, want 2 (tmux + pty)", out["total"])
	}
	// Deterministic order: sorted by display name.
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	first, _ := agents[0].(map[string]any)
	second, _ := agents[1].(map[string]any)
	if first["name"] != "pty:pw" || second["name"] != "tm" {
		t.Fatalf("listing not sorted: %v, %v", first["name"], second["name"])
	}
}

func TestMCPProjectsListsUnifiedSessions(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "unified-proj")
	markFakeTmuxDead(t)
	work := t.TempDir()
	reg := loadRegistry()
	reg["tm"] = Session{Name: "tm", Tmux: tmuxName("tm"), Workdir: work, Owner: "unified-proj"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	seedPtyRow(t, "pp", work)

	res, err := mcpCallTool("metis_projects", mustJSON(t, map[string]any{"workdir": work}))
	if err != nil {
		t.Fatalf("projects: %v", err)
	}
	if res.IsError {
		t.Fatalf("projects isError: %s", res.Content[0].Text)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, `"tm"`) || !strings.Contains(text, `"pty:pp"`) {
		t.Fatalf("project inspect must list tmux and PTY sessions: %s", text)
	}
}

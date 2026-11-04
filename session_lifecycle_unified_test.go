package main

// session_lifecycle_unified_test.go: P0 unified lifecycle (failing-first).
//
// metis_terminal status and metis_agents collapse backend lifecycles to
// an alive bool, conflating stopped/failed/unknown. The unified
// projection maps each backend's observed state to one vocabulary —
// running, ready, stopped, failed, unknown — with task occupancy
// reported separately: an idle worker with a one-shot harness is
// running, never ready, and a busy interactive worker stays ready.

import (
	"testing"
)

func TestUnifiedLifecyclePtyPure(t *testing.T) {
	cases := []struct {
		classified  string
		exitCode    int
		interactive bool
		wantLife    string
		wantAlive   bool
	}{
		{"running", 0, true, UnifiedReady, true},
		{"running", 0, false, UnifiedRunning, true},
		{"exited", 0, true, UnifiedStopped, false},
		{"exited", 1, true, UnifiedFailed, false},
		{"gone", -1, true, UnifiedStopped, false},
		{"worker-gone", -1, true, UnifiedFailed, false},
		{"orphaned", -1, true, UnifiedUnknown, false},
		{"starting", -1, true, UnifiedUnknown, false},
		{"", -1, true, UnifiedUnknown, false},
	}
	for _, c := range cases {
		gotLife, gotAlive := lifecycleForPty(c.classified, c.exitCode, c.interactive)
		if gotLife != c.wantLife || gotAlive != c.wantAlive {
			t.Fatalf("lifecycleForPty(%q,%d,%v) = (%q,%v), want (%q,%v)",
				c.classified, c.exitCode, c.interactive, gotLife, gotAlive, c.wantLife, c.wantAlive)
		}
	}
}

func TestUnifiedLifecycleTmuxPure(t *testing.T) {
	if life, alive := lifecycleForTmux(true, false, true); life != UnifiedStopped || alive {
		t.Fatalf("stopped tmux = (%q,%v), want (stopped,false)", life, alive)
	}
	if life, alive := lifecycleForTmux(false, true, true); life != UnifiedReady || !alive {
		t.Fatalf("running interactive tmux = (%q,%v), want (ready,true)", life, alive)
	}
	if life, alive := lifecycleForTmux(false, true, false); life != UnifiedRunning || !alive {
		t.Fatalf("running one-shot tmux = (%q,%v), want (running,true) — idle is not ready", life, alive)
	}
	if life, alive := lifecycleForTmux(false, false, true); life != UnifiedUnknown || alive {
		t.Fatalf("dead-but-unrecorded tmux = (%q,%v), want (unknown,false)", life, alive)
	}
}

func TestUnifiedLifecycleNativePure(t *testing.T) {
	if life, alive := lifecycleForNative(logicalStopped, true); life != UnifiedStopped || alive {
		t.Fatalf("stopped native = (%q,%v), want (stopped,false)", life, alive)
	}
	if life, alive := lifecycleForNative(logicalActive, true); life != UnifiedReady || !alive {
		t.Fatalf("active bound native = (%q,%v), want (ready,true)", life, alive)
	}
	if life, alive := lifecycleForNative(logicalActive, false); life != UnifiedRunning || !alive {
		t.Fatalf("active unbound native = (%q,%v), want (running,true)", life, alive)
	}
	if life, alive := lifecycleForNative("weird", true); life != UnifiedUnknown || alive {
		t.Fatalf("weird native state = (%q,%v), want (unknown,false)", life, alive)
	}
}

func TestHarnessInteractive(t *testing.T) {
	for _, h := range []string{"muse-code", "muse-native"} {
		if !harnessInteractive(h) {
			t.Fatalf("harness %q must be interactive-ready", h)
		}
	}
	for _, h := range []string{"", "demo", "codex", "claude", "muse", "hermes", "bogus"} {
		if harnessInteractive(h) {
			t.Fatalf("harness %q must NOT be interactive-ready", h)
		}
	}
}

// Occupancy uses only the dispatch-canonical spelling per backend:
// a task for tmux "w" must not mark pty:w busy (identity collision).
func TestOccupancyCanonicalSpellings(t *testing.T) {
	busy := map[string]bool{"w": true}
	if got := sessionOccupancyFor("w", busy, true); got != OccupancyBusy {
		t.Fatalf("tmux w occupancy = %q, want busy", got)
	}
	if got := sessionOccupancyFor("pty:w", busy, true); got != OccupancyIdle {
		t.Fatalf("pty:w occupancy must ignore tmux w task, got %q (collision)", got)
	}
	busyPty := map[string]bool{"pty:w": true}
	if got := sessionOccupancyFor("pty:w", busyPty, true); got != OccupancyBusy {
		t.Fatalf("pty:w occupancy = %q, want busy", got)
	}
	if got := sessionOccupancyFor("w", busyPty, true); got != OccupancyIdle {
		t.Fatalf("tmux w occupancy must ignore pty:w task, got %q (collision)", got)
	}
	if got := sessionOccupancyFor("w", busy, false); got != OccupancyUnknown {
		t.Fatalf("unknown store occupancy = %q, want unknown", got)
	}
	// Native dispatches under the bare logical name.
	if got := sessionOccupancyFor("native:w", busy, true); got != OccupancyBusy {
		t.Fatalf("native:w occupancy = %q, want busy (bare canonical)", got)
	}
}

// Terminal status must carry lifecycle + occupancy; a seeded tmux
// session (no harness) is running/idle, never ready.
func TestTerminalStatusCarriesUnifiedLifecycle(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "lc-owner")
	seedOwnedSession(t, "lc", "lc-owner")

	tm := terminalResult(t, mustTerminal(t, "lc", "status", 0))
	if tm["unified_lifecycle"] == nil || tm["unified_lifecycle"] == "" {
		t.Fatalf("terminal status must carry lifecycle: %v", tm)
	}
	if tm["unified_lifecycle"] != UnifiedRunning {
		t.Fatalf("live tmux with no interactive harness must be running, got %v (%v)",
			tm["unified_lifecycle"], tm)
	}
	if tm["task_occupancy"] != OccupancyIdle {
		t.Fatalf("session with no open task must be idle, got %v (%v)", tm["task_occupancy"], tm)
	}
	if tm["alive"] != true {
		t.Fatalf("live tmux alive must stay true: %v", tm)
	}
}

// Occupancy follows an open task; lifecycle (readiness) does not.
func TestUnifiedLifecycleOccupancyFollowsOpenTask(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "busy-owner")
	seedOwnedSession(t, "busy1", "busy-owner")

	idle := terminalResult(t, mustTerminal(t, "busy1", "status", 0))
	if idle["task_occupancy"] != OccupancyIdle {
		t.Fatalf("idle session occupancy must be idle, got %v (%v)", idle["task_occupancy"], idle)
	}
	if idle["unified_lifecycle"] != UnifiedRunning {
		t.Fatalf("idle one-shot session lifecycle must be running, got %v", idle["unified_lifecycle"])
	}

	tk, err := createTask("run-busy", "busy1", "do it", "done")
	if err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET status = 'dispatched' WHERE id = ?`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	busy := terminalResult(t, mustTerminal(t, "busy1", "status", 0))
	if busy["task_occupancy"] != OccupancyBusy {
		t.Fatalf("session with dispatched task must be busy, got %v (%v)",
			busy["task_occupancy"], busy)
	}
	if busy["unified_lifecycle"] != UnifiedRunning {
		t.Fatalf("busy one-shot session must stay running (occupancy is separate), got %v",
			busy["unified_lifecycle"])
	}
}

// Agents listing must carry the same fields per session.
func TestAgentsCarryUnifiedLifecycle(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "agents-lc-owner")
	seedOwnedSession(t, "al", "agents-lc-owner")

	res, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatalf("agents: %v", err)
	}
	if res.IsError {
		t.Fatalf("agents isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	agents, _ := out["agents"].([]any)
	found := false
	for _, a := range agents {
		m, _ := a.(map[string]any)
		if m["name"] == "al" {
			found = true
			if m["unified_lifecycle"] != UnifiedRunning {
				t.Fatalf("agents lifecycle for live one-shot session = %v, want running (%v)",
					m["unified_lifecycle"], m)
			}
			if m["task_occupancy"] != OccupancyIdle {
				t.Fatalf("agents occupancy for idle session = %v, want idle (%v)",
					m["task_occupancy"], m)
			}
		}
	}
	if !found {
		t.Fatalf("session al missing from agents: %v", out)
	}
}

// A dispatched task for tmux "w" must not mark a same-name PTY
// session busy (backend identity collision), and vice versa.
func TestOccupancyNoCrossBackendCollision(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "col-owner")
	seedOwnedSession(t, "w", "col-owner")
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	if _, err := db.Exec(`INSERT INTO pty_sessions (name, workdir, command, logpath, owner, status, heartbeat, harness, model) VALUES (?, ?, ?, ?, ?, 'exited', ?, '', '')`,
		"w", work, "true", work+"/pty.log", "col-owner", nowUTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-col", "w", "do it", "done")
	if err != nil {
		t.Fatal(err)
	}
	db, err = openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET status = 'dispatched' WHERE id = ?`, tk.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	tm := terminalResult(t, mustTerminal(t, "pty:w", "status", 0))
	if tm["task_occupancy"] == OccupancyBusy {
		t.Fatalf("pty:w must not inherit tmux w occupancy: %v", tm)
	}
	tm2 := terminalResult(t, mustTerminal(t, "w", "status", 0))
	if tm2["task_occupancy"] != OccupancyBusy {
		t.Fatalf("tmux w must be busy: %v", tm2)
	}
}

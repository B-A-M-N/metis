package main

// provision_liveness_test.go: F-05 failing-first.
//
// ProvisionSessionLive/ProvisionLaunchVerified must not treat
// backend-registry presence as liveness. A replay over a
// dead, replaced, foreign, or stale session must fail explicitly —
// never adopt the corpse as live (running+reconciled). Each test
// below seeds a reserved (crashed) intent plus a backend record
// that mere presence would have adopted, and asserts the replay
// errors with the row left non-running and nothing duplicated.

import (
	"strings"
	"testing"
)

func seedReservedIntent(t *testing.T, requestID, session, run, objective, workdir, backend, harness, command, model string) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMcpProvisionTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO mcp_provision
		(request_id, session, run, objective, workdir, backend, harness, command, model, state, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'reserved', ?)`,
		requestID, session, run, objective, workdir, backend, harness, command, model, nowUTC()); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
}

func seedRegistryEntry(t *testing.T, name, owner, workdir string, stopped bool) {
	t.Helper()
	reg := loadRegistry()
	reg[name] = Session{Name: name, Tmux: tmuxName(name), Workdir: workdir, Command: "sleep 60", Owner: owner, Stopped: stopped}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
}

func seedCorpsePtyRow(t *testing.T, name, workdir, status string) {
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
		(name, workdir, command, pts, logpath, worker_pid, worker_start,
			supervisor_pid, supervisor_start, owner, status, exit_code, heartbeat, generation, harness, model)
		VALUES (?, ?, 'sleep 60', '', '', 0, 0, 0, 0, 'pty-owner', ?, -1, '', 0, '', '')`,
		name, workdir, status); err != nil {
		t.Fatalf("seed pty row: %v", err)
	}
}

// replayProvision replays one provision payload and returns the tool
// result. Callers assert refusal: must-arrive-as-isError, never RPC.
func replayProvision(t *testing.T, args map[string]any) mcpToolResult {
	t.Helper()
	res, err := mcpCallTool("metis_provision", mustJSON(t, args))
	if err != nil {
		t.Fatalf("replay must arrive as isError, not RPC error: %v", err)
	}
	return res
}

func provisionRowState(t *testing.T, requestID string) string {
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

// A dead session — tmux cannot bind the name to a session ID
// (garbage display-message) — must never be adopted as live, even
// though the registry still names it.
func TestProvisionReplayDeadSessionNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "dead-owner")
	markFakeTmuxDead(t)
	t.Setenv("FAKE_TMUX_ID", "garbage-no-such-session")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "wd", "dead-owner", work, false)
	seedReservedIntent(t, "dead1", "wd", "r1", "do", work, "tmux", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "r1", "session": "wd", "objective": "do",
		"actor": "orch", "request_id": "dead1",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a dead session must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "dead1"); got == provisionRunning {
		t.Fatal("dead session adopted as running; history is not liveness")
	}
	if n := len(loadRegistry()); n != 1 {
		t.Fatalf("refused replay must not duplicate the session, got %d", n)
	}
}

// A replaced session — the live tmux name now carries another
// owner's token — must never be adopted as live.
func TestProvisionReplayReplacedSessionNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "impostor")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "wr", "original-owner", work, false)
	seedReservedIntent(t, "repl1", "wr", "r1", "do", work, "tmux", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "r1", "session": "wr", "objective": "do",
		"actor": "orch", "request_id": "repl1",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a replaced session must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "repl1"); got == provisionRunning {
		t.Fatal("replaced session adopted as running; ownership mismatch is not liveness")
	}
	if got := loadRegistry()["wr"]; got.Owner != "original-owner" {
		t.Fatalf("refused replay must not touch the registry entry: %+v", got)
	}
}

// A foreign session — live, but with no ownership credential from
// this tool — must never be adopted as live.
func TestProvisionReplayForeignSessionNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "someone-else")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "wf", "", work, false)
	seedReservedIntent(t, "for1", "wf", "r1", "do", work, "tmux", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "r1", "session": "wf", "objective": "do",
		"actor": "orch", "request_id": "for1",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a foreign session must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "for1"); got == provisionRunning {
		t.Fatal("foreign session adopted as running; a record without a credential is read-only")
	}
}

// A stopped entry is history, even when every other field matches:
// the replay must fail explicitly instead of adopting it.
func TestProvisionReplayStoppedSessionNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "stop-owner")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "ws", "stop-owner", work, true)
	seedReservedIntent(t, "stop1", "ws", "r1", "do", work, "tmux", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "r1", "session": "ws", "objective": "do",
		"actor": "orch", "request_id": "stop1",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a stopped session must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "stop1"); got == provisionRunning {
		t.Fatal("stopped session adopted as running")
	}
	if got := loadRegistry()["ws"]; !got.Stopped {
		t.Fatal("refused replay must leave the stopped mark alone")
	}
}

// A registry entry pointing at another root is a different session:
// workspace identity is part of liveness.
func TestProvisionReplayStaleWorkdirNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "home-owner")
	markFakeTmuxDead(t)
	elsewhere := t.TempDir()
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "wh", "home-owner", elsewhere, false)
	seedReservedIntent(t, "home1", "wh", "r1", "do", work, "tmux", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "r1", "session": "wh", "objective": "do",
		"actor": "orch", "request_id": "home1",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a re-homed entry must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "home1"); got == provisionRunning {
		t.Fatal("re-homed entry adopted as running; workspace identity is liveness")
	}
}

// A PTY row that merely SAYS running with no live supervisor or
// worker behind it (a corpse after a reboot) must never be adopted:
// liveness is proven aliveness plus the intent's workspace, not a
// status string.
func TestProvisionReplayPtyCorpseNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pty-corpse")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedCorpsePtyRow(t, "pc", work, "running")
	seedReservedIntent(t, "pcorp1", "pc", "rpty", "do", work, "pty", "", "sleep 60", "")

	res := replayProvision(t, map[string]any{
		"run": "rpty", "session": "pc", "objective": "do",
		"actor": "orch", "request_id": "pcorp1", "backend": "pty",
		"workdir": work, "command": "sleep 60",
	})
	if !res.IsError {
		t.Fatalf("replay over a PTY corpse must refuse, got: %s", res.Content[0].Text)
	}
	if got := provisionRowState(t, "pcorp1"); got == provisionRunning {
		t.Fatal("PTY corpse adopted as running; a status string with no live process is not liveness")
	}
	if ptyRowCount(t, "pc") != 1 {
		t.Fatal("refused replay must not duplicate the supervisor row")
	}
}

// A stopped logical record is terminal: a native replay must fail
// explicitly instead of adopting it, and the record stays stopped.
func TestProvisionReplayStoppedNativeNeverAdopted(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, false)
	wsRoot, err := canonicalRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := claimLogicalSession(logicalSession{
		Name: "ns", Backend: provisionBackendNative, Workspace: wsRoot,
		Harness: "muse-native", Provider: "echo",
	})
	if err != nil || !claimed {
		t.Fatalf("seed logical session: claimed=%v err=%v", claimed, err)
	}
	if err := stopLogicalSession("ns"); err != nil {
		t.Fatalf("seed stop: %v", err)
	}
	seedReservedIntent(t, "ns1", "ns", "rn", "do", work, "muse-native", "muse-native", "", "")

	res := replayProvision(t, map[string]any{
		"run": "rn", "session": "ns", "objective": "do",
		"actor": "orch", "request_id": "ns1", "backend": "muse-native",
		"workdir": work, "provider": "echo",
	})
	if !res.IsError {
		t.Fatalf("replay over a stopped logical session must refuse, got: %s", res.Content[0].Text)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "stopped") {
		t.Fatalf("refusal must name the stopped record: %s", text)
	}
	if got := provisionRowState(t, "ns1"); got == provisionRunning {
		t.Fatal("stopped logical session adopted as running")
	}
	cur, found, err := loadLogicalSession("ns")
	if err != nil || !found || cur.State != logicalStopped {
		t.Fatalf("refused replay must leave the stopped record alone: %+v %v %v", cur, found, err)
	}
}

// A live logical record with a different identity is a conflict,
// not an adoption: the replay fails naming the conflict and the
// established record is untouched.
func TestProvisionNativeConflictingLogicalIdentity(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	w1 := t.TempDir()
	w2 := t.TempDir()
	mustRegisterRoot(t, w1, false)
	mustRegisterRoot(t, w2, false)
	ws1, err := canonicalRoot(w1)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := claimLogicalSession(logicalSession{
		Name: "nc", Backend: provisionBackendNative, Workspace: ws1,
		Harness: "muse-native", Provider: "echo",
	})
	if err != nil || !claimed {
		t.Fatalf("seed logical session: claimed=%v err=%v", claimed, err)
	}
	seedReservedIntent(t, "nc1", "nc", "rn", "do", w2, "muse-native", "muse-native", "", "")

	res := replayProvision(t, map[string]any{
		"run": "rn", "session": "nc", "objective": "do",
		"actor": "orch", "request_id": "nc1", "backend": "muse-native",
		"workdir": w2, "provider": "echo",
	})
	if !res.IsError {
		t.Fatalf("replay over a foreign logical identity must refuse, got: %s", res.Content[0].Text)
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("refusal must name the conflict: %s", text)
	}
	if got := provisionRowState(t, "nc1"); got == provisionRunning {
		t.Fatal("foreign logical identity adopted as running")
	}
	cur, _, _ := loadLogicalSession("nc")
	if cur.Workspace != ws1 {
		t.Fatalf("refused replay must not move the established record: %+v", cur)
	}
}

// A healthy reserved intent — owned, unstopped, workspace-matched —
// still reconciles to its live session without launching a second
// one. The new validation must not refuse the crash-recovery case
// it exists to serve.
func TestProvisionReplayHealthyAdoptsLive(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "healthy-owner")
	// No dead marker: this pins the live-session adopt path (owned,
	// unstopped, workspace-matched entry + live backend ->
	// running+reconciled). A dead backend with a live-looking
	// registry entry is a distinct stale-entry case the service
	// refuses rather than adopting.
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	seedRegistryEntry(t, "ok", "healthy-owner", work, false)
	seedReservedIntent(t, "ok1", "ok", "r1", "do", work, "tmux", "", "sleep 60", "")

	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "ok", "objective": "do",
		"actor": "orch", "request_id": "ok1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("healthy reconcile: %v", err)
	}
	if res.IsError {
		t.Fatalf("healthy reconcile isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	if out["state"] != "running" || out["reconciled"] != true {
		t.Fatalf("healthy crash recovery must report running+reconciled, got %v", out)
	}
}

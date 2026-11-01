package main

// provision_service_test.go: P0-C failing-first tests for the
// provision decomposition.
//
//   - Stale transitions must not clobber a newer state: the
//     unconditional mcpProvisionSetState UPDATE lets a stale holder
//     overwrite running with failed. State changes need
//     expected-state+generation fencing.
//   - The workdir must be created AFTER the durable intent row
//     exists. A crash between directory creation and the commit
//     leaves files-without-commit; a replay must reconcile the
//     missing directory from the recorded intent instead of failing
//     the launch.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionStaleTransitionMustNotClobber(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
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
		VALUES ('clob1','wc','r1','do','/tmp/w','tmux','','sleep 60','','reserved',?)`, nowUTC()); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	// Legitimate claim, then a concurrent winner confirms running.
	committed, err := mcpProvisionSetState(db, "clob1", provisionReserved, 0, provisionStarting)
	if err != nil || !committed {
		t.Fatalf("claim: committed=%v err=%v", committed, err)
	}
	committed, err = mcpProvisionSetState(db, "clob1", provisionStarting, 1, provisionRunning)
	if err != nil || !committed {
		t.Fatalf("confirm: committed=%v err=%v", committed, err)
	}
	// A stale holder that still believes the row is starting at
	// generation 1 tries to record its own failure. The fence must
	// refuse, and the confirmed state must survive.
	committed, err = mcpProvisionSetState(db, "clob1", provisionStarting, 1, provisionFailed)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("stale generation must not commit")
	}
	var state string
	var generation int64
	if err := db.QueryRow(`SELECT state, generation FROM mcp_provision WHERE request_id = 'clob1'`).Scan(&state, &generation); err != nil {
		t.Fatal(err)
	}
	if state != provisionRunning {
		t.Fatalf("stale transition clobbered confirmed state: got %q, want %q", state, provisionRunning)
	}
	if generation != 2 {
		t.Fatalf("refused write must not bump the generation, got %d", generation)
	}
	// running is terminal: even a fresh expectation cannot leave it.
	committed, err = mcpProvisionSetState(db, "clob1", provisionRunning, 2, provisionStarting)
	if err != nil {
		t.Fatal(err)
	}
	if committed {
		t.Fatal("running must be terminal")
	}
}

func TestProvisionTransitionTable(t *testing.T) {
	allowed := [][2]string{
		{provisionReserved, provisionStarting},
		{provisionReserved, provisionRunning},
		{provisionStarting, provisionStarting},
		{provisionStarting, provisionRunning},
		{provisionStarting, provisionFailed},
		{provisionStarting, provisionUnknown},
		{provisionFailed, provisionStarting},
		{provisionFailed, provisionRunning},
		{provisionUnknown, provisionStarting},
		{provisionUnknown, provisionRunning},
	}
	for _, tc := range allowed {
		if !provisionTransitionAllowed(tc[0], tc[1]) {
			t.Fatalf("transition %q -> %q must be allowed", tc[0], tc[1])
		}
	}
	denied := [][2]string{
		{provisionRunning, provisionStarting},
		{provisionRunning, provisionRunning},
		{provisionRunning, provisionFailed},
		{provisionReserved, provisionFailed},
		{provisionReserved, provisionUnknown},
		{provisionFailed, provisionFailed},
		{provisionFailed, provisionUnknown},
		{provisionUnknown, provisionFailed},
		{"bogus", provisionStarting},
		{provisionStarting, "bogus"},
	}
	for _, tc := range denied {
		if provisionTransitionAllowed(tc[0], tc[1]) {
			t.Fatalf("transition %q -> %q must be denied", tc[0], tc[1])
		}
	}
}

func TestProvisionConflictCreatesNoWorkdir(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-conflict-dir")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	first, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "cw1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil || first.IsError {
		t.Fatalf("provision: %v %v", err, first)
	}
	// A conflicting replay must be refused from the durable row
	// before any directory is created: refusal leaves no
	// files-without-commit behind.
	rival := filepath.Join(work, "rival-dir")
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w2", "objective": "different",
		"actor": "orch", "request_id": "cw1",
		"workdir": rival, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused request ID with a different session must conflict")
	}
	if _, err := os.Stat(rival); !os.IsNotExist(err) {
		t.Fatalf("refused provision must not create its workdir, stat err = %v", err)
	}
}

func TestProvisionReservedIntentReconcilesMissingWorkdir(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-reconcile")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	missing := filepath.Join(work, "not-yet-created")
	abs, err := filepath.Abs(missing)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMcpProvisionTable(db); err != nil {
		t.Fatal(err)
	}
	// Durable intent without the directory: a crash between the
	// commit and directory creation (intent-without-files). The
	// replay must reconcile the missing directory from the
	// recorded intent and converge instead of failing the launch.
	if _, err := db.Exec(`INSERT INTO mcp_provision
		(request_id, session, run, objective, workdir, backend, harness, command, model, state, created)
		VALUES ('rec1','wrec','r1','do',?,'tmux','','sleep 60','','reserved',?)`, abs, nowUTC()); err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	db.Close()
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "wrec", "objective": "do",
		"actor": "orch", "request_id": "rec1",
		"workdir": missing, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.IsError {
		t.Fatalf("reconcile isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	if out["state"] != "running" {
		t.Fatalf("reconcile must reach running, got %v", out)
	}
	db2, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var state string
	if err := db2.QueryRow(`SELECT state FROM mcp_provision WHERE request_id = 'rec1'`).Scan(&state); err != nil || state != "running" {
		t.Fatalf("reconciled state = %q err %v, want running", state, err)
	}
}

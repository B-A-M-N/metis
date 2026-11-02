package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTaskLifecycle(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "sess1", "o1")
	seedOwnedSession(t, "sess2", "o1")
	seedOwnedSession(t, "s", "o1")
	if _, err := createTask("", "agent", "obj", ""); err == nil {
		t.Fatal("create without run should fail")
	}
	tk, err := createTask("run1", "agent1", "do thing", "done means done")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if tk.Status != TaskPending {
		t.Fatalf("new task should be pending, got %s", tk.Status)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "early", Source: SourceCLISubmission, Reporter: "test"}); err == nil {
		t.Fatal("report before dispatch should fail")
	}
	if err := dispatchTask(tk.ID, "sess1"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := dispatchTask(tk.ID, "sess2"); err == nil {
		t.Fatal("double dispatch should fail")
	}
	if err := markTaskWorking(tk.ID, 1, testReq("test")); err != nil {
		t.Fatalf("working: %v", err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "findings: ok", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != TaskReportReady || got.Report != "findings: ok" || got.Attempt != 1 || got.Agent != "sess1" {
		t.Fatalf("bad post-report state: %+v", got)
	}
	// Verification requires evidence: attach a file from the worker's
	// own workspace, then the passing verdict is allowed.
	sessWork := loadRegistry()["sess1"].Workdir
	if err := os.WriteFile(filepath.Join(sessWork, "out.txt"), []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, filepath.Join(sessWork, "out.txt")); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Hashes alone never verify: the criterion needs an independent
	// reviewer's PASS verdict citing the evidence.
	fileAllPassVerdicts(t, tk.ID, filepath.Join(sessWork, "out.txt"), "reviewer-01")
	if err := verifyTask(tk.ID, true, "", testReq("reviewer-01")); err != nil {
		t.Fatalf("verify with evidence: %v", err)
	}
	if err := cancelTask(tk.ID, testReq("test")); err == nil {
		t.Fatal("cancel of verified task should fail")
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "late", Source: SourceCLISubmission, Reporter: "test"}); err == nil {
		t.Fatal("report on terminal task should fail")
	}
	child, err := followupTask(tk.ID, "fix the rest", testReq("test"))
	if err != nil {
		t.Fatalf("followup: %v", err)
	}
	if child.Parent != tk.ID || child.Status != TaskPending || child.Run != "run1" {
		t.Fatalf("bad followup linkage: %+v", child)
	}
	if err := cancelTask(child.ID, testReq("test")); err != nil {
		t.Fatalf("cancel pending should work: %v", err)
	}
	if _, err := getTask("task-nonexistent"); err == nil {
		t.Fatal("get unknown should fail")
	}
	if err := dispatchTask("task-nonexistent", "s"); err == nil {
		t.Fatal("dispatch unknown should fail")
	}
	all, err := listTasks("")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: %v %+v", err, all)
	}
	none, err := listTasks("other-run")
	if err != nil || len(none) != 0 {
		t.Fatalf("list filter: %v %+v", err, none)
	}
	one, err := listTasks("run1")
	if err != nil || len(one) != 2 {
		t.Fatalf("list run1: %v %+v", err, one)
	}
	// Persistence across handles: reopen and read back.
	again, err := getTask(tk.ID)
	if err != nil || again.Status != TaskVerified {
		t.Fatalf("task not durable: %v %+v", err, again)
	}
}

func TestTaskVerifyFail(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")
	tk, err := createTask("r", "a", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "s"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "rep", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("test")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
}

func TestDispatchRejectsUnusableSession(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "o9")
	seedOwnedSession(t, "live", "o9")
	seedOwnedSession(t, "gone", "o9")
	reg := loadRegistry()
	stopped := reg["gone"]
	stopped.Stopped = true
	reg["gone"] = stopped
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("r", "a", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	reject := func(session string) {
		t.Helper()
		other, err := createTask("r", "a", "obj", "")
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatchTask(other.ID, session); err == nil {
			t.Fatalf("dispatch to %q should fail", session)
		}
	}
	reject("no-such-session")
	reject("gone")      // registered but stopped
	reject("pty-ghost") // no such PTY session
	// Kill the fake tmux session: the registry entry survives but the
	// worker is dead, so dispatch must refuse.
	if err := os.WriteFile(filepath.Join(os.Getenv("FAKE_TMUX_DIR"), "dead"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	reject("live")
	if err := dispatchTask(tk.ID, "gone"); err == nil {
		t.Fatal("dispatch to stopped session should fail")
	}
	// The refused task is still pending and dispatchable elsewhere.
	if got, _ := getTask(tk.ID); got.Status != TaskPending {
		t.Fatalf("refused dispatch changed status to %s", got.Status)
	}
}

func TestConcurrentDispatchOneWinner(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "o8")
	seedOwnedSession(t, "w", "o8")
	tk, err := createTask("r", "a", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if dispatchTask(tk.ID, "w") == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one dispatch winner, got %d", wins.Load())
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched || got.Attempt != 1 || got.Agent != "w" {
		t.Fatalf("bad post-race state: %+v", got)
	}
}

func TestCreateTaskIdempotent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTaskIdem("run-k", "agent", "obj", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTaskIdem("run-k", "agent", "obj", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same key returned different tasks: %s vs %s", a.ID, b.ID)
	}
	c, err := createTaskIdem("run-k", "agent", "obj", "", "k2")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == a.ID {
		t.Fatal("different keys must create different tasks")
	}
	// Same key in a different run is a different task.
	d, err := createTaskIdem("other-run", "agent", "obj", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID == a.ID {
		t.Fatal("same key in another run must not dedupe")
	}
	// Empty keys never dedupe.
	e, err := createTask("run-k", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := createTask("run-k", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == f.ID {
		t.Fatal("keyless creates must not dedupe")
	}
	// Concurrent creates with one key converge on a single row.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = createTaskIdem("run-race", "agent", "obj", "", "rk")
		}()
	}
	wg.Wait()
	all, err := listTasks("run-race")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("concurrent keyed creates left %d rows, want 1", len(all))
	}
}

// Databases created before the idempotency key existed must migrate on
// first open: old rows stay readable and keyed creates work afterwards.
func TestTaskKeyMigration(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := sql.Open("sqlite", tasksDBPath())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY, run TEXT NOT NULL, agent TEXT NOT NULL,
		objective TEXT NOT NULL, acceptance TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending', attempt INTEGER NOT NULL DEFAULT 0,
		parent TEXT NOT NULL DEFAULT '', report TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL, updated TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO tasks (id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated)
		VALUES ('task-old', 'r', 'a', 'obj', '', 'pending', 0, '', '', 't', 't')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := getTask("task-old")
	if err != nil {
		t.Fatalf("old row unreadable after migration: %v", err)
	}
	if got.Key != "" {
		t.Fatalf("old row should have empty key, got %q", got.Key)
	}
	a, err := createTaskIdem("r", "a", "obj", "", "mk")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTaskIdem("r", "a", "obj", "", "mk")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatal("keyed dedupe missing after migration")
	}
}

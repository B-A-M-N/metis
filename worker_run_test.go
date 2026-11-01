package main

// Slice 1c: the managed worker loop. Strictly mechanical — it claims
// one committed attempt for its own session, launches the provider
// as a child, files the observed result. No scheduling, no provider
// fallback, no inference about success.

import (
	"strings"
	"sync"
	"testing"
)

// fakeWorkerAdapter runs a fixed local command instead of a model.
// It stands in for a real provider CLI: same argv-exec contract,
// no quota, no network.
func fakeWorkerAdapter(argv ...string) Adapter {
	return Adapter{
		Harness:   "test-fake",
		OneShot:   true,
		ManagedArgv: func(workdir, prompt, model string) ([]string, error) {
			return argv, nil
		},
	}
}

// A dispatched task bound to the loop's session is consumed exactly
// once: claimed, executed as a child process, result filed under
// the attempt with an execution receipt.
func TestWorkerConsumesDispatchedTask(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW3")
	seedOwnedSession(t, "w", "pW3")
	tk, err := createTask("run-pW3", "w", "print hello", "output says hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo model-output"), testReq("op"))
	if err != nil {
		t.Fatalf("loop failed: %v", err)
	}
	if !ran {
		t.Fatal("loop must report it executed the task")
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskReportReady {
		t.Fatalf("expected report_ready, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND reporter LIKE 'worker-loop/%' AND source = 'runner-observation'`, tk.ID); n != 1 {
		t.Fatalf("loop must file a runner-observation under its own identity, got %d rows", n)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var exitCode int
	var pid int
	var digest string
	if err := db.QueryRow(`SELECT exit_code, pid, output_digest FROM execution_receipts WHERE task_id = ? AND attempt_no = 1`, tk.ID).Scan(&exitCode, &pid, &digest); err != nil {
		t.Fatalf("execution receipt missing: %v", err)
	}
	if exitCode != 0 || pid <= 0 || digest == "" {
		t.Fatalf("receipt must carry native exit status: exit=%d pid=%d digest=%q", exitCode, pid, digest)
	}
	if arts, err := currentAttemptArtifacts(tk.ID, 1); err != nil || len(arts) == 0 {
		t.Fatalf("worker output must be attached as attempt evidence: %v %+v", err, arts)
	}
}

// Two loops racing one task: exactly one executes. The loser is
// refused — no duplicate launch, no second execution receipt.
func TestWorkerClaimExactlyOnce(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW4")
	seedOwnedSession(t, "w", "pW4")
	tk, err := createTask("run-pW4", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op"))
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, r := range results {
		if r {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one loop must execute, got %d (%v)", wins, errs)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("exactly one execution receipt, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("exactly one history row, got %d", n)
	}
}

// A claim for a superseded attempt is stale and refused: the task
// moved on, the old claim must not touch it.
func TestWorkerStaleAttemptRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW5")
	seedOwnedSession(t, "w", "pW5")
	tk, err := createTask("run-pW5", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := retryDispatch(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := markTaskWorking(tk.ID, 1, testReq("op")); err == nil {
		t.Fatal("claim for superseded attempt 1 must be refused")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("refusal must name staleness: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched {
		t.Fatalf("refused claim must not move the task, got %s", got.Status)
	}
}

// Two tasks dispatched to one session is orchestrator ambiguity:
// the loop executes neither and names both.
func TestWorkerAmbiguousQueueRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW6")
	seedOwnedSession(t, "w", "pW6")
	var ids []string
	for _, obj := range []string{"one", "two"} {
		tk, err := createTask("run-pW6", "w", obj, "acc")
		if err != nil {
			t.Fatal(err)
		}
		// Shared root is deliberate here: the test needs two live
		// tasks in one workspace to prove the loop refuses to pick.
		if err := authorizeSharedWorkspace(tk.ID, testReq("orch")); err != nil {
			t.Fatal(err)
		}
		if err := dispatchTask(tk.ID, "w"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, tk.ID)
	}
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op"))
	if err == nil {
		t.Fatal("ambiguous queue must be refused")
	}
	if ran {
		t.Fatal("refused loop must not execute")
	}
	for _, id := range ids {
		if !strings.Contains(err.Error(), id) {
			t.Fatalf("refusal must name %s: %v", id, err)
		}
		if got, _ := getTask(id); got.Status != TaskDispatched {
			t.Fatalf("refused loop must not touch %s, got %s", id, got.Status)
		}
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts`); n != 0 {
		t.Fatalf("no execution may be recorded, got %d rows", n)
	}
}

// No committed task: the loop stays idle, launches nothing, and
// records nothing.
func TestWorkerIdleWhenNothingDispatched(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW7")
	seedOwnedSession(t, "w", "pW7")
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op"))
	if err != nil {
		t.Fatalf("idle loop must not error: %v", err)
	}
	if ran {
		t.Fatal("idle loop must report it ran nothing")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts`); n != 0 {
		t.Fatalf("idle loop must record nothing, got %d rows", n)
	}
}

// Structured invocation: real adapters render argv directly, never
// through a shell. The prompt is one trailing element; a hostile
// prompt cannot inject flags or commands.
func TestManagedArgvNoShell(t *testing.T) {
	for _, h := range []string{"codex", "claude", "muse", "hermes"} {
		a, err := lookupAdapter(h)
		if err != nil {
			t.Fatal(err)
		}
		argv, err := a.ManagedArgv("/work", "do; rm -rf /", "m")
		if err != nil {
			t.Fatalf("%s: %v", h, err)
		}
		if len(argv) < 2 {
			t.Fatalf("%s: argv too short: %v", h, argv)
		}
		if argv[len(argv)-1] != "do; rm -rf /" {
			t.Fatalf("%s: prompt must be one trailing element, got %q", h, argv)
		}
		joined := strings.Join(argv[:len(argv)-1], " ")
		if strings.Contains(joined, "do; rm -rf /") {
			t.Fatalf("%s: prompt leaked into flags: %v", h, argv)
		}
	}
}

// Fleet identity: two sessions on the same harness execute only
// their own committed task. Provider name never substitutes for
// session identity — receipts separate by session and attempt.
func TestFleetSameHarnessIndependentSessions(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW8")
	seedOwnedSession(t, "w-a", "pW8")
	seedOwnedSession(t, "w-b", "pW8")
	tkA, err := createTask("run-pW8", "w-a", "job a", "done a")
	if err != nil {
		t.Fatal(err)
	}
	tkB, err := createTask("run-pW8", "w-b", "job b", "done b")
	if err != nil {
		t.Fatal(err)
	}
	// Independent workspaces: neither task blocks the other.
	if err := authorizeSharedWorkspace(tkA.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := authorizeSharedWorkspace(tkB.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tkA.ID, "w-a"); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tkB.ID, "w-b"); err != nil {
		t.Fatal(err)
	}
	a := fakeWorkerAdapter("sh", "-c", "echo from-a")
	b := fakeWorkerAdapter("sh", "-c", "echo from-b")
	if ran, err := runWorkerOnce("w-a", a, testReq("op")); err != nil || !ran {
		t.Fatalf("session a must execute: ran=%v err=%v", ran, err)
	}
	if ran, err := runWorkerOnce("w-b", b, testReq("op")); err != nil || !ran {
		t.Fatalf("session b must execute: ran=%v err=%v", ran, err)
	}
	// Cross-consumption is impossible: each session's receipt names
	// its own session, and neither task ran twice.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct{ task, sess string }{{tkA.ID, "w-a"}, {tkB.ID, "w-b"}} {
		var sess string
		var n int
		if err := db.QueryRow(`SELECT session, COUNT(*) OVER () FROM execution_receipts WHERE task_id = ?`, tc.task).Scan(&sess, &n); err != nil {
			t.Fatalf("receipt for %s: %v", tc.task, err)
		}
		if sess != tc.sess || n != 1 {
			t.Fatalf("task %s must have exactly one receipt from %s, got %s x %d", tc.task, tc.sess, sess, n)
		}
	}
	// Each loop now idles: no cross-read of the other's queue.
	if ran, err := runWorkerOnce("w-a", a, testReq("op2")); err != nil || ran {
		t.Fatalf("session a must idle after its task reported: ran=%v err=%v", ran, err)
	}
}

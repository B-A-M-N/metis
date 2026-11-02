package main

// dispatch_atomicity_test.go: retry and admission races on the real
// production paths. Each test fails on the current code for a
// distinct, line-grounded reason:
//   - retry never rewrites the attempt sidecar, so a retried task
//     can never pass pickup (worker_run.go checkAttemptSidecar).
//   - reconcile bumps the attempt in three separate statements with
//     no attempt predicate, so concurrent retries double-claim.
//   - the run-limit check runs before the claim with no atomic
//     enforcement, so racing dispatchers jointly exceed the bound.

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// A retried task must be pick-up-able: the filesystem sidecar must
// name the new ledger attempt, or runWorkerOnce refuses the pickup
// as a file/ledger divergence and the retry is dead on arrival.
func TestRetryRefreshesAttemptSidecar(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "atom1")
	seedOwnedSession(t, "w", "atom1")
	tk, err := createTask("run-atom1", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := retryDispatch(tk.ID, "w", testReq("orch-retry")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	fresh, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Attempt != 2 {
		t.Fatalf("retry must claim attempt 2, got %+v", fresh)
	}
	root, err := sessionRoot("w")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "task-"+tk.ID+".attempt.json"))
	if err != nil {
		t.Fatalf("sidecar missing after retry: %v", err)
	}
	t.Logf("sidecar bytes: %s", string(raw))
	if err := checkAttemptSidecar(root, fresh); err != nil {
		t.Fatalf("retried task must pass pickup: %v", err)
	}
}

// Concurrent retries of the same uncertain attempt must yield
// exactly one replacement: one winner, one attempt bump, one fresh
// claimed row. Anything else is a duplicate execution lineage.
func TestConcurrentReconcileSingleWinner(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "atom2")
	seedOwnedSession(t, "w", "atom2")
	tk, err := createTask("run-atom2", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			db, err := openTasksDB()
			if err != nil {
				return
			}
			defer db.Close()
			if _, err := reconcileForRetry(db, tk.ID, testReq("orch")); err == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one reconcile winner, got %d", wins.Load())
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 2 {
		t.Fatalf("exactly one bump required, task at attempt %d", got.Attempt)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND state = 'claimed'`, tk.ID); n != 1 {
		t.Fatalf("expected 1 live claimed attempt row, got %d", n)
	}
}

// A run at its concurrency limit must refuse a second dispatch even
// when two dispatchers race: the bound is enforced by the claim
// itself, not by a pre-check that both racers can pass together.
// Two sessions (distinct roots) isolate this from workspace occupancy.
func TestConcurrentDispatchRespectsRunLimit(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "atom3")
	seedOwnedSession(t, "w1", "atom3")
	seedOwnedSession(t, "w2", "atom3")
	if err := setRunLimit("run-atom3", 1, testReq("orch")); err != nil {
		t.Fatalf("setRunLimit: %v", err)
	}
	ta, err := createTask("run-atom3", "agent", "obj-a", "")
	if err != nil {
		t.Fatal(err)
	}
	tb, err := createTask("run-atom3", "agent", "obj-b", "")
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, d := range []struct{ id, session string }{{ta.ID, "w1"}, {tb.ID, "w2"}} {
		wg.Add(1)
		go func(id, session string) {
			defer wg.Done()
			<-start
			if dispatchTaskWithRequest(id, session, testReq("orch")) == nil {
				wins.Add(1)
			}
		}(d.id, d.session)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("run limit 1 must admit exactly one racer, got %d winners", wins.Load())
	}
	if n, err := liveTaskCount("run-atom3"); err != nil || n != 1 {
		t.Fatalf("exactly one live task required, got %d (%v)", n, err)
	}
}

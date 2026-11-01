package main

// dispatch_service_test.go: P0-A decomposition races on the
// production dispatch path. Each test fails on the pre-decomposition
// code for a distinct, line-grounded reason:
//   - cross-session racers for one task each prepare files in their
//     own root before the claim; the loser leaves instruction +
//     sidecar files behind with no ledger binding (orphans).
//   - the claim commits whatever admission saw: a dependency added
//     between the pre-claim check and the commit is never
//     re-enforced, so the commit succeeds despite a now-blocking dep.

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Two dispatchers racing one task from different sessions (distinct
// roots) must yield exactly one winner, one attempt, and no orphan
// files: the loser's prepared files live in a root the committed
// task never executes in, so reconciliation must remove them.
func TestDispatchConcurrentCrossSessionOneWinnerNoOrphan(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "dsr1")
	seedOwnedSession(t, "w1", "dsr1")
	seedOwnedSession(t, "w2", "dsr1")
	tk, err := createTask("run-dsr1", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	root1, err := sessionRoot("w1")
	if err != nil {
		t.Fatal(err)
	}
	root2, err := sessionRoot("w2")
	if err != nil {
		t.Fatal(err)
	}
	if root1 == root2 {
		t.Fatalf("sessions must execute in distinct roots, both %q", root1)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, session := range []string{"w1", "w2", "w1", "w2"} {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			<-start
			if dispatchTaskWithRequest(tk.ID, s, testReq("orch")) == nil {
				wins.Add(1)
			}
		}(session)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one dispatch winner, got %d", wins.Load())
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 {
		t.Fatalf("exactly one attempt bump required, task at attempt %d", got.Attempt)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND state = 'claimed'`, tk.ID); n != 1 {
		t.Fatalf("expected 1 claimed attempt row, got %d", n)
	}
	// The loser's root must hold no files for this task: nothing in
	// the ledger binds them, so leaving them is an orphan.
	for _, root := range []string{root1, root2} {
		if root == got.ExecutionRoot {
			continue
		}
		matches, merr := filepath.Glob(filepath.Join(root, "task-"+tk.ID+".*"))
		if merr != nil {
			t.Fatal(merr)
		}
		for _, m := range matches {
			if _, serr := os.Stat(m); serr == nil {
				t.Fatalf("orphan prepared file %s in non-executing root (no ledger binding)", m)
			}
		}
	}
	// The winner's binding must be pickup-clean: sidecar names the
	// committed attempt and the reservation names the task.
	if err := checkAttemptSidecar(got.ExecutionRoot, got); err != nil {
		t.Fatalf("winner sidecar must verify: %v", err)
	}
	if holder := workspaceReservationHolder(got.ExecutionRoot); holder != tk.ID {
		t.Fatalf("reservation for %s held by %q, want %s", got.ExecutionRoot, holder, tk.ID)
	}
}

// A dispatcher that admits after a rival's commit must be refused
// at admission — before preparing any files. Otherwise its prepare
// overwrites the winner's attempt sidecar with an attempt the
// ledger will never bind, and the same-root keep rule preserves
// the divergence.
func TestDispatchLateAdmissionRefusedBeforePrepare(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "dsr-late")
	seedOwnedSession(t, "w", "dsr-late")
	tk, err := createTask("run-dsr-late", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	req := testReq("orch")
	if _, err := (DispatchService{}).Dispatch(tk.ID, "w", req); err != nil {
		t.Fatalf("first dispatch must win: %v", err)
	}
	if _, err := AdmitDispatch(tk.ID, "w", testReq("late")); err == nil {
		t.Fatal("admission after commit must refuse a non-pending task")
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != 1 {
		t.Fatalf("ledger attempt moved to %d, want 1", got.Attempt)
	}
	if err := checkAttemptSidecar(got.ExecutionRoot, got); err != nil {
		t.Fatalf("winner sidecar must still verify: %v", err)
	}
}

// A dependency added after admission but before the claim commits
// must be re-enforced by the claim: the commit is the authority,
// not the pre-claim snapshot.
func TestDispatchCommitRevalidatesDependencies(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "dsr2")
	seedOwnedSession(t, "w", "dsr2")
	tk, err := createTask("run-dsr2", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTask("run-dsr2", "agent", "dep-obj", "")
	if err != nil {
		t.Fatal(err)
	}
	adm, err := AdmitDispatch(tk.ID, "w", testReq("orch"))
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	// Mutable state changes after the admission snapshot: the claim
	// must see it.
	if err := addTaskDep(tk.ID, dep.ID, testReq("orch-dep")); err != nil {
		t.Fatalf("addTaskDep: %v", err)
	}
	prep, err := DeliveryPreparer{}.Prepare(adm, testReq("orch"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := (AttemptCommitter{}).Commit(adm, prep, testReq("orch"), "seam-test"); err == nil {
		t.Fatalf("commit with a newly-blocking dependency must be refused")
	}
	cur, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != TaskPending || cur.Attempt != 0 {
		t.Fatalf("refused commit must leave task pending at attempt 0, got %+v", cur)
	}
}

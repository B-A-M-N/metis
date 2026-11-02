package main

// run_policy_split_test.go: characterization tests for the
// run_policy.go split (task_dependencies.go, run_capacity.go,
// run_finalization.go).
//
// Pins the consolidation contract: both dependency entry points
// share one admission implementation, so the same cycle or conflict
// through either path yields an identical outcome; plus capacity
// and finalization behavior.

import (
	"strings"
	"testing"
)

// The same reverse edge refused through both entry points must
// produce the identical outcome: a cycle refusal naming the cycle.
// Before consolidation these were two copied cycle loops; this test
// fails if the paths ever diverge again.
func TestDependPathsConvergeOnCycle(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-depconv", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-depconv", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(a.ID, b.ID, testReq("orch")); err != nil {
		t.Fatalf("forward edge: %v", err)
	}
	plainErr := addTaskDep(b.ID, a.ID, testReq("orch"))
	if plainErr == nil || !strings.Contains(plainErr.Error(), "cycle") {
		t.Fatalf("plain path must refuse the reverse edge as a cycle, got: %v", plainErr)
	}
	idemErrSuffix := addTaskDepIdempotentTx(b.ID, a.ID, "cycle-key-1", t)
	if idemErrSuffix == nil || !strings.Contains(idemErrSuffix.Error(), "cycle") {
		t.Fatalf("idempotent path must refuse the reverse edge as a cycle, got: %v", idemErrSuffix)
	}
	if plainErr.Error() != idemErrSuffix.Error() {
		t.Fatalf("paths diverged:\nplain: %v\nidem:  %v", plainErr, idemErrSuffix)
	}
}

func addTaskDepIdempotentTx(taskID, depID, key string, t *testing.T) error {
	t.Helper()
	_, err := addTaskDepIdempotent(taskID, depID, testReq("orch"), key)
	return err
}

// Idempotent key semantics: fresh key records, identical replay
// converges, same key with a different edge conflicts.
func TestDependIdempotentReplayAndConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-depidem", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-depidem", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := createTask("run-depidem", "agent", "c", "")
	if err != nil {
		t.Fatal(err)
	}
	out, err := addTaskDepIdempotent(a.ID, b.ID, testReq("orch"), "dep-key-1")
	if err != nil {
		t.Fatalf("fresh key: %v", err)
	}
	if out.Replay {
		t.Fatal("fresh key must not replay")
	}
	re, err := addTaskDepIdempotent(a.ID, b.ID, testReq("orch"), "dep-key-1")
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if !re.Replay {
		t.Fatal("identical replay must converge with Replay=true")
	}
	_, err = addTaskDepIdempotent(a.ID, c.ID, testReq("orch"), "dep-key-1")
	if err == nil {
		t.Fatal("same key with a different edge must conflict")
	}
	if _, ok := err.(*dependKeyConflict); !ok {
		t.Fatalf("conflict must be *dependKeyConflict, got %T: %v", err, err)
	}
	// A fresh key over an already-recorded edge converges too.
	fe, err := addTaskDepIdempotent(a.ID, b.ID, testReq("orch"), "dep-key-2")
	if err != nil {
		t.Fatalf("fresh key over existing edge: %v", err)
	}
	if !fe.Replay {
		t.Fatal("fresh key over existing edge must replay")
	}
}

// Capacity: validation, unbounded default, and limit enforcement
// against live tasks. Capacity admission must not close the run.
func TestCapacityLimitAdmission(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "cap")
	seedOwnedSession(t, "w", "cap")
	if err := setRunLimit("", 1, testReq("orch")); err == nil {
		t.Fatal("empty run must be refused")
	}
	if err := setRunLimit("run-cap", 0, testReq("orch")); err == nil {
		t.Fatal("zero limit must be refused")
	}
	if _, set, err := getRunLimit("run-cap"); err != nil || set {
		t.Fatalf("unset run must be unbounded, set=%v err=%v", set, err)
	}
	a, err := createTask("run-cap", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-cap", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := setRunLimit("run-cap", 1, testReq("orch")); err != nil {
		t.Fatalf("setRunLimit: %v", err)
	}
	if lim, set, err := getRunLimit("run-cap"); err != nil || !set || lim != 1 {
		t.Fatalf("getRunLimit = %d,%v,%v, want 1,true,nil", lim, set, err)
	}
	if _, _, err := admitRunLimit("run-cap", a.ID); err != nil {
		t.Fatalf("empty run must admit: %v", err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if n, err := liveTaskCount("run-cap"); err != nil || n != 1 {
		t.Fatalf("liveTaskCount = %d,%v, want 1,nil", n, err)
	}
	if _, _, err := admitRunLimit("run-cap", b.ID); err == nil {
		t.Fatal("run at limit must refuse the second dispatch")
	} else if !strings.Contains(err.Error(), "concurrency limit") {
		t.Fatalf("refusal must name the concurrency limit, got: %v", err)
	}
	if closed, err := runClosed("run-cap"); err != nil || closed {
		t.Fatalf("capacity admission must never close the run, closed=%v err=%v", closed, err)
	}
}

// Finalization is the only authority that closes a run: open tasks
// block it by name, and once closed both dependency paths refuse.
func TestFinalizeOnlyAuthorityClosesRun(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-fin", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-fin", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeRun("run-fin", testReq("orch")); err == nil {
		t.Fatal("open run must not finalize")
	} else if !strings.Contains(err.Error(), a.ID) && !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("refusal must name blocking tasks, got: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if err := cancelTask(id, testReq("orch")); err != nil {
			t.Fatalf("cancel %s: %v", id, err)
		}
	}
	sum, err := finalizeRun("run-fin", testReq("orch"))
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if sum.Total != 2 || sum.Cancelled != 2 {
		t.Fatalf("summary = %+v, want total=2 cancelled=2", sum)
	}
	if closed, err := runClosed("run-fin"); err != nil || !closed {
		t.Fatalf("run must be closed, closed=%v err=%v", closed, err)
	}
	if err := addTaskDep(a.ID, b.ID, testReq("orch")); err == nil {
		t.Fatal("closed run must refuse plain edge edits")
	}
	if _, err := addTaskDepIdempotent(a.ID, b.ID, testReq("orch"), "fin-key-1"); err == nil {
		t.Fatal("closed run must refuse idempotent edge edits")
	}
}

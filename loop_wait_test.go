package main

// WP1a: the managed worker loop waits for committed work instead of
// exiting when it starts before dispatch, and reconciles
// incomplete executions after a restart before any replacement.

import (
	"strings"
	"testing"
	"time"
)

// No committed work: the wait ends at the deadline and reports
// nothing to do. Bounded by construction.
func TestWaitForWorkTimesOut(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "lw1")
	seedOwnedSession(t, "w", "lw1")
	start := time.Now()
	if waitForCommittedWork("w", 10*time.Millisecond, 60*time.Millisecond, nil) {
		t.Fatal("no committed task: wait must report idle")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("wait must be bounded, took %v", elapsed)
	}
}

// Work committed mid-wait wakes the loop: start the worker before
// dispatch, dispatch later, and the loop must notice without
// another manual invocation.
func TestWaitForWorkWakesOnDispatch(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "lw2")
	seedOwnedSession(t, "w", "lw2")
	tk, err := createTask("run-lw2", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = dispatchTaskWithRequest(tk.ID, "w", testReq("orch"))
	}()
	if !waitForCommittedWork("w", 10*time.Millisecond, 5*time.Second, nil) {
		t.Fatal("dispatch during wait must wake the loop")
	}
}

// A closed stop channel interrupts the wait immediately.
func TestWaitForWorkStopsOnSignal(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "lw3")
	seedOwnedSession(t, "w", "lw3")
	stop := make(chan struct{})
	close(stop)
	start := time.Now()
	if waitForCommittedWork("w", 10*time.Millisecond, 5*time.Second, stop) {
		t.Fatal("stopped wait must report no work")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stop must interrupt promptly, took %v", elapsed)
	}
}

// Restart reconciliation: a working task with an open execution
// receipt (dead loop, outcome unknown) is named, not moved. A
// replacement attempt must reconcile first, never silently
// overwrite.
func TestLoopRestartNamesOpenExecutions(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "lw4")
	seedOwnedSession(t, "w", "lw4")
	tk, err := createTask("run-lw4", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("worker-loop/w")); err != nil {
		t.Fatal(err)
	}
	if err := recordExecutionStart(tk.ID, got.Attempt, "w", "test-fake", `["sh"]`, 4242, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	n, err := reconcileLoopRestart("w", testReq("op"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("one open execution must be named, got %d", n)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("restart probe must not move the task, got %s", got.Status)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "loop_restart_observed" && strings.Contains(r.Detail, "UNKNOWN") {
			found = true
		}
	}
	if !found {
		t.Fatal("restart must receipt the uncertain execution as UNKNOWN")
	}
	if n, err := reconcileLoopRestart("nosuch", testReq("op")); err == nil || n != 0 {
		t.Fatalf("unknown session must error, got n=%d err=%v", n, err)
	}
}

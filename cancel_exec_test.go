package main

// WP1c: cancellation reaches the actual managed model process, not
// just the placeholder session. Requested, confirmed, and
// uncertain are distinct; a reused PID is never signaled.

import (
	"os/exec"
	"testing"
	"time"
)

// A live managed execution cancelled through the controller is
// actually stopped: the exact PID bound to the attempt is gone
// afterwards and the receipt says confirmed.
func TestCancelStopsManagedProcess(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cx1")
	seedOwnedSession(t, "w", "cx1")
	tk, err := createTask("run-cx1", "w", "obj", "acc")
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
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill() }()
	if err := recordExecutionStart(tk.ID, got.Attempt, "w", "test-fake", `["sleep","60"]`, pid, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	// Full controller path: the task cancels, the exact bound
	// process dies, and the receipt names the confirmed outcome —
	// never the placeholder session's fate.
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskCancelled {
		t.Fatalf("task must be cancelled, got %s", got.Status)
	}
	if procAlive(pid) {
		t.Fatal("managed process must be dead after confirmed cancel")
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "cancel_execution_"+ExecutionCancelConfirmed {
			found = true
		}
	}
	if !found {
		t.Fatal("cancel must receipt cancel_execution_confirmed")
	}
}

// A PID that has been recycled (start time no longer matches the
// bound execution) must never be signaled: the result is
// uncertain, not confirmed.
func TestCancelRefusesRecycledPID(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cx2")
	seedOwnedSession(t, "w", "cx2")
	tk, err := createTask("run-cx2", "w", "obj", "acc")
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
	// Short-lived process, already gone; receipt claims it anyway
	// with a deliberately wrong start identity.
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	realStart, _ := procStartTime(1)
	if err := recordExecutionStartWithProc(tk.ID, got.Attempt, "w", "test-fake", `["true"]`, pid, realStart+1000000, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	disp, err := stopManagedExecution(tk.ID, got.Attempt, testReq("op"))
	if err != nil {
		t.Fatal(err)
	}
	if disp == ExecutionCancelConfirmed {
		t.Fatal("mismatched process identity must never report confirmed")
	}
}

// The loop honors a mid-run cancellation: the process result is
// still recorded (facts are facts) but no report is filed on a
// task that is no longer working.
func TestLoopHonorsMidRunCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (timing-bound); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cx3")
	seedOwnedSession(t, "w", "cx3")
	tk, err := createTask("run-cx3", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	slow := fakeWorkerAdapter("sh", "-c", "sleep 2; echo late-output")
	done := make(chan error, 1)
	go func() {
		_, err := runWorkerOnce("w", slow, testReq("op"))
		done <- err
	}()
	time.Sleep(300 * time.Millisecond)
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("loop must absorb cancellation cleanly: %v", err)
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskCancelled {
		t.Fatalf("task stays cancelled, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("cancelled task must gain no report, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND exit_code IS NOT NULL`, tk.ID); n != 1 {
		t.Fatalf("process outcome must still be recorded, got %d rows", n)
	}
}

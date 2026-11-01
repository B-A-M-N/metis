package main

// cancel_two_stage_test.go: cancellation requested is not
// cancellation confirmed. A task whose execution cannot be proven
// stopped must not release its workspace or read as cancelled:
// it becomes termination_unknown, which still occupies the root
// and still blocks run finalization.

import (
	"os/exec"
	"testing"
)

func TestCancelUncertainKeepsWorkspaceBlocked(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cxu")
	seedOwnedSession(t, "w", "cxu")
	tk, err := createTask("run-cxu", "w", "obj", "acc")
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
	// Bound to a recycled identity: stopManagedExecution reports
	// uncertain and must never signal.
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
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	cur, _ := getTask(tk.ID)
	if cur.Status != TaskTerminationUnknown {
		t.Fatalf("uncertain stop must leave termination_unknown, got %s", cur.Status)
	}
	// One bounded escalation, deduplicated on repeat.
	open, err := listAttention("run-cxu", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "uncertain_outcome" || open[0].TaskID != tk.ID {
		t.Fatalf("cancel must raise one uncertain_outcome attention, got %+v", open)
	}
	again, err := raiseAttention("run-cxu", tk.ID, cur.Attempt, "w", open[0].Kind,
		open[0].Reason, open[0].Evidence)
	if err != nil {
		t.Fatal(err)
	}
	if again != open[0].ID {
		t.Fatalf("repeat observation must reuse %s, got %s", open[0].ID, again)
	}
	if open2, _ := listAttention("run-cxu", true); len(open2) != 1 {
		t.Fatalf("dedupe must keep one row, got %d", len(open2))
	}
	// The root stays occupied: a new task on the same workspace
	// is refused while termination is unconfirmed.
	tk2, err := createTask("run-cxu", "w", "obj2", "acc2")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk2.ID, "w", testReq("orch")); err == nil {
		t.Fatal("workspace must stay blocked while termination is unknown")
	}
	// Run finalization stays blocked too.
	if _, err := finalizeRun("run-cxu", testReq("orch")); err == nil {
		t.Fatal("run with termination_unknown task must not finalize")
	}
}

func TestCancelConfirmedReleasesWorkspace(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cxc")
	seedOwnedSession(t, "w", "cxc")
	tk, err := createTask("run-cxc", "w", "obj", "acc")
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
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if cur, _ := getTask(tk.ID); cur.Status != TaskCancelled {
		t.Fatalf("confirmed stop must cancel, got %s", cur.Status)
	}
	if procAlive(pid) {
		t.Fatal("managed process must be dead after confirmed cancel")
	}
}

func TestCancelPendingNeedsNoStop(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cxp")
	seedOwnedSession(t, "w", "cxp")
	tk, err := createTask("run-cxp", "w", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if cur, _ := getTask(tk.ID); cur.Status != TaskCancelled {
		t.Fatalf("pending task must cancel directly, got %s", cur.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_receipts WHERE task_id = ? AND kind LIKE 'cancel_execution_%'`, tk.ID); n != 0 {
		t.Fatal("pending cancel must not record execution dispositions")
	}
}

// A native turn in flight has no OS process to signal: its
// receipt carries the MSP binding (pid 0), not a missing row. A
// missing row means nothing was ever bound (NoBinding); a native
// row means execution may be server-side, so cancelling must go
// through turn/cancel and, unconfirmed, land termination_unknown
// — never cancelled with work possibly still running.
func TestCancelLiveNativeTurnStaysUnconfirmed(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cxn")
	seedOwnedSession(t, "w", "cxn")
	tk, err := createTask("run-cxn", "w", "obj", "acc")
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
	// The session runs the native harness, but the live turn filed
	// no execution receipt (the pre-submit binding is missing) and
	// there is deliberately no muse binding, so the MSP cancel
	// path fails closed instead of confirming anything. A missing
	// row on a native session is ambiguous — pre-submit failure or
	// unrecorded server-side execution — and must not read as
	// NoBinding.
	reg := loadRegistry()
	entry := reg["w"]
	entry.Harness = "muse-native"
	reg["w"] = entry
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	disp, derr := stopManagedExecution(tk.ID, got.Attempt, testReq("orch"))
	if derr != nil {
		t.Fatal(derr)
	}
	if disp == ExecutionCancelNoBinding {
		t.Fatal("missing receipt on a native session is ambiguous, not NoBinding: the turn may be server-side")
	}
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	cur, _ := getTask(tk.ID)
	if cur.Status != TaskTerminationUnknown {
		t.Fatalf("unconfirmed native stop must leave termination_unknown, got %s", cur.Status)
	}
	tk2, err := createTask("run-cxn", "w", "obj2", "acc2")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk2.ID, "w", testReq("orch")); err == nil {
		t.Fatal("workspace must stay blocked while native termination is unknown")
	}
}

// A filed native receipt with no binding still fails closed: the
// row proves an MSP execution was bound, and without the binding
// Metis cannot address its turn.
func TestCancelNativeReceiptWithoutBinding(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "cxn2")
	seedOwnedSession(t, "w", "cxn2")
	tk, err := createTask("run-cxn2", "w", "obj", "acc")
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
	if err := recordExecutionStartWithProc(tk.ID, got.Attempt, "w", "muse-native",
		`["msp","turn/start","cmd-1"]`, 0, 0, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	disp, derr := stopManagedExecution(tk.ID, got.Attempt, testReq("orch"))
	if derr != nil {
		t.Fatal(derr)
	}
	if disp == ExecutionCancelNoBinding || disp == ExecutionCancelConfirmed {
		t.Fatalf("native receipt without binding must stay unconfirmed, got %s", disp)
	}
}

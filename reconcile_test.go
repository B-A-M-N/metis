package main

// Slice 3 (remediation Phase C): attempt-bound exit/recovery
// reconciliation. The PTY supervisor records process termination,
// but a dead process is not a task outcome. This probe tells the
// orchestrator exactly what is known about the current attempt —
// delivery, execution, report, verdicts — and what to do next,
// without launching, moving, or judging anything itself.

import (
	"strings"
	"testing"
)

func reconcileOrFail(t *testing.T, taskID string) TaskReconciliation {
	t.Helper()
	r, err := reconcileTask(taskID)
	if err != nil {
		t.Fatalf("reconcile %s: %v", taskID, err)
	}
	return r
}

// A dispatched task nothing has picked up: no execution receipt,
// recommendation is to wait for (or investigate) the worker loop,
// never to assume work happened.
func TestReconcileDispatchedWaitsForPickup(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "rc1")
	seedOwnedSession(t, "w", "rc1")
	tk, err := createTask("run-rc", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	r := reconcileOrFail(t, tk.ID)
	if r.ExecutionStarted {
		t.Fatal("no execution may be recorded before pickup")
	}
	if r.ReportFiled {
		t.Fatal("no report exists yet")
	}
	if !strings.Contains(strings.ToUpper(r.Recommendation+" "+r.Reason), "WAIT") {
		t.Fatalf("orchestrator must wait, got %q: %s", r.Recommendation, r.Reason)
	}
}

// An open execution receipt (started, no result) means the outcome
// is unknown — not success, not failure. The task stays working.
func TestReconcileOpenExecutionIsUncertain(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "rc2")
	seedOwnedSession(t, "w", "rc2")
	tk, err := createTask("run-rc", "w", "obj", "acc")
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
	r := reconcileOrFail(t, tk.ID)
	if !r.ExecutionStarted || r.ExecutionFinished {
		t.Fatalf("execution must show started-but-open: %+v", r)
	}
	if !strings.Contains(strings.ToUpper(r.Recommendation+" "+r.Reason), "WAIT") {
		t.Fatalf("open execution means wait, got %q: %s", r.Recommendation, r.Reason)
	}
	if !strings.Contains(strings.ToUpper(r.Reason), "UNKNOWN") {
		t.Fatalf("reason must say the outcome is unknown: %s", r.Reason)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("reconcile must not move the task, got %s", got.Status)
	}
}

// A failed exit with no report stays unresolved with its failure
// detail preserved — never auto-retried, never failed by fiat.
func TestReconcileFailedExitPreservesEvidence(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "rc3")
	seedOwnedSession(t, "w", "rc3")
	tk, err := createTask("run-rc", "w", "obj", "acc")
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
	if err := recordExecutionResult(tk.ID, got.Attempt, 137, "sha256:killed"); err != nil {
		t.Fatal(err)
	}
	r := reconcileOrFail(t, tk.ID)
	if !r.ExecutionFinished || r.ExitCode == nil || *r.ExitCode != 137 {
		t.Fatalf("exit detail must be preserved: %+v", r)
	}
	if r.ReportFiled {
		t.Fatal("no report was filed")
	}
	up := strings.ToUpper(r.Recommendation + " " + r.Reason)
	if !strings.Contains(up, "INVESTIGATE") {
		t.Fatalf("abnormal exit without report needs investigation, got %q: %s", r.Recommendation, r.Reason)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("failed exit alone must not move the task, got %s", got.Status)
	}
}

// Result plus report but no verdicts: the task is ready for review,
// and reconcile says exactly that instead of implying verification.
func TestReconcileReportReadyAwaitsVerdicts(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "rc4")
	seedOwnedSession(t, "w", "rc4")
	tk, err := createTask("run-rc", "w", "obj", "widget exists")
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
	if err := recordExecutionStart(tk.ID, got.Attempt, "w", "test-fake", `["sh"]`, 99, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	if err := recordExecutionResult(tk.ID, got.Attempt, 0, "sha256:done"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceWorkerClaim, Reporter: "w"}); err != nil {
		t.Fatal(err)
	}
	r := reconcileOrFail(t, tk.ID)
	if !r.ExecutionFinished || *r.ExitCode != 0 || !r.ReportFiled {
		t.Fatalf("execution+report must show: %+v", r)
	}
	if len(r.Verdicts) != 0 {
		t.Fatalf("no verdicts filed yet: %+v", r.Verdicts)
	}
	if !strings.Contains(strings.ToUpper(r.Recommendation+" "+r.Reason), "VERDICT") {
		t.Fatalf("must await criterion verdicts, got %q: %s", r.Recommendation, r.Reason)
	}
}

// Terminal tasks reconcile as done; reconcile itself is read-only.
func TestReconcileTerminalIsDoneAndReadOnly(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "rc5")
	seedOwnedSession(t, "w", "rc5")
	tk, err := createTask("run-rc", "w", "obj", "acc")
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
	if err := submitReport(tk.ID, ReportFiling{Report: "no", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	before, _ := getTask(tk.ID)
	nBefore := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID)
	r := reconcileOrFail(t, tk.ID)
	if !strings.Contains(strings.ToUpper(r.Recommendation+" "+r.Reason), "DONE") {
		t.Fatalf("terminal task must reconcile as done, got %q", r.Recommendation)
	}
	// DONE never means empty: the terminal probe still reports the
	// durable facts behind the outcome.
	if !r.ReportFiled {
		t.Fatal("terminal reconcile must show the filed report")
	}
	if r.AttemptState == "" || r.DeliveryState == "" {
		t.Fatalf("terminal reconcile must show attempt/delivery states: %+v", r)
	}
	after, _ := getTask(tk.ID)
	if before != after {
		t.Fatalf("reconcile moved the task: %+v -> %+v", before, after)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != nBefore {
		t.Fatalf("reconcile wrote history: %d -> %d rows", nBefore, n)
	}
}

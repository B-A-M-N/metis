package main

import (
	"fmt"
	"strings"
	"testing"
)

// Fleet settlement error handling: a failed settlement must surface
// an actionable typed result without rewriting the attempt's
// established outcome, and reconcileFleetSettlement must repair the
// stranded binding only after independently confirming the outcome —
// never by launching another execution.

// mustSettleFixture dispatches one task for session "w" and places it
// on the local node. It returns the committed task, the node, and the
// attempt the worker will execute.
func mustSettleFixture(t *testing.T, name string) (Task, string, int) {
	t.Helper()
	tk, err := createTask("run-"+name, "w", "objective "+name, "accept "+name)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	node, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("local node: %v", err)
	}
	if err := setFleetNodeCapacity(node, 2); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	cur, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offerFleetExecution(node, tk.ID, cur.Attempt, "w",
		OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	return cur, node, cur.Attempt
}

// fleetExecState reads the raw binding row for assertions.
func fleetExecState(t *testing.T, taskID, attemptID string) (holder, state string, found bool) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT node_id, state FROM fleet_executions WHERE task_id = ? AND attempt_id = ?`,
		taskID, attemptID).Scan(&holder, &state); err != nil {
		return "", "", false
	}
	return holder, state, true
}

// A settlement database failure must surface a typed error while
// preserving everything durable: the filed report and task status,
// the claimed fleet row, and the capacity reservation.
func TestFleetSettleFailurePreservesOutcome(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "fs1")
	seedOwnedSession(t, "w", "fs1")
	_, node, attempt := mustSettleFixture(t, "fs1")
	tkID := ""
	ad := fakeWorkerAdapter("sh", "-c", "echo model-output")
	asn, idle, err := workerResolveAssignment("w", ad, testReq("op"))
	if err != nil || idle {
		t.Fatalf("resolve: idle=%v err=%v", idle, err)
	}
	tkID = asn.task.ID
	cl, err := workerClaimAttempt("w", asn)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !cl.fleetOwned || cl.fleetNode != node {
		t.Fatalf("claim must bind the offered node, got %+v", cl)
	}
	out, err := workerExecuteAttempt("w", ad, cl)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.native {
		t.Fatal("fake adapter must take the subprocess path")
	}
	if done, err := workerPersistOutcome("w", ad, cl, out.exec); err != nil || !done {
		t.Fatalf("persist: done=%v err=%v", done, err)
	}
	if got, _ := getTask(tkID); got.Status != TaskReportReady {
		t.Fatalf("evidence commit must reach report_ready, got %s", got.Status)
	}

	fleetSettleProbe = func() error { return fmt.Errorf("injected settlement outage") }
	defer func() { fleetSettleProbe = nil }()
	res := workerSettleFleetOwnership(cl, nil)
	if res.Err == nil {
		t.Fatal("settlement DB failure must surface an error, got nil")
	}
	if res.Settled {
		t.Fatal("failed settlement must not report settled")
	}
	if res.Disposition != fleetSettleUnsettled {
		t.Fatalf("disposition = %q, want %q", res.Disposition, fleetSettleUnsettled)
	}
	if res.TaskID != tkID || res.Attempt != attempt || res.NodeID != node {
		t.Fatalf("result must name (task, attempt, node), got %+v", res)
	}
	if !strings.Contains(res.Err.Error(), "reconcile") {
		t.Fatalf("settlement error must name the reconcile repair, got: %v", res.Err)
	}

	// Established outcome stands: report filed, status untouched.
	if got, _ := getTask(tkID); got.Status != TaskReportReady {
		t.Fatalf("settlement failure rewrote task status to %s, want report_ready", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tkID); n != 1 {
		t.Fatalf("report must survive settlement failure, got %d rows", n)
	}
	// Durable claimed state stands: row still claimed, slot still held.
	if _, state, found := fleetExecState(t, tkID, fleetAttemptID(attempt)); !found || state != fleetExecClaimed {
		t.Fatalf("fleet row must stay claimed, got found=%v state=%q", found, state)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_reservations WHERE node_id = ?`, node); n != 1 {
		t.Fatalf("capacity reservation must survive settlement failure, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_receipts WHERE task_id = ? AND kind = 'fleet_settle_failed'`, tkID); n != 1 {
		t.Fatalf("settlement failure must leave a discrepancy receipt, got %d", n)
	}

	// Repair after the outage: reconcile confirms the filed report
	// and settles without launching another execution.
	fleetSettleProbe = nil
	execsBefore := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tkID)
	rec := reconcileFleetSettlement(node, tkID, attempt)
	if rec.Err != nil || !rec.Settled || rec.Disposition != fleetExecCompleted {
		t.Fatalf("reconcile must repair, got %+v err=%v", rec, rec.Err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tkID); n != 1 {
		t.Fatalf("reconcile must not file another report, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tkID); n != execsBefore {
		t.Fatalf("reconcile must never launch another execution, receipts %d -> %d", execsBefore, n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_reservations WHERE node_id = ?`, node); n != 0 {
		t.Fatalf("reconcile must free the slot, %d reservations remain", n)
	}
}

// Settling twice is refused, and reconciling a settled binding
// converges as a no-op. Reconciling a row that never existed is an
// error, never a repair.
func TestFleetSettleDuplicateRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	node, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("local node: %v", err)
	}
	if err := setFleetNodeCapacity(node, 2); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	tx := uniqueTaskKey("task-dup")
	aid := fleetAttemptID(7)
	if err := claimFleetExecution(node, tx, aid, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := completeFleetExecution(node, tx, aid, "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := completeFleetExecution(node, tx, aid, "done"); err == nil {
		t.Fatal("double settle succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "no claimed execution") {
		t.Fatalf("double settle error = %q, want no-claimed-execution refusal", err)
	}
	rec := reconcileFleetSettlement(node, tx, 7)
	if rec.Err != nil || !rec.Settled || rec.Disposition != fleetExecCompleted {
		t.Fatalf("reconcile of a settled binding must converge, got %+v err=%v", rec, rec.Err)
	}
	// A claim with no attempt evidence must not settle: the outcome
	// is unconfirmed, so the binding stays claimed.
	tx2 := uniqueTaskKey("task-dupbare")
	if err := claimFleetExecution(node, tx2, aid, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("claim bare: %v", err)
	}
	bare := reconcileFleetSettlement(node, tx2, 7)
	if bare.Err == nil || bare.Settled {
		t.Fatalf("reconcile without evidence must refuse, got %+v", bare)
	}
	if _, state, found := fleetExecState(t, tx2, aid); !found || state != fleetExecClaimed {
		t.Fatalf("refused reconcile must preserve claimed, got found=%v state=%q", found, state)
	}
	absent := reconcileFleetSettlement(node, uniqueTaskKey("task-never"), 7)
	if absent.Err == nil || absent.Disposition != fleetSettleAbsent {
		t.Fatalf("reconcile of nothing must report absent, got %+v", absent)
	}
}

// A process exit after the evidence commit but before settlement
// leaves a claimed binding over a decided attempt. Reconcile must
// confirm the filed evidence and settle it — exactly once, without
// executing anything new — and a post-restart replay must converge.
func TestFleetSettleCrashBeforeSettlement(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "fs2")
	seedOwnedSession(t, "w", "fs2")
	_, node, attempt := mustSettleFixture(t, "fs2")
	ad := fakeWorkerAdapter("sh", "-c", "echo model-output")
	asn, idle, err := workerResolveAssignment("w", ad, testReq("op"))
	if err != nil || idle {
		t.Fatalf("resolve: idle=%v err=%v", idle, err)
	}
	tkID := asn.task.ID
	cl, err := workerClaimAttempt("w", asn)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	out, err := workerExecuteAttempt("w", ad, cl)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if done, err := workerPersistOutcome("w", ad, cl, out.exec); err != nil || !done {
		t.Fatalf("persist: done=%v err=%v", done, err)
	}
	// Crash here: settlement never ran.
	if _, state, found := fleetExecState(t, tkID, fleetAttemptID(attempt)); !found || state != fleetExecClaimed {
		t.Fatalf("pre-reconcile binding must be claimed, got found=%v state=%q", found, state)
	}
	reportsBefore := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tkID)
	execsBefore := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tkID)

	rec := reconcileFleetSettlement(node, tkID, attempt)
	if rec.Err != nil || !rec.Settled || rec.Disposition != fleetExecCompleted {
		t.Fatalf("reconcile must settle the evidenced attempt, got %+v err=%v", rec, rec.Err)
	}
	if rec.TaskID != tkID || rec.Attempt != attempt || rec.NodeID != node {
		t.Fatalf("reconcile result must name (task, attempt, node), got %+v", rec)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tkID); n != reportsBefore {
		t.Fatalf("reconcile must not file reports, rows %d -> %d", reportsBefore, n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tkID); n != execsBefore {
		t.Fatalf("reconcile must never launch another execution, receipts %d -> %d", execsBefore, n)
	}
	if got, _ := getTask(tkID); got.Status != TaskReportReady {
		t.Fatalf("reconcile must not touch the established outcome, got %s", got.Status)
	}

	// Restart replay: the same reconcile converges without new work.
	again := reconcileFleetSettlement(node, tkID, attempt)
	if again.Err != nil || !again.Settled || again.Disposition != fleetExecCompleted {
		t.Fatalf("replay after restart must converge, got %+v err=%v", again, again.Err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tkID); n != execsBefore {
		t.Fatalf("replay must not execute, receipts %d -> %d", execsBefore, n)
	}
}

// The full loop surfaces a settlement outage as an error while
// keeping the attempt's outcome: done stays true, the report stands,
// and a later reconcile repairs the stranded binding.
func TestFleetSettleRunSurfacesSettlementError(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "fs3")
	seedOwnedSession(t, "w", "fs3")
	tk, node, attempt := mustSettleFixture(t, "fs3")

	fleetSettleProbe = func() error { return fmt.Errorf("injected settlement outage") }
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo model-output"), testReq("op"))
	fleetSettleProbe = nil
	if err == nil {
		t.Fatal("runWorkerOnce must surface the settlement failure, got nil error")
	}
	if !ran {
		t.Fatal("runWorkerOnce must still report the executed attempt (done=true)")
	}
	if !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("surfaced error must name the reconcile repair, got: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("task outcome must stand as report_ready, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("exactly one report must stand, got %d", n)
	}
	if _, state, found := fleetExecState(t, tk.ID, fleetAttemptID(attempt)); !found || state != fleetExecClaimed {
		t.Fatalf("binding must stay claimed for repair, got found=%v state=%q", found, state)
	}

	// Post-restart repair, then a duplicate repair converges.
	rec := reconcileFleetSettlement(node, tk.ID, attempt)
	if rec.Err != nil || !rec.Settled {
		t.Fatalf("post-restart reconcile must repair, got %+v err=%v", rec, rec.Err)
	}
	again := reconcileFleetSettlement(node, tk.ID, attempt)
	if again.Err != nil || !again.Settled || again.Disposition != fleetExecCompleted {
		t.Fatalf("duplicate repair must converge, got %+v err=%v", again, again.Err)
	}
}

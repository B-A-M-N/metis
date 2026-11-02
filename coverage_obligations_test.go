package main

// coverage_obligations_test.go: failing-first proof for the
// delegation-coverage obligation ledger (OCO-B). Obligations are
// durable review duties (independent review, security verification,
// integration testing) bound to a task and the exact revision they
// review. Verifying a task never auto-satisfies an unrelated
// obligation, and unchanged already-verified evidence never causes
// re-delegation.

import (
	"testing"
)

func TestObligationLifecycle(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-ob", "agent", "ship feature", "feature works")
	if err != nil {
		t.Fatal(err)
	}
	id, err := requireObligation("run-ob", tk.ID, "independent_review", "digest:abc", `["ev-1"]`, testReq("lead"))
	if err != nil {
		t.Fatalf("require: %v", err)
	}
	if id == "" {
		t.Fatal("require must return an id")
	}
	got, err := getObligation(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ObligationRequired {
		t.Fatalf("new obligation must be %q, got %q", ObligationRequired, got.Status)
	}
	if err := setObligationState(id, ObligationRunning, testReq("lead")); err != nil {
		t.Fatalf("required->running: %v", err)
	}
	if err := setObligationState(id, ObligationSatisfied, testReq("lead")); err != nil {
		t.Fatalf("running->satisfied: %v", err)
	}
	got, err = getObligation(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ObligationSatisfied {
		t.Fatalf("expected satisfied, got %q", got.Status)
	}
}

// A satisfied obligation is terminal: it is never silently
// reopened, even back into running.
func TestObligationIllegalTransition(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-ob", "agent", "ship feature", "feature works")
	if err != nil {
		t.Fatal(err)
	}
	id, err := requireObligation("run-ob", tk.ID, "security_verification", "digest:abc", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(id, ObligationRunning, testReq("lead")); err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(id, ObligationSatisfied, testReq("lead")); err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(id, ObligationRunning, testReq("lead")); err == nil {
		t.Fatal("satisfied->running must be refused: satisfied is terminal")
	}
	if got, _ := getObligation(id); got.Status != ObligationSatisfied {
		t.Fatalf("refused transition must not move the row, got %q", got.Status)
	}
}

// Unchanged already-verified evidence does not cause
// re-delegation: the same (task, kind, targetRev) returns the
// original row, never a duplicate.
func TestObligationRequireIdempotent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-ob", "agent", "ship feature", "feature works")
	if err != nil {
		t.Fatal(err)
	}
	first, err := requireObligation("run-ob", tk.ID, "integration_testing", "digest:abc", `["ev-1"]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := requireObligation("run-ob", tk.ID, "integration_testing", "digest:abc", `["ev-1"]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("idempotent require must return %s, got %s", first, second)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM coverage_obligations WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("idempotent require must not duplicate rows, got %d", n)
	}
	// A new target revision is new work, not a duplicate.
	third, err := requireObligation("run-ob", tk.ID, "integration_testing", "digest:def", `["ev-2"]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("a new targetRev must open a distinct obligation")
	}
}

// Verifying a task does not auto-satisfy an unrelated obligation:
// the verdict judges the task's claim, not the outstanding review
// duty bound to it.
func TestCoverageVerifiedDoesNotSatisfyObligation(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv-ob")
	seedOwnedSession(t, "w", "pv-ob")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	tk, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	ob, err := requireObligation(tk.Run, tk.ID, "independent_review", "digest:"+p, `["`+p+`"]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	fileAllPassVerdicts(t, id, p, "reviewer-01")
	if err := verifyTask(id, true, "", OperationRequest{Actor: "reviewer-01", RequestID: newRequestID()}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got, _ := getTask(id); got.Status != TaskVerified {
		t.Fatalf("expected verified task, got %s", got.Status)
	}
	got, err := getObligation(ob)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ObligationRequired {
		t.Fatalf("unrelated obligation must stay required after verify, got %q", got.Status)
	}
}

// The blocking set is the work still owed: required, running,
// and stale rows block; satisfied and excepted rows do not.
func TestObligationBlockingSet(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-ob", "agent", "ship feature", "feature works")
	if err != nil {
		t.Fatal(err)
	}
	reqd, err := requireObligation("run-ob", tk.ID, "independent_review", "digest:a", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := requireObligation("run-ob", tk.ID, "security_verification", "digest:a", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(run, ObligationRunning, testReq("lead")); err != nil {
		t.Fatal(err)
	}
	done, err := requireObligation("run-ob", tk.ID, "integration_testing", "digest:a", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(done, ObligationRunning, testReq("lead")); err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(done, ObligationSatisfied, testReq("lead")); err != nil {
		t.Fatal(err)
	}
	waived, err := requireObligation("run-ob", tk.ID, "independent_review", "digest:b", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(waived, ObligationExcepted, testReq("lead"), "out of scope for this run"); err != nil {
		t.Fatalf("except with reason: %v", err)
	}
	// Blocking without reason is refused: the ledger must say why.
	hold, err := requireObligation("run-ob", tk.ID, "security_verification", "digest:b", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setObligationState(hold, ObligationBlocked, testReq("lead")); err == nil {
		t.Fatal("blocked without a reason must be refused")
	}
	// Supersede parks the old row as stale and opens the new revision.
	stale, err := requireObligation("run-ob", tk.ID, "integration_testing", "digest:old", `[]`, testReq("lead"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supersedeObligation(stale, "digest:new", testReq("lead")); err != nil {
		t.Fatalf("supersede: %v", err)
	}
	blocking, err := obligationsBlocking("run-ob")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range blocking {
		seen[o.ID] = true
	}
	for _, want := range []string{reqd, run, hold, stale} {
		if !seen[want] {
			t.Fatalf("obligation %s must block, got %+v", want, blocking)
		}
	}
	for _, notWant := range []string{done, waived} {
		if seen[notWant] {
			t.Fatalf("obligation %s must not block, got %+v", notWant, blocking)
		}
	}
	// Direct transition to stale is refused: staleness comes only
	// from an explicit supersede carrying the new target revision.
	if err := setObligationState(hold, ObligationStale, testReq("lead")); err == nil {
		t.Fatal("direct transition to stale must be refused; use supersede")
	}
}

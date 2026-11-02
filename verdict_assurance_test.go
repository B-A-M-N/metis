package main

// verdict_assurance_test.go: failing-first proof for F-03/F-04.
//
// F-03: verification proved evidence integrity, not task
// correctness — verifyTask accepted an asserted reviewer string
// plus PASS as technical proof. Now judgments carry a typed
// evidence source and only machine_check / independent_review
// count as technical evidence; operator acceptance never
// promotes.
//
// F-04: task_verdicts keyed one row per (task, attempt,
// criterion) discarded conflicting same-attempt judgments. Now
// judgments are append-only under judgment IDs; PASS+FAIL on one
// criterion and attempt both persist, the disagreement stays
// visible, and verification blocks until independent resolution.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileJudgment files one judgment with an explicit method,
// bypassing the fileAllPassVerdicts default.
func fileJudgment(t *testing.T, taskID, criterion, verdict, reviewer, method, path, reason string) {
	t.Helper()
	got, err := getTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	v := CriterionVerdict{
		CriterionID: criterion,
		Verdict:     verdict,
		Reviewer:    reviewer,
		Method:      method,
		Evidence:    []string{path},
		Reason:      reason,
		Attempt:     got.Attempt,
	}
	if err := fileCriterionVerdict(taskID, v, testReq("probe")); err != nil {
		t.Fatalf("file %s by %s: %v", verdict, reviewer, err)
	}
}

func judgmentCount(t *testing.T, taskID string, attempt int) int {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	all, err := judgmentsForAttempt(db, taskID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	return len(all)
}

// A fabricated PASS on a hash-valid but wrong artifact must not
// verify when its only support is operator acceptance: asserted
// strings without independent outcome evidence prove nothing.
func TestAssuranceOperatorAcceptanceIsNotTechnicalProof(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa1")
	seedOwnedSession(t, "w", "fa1")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	// The artifact is intact (hash-valid) but substantively wrong;
	// the only judgment is an operator's bare acceptance.
	fileJudgment(t, id, "C1", VerdictPass, "op-1", JudgmentMethodOperatorAcceptance, p, "looks fine to me")
	err := verifyTask(id, true, "", testReq("op-1"))
	if err == nil {
		t.Fatal("operator acceptance alone must not verify")
	} else if !strings.Contains(err.Error(), "not technical evidence") {
		t.Fatalf("refusal must name the missing technical evidence: %v", err)
	}
	if got, _ := getTask(id); got.Status != TaskReportReady {
		t.Fatalf("refused verify must not move the task, got %s", got.Status)
	}
	assurance, err := taskAssurance(id)
	if err != nil {
		t.Fatal(err)
	}
	if assurance.Assurance != AssuranceNone {
		t.Fatalf("assurance = %q, want none", assurance.Assurance)
	}
	c, ok := assurance.Criteria["C1"]
	if !ok {
		t.Fatal("C1 missing from assurance projection")
	}
	if c.Disposition != AssessmentOperatorAccepted {
		t.Fatalf("C1 disposition = %q, want operator-accepted", c.Disposition)
	}
	if len(c.Judgments) != 1 {
		t.Fatalf("operator judgment must persist as an honest record, got %d", len(c.Judgments))
	}
}

// PASS and FAIL on the same criterion and attempt both persist;
// the disagreement is visible and verification blocks until
// independent resolution.
func TestAssuranceConflictingJudgmentsPersistAndBlockVerify(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa2")
	seedOwnedSession(t, "w", "fa2")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	fileJudgment(t, id, "C1", VerdictPass, "reviewer-a", JudgmentMethodIndependentReview, p, "reproduced the widget")
	fileJudgment(t, id, "C1", VerdictFail, "reviewer-b", JudgmentMethodIndependentReview, p, "widget missing on my checkout")
	if n := judgmentCount(t, id, got.Attempt); n != 2 {
		t.Fatalf("both conflicting judgments must persist, got %d", n)
	}
	err := verifyTask(id, true, "", testReq("reviewer-a"))
	if err == nil {
		t.Fatal("a disputed criterion must block verification")
	} else if !strings.Contains(err.Error(), "conflicting") || !strings.Contains(err.Error(), "independent resolution") {
		t.Fatalf("refusal must name the conflict and demand independent resolution: %v", err)
	}
	assurance, err := taskAssurance(id)
	if err != nil {
		t.Fatal(err)
	}
	if assurance.Disposition != TaskDispositionDisputed {
		t.Fatalf("task disposition = %q, want disputed", assurance.Disposition)
	}
	c := assurance.Criteria["C1"]
	if c.Disposition != AssessmentDisputed {
		t.Fatalf("C1 disposition = %q, want disputed", c.Disposition)
	}
	if len(c.Judgments) != 2 {
		t.Fatalf("both sides must stay visible, got %d judgments", len(c.Judgments))
	}
}

// A reviewer superseding their own finding resolves their share:
// latest-per-reviewer unanimity at the current revision unblocks
// verification without rewriting history.
func TestAssuranceSelfSupersedeResolvesDispute(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa3")
	seedOwnedSession(t, "w", "fa3")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	fileJudgment(t, id, "C1", VerdictPass, "reviewer-a", JudgmentMethodIndependentReview, p, "reproduced the widget")
	fileJudgment(t, id, "C1", VerdictFail, "reviewer-b", JudgmentMethodIndependentReview, p, "stale checkout, my mistake pending")
	fileJudgment(t, id, "C1", VerdictPass, "reviewer-b", JudgmentMethodIndependentReview, p, "re-checked on fresh checkout: present")
	if n := judgmentCount(t, id, got.Attempt); n != 3 {
		t.Fatalf("supersession appends; history must hold 3 judgments, got %d", n)
	}
	if err := verifyTask(id, true, "", testReq("reviewer-a")); err != nil {
		t.Fatalf("unanimous latest-per-reviewer PASS must verify: %v", err)
	}
	assurance, err := taskAssurance(id)
	if err != nil {
		t.Fatal(err)
	}
	if assurance.Disposition != TaskDispositionVerified || assurance.Assurance != AssuranceTechnical {
		t.Fatalf("verified task must report verified/technical, got %s/%s",
			assurance.Disposition, assurance.Assurance)
	}
}

// A judgment reviews the evidence revision it was filed at: new
// evidence supersedes the old judgment instead of inheriting it.
func TestAssuranceStaleRevisionJudgmentDoesNotCarry(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa4")
	seedOwnedSession(t, "w", "fa4")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	fileJudgment(t, id, "C1", VerdictPass, "reviewer-a", JudgmentMethodIndependentReview, p, "checked the filed evidence")
	workdir := loadRegistry()["w"].Workdir
	p2 := workdir + "/result2.txt"
	if err := os.WriteFile(p2, []byte("more evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(id, p2); err != nil {
		t.Fatal(err)
	}
	err := verifyTask(id, true, "", testReq("reviewer-a"))
	if err == nil {
		t.Fatal("a judgment on a superseded revision must not verify new evidence")
	} else if !strings.Contains(err.Error(), "current-revision") {
		t.Fatalf("refusal must name the revision binding: %v", err)
	}
	got, _ := getTask(id)
	if n := judgmentCount(t, id, got.Attempt); n != 1 {
		t.Fatalf("stale judgments stay in history (append-only), got %d", n)
	}
}

// Legacy task_verdicts rows migrate into the append-only ledger
// with reviewer, verdict, evidence, reason, and request ID
// intact — old judgments keep counting instead of vanishing.
func TestAssuranceLegacyRowsMigrateWithAttribution(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa5")
	seedOwnedSession(t, "w", "fa5")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureVerdictsTable(db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO task_verdicts
		(task_id, attempt_no, criterion_id, verdict, reviewer, evidence, reason, request_id, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, got.Attempt, "C1", VerdictPass, "legacy-reviewer", `["`+p+`"]`,
		"legacy reason", "req-legacy-1", nowUTC()); err != nil {
		db.Close()
		t.Fatalf("seed legacy verdict: %v", err)
	}
	db.Close()
	// Filing a conflicting judgment triggers migration; both the
	// legacy PASS and the new FAIL must persist side by side.
	fileJudgment(t, id, "C1", VerdictFail, "reviewer-new", JudgmentMethodIndependentReview, p, "fresh eyes disagree")
	db2, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	all, err := judgmentsForAttempt(db2, id, got.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("legacy + new judgments must both persist, got %d", len(all))
	}
	byReviewer := map[string]CriterionJudgment{}
	for _, j := range all {
		byReviewer[j.Reviewer] = j
	}
	legacy, ok := byReviewer["legacy-reviewer"]
	if !ok {
		t.Fatalf("migrated legacy judgment missing: %+v", all)
	}
	if legacy.Verdict != VerdictPass || legacy.Reason != "legacy reason" || legacy.RequestID != "req-legacy-1" {
		t.Fatalf("legacy attribution mangled: %+v", legacy)
	}
	if len(legacy.Evidence) != 1 || legacy.Evidence[0] != p {
		t.Fatalf("legacy evidence mangled: %+v", legacy.Evidence)
	}
	if err := verifyTask(id, true, "", testReq("reviewer-new")); err == nil {
		t.Fatal("migrated PASS vs new FAIL is a dispute; verification must block")
	} else if !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("refusal must name the conflict: %v", err)
	}
}

// An evidence-free task completed via operator override reports
// override / operator-accepted: honest approval, never technical
// correctness.
func TestAssuranceOperatorOverrideDisposition(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa6")
	seedOwnedSession(t, "worker2", "fa6")
	tk, err := createTask("runv", "worker2", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "worker2"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "op-1", testReq("op-1")); err != nil {
		t.Fatalf("operator approval should complete the evidence-free task: %v", err)
	}
	assurance, err := taskAssurance(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assurance.Disposition != TaskDispositionOverride {
		t.Fatalf("disposition = %q, want override", assurance.Disposition)
	}
	if assurance.Assurance != AssuranceOperatorAccepted {
		t.Fatalf("assurance = %q, want operator-accepted, never technical", assurance.Assurance)
	}
}

// Unknown judgment methods are refused at the gate: the typed
// source is closed, not free-form.
func TestAssuranceUnknownMethodRefused(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fa7")
	seedOwnedSession(t, "w", "fa7")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	v := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass, Reviewer: "reviewer-01",
		Method: "vibes", Evidence: []string{p}, Reason: "trust me", Attempt: got.Attempt}
	if err := fileCriterionVerdict(id, v, testReq("reviewer")); err == nil {
		t.Fatal("unknown judgment method must be refused")
	} else if !strings.Contains(err.Error(), "method") {
		t.Fatalf("refusal must name the method: %v", err)
	}
}

// The coverage gate keeps its required-reviews role only:
// judgments never satisfy review duties, and satisfying duties
// never substitutes for a judgment. Both directions pinned.
func TestCoverageGateKeepsRequiredReviewsRole(t *testing.T) {
	run, work := coverageSetup(t, "assure")
	tk, err := createTask(run, "w", "touch auth", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	p := filepath.Join(work, "auth.go")
	if err := os.WriteFile(p, []byte("package auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	// Judgments filed while the review duty is outstanding change
	// nothing about the duty: it still blocks.
	fileAllPassVerdicts(t, tk.ID, p, "reviewer")
	decision, err := assessCoverageGate(tk.ID, tk.Attempt, reportDigestOf("done"))
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Blocked() {
		t.Fatal("outstanding review duty must block despite filed judgments")
	}
	if !gateBlockingIsReviewOnly(decision) {
		t.Fatalf("gate blocking set must carry review duties only: %+v", decision.Blocking)
	}
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err == nil {
		t.Fatal("verify must refuse while the review duty is outstanding")
	} else if !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("refusal must name the obligation: %v", err)
	}
	// Satisfying the duty unblocks completion but proves no
	// correctness: remove the judgments' force by checking the
	// gate opens, while correctness still rests on the ledger.
	obs, err := listObligations(run, tk.ID)
	if err != nil || len(obs) != 1 {
		t.Fatalf("setup: obligations = %+v, err %v", obs, err)
	}
	if err := setObligationState(obs[0].ID, ObligationRunning, testReq("reviewer")); err != nil {
		t.Fatalf("reviewer pickup: %v", err)
	}
	if err := setObligationState(obs[0].ID, ObligationSatisfied, testReq("reviewer")); err != nil {
		t.Fatalf("independent satisfaction refused: %v", err)
	}
	decision, err = assessCoverageGate(tk.ID, tk.Attempt, reportDigestOf("done"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.Blocked() {
		t.Fatalf("satisfied duty must open the gate: %+v", decision.Blocking)
	}
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err != nil {
		t.Fatalf("verify after duty satisfaction refused: %v", err)
	}
}

// Same-second filings must keep filing order: the created stamp is
// second-resolution while IDs carry a random suffix (P0-A), so an
// id tiebreak shuffles same-second rows and latest-per-reviewer
// becomes a coin flip. The tiebreak must be insertion order.
func TestJudgmentsSameSecondKeepFilingOrder(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureJudgmentsTable(db); err != nil {
		t.Fatal(err)
	}
	const sameSecond = "2026-01-01T00:00:00Z"
	filed := []struct{ id, verdict string }{
		{"judg-zzz-filed-first", VerdictFail},
		{"judg-aaa-filed-second", VerdictPass},
	}
	for _, f := range filed {
		if _, err := db.Exec(`INSERT INTO criterion_judgments
			(id, task_id, criterion_id, verdict, reviewer, method,
			 evidence, evidence_rev, reason, attempt_no, request_id, created)
			VALUES (?, 'task-tie', 'C1', ?, 'reviewer-b', 'independent_review',
			 '[]', 'rev1', 'tie pin', 1, 'req-tie', ?)`,
			f.id, f.verdict, sameSecond); err != nil {
			t.Fatalf("insert %s: %v", f.id, err)
		}
	}
	got, err := judgmentsForAttempt(db, "task-tie", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != filed[0].id || got[1].ID != filed[1].id {
		ids := []string{}
		for _, j := range got {
			ids = append(ids, j.ID)
		}
		t.Fatalf("same-second rows must keep filing order, got %v", ids)
	}
}

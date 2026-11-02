package main

// Slice 2 (remediation Phase B): criterion-specific external
// verification. Evidence integrity (digests match) is not
// requirement satisfaction (an independent reviewer says each
// criterion is met, citing evidence). These tests pin that hashes
// alone never verify, INCONCLUSIVE never counts as success, and
// verdicts are bound to the exact attempt and evidence.

import (
	"os"
	"strings"
	"testing"
)

// fileAllPassVerdicts files a PASS verdict for every acceptance
// criterion of the task's current attempt, citing path as evidence.
// Shared by suites that build a reported task by hand and then
// assert verification succeeds.
func fileAllPassVerdicts(t *testing.T, taskID, path, reviewer string) {
	t.Helper()
	got, err := getTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range criteriaFromAcceptance(got.Acceptance) {
		v := CriterionVerdict{
			CriterionID: c.ID,
			Verdict:     VerdictPass,
			Reviewer:    reviewer,
			Evidence:    []string{path},
			Reason:      "independently reproduced",
			Attempt:     got.Attempt,
		}
		if err := fileCriterionVerdict(taskID, v, testReq("reviewer")); err != nil {
			t.Fatalf("verdict %s: %v", c.ID, err)
		}
	}
}

func setupReportedTask(t *testing.T, session, objective, acceptance string) (taskID, artifactPath string) {
	t.Helper()
	tk, err := createTask("run-v", "agent", objective, acceptance)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, session, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	// Dispatch claims the attempt; the pickup names the committed
	// attempt, not the pre-dispatch zero value.
	committed, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTaskWorking(tk.ID, committed.Attempt, testReq("worker-loop/"+session)); err != nil {
		t.Fatal(err)
	}
	workdir := loadRegistry()[session].Workdir
	p := workdir + "/result.txt"
	if err := os.WriteFile(p, []byte("evidence bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceWorkerClaim, Reporter: session}); err != nil {
		t.Fatal(err)
	}
	return tk.ID, p
}

// Digest-valid artifacts with no criterion verdicts must not verify:
// hashes prove the bytes are unchanged, not that the requirement
// is satisfied (TD-100).
func TestVerifyRefusedWithoutCriterionVerdicts(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv1")
	seedOwnedSession(t, "w", "pv1")
	id, _ := setupReportedTask(t, "w", "build widget", "widget exists\nwidget passes checks")
	err := verifyTask(id, true, "", OperationRequest{Actor: "reviewer-01", RequestID: newRequestID()})
	if err == nil {
		t.Fatal("verify must be refused when no criterion verdict exists")
	}
	if !strings.Contains(err.Error(), "verdict") {
		t.Fatalf("refusal must name the missing verdicts: %v", err)
	}
	if got, _ := getTask(id); got.Status != TaskReportReady {
		t.Fatalf("refused verify must not move the task, got %s", got.Status)
	}
}

// INCONCLUSIVE is an honest non-result, not success: verify stays
// refused until every criterion has a PASS.
func TestVerifyRefusedOnInconclusive(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv2")
	seedOwnedSession(t, "w", "pv2")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists\nwidget passes checks")
	got, _ := getTask(id)
	v := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "saw it", Attempt: got.Attempt}
	if err := fileCriterionVerdict(id, v, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	v2 := CriterionVerdict{CriterionID: "C2", Verdict: VerdictInconclusive, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "could not reproduce either way", Attempt: got.Attempt}
	if err := fileCriterionVerdict(id, v2, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	err := verifyTask(id, true, "", OperationRequest{Actor: "reviewer-01", RequestID: newRequestID()})
	if err == nil {
		t.Fatal("INCONCLUSIVE must not count as success")
	}
}

// A FAIL verdict blocks --pass: the reviewer found the requirement
// unmet, and the ledger must not overrule that judgment.
func TestVerifyRefusedOnFailVerdict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv3")
	seedOwnedSession(t, "w", "pv3")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	v := CriterionVerdict{CriterionID: "C1", Verdict: VerdictFail, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "widget missing", Attempt: got.Attempt}
	if err := fileCriterionVerdict(id, v, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(id, true, "", OperationRequest{Actor: "reviewer-01", RequestID: newRequestID()}); err == nil {
		t.Fatal("a FAIL verdict must block verification")
	}
}

// Full path: PASS verdicts on every criterion, bound to the
// attempt with cited evidence, verify the task.
func TestVerifyPassesWithAllCriteriaSatisfied(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv4")
	seedOwnedSession(t, "w", "pv4")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists\nwidget passes checks")
	fileAllPassVerdicts(t, id, p, "reviewer-01")
	if err := verifyTask(id, true, "", OperationRequest{Actor: "reviewer-01", RequestID: newRequestID()}); err != nil {
		t.Fatalf("verify with all criteria PASS must succeed: %v", err)
	}
	if got, _ := getTask(id); got.Status != TaskVerified {
		t.Fatalf("expected verified, got %s", got.Status)
	}
}

// Verdict bindings: stale attempt, foreign evidence, unknown
// criterion, empty reason, self-review, and double-filing are all
// refused.
func TestCriterionVerdictBindings(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv5")
	seedOwnedSession(t, "w", "pv5")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	base := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "ok", Attempt: got.Attempt}

	stale := base
	stale.Attempt = got.Attempt + 1
	if err := fileCriterionVerdict(id, stale, testReq("r")); err == nil {
		t.Fatal("verdict for a non-current attempt must be refused")
	} else if !strings.Contains(err.Error(), "attempt") {
		t.Fatalf("refusal must name the attempt binding: %v", err)
	}

	foreign := base
	foreign.Evidence = []string{"/elsewhere/other.txt"}
	if err := fileCriterionVerdict(id, foreign, testReq("r")); err == nil {
		t.Fatal("verdict citing non-attempt evidence must be refused")
	}

	unknown := base
	unknown.CriterionID = "C9"
	if err := fileCriterionVerdict(id, unknown, testReq("r")); err == nil {
		t.Fatal("verdict for an unknown criterion must be refused")
	}

	silent := base
	silent.Reason = "  "
	if err := fileCriterionVerdict(id, silent, testReq("r")); err == nil {
		t.Fatal("verdict without a reason must be refused")
	}

	self := base
	self.Reviewer = "w"
	if err := fileCriterionVerdict(id, self, testReq("r")); err == nil {
		t.Fatal("worker self-review must be refused (TD-094)")
	}

	bad := base
	bad.Verdict = "MAYBE"
	if err := fileCriterionVerdict(id, bad, testReq("r")); err == nil {
		t.Fatal("unknown disposition must be refused")
	}

	if err := fileCriterionVerdict(id, base, testReq("r")); err != nil {
		t.Fatalf("first filing must succeed: %v", err)
	}
	if err := fileCriterionVerdict(id, base, testReq("r2")); err == nil {
		t.Fatal("second verdict for the same criterion+attempt must be refused")
	} else if !strings.Contains(err.Error(), "already") {
		t.Fatalf("refusal must name the duplicate: %v", err)
	}
}

// A criterion failing at two attempts files one bounded
// repeated_failure escalation; a criterion passing then failing
// files conflicting_findings (the correction regressed it). A
// first-time FAIL files nothing: one failure is a verdict, not a
// pattern.
func TestRepeatedFailureAttention(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv4")
	seedOwnedSession(t, "w", "pv4")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists\nwidget passes checks")
	fail := func(criterion string, attempt int, path string) {
		t.Helper()
		v := CriterionVerdict{CriterionID: criterion, Verdict: VerdictFail, Reviewer: "reviewer-01",
			Evidence: []string{path}, Reason: "widget missing", Attempt: attempt}
		if err := fileCriterionVerdict(id, v, testReq("reviewer")); err != nil {
			t.Fatal(err)
		}
	}
	fail("C1", 1, p)
	if open, err := listAttention("run-v", true); err != nil || len(open) != 0 {
		t.Fatalf("first FAIL is a verdict, not a pattern: %+v %v", open, err)
	}
	// Second attempt: bump the ledger, attach fresh evidence, fail again.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET attempt = 2 WHERE id = ?`, id); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	workdir := loadRegistry()["w"].Workdir
	p2 := workdir + "/result2.txt"
	if err := os.WriteFile(p2, []byte("evidence bytes 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(id, p2); err != nil {
		t.Fatal(err)
	}
	fail("C1", 2, p2)
	open, err := listAttention("run-v", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "repeated_failure" || open[0].TaskID != id {
		t.Fatalf("second FAIL must file one repeated_failure: %+v", open)
	}
}

// PASS then FAIL on the same criterion is a genuine conflict
// across attempts, not a repeat.
func TestRegressedCriterionAttention(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pv5")
	seedOwnedSession(t, "w", "pv5")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists")
	got, _ := getTask(id)
	pass := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "saw it", Attempt: got.Attempt}
	if err := fileCriterionVerdict(id, pass, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET attempt = 2 WHERE id = ?`, id); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	workdir := loadRegistry()["w"].Workdir
	p2 := workdir + "/result2.txt"
	if err := os.WriteFile(p2, []byte("evidence bytes 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(id, p2); err != nil {
		t.Fatal(err)
	}
	fail := CriterionVerdict{CriterionID: "C1", Verdict: VerdictFail, Reviewer: "reviewer-01",
		Evidence: []string{p2}, Reason: "regressed", Attempt: 2}
	if err := fileCriterionVerdict(id, fail, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	open, err := listAttention("run-v", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "conflicting_findings" {
		t.Fatalf("pass-then-fail must file conflicting_findings: %+v", open)
	}
}

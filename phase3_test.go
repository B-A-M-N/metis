package main

// Phase-3 slice 1: snapshot-bound verification + worker-claim separation
// (TD-085-100, CB-06/07/08, CB-14).
//
// A digest proves bytes are unchanged. It does not prove the bytes
// satisfy the assignment. These tests pin the corrected contract:
// verification judges the worker's filed claim against the task's
// immutable acceptance criteria and the exact attempt's evidence,
// decided by an attributed verifier — never by the worker itself.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func phase3Setup(t *testing.T, owner, sess string) string {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, owner)
	seedOwnedSession(t, sess, owner)
	return loadRegistry()[sess].Workdir
}

// Verification must judge the claim against declared acceptance criteria:
// a digest-valid artifact that fulfills no stated requirement must not
// verify the task (TD-100, review §C acceptance test).
func TestVerifyRequiresAcceptance(t *testing.T) {
	work := phase3Setup(t, "p3a", "w")
	tk, err := createTask("run-p3a", "agent", "fix it", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "fixed!", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("digest-valid artifact with no acceptance criteria must not verify")
	} else if !strings.Contains(err.Error(), "acceptance") {
		t.Fatalf("refusal must name acceptance criteria: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("refused verify changed status to %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM verification_evidence WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("refused verify must record no evidence rows, got %d", n)
	}
}

// A worker's success claim plus tampered evidence must not verify
// (TD-098, CB-06): the report digest is intact while the file bytes
// changed, so the claim is judged against the evidence and refused.
// The filed claim survives the refusal; restoring the bytes verifies.
func TestWorkerClaimCannotSelfVerify(t *testing.T) {
	work := phase3Setup(t, "p3b", "w")
	tk, err := createTask("run-p3b", "agent", "fix it", "tests pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "all fixed", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("forged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("tampered evidence must not verify")
	} else if !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("refusal must name evidence integrity: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("refused verify changed status to %s", got.Status)
	}
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("all fixed")))
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND digest = ?`, tk.ID, want); n != 1 {
		t.Fatalf("filed worker claim must survive refused verification, got %d rows", n)
	}
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Restored digests still need a criterion judgment: hashes alone
	// never verify.
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("intact digests without a verdict must not verify")
	}
	fileAllPassVerdicts(t, tk.ID, p, "verifier")
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err != nil {
		t.Fatalf("restored evidence should verify: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskVerified {
		t.Fatalf("expected verified, got %s", got.Status)
	}
}

// Evidence binds to the attempt that produced it (TD-088): tampered
// attempt-1 evidence is out of scope once attempt 2 is current. Only
// the current attempt's artifacts are judged.
func TestArtifactScopedToAttempt(t *testing.T) {
	work := phase3Setup(t, "p3c", "w")
	tk, err := createTask("run-p3c", "agent", "fix it", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	ev1 := filepath.Join(work, "ev1.txt")
	if err := os.WriteFile(ev1, []byte("a1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, ev1); err != nil {
		t.Fatal(err)
	}
	if err := retryDispatch(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	ev2 := filepath.Join(work, "ev2.txt")
	if err := os.WriteFile(ev2, []byte("a2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, ev2); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	// Tamper the superseded attempt's evidence: out of scope.
	if err := os.WriteFile(ev1, []byte("forged"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileAllPassVerdicts(t, tk.ID, ev2, "verifier")
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err != nil {
		t.Fatalf("stale attempt-1 evidence must be out of scope: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskVerified {
		t.Fatalf("expected verified, got %s", got.Status)
	}
	var att1, att2 int
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT attempt_no FROM artifacts WHERE task_id = ? AND path = ?`, tk.ID, ev1).Scan(&att1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT attempt_no FROM artifacts WHERE task_id = ? AND path = ?`, tk.ID, ev2).Scan(&att2); err != nil {
		t.Fatal(err)
	}
	if att1 != 1 || att2 != 2 {
		t.Fatalf("artifacts must bind their producing attempts, got %d and %d", att1, att2)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND attempt_no = 2`, tk.ID); n != 1 {
		t.Fatalf("report history must bind attempt 2, got %d rows", n)
	}
}

// One report per attempt (TD-087): a second report at the same attempt
// is refused and history stays append-only — the first claim is never
// overwritten by a louder one.
func TestDuplicateReportRefused(t *testing.T) {
	phase3Setup(t, "p3d", "w")
	tk, err := createTask("run-p3d", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r1", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r2", Source: SourceCLISubmission, Reporter: "test"}); err == nil {
		t.Fatal("second report at the same attempt must be refused")
	} else if !strings.Contains(err.Error(), "already") {
		t.Fatalf("refusal must say a report was already filed: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Report != "r1" {
		t.Fatalf("filed claim must not be overwritten, got %q", got.Report)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("report history must stay append-only with 1 row, got %d", n)
	}
}

// Rejection is an attributed verifier decision (TD-095, CB-08): the
// fail verdict names who decided and under which request. The operator
// path is likewise attributed, recorded as an override.
func TestVerifyAttributesVerifier(t *testing.T) {
	phase3Setup(t, "p3e", "w")
	tk, err := createTask("run-p3e", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	req := testReq("verifier-1")
	if err := verifyTask(tk.ID, false, "", req); err != nil {
		t.Fatal(err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "rejected" && strings.Contains(r.Detail, "verifier-1") && strings.Contains(r.Detail, req.RequestID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejection must attribute verifier and request: %+v", listReceiptsOrFail(t, tk.ID))
	}
	tk2, err := createTask("run-p3e", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk2.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk2.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	opReq := testReq("op-9")
	if err := verifyTask(tk2.ID, true, "op-9", opReq); err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range listReceiptsOrFail(t, tk2.ID) {
		if r.Kind == "operator_approved" && strings.Contains(r.Detail, "op-9") && strings.Contains(r.Detail, opReq.RequestID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("operator override must attribute approver and request: %+v", listReceiptsOrFail(t, tk2.ID))
	}
}

// Corrections carry the exact prior evidence (CB-14): the child records
// which parent artifacts it must address, or records that there were none.
func TestCorrectionCarriesPriorArtifacts(t *testing.T) {
	work := phase3Setup(t, "p3f", "w")
	tk, err := createTask("run-p3f", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := attachArtifact(tk.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	child, err := followupTask(tk.ID, "fix it", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, child.ID) {
		if r.Kind == "correction_basis" && strings.Contains(r.Detail, tk.ID) && strings.Contains(r.Detail, a.Digest) {
			found = true
		}
	}
	if !found {
		t.Fatalf("correction must cite parent artifact evidence: %+v", listReceiptsOrFail(t, child.ID))
	}
	seedOwnedSession(t, "w2", "p3f")
	bare, err := createTask("run-p3f", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(bare.ID, "w2"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(bare.ID, ReportFiling{Report: "nothing to show", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	bareChild, err := followupTask(bare.ID, "do better", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range listReceiptsOrFail(t, bareChild.ID) {
		if r.Kind == "correction_basis" && strings.Contains(r.Detail, "artifacts=none") {
			found = true
		}
	}
	if !found {
		t.Fatalf("correction with no parent evidence must say so: %+v", listReceiptsOrFail(t, bareChild.ID))
	}
}

// No self-approval (TD-094): the worker session that produced the
// output MUST NOT satisfy its own independent-review condition. A
// pass verdict or operator override decided by the worker's own
// identity is refused. A self-reported failure is not self-approval
// and stays allowed — but still attributed.
func TestVerifierMustBeIndependent(t *testing.T) {
	work := phase3Setup(t, "p3i", "w")
	tk, err := createTask("run-p3i", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("w")); err == nil {
		t.Fatal("worker must not verify its own output")
	} else if !strings.Contains(err.Error(), "independent") {
		t.Fatalf("refusal must name independence: %v", err)
	}
	if err := verifyTask(tk.ID, true, "w", testReq("w")); err == nil {
		t.Fatal("worker must not approve its own override")
	}
	// Restore path: an independent reviewer judges the criterion,
	// then an independent verifier decides.
	fileAllPassVerdicts(t, tk.ID, p, "reviewer")
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err != nil {
		t.Fatalf("independent verifier refused: %v", err)
	}
	fail, err := createTask("run-p3i", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(fail.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(fail.ID, ReportFiling{Report: "broken", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(fail.ID, false, "", testReq("w")); err != nil {
		t.Fatalf("self-reported failure must stay allowed: %v", err)
	}
	if got, _ := getTask(fail.ID); got.Status != TaskFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
}

// Failure preservation (TD-091): a failed attempt stays recorded —
// report, rejection, and evidence — even after a linked correction
// succeeds.
func TestFailurePreservedAcrossCorrection(t *testing.T) {
	work := phase3Setup(t, "p3j", "w")
	tk, err := createTask("run-p3j", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "wrong answer", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	child, err := followupTask(tk.ID, "correct it", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	seedOwnedSession(t, "w2", "p3j")
	if err := dispatchTask(child.ID, "w2"); err != nil {
		t.Fatal(err)
	}
	cwork := loadRegistry()["w2"].Workdir
	cp := filepath.Join(cwork, "fix.txt")
	if err := os.WriteFile(cp, []byte("right"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(child.ID, cp); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(child.ID, ReportFiling{Report: "fixed", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	fileAllPassVerdicts(t, child.ID, cp, "reviewer")
	if err := verifyTask(child.ID, true, "", testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	parent, _ := getTask(tk.ID)
	if parent.Status != TaskFailed || parent.Report != "wrong answer" {
		t.Fatalf("parent failure must be preserved: %+v", parent)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("parent rejection receipt must survive the correction")
	}
}

// Contradiction preservation (TD-093, CB-07): conflicting findings
// from different workers stay independently attributed; no invented
// consensus is merged anywhere.
func TestContradictionsStayAttributed(t *testing.T) {
	phase3Setup(t, "p3k", "w")
	seedOwnedSession(t, "w2", "p3k")
	a, err := createTask("run-p3k", "agent", "audit", "acc")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p3k", "agent", "audit", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(a.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(b.ID, "w2"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(a.ID, ReportFiling{Report: "vulnerable", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(b.ID, ReportFiling{Report: "clean", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	got, err := compareReports("run-p3k")
	if err != nil || len(got) != 2 {
		t.Fatalf("both conflicting reports must compare: %v %+v", err, got)
	}
	seen := map[string]bool{}
	for _, id := range []string{a.ID, b.ID} {
		rows := 0
		db, err := openTasksDB()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, id).Scan(&rows); err != nil {
			db.Close()
			t.Fatal(err)
		}
		db.Close()
		if rows != 1 {
			t.Fatalf("report history for %s must hold exactly its claim, got %d rows", id, rows)
		}
		cur, err := getTask(id)
		if err != nil {
			t.Fatal(err)
		}
		seen[cur.Report] = true
	}
	if !seen["vulnerable"] || !seen["clean"] {
		t.Fatalf("both findings must survive: %v", seen)
	}
}

// Review snapshot binding across workers (CB-08, TD-095): when another
// worker with authorized shared access modifies the evidence file
// after attach, verification refuses instead of judging the new bytes
// as the submission.
func TestReviewBoundAcrossWorkers(t *testing.T) {
	work := phase3Setup(t, "p3l", "w")
	seedOwnedSession(t, "w2", "p3l")
	tk, err := createTask("run-p3l", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("submission"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	other, err := createTask("run-p3l", "agent", "other", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeSharedWorkspace(other.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(other.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	// The other worker modifies the shared file after the submission.
	if err := os.WriteFile(p, []byte("changed by w"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err == nil {
		t.Fatal("modified-after-attach evidence must not verify the original submission")
	}
	if err := os.WriteFile(p, []byte("submission"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Digests match again, but verification still needs the
	// reviewer's criterion judgment.
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err == nil {
		t.Fatal("matching digests without a verdict must not verify")
	}
	fileAllPassVerdicts(t, tk.ID, p, "reviewer")
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err != nil {
		t.Fatalf("restored submission should verify: %v", err)
	}
}

// Immutable acceptance criteria (TD-087): no lifecycle step — report,
// refusal, verify, or correction — weakens or replaces the criteria
// the task was accepted under.
func TestAcceptanceImmutable(t *testing.T) {
	phase3Setup(t, "p3m", "w")
	tk, err := createTask("run-p3m", "agent", "obj", "original criteria")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	child, err := followupTask(tk.ID, "fix", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{tk.ID, child.ID} {
		got, _ := getTask(id)
		if got.Acceptance != "original criteria" {
			t.Fatalf("acceptance mutated on %s: %q", id, got.Acceptance)
		}
	}
}

// Report provenance (TD-092): every filed claim stays attributable to
// the worker, task, and attempt that produced it.
func TestReportProvenance(t *testing.T) {
	phase3Setup(t, "p3n", "w")
	tk, err := createTask("run-p3n", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "claim", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var taskID, actor, digest string
	var attempt int
	if err := db.QueryRow(`SELECT task_id, attempt_no, actor, digest FROM task_reports WHERE task_id = ?`, tk.ID).Scan(&taskID, &attempt, &actor, &digest); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("claim")))
	if taskID != tk.ID || attempt != got.Attempt || actor != "w" || digest != want {
		t.Fatalf("report history must attribute worker/task/attempt/digest, got %s %d %s %s", taskID, attempt, actor, digest)
	}
}

// The same path attached twice in one attempt is a duplicate filing,
// not independent evidence (TD-088).
func TestAttachSamePathSameAttemptRefused(t *testing.T) {
	work := phase3Setup(t, "p3g", "w")
	tk, err := createTask("run-p3g", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err == nil {
		t.Fatal("re-attaching the same path in one attempt must be refused")
	} else if !strings.Contains(err.Error(), "already attached") {
		t.Fatalf("refusal must name the duplicate: %v", err)
	}
}

// Evidence cannot be planted after the verdict: attachment on a
// terminal task is refused.
func TestAttachTerminalRefused(t *testing.T) {
	work := phase3Setup(t, "p3h", "w")
	tk, err := createTask("run-p3h", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "op", testReq("op")); err != nil {
		t.Fatal(err)
	}
	late := filepath.Join(work, "late.txt")
	if err := os.WriteFile(late, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, late); err == nil {
		t.Fatal("attaching evidence to a terminal task must be refused")
	}
}

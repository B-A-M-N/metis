package main

// verification.go: independent verdicts on reported tasks.
//
// Verification judges a worker's claim against evidence; it never
// produces the claim. A pass verdict requires the task's immutable
// acceptance criteria, per-criterion PASS verdicts from an
// independent reviewer, and evidence integrity at this attempt.
// Extracted from controller.go; behavior unchanged.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
)

// verifyTask records the independent verdict on a reported task, made
// by the attributed verifier in req — never by the worker whose claim
// is being judged (TD-095, CB-06/07/08). A passing verdict requires
// all of: the task's immutable acceptance criteria to be non-empty
// (TD-100), every current-attempt artifact to re-hash to its filed
// digest (TD-098, TD-088), and the artifacts to belong to the exact
// attempt under review. A task with no current-attempt evidence needs
// an explicit operator decision recorded as an override, not as
// technical verification (TD-096). Each per-artifact check is written
// to verification_evidence so the verdict is auditable (TD-098). A
// worker's report alone can never produce a verified status.
// SECURITY (P0 review): req.Actor and approve are asserted local
// identities, authenticated only by OS process ownership of the CLI
// invocation. That suffices while every caller is local: TD-094
// (worker != verifier/approver) is enforced and tested. Once a
// remote transport (MCP/node runner) exists, remote principals MUST
// present an authenticated identity here — a bare reviewer string
// from across the transport must never satisfy independence.
func verifyTask(id string, pass bool, approve string, req OperationRequest) (opErr error) {
	defer func() { observeOps(opsPhaseVerification, req.RequestID, id, 0, "", opErr) }()
	if err := requireRequest(req); err != nil {
		return err
	}
	// A remote principal may assert any actor name, so it can
	// never satisfy independence: pass verdicts and overrides
	// require the local operator. Recording failure stays
	// allowed — retracting a claim confers no approval.
	if pass && req.Remote {
		return fmt.Errorf("remote principal %q cannot record a pass verdict or override for task %s; independence requires the local operator (asserted actor %q is not authority)",
			req.Principal, id, req.Actor)
	}
	t, err := getTask(id)
	if err != nil {
		return err
	}
	if t.Status != TaskReportReady {
		return fmt.Errorf("task %s is %s; only a reported task can be verified or rejected", id, t.Status)
	}
	if !pass {
		if err := transitionTask(id, []string{TaskReportReady}, TaskFailed, ""); err != nil {
			return err
		}
		recordReceipt(id, "rejected", fmt.Sprintf("actor=%s req=%s verdict=FAIL", req.Actor, req.RequestID))
		if _, err := recordOrchEvent(OrchEvent{
			RunID: t.Run, OriginPrincipal: req.Actor, TaskID: id, Attempt: t.Attempt,
			EventType: OrchVerificationRecorded, RequestID: req.RequestID,
			Disposition: "rejected",
		}); err != nil {
			recordReceipt(id, "orch_gap", fmt.Sprintf("verification.recorded feed write failed: %v", err))
		}
		return nil
	}
	// No self-approval (TD-094): the worker session that produced the
	// output must not satisfy its own independent-review condition.
	// A self-reported failure stays allowed — retracting a claim is
	// not approving it — but every pass verdict needs a reviewer the
	// worker does not control.
	if pass && req.Actor == t.Agent {
		return fmt.Errorf("task %s: verifier %q is the worker that produced attempt %d; pass verdicts require an independent reviewer (TD-094)", id, req.Actor, t.Attempt)
	}
	arts, err := currentAttemptArtifacts(id, t.Attempt)
	if err != nil {
		return err
	}
	if len(arts) == 0 {
		if strings.TrimSpace(approve) == "" {
			return fmt.Errorf("task %s attempt %d has no evidence; attach artifacts or approve explicitly as an override", id, t.Attempt)
		}
		// An override is still a completion: outstanding coverage
		// duties refuse it. Waive explicitly via excepted, never by
		// overriding past the duty.
		if gerr := coverageGate(id, t.Attempt, req); gerr != nil {
			return gerr
		}
		if approve == t.Agent {
			return fmt.Errorf("task %s: approver %q is the worker that produced attempt %d; overrides require an identity independent of the worker (TD-094)", id, approve, t.Attempt)
		}
		if err := transitionTask(id, []string{TaskReportReady}, TaskVerified, ""); err != nil {
			return err
		}
		recordReceipt(id, "operator_approved", fmt.Sprintf("actor=%s req=%s override by %s without attempt-%d evidence", req.Actor, req.RequestID, approve, t.Attempt))
		if _, err := recordOrchEvent(OrchEvent{
			RunID: t.Run, OriginPrincipal: req.Actor, TaskID: id, Attempt: t.Attempt,
			EventType: OrchVerificationRecorded, RequestID: req.RequestID,
			Payload: fmt.Sprintf("override by %s", approve), Disposition: "override",
		}); err != nil {
			recordReceipt(id, "orch_gap", fmt.Sprintf("verification.recorded feed write failed: %v", err))
		}
		return nil
	}
	// A digest proves the bytes are unchanged, not that they satisfy
	// the assignment: evidence without declared criteria to judge it
	// against cannot verify (TD-100).
	if strings.TrimSpace(t.Acceptance) == "" {
		return fmt.Errorf("task %s has no acceptance criteria; digest-valid evidence alone cannot verify it (TD-100)", id)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return err
	}
	for _, a := range arts {
		data, rerr := os.ReadFile(a.Path)
		sum := ""
		checkErr := ""
		if rerr != nil {
			checkErr = rerr.Error()
		} else {
			sum = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
		}
		ok := checkErr == "" && sum == a.Digest
		if _, ierr := db.Exec(`INSERT INTO verification_evidence
			(task_id, attempt_no, path, digest_at_verify, ok, verifier, request_id, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, t.Attempt, a.Path, sum, boolToInt(ok), req.Actor, req.RequestID, nowUTC()); ierr != nil {
			return ierr
		}
		if !ok {
			detail := "digest mismatch"
			if checkErr != "" {
				detail = checkErr
			}
			recordReceipt(id, "evidence_checked", fmt.Sprintf("actor=%s req=%s attempt=%d %s %s", req.Actor, req.RequestID, t.Attempt, a.Path, detail))
			return fmt.Errorf("artifact %s failed evidence integrity for task %s attempt %d; task stays %s", a.Path, id, t.Attempt, t.Status)
		}
		recordReceipt(id, "evidence_checked", fmt.Sprintf("actor=%s req=%s attempt=%d %s ok %s", req.Actor, req.RequestID, t.Attempt, a.Path, a.Digest))
	}
	// Requirement satisfaction (remediation Phase B, F-03/F-04):
	// digests prove the bytes are unchanged, not that they satisfy
	// the assignment. Every acceptance criterion needs a
	// current-revision PASS from a technical evidence source
	// (machine_check or independent_review) — an asserted reviewer
	// string plus PASS with no independent outcome evidence proves
	// nothing, and operator acceptance never promotes to
	// technical correctness. Conflicting same-attempt judgments
	// persist in the append-only ledger, so a PASS/FAIL split at
	// the revision under review blocks verification until
	// independent resolution. A FAIL blocks verification;
	// INCONCLUSIVE is a non-result, never success.
	criteria := criteriaFromAcceptance(t.Acceptance)
	currentRev, rerr := evidenceRevisionFor(id, t.Attempt)
	if rerr != nil {
		return rerr
	}
	assessments, verr := assessmentsForAttempt(db, id, t.Attempt, currentRev)
	if verr != nil {
		return verr
	}
	for _, c := range criteria {
		a, ok := assessments[c.ID]
		if !ok {
			return fmt.Errorf("task %s attempt %d: criterion %s (%s) has no verdict; evidence integrity alone cannot verify (TD-100)",
				id, t.Attempt, c.ID, c.Requirement)
		}
		switch a.Disposition {
		case AssessmentPass:
		case AssessmentDisputed:
			return fmt.Errorf("task %s attempt %d: criterion %s has conflicting judgments at the current evidence revision (%s vs %s); independent resolution required before verification",
				id, t.Attempt, c.ID, describeSide(a, VerdictPass), describeSide(a, VerdictFail))
		case AssessmentFail:
			return fmt.Errorf("task %s attempt %d: criterion %s judged FAIL by %s: %s",
				id, t.Attempt, c.ID, describeSide(a, VerdictFail), failReason(a))
		case AssessmentOperatorAccepted:
			return fmt.Errorf("task %s attempt %d: criterion %s has only operator acceptance (by %s); operator acceptance is not technical evidence — a machine check or independent review at the current evidence revision is required",
				id, t.Attempt, c.ID, describeSide(a, VerdictPass))
		case AssessmentStale:
			return fmt.Errorf("task %s attempt %d: criterion %s has no current-revision verdict (%d judgment(s) review superseded evidence); re-judge the current evidence revision before verification",
				id, t.Attempt, c.ID, len(a.Judgments))
		case AssessmentInconclusive:
			return fmt.Errorf("task %s attempt %d: criterion %s is %s, not PASS; INCONCLUSIVE never counts as success",
				id, t.Attempt, c.ID, VerdictInconclusive)
		default:
			return fmt.Errorf("task %s attempt %d: criterion %s (%s) has no current-revision verdict; evidence integrity alone cannot verify (TD-100)",
				id, t.Attempt, c.ID, c.Requirement)
		}
	}
	// Coverage is a completion gate, not a verdict input: criteria
	// can all pass while a mandatory review duty is still owed.
	if gerr := coverageGate(id, t.Attempt, req); gerr != nil {
		return gerr
	}
	if err := transitionTask(id, []string{TaskReportReady}, TaskVerified, ""); err != nil {
		return err
	}
	recordReceipt(id, "verified", fmt.Sprintf("actor=%s req=%s attempt=%d %d artifact(s) integrity-ok, %d criteria PASS",
		req.Actor, req.RequestID, t.Attempt, len(arts), len(criteria)))
	if _, err := recordOrchEvent(OrchEvent{
		RunID: t.Run, OriginPrincipal: req.Actor, TaskID: id, Attempt: t.Attempt,
		EventType: OrchVerificationRecorded, RequestID: req.RequestID,
		Payload: fmt.Sprintf("attempt=%d artifacts=%d criteria=%d PASS",
			t.Attempt, len(arts), len(criteria)), Disposition: "verified",
	}); err != nil {
		recordReceipt(id, "orch_gap", fmt.Sprintf("verification.recorded feed write failed: %v", err))
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// currentSide returns the latest-per-reviewer judgments behind
// one side (PASS or FAIL) of an assessment at the revision under
// review: grandfathered rows count, superseded revisions do not.
func currentSide(a CriterionAssessment, verdict string) []CriterionJudgment {
	latest := map[string]CriterionJudgment{}
	for _, j := range a.Judgments {
		if j.EvidenceRev != "" && j.EvidenceRev != a.EvidenceRev {
			continue
		}
		latest[j.Reviewer] = j
	}
	var out []CriterionJudgment
	for _, j := range latest {
		if j.Verdict == verdict {
			out = append(out, j)
		}
	}
	return out
}

// describeSide names the reviewers behind one side of an
// assessment (PASS or FAIL), latest-per-reviewer at the current
// revision, so refusals attribute the disagreement instead of
// hiding it.
func describeSide(a CriterionAssessment, verdict string) string {
	var names []string
	for _, j := range currentSide(a, verdict) {
		names = append(names, j.Reviewer+" ("+methodOrDefault(j.Method)+")")
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// failReason returns the reason behind the current-revision FAIL
// side of an assessment for refusal messages.
func failReason(a CriterionAssessment) string {
	for _, j := range currentSide(a, VerdictFail) {
		if isTechnicalMethod(j.Method) {
			return j.Reason
		}
	}
	return "no reason recorded"
}

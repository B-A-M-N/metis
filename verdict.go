package main

// verdict.go: criterion-specific external verification
// (remediation Phase B, F-03/F-04 assurance hardening).
//
// Three concepts stay distinct:
//
// Evidence integrity — did the expected attempt produce the
// recorded artifact, and does its digest match? (Checked by
// verifyTask against verification_evidence.)
//
// Requirement satisfaction — did an independent reviewer
// determine that the exact requirement was met, citing evidence?
// (A criterion judgment, filed here.)
//
// Operator approval — did an authorized operator accept the
// result or explicitly override missing verification? (The
// --approve path in verifyTask, recorded as an override.)
//
// F-03: verification used to accept an asserted reviewer string
// plus PASS as technical proof. Judgments now carry a typed
// evidence source (method): only machine_check and
// independent_review outcomes count as technical evidence.
// operator_acceptance is an honest record that NEVER promotes to
// technical correctness — verifyTask refuses a task whose only
// support is operator acceptance.
//
// F-04: task_verdicts keyed one row per (task, attempt,
// criterion) silently discarded conflicting same-attempt
// judgments. Judgments are now append-only rows in
// criterion_judgments keyed by judgment ID; disagreements persist
// side by side and the separate criterion_assessments projection
// aggregates them. verifyTask blocks on a disputed criterion
// until independent resolution (a reviewer superseding their own
// judgment, or a fresh evidence revision re-judged unanimously).
//
// Metis validates verdict schema, reviewer independence,
// attempt binding, evidence-revision binding, and evidence
// references. It never generates a judgment by inference: a
// report with only valid hashes never transitions to verified,
// and INCONCLUSIVE never counts as success.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Verdict dispositions. INCONCLUSIVE is an honest non-result:
// it blocks verification, never satisfies a criterion.
const (
	VerdictPass         = "PASS"
	VerdictFail         = "FAIL"
	VerdictInconclusive = "INCONCLUSIVE"
)

// Judgment methods: the typed evidence source behind one
// judgment. Only the technical methods can satisfy a criterion;
// operator_acceptance is recorded honestly but never counts as
// technical proof (F-03).
const (
	JudgmentMethodMachineCheck       = "machine_check"
	JudgmentMethodIndependentReview  = "independent_review"
	JudgmentMethodOperatorAcceptance = "operator_acceptance"
)

// Assessment dispositions for one criterion at one evidence
// revision: the aggregate projection over every current-revision
// judgment, latest per reviewer.
const (
	AssessmentUnjudged         = "unjudged"
	AssessmentPass             = "pass"
	AssessmentFail             = "fail"
	AssessmentInconclusive     = "inconclusive"
	AssessmentDisputed         = "disputed"
	AssessmentOperatorAccepted = "operator-accepted"
	AssessmentStale            = "stale"
)

// Assurance levels: what the ledger actually knows. Technical
// assurance requires unanimous technical-method PASS judgments
// at the current evidence revision; operator acceptance never
// rises above operator-accepted (F-03).
const (
	AssuranceNone             = "none"
	AssuranceTechnical        = "technical"
	AssuranceOperatorAccepted = "operator-accepted"
)

// CriterionVerdict is one reviewer's judgment on one acceptance
// criterion at one attempt, with cited evidence and a reason.
// The reviewer is asserted (like --reporter/--verifier), not
// authenticated — but it must be independent of the worker.
// Method names the typed evidence source; empty defaults to
// independent_review on filing for backward compatibility.
type CriterionVerdict struct {
	CriterionID string   `json:"criterion_id"`
	Verdict     string   `json:"verdict"`
	Reviewer    string   `json:"reviewer"`
	Method      string   `json:"method,omitempty"`
	Evidence    []string `json:"evidence"`
	Reason      string   `json:"reason"`
	Attempt     int      `json:"attempt"`
}

// CriterionJudgment is one append-only judgment row: the filing
// principal, the typed method, the evidence revision reviewed,
// the verdict, and the request that filed it. Conflicting
// judgments on the same criterion and attempt persist side by
// side under distinct judgment IDs (F-04).
type CriterionJudgment struct {
	ID          string   `json:"id"`
	CriterionID string   `json:"criterion_id"`
	Verdict     string   `json:"verdict"`
	Reviewer    string   `json:"reviewer"`
	Method      string   `json:"method"`
	Evidence    []string `json:"evidence"`
	EvidenceRev string   `json:"evidence_rev"`
	Reason      string   `json:"reason"`
	Attempt     int      `json:"attempt"`
	RequestID   string   `json:"request_id"`
	Created     string   `json:"created"`
}

// CriterionAssessment is the aggregate projection over one
// criterion's current-revision judgments: latest judgment per
// reviewer, then a disposition. The raw judgments stay attached
// so no disagreement is ever hidden by the aggregate.
type CriterionAssessment struct {
	CriterionID string              `json:"criterion_id"`
	Attempt     int                 `json:"attempt"`
	EvidenceRev string              `json:"evidence_rev"`
	Disposition string              `json:"disposition"`
	Assurance   string              `json:"assurance"`
	Judgments   []CriterionJudgment `json:"judgments"`
}

// Validate checks the verdict's shape against the task it judges:
// known disposition, typed method, non-empty reason, a criterion
// from the task's own acceptance list, the current attempt, a
// reviewer independent of the worker, and evidence paths that
// are all artifacts of that same attempt.
func (v CriterionVerdict) Validate(t Task, attemptPaths map[string]bool) error {
	switch v.Verdict {
	case VerdictPass, VerdictFail, VerdictInconclusive:
	default:
		return fmt.Errorf("unknown verdict %q: want PASS, FAIL, or INCONCLUSIVE", v.Verdict)
	}
	switch v.Method {
	case "", JudgmentMethodMachineCheck, JudgmentMethodIndependentReview, JudgmentMethodOperatorAcceptance:
	default:
		return fmt.Errorf("unknown judgment method %q: want machine_check, independent_review, or operator_acceptance", v.Method)
	}
	if strings.TrimSpace(v.Reason) == "" {
		return fmt.Errorf("verdict on %s needs a reason; an unexplained judgment verifies nothing", v.CriterionID)
	}
	known := false
	for _, c := range criteriaFromAcceptance(t.Acceptance) {
		if c.ID == v.CriterionID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("criterion %q is not in task %s acceptance; verdicts cite declared criteria only", v.CriterionID, t.ID)
	}
	if v.Attempt != t.Attempt {
		return fmt.Errorf("verdict names attempt %d but task %s is at attempt %d; verdicts bind to the current attempt",
			v.Attempt, t.ID, t.Attempt)
	}
	if strings.TrimSpace(v.Reviewer) == "" {
		return fmt.Errorf("verdict on %s needs a reviewer identity (asserted, not authenticated)", v.CriterionID)
	}
	if v.Reviewer == t.Agent {
		return fmt.Errorf("reviewer %q is the worker that produced attempt %d; pass verdicts require an independent reviewer (TD-094)",
			v.Reviewer, t.Attempt)
	}
	if len(v.Evidence) == 0 {
		return fmt.Errorf("verdict on %s cites no evidence; judgments cite current-attempt artifacts", v.CriterionID)
	}
	for _, p := range v.Evidence {
		if !attemptPaths[p] {
			return fmt.Errorf("verdict on %s cites %s, which is not an artifact of task %s attempt %d",
				v.CriterionID, p, t.ID, t.Attempt)
		}
	}
	return nil
}

// methodOrDefault resolves an empty method to the legacy
// default: filings that predate typed sources were independent
// reviewer judgments.
func methodOrDefault(m string) string {
	if strings.TrimSpace(m) == "" {
		return JudgmentMethodIndependentReview
	}
	return m
}

// isTechnicalMethod reports whether a judgment method can carry
// technical proof. Operator acceptance is honest attribution,
// never technical evidence (F-03).
func isTechnicalMethod(m string) bool {
	switch methodOrDefault(m) {
	case JudgmentMethodMachineCheck, JudgmentMethodIndependentReview:
		return true
	default:
		return false
	}
}

func ensureVerdictsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_verdicts (
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		criterion_id TEXT NOT NULL,
		verdict TEXT NOT NULL,
		reviewer TEXT NOT NULL,
		evidence TEXT NOT NULL DEFAULT '[]',
		reason TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		PRIMARY KEY (task_id, attempt_no, criterion_id)
	)`)
	return err
}

// ensureJudgmentsTable creates the append-only judgment ledger
// (F-04): one row per judgment keyed by judgment ID. The
// no-duplicate index refuses exact re-filings (same criterion,
// reviewer, verdict, and method) while conflicting judgments
// persist side by side.
func ensureJudgmentsTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS criterion_judgments (
		id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		criterion_id TEXT NOT NULL,
		verdict TEXT NOT NULL,
		reviewer TEXT NOT NULL,
		method TEXT NOT NULL DEFAULT 'independent_review',
		evidence TEXT NOT NULL DEFAULT '[]',
		evidence_rev TEXT NOT NULL DEFAULT '',
		reason TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS criterion_judgments_task
		ON criterion_judgments (task_id, attempt_no, criterion_id)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS criterion_judgments_nodup
		ON criterion_judgments (task_id, attempt_no, criterion_id, reviewer, verdict, method)`)
	return err
}

// migrateLegacyVerdicts copies pre-append-only task_verdicts rows
// into criterion_judgments preserving every attribution field
// (reviewer, verdict, evidence, reason, request_id). Legacy rows
// carry no evidence revision and predate typed methods: they
// project as grandfathered independent_review judgments, so old
// verdicts keep counting instead of silently vanishing. The
// copy is idempotent — already-migrated rows match on content
// and are skipped.
func migrateLegacyVerdicts(db *sql.DB) error {
	if err := ensureJudgmentsTable(db); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT rowid, task_id, attempt_no, criterion_id, verdict,
		reviewer, evidence, reason, request_id, created FROM task_verdicts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type legacy struct {
		rowid                                                                  int64
		taskID, criterionID, verdict, reviewer, evidence, reason, req, created string
		attempt                                                                int
	}
	var pending []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.rowid, &l.taskID, &l.attempt, &l.criterionID, &l.verdict,
			&l.reviewer, &l.evidence, &l.reason, &l.req, &l.created); err != nil {
			return err
		}
		pending = append(pending, l)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, l := range pending {
		var dup int
		if err := db.QueryRow(`SELECT COUNT(*) FROM criterion_judgments
			WHERE task_id = ? AND attempt_no = ? AND criterion_id = ?
			AND reviewer = ? AND verdict = ? AND reason = ? AND request_id = ?`,
			l.taskID, l.attempt, l.criterionID, l.reviewer, l.verdict, l.reason, l.req).Scan(&dup); err != nil {
			return err
		}
		if dup > 0 {
			continue
		}
		id := fmt.Sprintf("judg-migrated-%d", l.rowid)
		if _, err := db.Exec(`INSERT INTO criterion_judgments
			(id, task_id, attempt_no, criterion_id, verdict, reviewer, method,
				evidence, evidence_rev, reason, request_id, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
			id, l.taskID, l.attempt, l.criterionID, l.verdict, l.reviewer,
			JudgmentMethodIndependentReview, l.evidence, l.reason, l.req, l.created); err != nil {
			if strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE") {
				continue
			}
			return err
		}
	}
	return nil
}

// newJudgmentID mints an append-only judgment identity,
// collision-resistant across processes (P0-A).
func newJudgmentID() string {
	return newUniqueID("judg")
}

// evidenceRevisionFor binds a judgment to the exact evidence it
// reviewed: the filed report digest plus every current-attempt
// artifact digest, hashed in stable order. A judgment counts at
// verification only when its revision matches the revision under
// review; new evidence supersedes old judgments instead of
// inheriting them.
func evidenceRevisionFor(taskID string, attempt int) (string, error) {
	arts, err := currentAttemptArtifacts(taskID, attempt)
	if err != nil {
		return "", err
	}
	digest, err := reportDigestFor(taskID, attempt)
	if err != nil {
		return "", err
	}
	if len(arts) == 0 && strings.TrimSpace(digest) == "" {
		return "", nil
	}
	h := sha256.New()
	fmt.Fprintf(h, "report:%s\x00", digest)
	for _, a := range arts {
		fmt.Fprintf(h, "%s\x00%s\x00", a.Path, a.Digest)
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}

// fileCriterionVerdict records one external judgment.
// Append-only (F-04): a conflicting judgment on the same
// criterion and attempt persists beside the first — PASS+FAIL
// stays visible as a dispute instead of being refused or
// overwriting. Only an exact duplicate (same criterion,
// reviewer, verdict, and method) is refused, so a retried
// filing cannot stack identical rows. Only a reported task can
// collect verdicts.
func fileCriterionVerdict(taskID string, v CriterionVerdict, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	t, err := getTask(taskID)
	if err != nil {
		return err
	}
	if t.Status != TaskReportReady {
		return fmt.Errorf("task %s is %s; verdicts are filed against a reported task", taskID, t.Status)
	}
	arts, err := currentAttemptArtifacts(taskID, t.Attempt)
	if err != nil {
		return err
	}
	paths := map[string]bool{}
	for _, a := range arts {
		paths[a.Path] = true
	}
	if err := v.Validate(t, paths); err != nil {
		return err
	}
	v.Method = methodOrDefault(v.Method)
	rev, err := evidenceRevisionFor(taskID, t.Attempt)
	if err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureJudgmentsTable(db); err != nil {
		return err
	}
	if err := migrateLegacyVerdicts(db); err != nil {
		return err
	}
	ev, _ := json.Marshal(v.Evidence)
	if _, err := db.Exec(`INSERT INTO criterion_judgments
		(id, task_id, attempt_no, criterion_id, verdict, reviewer, method,
			evidence, evidence_rev, reason, request_id, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		newJudgmentID(), taskID, t.Attempt, v.CriterionID, v.Verdict, v.Reviewer,
		v.Method, string(ev), rev, v.Reason, req.RequestID, nowUTC()); err != nil {
		if strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("criterion %s of task %s attempt %d already has this %s judgment by %s; identical re-filings are refused, conflicting judgments persist side by side",
				v.CriterionID, taskID, t.Attempt, v.Verdict, v.Reviewer)
		}
		return err
	}
	recordReceipt(taskID, "verdict_filed", fmt.Sprintf("actor=%s req=%s %s=%s by %s method=%s rev=%s attempt=%d",
		req.Actor, req.RequestID, v.CriterionID, v.Verdict, v.Reviewer, v.Method, shortRev(rev), t.Attempt))
	// A filed FAIL is an authoritative transition: check whether the
	// same criterion already failed (repeated failure) or once passed
	// (the correction regressed it — genuinely conflicting findings
	// across attempts). Best-effort: filing is not safely retryable,
	// so a failed escalation must not fail the filing.
	raiseVerdictAttention(db, t, v.CriterionID, v.Verdict)
	return nil
}

// shortRev renders an evidence revision for receipts: a stable
// prefix instead of the full digest.
func shortRev(rev string) string {
	if len(rev) > 19 {
		return rev[:19]
	}
	if rev == "" {
		return "unrevisioned"
	}
	return rev
}

// raiseVerdictAttention files bounded escalations for verdict
// patterns the ledger alone cannot resolve. Identity is
// task-and-criterion scoped with stable reason/evidence, so the
// third and fourth failures reuse the row instead of opening new
// ones. Errors are swallowed deliberately (see caller).
func raiseVerdictAttention(db *sql.DB, t Task, criterionID, verdict string) {
	if verdict != VerdictFail {
		return
	}
	rows, err := db.Query(`SELECT verdict FROM criterion_judgments
		WHERE task_id = ? AND criterion_id = ? AND attempt_no < ?`, t.ID, criterionID, t.Attempt)
	if err != nil {
		return
	}
	defer rows.Close()
	sawFail, sawPass := false, false
	for rows.Next() {
		var prior string
		if err := rows.Scan(&prior); err != nil {
			return
		}
		switch prior {
		case VerdictFail:
			sawFail = true
		case VerdictPass:
			sawPass = true
		}
	}
	if err := rows.Err(); err != nil {
		return
	}
	file := func(kind, reason string) {
		evidence := fmt.Sprintf(`["criterion:%s"]`, criterionID)
		req := OperationRequest{Actor: "metis-control-plane",
			RequestID: fmt.Sprintf("attention:%s:%s:%s", kind, t.ID, criterionID)}
		_, _ = openAttention(req, t.Run, t.ID, t.Agent, kind, reason, evidence)
	}
	if sawFail {
		file("repeated_failure",
			fmt.Sprintf("criterion %s failed again (task %s)", criterionID, t.ID))
	}
	if sawPass {
		file("conflicting_findings",
			fmt.Sprintf("criterion %s passed before but fails now (task %s)", criterionID, t.ID))
	}
}

// scanJudgmentRow scans one union-ordered judgment row: the
// judgments-table columns plus a source tag. Shared by every
// read path so the snapshot, the legacy map, and the assessment
// projection never disagree on what a row means.
func scanJudgmentRow(rows *sql.Rows, source string) (CriterionJudgment, error) {
	var j CriterionJudgment
	var ev string
	err := rows.Scan(&j.ID, &j.CriterionID, &j.Verdict, &j.Reviewer, &j.Method,
		&ev, &j.EvidenceRev, &j.Reason, &j.Attempt, &j.RequestID, &j.Created)
	if err != nil {
		return j, err
	}
	_ = json.Unmarshal([]byte(ev), &j.Evidence)
	if j.Evidence == nil {
		j.Evidence = []string{}
	}
	j.Method = methodOrDefault(j.Method)
	_ = source
	return j, nil
}

// judgmentsForAttempt returns every judgment filed at one
// attempt — legacy task_verdicts rows (grandfathered as
// unrevisioned independent_review) plus append-only
// criterion_judgments rows — oldest first. Migrated copies are
// deduplicated by content so one judgment never counts twice.
func judgmentsForAttempt(db *sql.DB, taskID string, attempt int) ([]CriterionJudgment, error) {
	if err := ensureJudgmentsTable(db); err != nil {
		return nil, err
	}
	if err := migrateLegacyVerdicts(db); err != nil {
		return nil, err
	}
	// Tiebreak is rowid (insertion order), never the random-suffix
	// judgment id: created is second-resolution, so same-second
	// filings must keep filing order for latest-per-reviewer.
	rows, err := db.Query(`SELECT id, criterion_id, verdict, reviewer, method,
		evidence, evidence_rev, reason, attempt_no, request_id, created
		FROM criterion_judgments WHERE task_id = ? AND attempt_no = ?
		ORDER BY created, rowid`, taskID, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CriterionJudgment
	seen := map[string]bool{}
	for rows.Next() {
		j, err := scanJudgmentRow(rows, "judgments")
		if err != nil {
			return nil, err
		}
		key := judgmentDedupKey(j)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Legacy rows seeded directly (tests) or filed before the
	// first migration touchpoint: project them as grandfathered
	// judgments. Content-dedup against migrated copies.
	lrows, err := db.Query(`SELECT criterion_id, verdict, reviewer, evidence, reason, request_id, created
		FROM task_verdicts WHERE task_id = ? AND attempt_no = ?`, taskID, attempt)
	if err != nil {
		// A database that predates the verdicts table reports no
		// legacy judgments rather than failing the read.
		if strings.Contains(err.Error(), "no such table") {
			return out, nil
		}
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var j CriterionJudgment
		var ev string
		if err := lrows.Scan(&j.CriterionID, &j.Verdict, &j.Reviewer, &ev, &j.Reason, &j.RequestID, &j.Created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &j.Evidence)
		if j.Evidence == nil {
			j.Evidence = []string{}
		}
		j.ID = "legacy:" + j.RequestID + ":" + j.CriterionID
		j.Method = JudgmentMethodIndependentReview
		j.EvidenceRev = ""
		j.Attempt = attempt
		key := judgmentDedupKey(j)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, j)
	}
	if err := lrows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// judgmentsForAttemptTx is judgmentsForAttempt on an open
// transaction: the read-model snapshot's pinned revision must
// see both stores inside the same transaction. No migration
// writes here — a read transaction must never mutate — so
// legacy rows project directly and judgments-table rows are
// read as-is; content-dedup keeps migrated copies singular.
func judgmentsForAttemptTx(tx *sql.Tx, taskID string, attempt int) ([]CriterionJudgment, error) {
	// Tiebreak is rowid (insertion order), never the random-suffix
	// judgment id: created is second-resolution, so same-second
	// filings must keep filing order for latest-per-reviewer.
	rows, err := tx.Query(`SELECT id, criterion_id, verdict, reviewer, method,
		evidence, evidence_rev, reason, attempt_no, request_id, created
		FROM criterion_judgments WHERE task_id = ? AND attempt_no = ?
		ORDER BY created, rowid`, taskID, attempt)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			rows = nil
		} else {
			return nil, err
		}
	}
	var out []CriterionJudgment
	seen := map[string]bool{}
	if rows != nil {
		for rows.Next() {
			j, err := scanJudgmentRow(rows, "judgments")
			if err != nil {
				rows.Close()
				return nil, err
			}
			key := judgmentDedupKey(j)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, j)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	lrows, err := tx.Query(`SELECT criterion_id, verdict, reviewer, evidence, reason, request_id, created
		FROM task_verdicts WHERE task_id = ? AND attempt_no = ?`, taskID, attempt)
	if err != nil {
		return nil, err
	}
	defer lrows.Close()
	for lrows.Next() {
		var j CriterionJudgment
		var ev string
		if err := lrows.Scan(&j.CriterionID, &j.Verdict, &j.Reviewer, &ev, &j.Reason, &j.RequestID, &j.Created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &j.Evidence)
		if j.Evidence == nil {
			j.Evidence = []string{}
		}
		j.ID = "legacy:" + j.RequestID + ":" + j.CriterionID
		j.Method = JudgmentMethodIndependentReview
		j.EvidenceRev = ""
		j.Attempt = attempt
		key := judgmentDedupKey(j)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, j)
	}
	return out, lrows.Err()
}

// judgmentDedupKey identifies one judgment by content so a
// legacy row and its migrated copy collapse to one.
func judgmentDedupKey(j CriterionJudgment) string {
	return strings.Join([]string{
		j.CriterionID, j.Reviewer, methodOrDefault(j.Method),
		j.Verdict, j.Reason, j.RequestID, strings.Join(j.Evidence, "\x00"),
	}, "\x01")
}

// verdictsForAttempt returns the latest judgment per criterion at
// one attempt, keyed by criterion ID — the backward-compatible
// display projection over the append-only ledger. Disagreements
// are NOT collapsed here by fiat: the newest judgment wins the
// display slot, and the full dispute stays visible in the
// assessment projection (assessmentsForAttempt) and the task
// assurance read model.
func verdictsForAttempt(db *sql.DB, taskID string, attempt int) (map[string]CriterionVerdict, error) {
	all, err := judgmentsForAttempt(db, taskID, attempt)
	if err != nil {
		return nil, err
	}
	return latestVerdictMap(all), nil
}

// latestVerdictMap projects the newest judgment per criterion —
// the backward-compatible display over the append-only ledger.
// Single shared projector: the read-model snapshot and the
// legacy map entry point render byte-identical bundles.
func latestVerdictMap(all []CriterionJudgment) map[string]CriterionVerdict {
	out := map[string]CriterionVerdict{}
	for _, j := range all {
		out[j.CriterionID] = CriterionVerdict{
			CriterionID: j.CriterionID,
			Verdict:     j.Verdict,
			Reviewer:    j.Reviewer,
			Method:      j.Method,
			Evidence:    j.Evidence,
			Reason:      j.Reason,
			Attempt:     j.Attempt,
		}
	}
	return out
}

// assessmentsForAttempt aggregates one attempt's judgments into
// the per-criterion projection verification consumes. Only
// judgments reviewing the current evidence revision count; an
// empty revision is grandfathered (pre-revision legacy rows).
// Within the current revision each reviewer's latest judgment
// stands — a reviewer superseding their own finding resolves
// their share — and any remaining PASS/FAIL split across
// reviewers is a dispute that blocks verification until
// independent resolution.
func assessmentsForAttempt(db *sql.DB, taskID string, attempt int, currentRev string) (map[string]CriterionAssessment, error) {
	all, err := judgmentsForAttempt(db, taskID, attempt)
	if err != nil {
		return nil, err
	}
	return aggregateAssessments(all, attempt, currentRev), nil
}

// aggregateAssessments is the pure projection core: group by
// criterion, keep current-revision rows, collapse to latest per
// reviewer, then dispose. No DB, no clock — same judgments plus
// same revision always yield the same assessment.
func aggregateAssessments(all []CriterionJudgment, attempt int, currentRev string) map[string]CriterionAssessment {
	byCriterion := map[string][]CriterionJudgment{}
	for _, j := range all {
		if j.Attempt != attempt {
			continue
		}
		if j.EvidenceRev != "" && j.EvidenceRev != currentRev {
			continue
		}
		byCriterion[j.CriterionID] = append(byCriterion[j.CriterionID], j)
	}
	// Group every judgment by criterion first: criteria judged
	// only at superseded revisions are stale, not unjudged — the
	// history names what changed, so re-judging knows its target.
	allByCriterion := map[string][]CriterionJudgment{}
	for _, j := range all {
		if j.Attempt != attempt {
			continue
		}
		allByCriterion[j.CriterionID] = append(allByCriterion[j.CriterionID], j)
	}
	out := map[string]CriterionAssessment{}
	for cid, js := range byCriterion {
		latest := map[string]CriterionJudgment{}
		for _, j := range js {
			latest[j.Reviewer] = j
		}
		var cur []CriterionJudgment
		for _, j := range latest {
			cur = append(cur, j)
		}
		disposition, assurance := disposeCurrent(cur)
		out[cid] = CriterionAssessment{
			CriterionID: cid,
			Attempt:     attempt,
			EvidenceRev: currentRev,
			Disposition: disposition,
			Assurance:   assurance,
			Judgments:   allByCriterion[cid],
		}
	}
	for cid, js := range allByCriterion {
		if _, ok := out[cid]; ok {
			continue
		}
		out[cid] = CriterionAssessment{
			CriterionID: cid,
			Attempt:     attempt,
			EvidenceRev: currentRev,
			Disposition: AssessmentStale,
			Assurance:   AssuranceNone,
			Judgments:   js,
		}
	}
	return out
}

// disposeCurrent disposes one criterion from its reviewers'
// latest current-revision judgments. A PASS/FAIL split is a
// dispute; a lone FAIL fails; PASS with no FAIL passes even
// beside an INCONCLUSIVE (an honest non-result is not a
// counter-finding); operator acceptance alone is recorded but
// never technical.
func disposeCurrent(cur []CriterionJudgment) (disposition, assurance string) {
	var pass, fail, inconclusive, operatorOnly bool
	for _, j := range cur {
		if !isTechnicalMethod(j.Method) {
			if j.Verdict == VerdictPass {
				operatorOnly = true
			}
			continue
		}
		switch j.Verdict {
		case VerdictPass:
			pass = true
		case VerdictFail:
			fail = true
		case VerdictInconclusive:
			inconclusive = true
		}
	}
	switch {
	case pass && fail:
		return AssessmentDisputed, AssuranceNone
	case fail:
		return AssessmentFail, AssuranceNone
	case pass:
		return AssessmentPass, AssuranceTechnical
	case inconclusive:
		return AssessmentInconclusive, AssuranceNone
	case operatorOnly:
		return AssessmentOperatorAccepted, AssuranceNone
	default:
		return AssessmentUnjudged, AssuranceNone
	}
}

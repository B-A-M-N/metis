package main

// coverage_service.go: coordination of revision supersession and
// idempotent obligation generation through the repository.
//
// The service binds pure policy specs (coverage_policy.go) to
// durable rows (coverage_store.go): outstanding duties pointing at
// an older revision are superseded to the new digest first, then
// fresh duties are required idempotently. Every operation here is
// idempotent, so report, verify, and finalize checkpoints can all
// invoke it safely.
//
// The service decides NO verification outcomes: it never approves,
// verifies, or rejects a task, and never satisfies an obligation.
// Completion decisions belong to the verification authority via
// the gate (coverage_gate.go).

import (
	"database/sql"
	"fmt"
	"strings"
)

// generateCoverageObligations evaluates the run's pinned coverage
// rules against the task's filed evidence at digest and records
// what is owed. Outstanding duties pointing at an older revision
// are superseded to the new digest first — a changed revision
// invalidates the old review instead of stacking a second required
// row. Runs without a pinned policy, or without a filed report,
// generate nothing. Every operation here is idempotent, so report,
// verify, and finalize checkpoints can all invoke it safely.
func generateCoverageObligations(taskID, digest string, req OperationRequest) error {
	if strings.TrimSpace(digest) == "" {
		return nil
	}
	t, err := getTask(taskID)
	if err != nil {
		return err
	}
	pol, err := pinnedTeamPolicy(t.Run)
	if err != nil {
		if strings.Contains(err.Error(), "has no pinned policy") {
			return nil
		}
		return err
	}
	if len(pol.Coverage) == 0 {
		return nil
	}
	outstanding, err := listObligations(t.Run, taskID)
	if err != nil {
		return err
	}
	for _, o := range outstanding {
		if o.TargetRev == digest {
			continue
		}
		switch o.Status {
		case ObligationRequired, ObligationRunning, ObligationBlocked:
			if _, serr := supersedeObligation(o.ID, digest, req); serr != nil {
				return serr
			}
		}
	}
	return requireCoverageObligations(t, pol, digest, req)
}

// requireCoverageObligations records fresh duties for the filed
// evidence at digest without touching older rows: no supersede.
// The report checkpoint uses the full generate (new evidence
// invalidates old reviews); the verify checkpoint uses only this,
// because verification files no new evidence — superseding here
// would rewrite a duty's review target without new facts.
func requireCoverageObligations(t Task, pol teamPolicy, digest string, req OperationRequest) error {
	arts, err := currentAttemptArtifacts(t.ID, t.Attempt)
	if err != nil {
		return err
	}
	specs, err := evaluateCoveragePolicy(pol, arts)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if _, err := requireObligation(t.Run, t.ID, spec.Kind, digest, spec.EvidenceRefs, req); err != nil {
			return err
		}
	}
	return nil
}

// Transaction-scoped variants.
//
// generateCoverageObligationsTx and requireCoverageObligationsTx
// perform the same checkpoint as their connection-scoped twins,
// but every ledger write runs on tx so the caller can commit the
// coverage mutation atomically with its own rows. The report
// submission uses these inside its admission transaction: the
// duplicate-report check runs first on the same tx, so a refused
// report rolls back before any duty is superseded or required — a
// rejected digest never becomes the ledger's review target.
//
// Semantics match the twins exactly: same supersede-then-require
// order, same idempotency boundary (task, kind, targetRev), same
// error strings. Only the connection differs. Policy evaluation
// (pinnedTeamPolicy) and the evidence snapshot
// (currentAttemptArtifacts) are reads against tables this
// transaction never writes, so they stay on their own connections.
//
// Receipts are recorded in-tx via recordReceiptTx: advisory,
// best-effort, durable exactly when the transaction commits. A
// receipt write never fails the checkpoint — the reliability
// policy in task_receipts.go forbids it — so receipt errors are
// dropped and only ledger errors propagate.

// ensureObligationTableTx creates the ledger inside a transaction.
// DDL mirrors ensureObligationTable (coverage_store.go), which owns
// the canonical schema; this is its tx-scoped spelling.
func ensureObligationTableTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS coverage_obligations (
		id TEXT NOT NULL PRIMARY KEY,
		run_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		target_rev TEXT NOT NULL DEFAULT '',
		evidence_refs TEXT NOT NULL DEFAULT '[]',
		status TEXT NOT NULL DEFAULT 'required',
		reason TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS coverage_obligations_identity
		ON coverage_obligations (task_id, kind, target_rev)`)
	return err
}

// newObligationIDTx mirrors newObligationID for rows opened inside
// a transaction: same collision-resistant ob- identity (P0-A).
func newObligationIDTx() string {
	return newUniqueID("ob")
}

// requireObligationTx is requireObligation on tx: the duty to
// review (task, kind) at targetRev, or the existing row when the
// same evidence was already required.
func requireObligationTx(tx *sql.Tx, run, task, kind, targetRev, evidence string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if !obligationKinds[kind] {
		return "", fmt.Errorf("unknown obligation kind %q", kind)
	}
	if strings.TrimSpace(run) == "" || strings.TrimSpace(task) == "" {
		return "", fmt.Errorf("obligation requires run and task scope")
	}
	if strings.TrimSpace(targetRev) == "" {
		return "", fmt.Errorf("obligation requires a target revision to review")
	}
	if evidence == "" {
		evidence = "[]"
	}
	if err := ensureObligationTableTx(tx); err != nil {
		return "", err
	}
	var id string
	qerr := tx.QueryRow(`SELECT id FROM coverage_obligations
		WHERE task_id = ? AND kind = ? AND target_rev = ?`, task, kind, targetRev).Scan(&id)
	if qerr == nil {
		return id, nil
	}
	if qerr != sql.ErrNoRows {
		return "", qerr
	}
	now := nowUTC()
	id = newObligationIDTx()
	if _, err := tx.Exec(`INSERT INTO coverage_obligations
		(id, run_id, task_id, kind, target_rev, evidence_refs, status, created, updated, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, run, task, kind, targetRev, evidence, ObligationRequired, now, now, req.RequestID); err != nil {
		// Lost a concurrent require for the same identity: the
		// winner's row is the answer, never a second row.
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint failed") {
			if rerr := tx.QueryRow(`SELECT id FROM coverage_obligations
				WHERE task_id = ? AND kind = ? AND target_rev = ?`, task, kind, targetRev).Scan(&id); rerr == nil {
				return id, nil
			}
		}
		return "", err
	}
	_ = recordReceiptTx(tx, run, "obligation_required",
		fmt.Sprintf("actor=%s req=%s obligation=%s task=%s kind=%s rev=%s", req.Actor, req.RequestID, id, task, kind, targetRev))
	return id, nil
}

// supersedeObligationTx is supersedeObligation on tx: parks the
// non-terminal row as stale and opens its replacement at
// newTargetRev. Same guards, same error strings.
func supersedeObligationTx(tx *sql.Tx, id, newTargetRev string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(newTargetRev) == "" {
		return "", fmt.Errorf("supersede requires a new target revision")
	}
	if err := ensureObligationTableTx(tx); err != nil {
		return "", err
	}
	var cur coverageObligation
	err := tx.QueryRow(`SELECT id, run_id, task_id, kind, target_rev, evidence_refs,
		status, reason, created, updated, request_id
		FROM coverage_obligations WHERE id = ?`, id).Scan(
		&cur.ID, &cur.RunID, &cur.TaskID, &cur.Kind, &cur.TargetRev, &cur.Evidence,
		&cur.Status, &cur.Reason, &cur.Created, &cur.Updated, &cur.RequestID)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("no such obligation: %s", id)
	}
	if err != nil {
		return "", err
	}
	if cur.Status == ObligationSatisfied || cur.Status == ObligationStale || cur.Status == ObligationExcepted {
		return "", fmt.Errorf("obligation %s is %s (terminal); supersede applies only to outstanding work", id, cur.Status)
	}
	if newTargetRev == cur.TargetRev {
		return "", fmt.Errorf("obligation %s already reviews %q; supersede requires a new target revision", id, newTargetRev)
	}
	res, err := tx.Exec(`UPDATE coverage_obligations SET status = ?, updated = ?
		WHERE id = ? AND status NOT IN ('satisfied','stale','excepted')`,
		ObligationStale, nowUTC(), id)
	if err != nil {
		return "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", fmt.Errorf("obligation %s is no longer outstanding; refusing supersede", id)
	}
	next := newObligationIDTx()
	now := nowUTC()
	if _, err := tx.Exec(`INSERT INTO coverage_obligations
		(id, run_id, task_id, kind, target_rev, evidence_refs, status, created, updated, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		next, cur.RunID, cur.TaskID, cur.Kind, newTargetRev, cur.Evidence, ObligationRequired, now, now, req.RequestID); err != nil {
		return "", err
	}
	_ = recordReceiptTx(tx, cur.RunID, "obligation_superseded",
		fmt.Sprintf("actor=%s req=%s obligation=%s superseded-by=%s rev=%s", req.Actor, req.RequestID, id, next, newTargetRev))
	return next, nil
}

// listObligationsTx reads a run's obligations on tx; a non-empty
// task narrows the projection to one task.
func listObligationsTx(tx *sql.Tx, run, task string) ([]coverageObligation, error) {
	if err := ensureObligationTableTx(tx); err != nil {
		return nil, err
	}
	q := `SELECT id, run_id, task_id, kind, target_rev, evidence_refs,
		status, reason, created, updated, request_id
		FROM coverage_obligations WHERE run_id = ?`
	args := []any{run}
	if task != "" {
		q += ` AND task_id = ?`
		args = append(args, task)
	}
	// rowid tiebreak: obligation ids carry a random suffix, so
	// same-second rows must keep insertion order, not id order.
	q += ` ORDER BY created, rowid`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []coverageObligation
	for rows.Next() {
		var o coverageObligation
		if err := rows.Scan(&o.ID, &o.RunID, &o.TaskID, &o.Kind, &o.TargetRev, &o.Evidence,
			&o.Status, &o.Reason, &o.Created, &o.Updated, &o.RequestID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	if out == nil {
		out = []coverageObligation{}
	}
	return out, rows.Err()
}

// generateCoverageTxFault is a test-only fault injector for the
// tx-scoped coverage checkpoint: when non-nil, a non-nil return
// refuses the checkpoint with that error. Production code never
// sets it; nil is the steady state and preserves existing behavior
// exactly.
var generateCoverageTxFault func(taskID string) error

// generateCoverageObligationsTx is generateCoverageObligations on
// tx: outstanding duties pointing at an older revision are
// superseded to digest first, then fresh duties are required
// idempotently. Runs without a pinned policy, or without filed
// coverage rules, generate nothing.
func generateCoverageObligationsTx(tx *sql.Tx, t Task, digest string, req OperationRequest) error {
	if strings.TrimSpace(digest) == "" {
		return nil
	}
	pol, err := pinnedTeamPolicy(t.Run)
	if err != nil {
		if strings.Contains(err.Error(), "has no pinned policy") {
			return nil
		}
		return err
	}
	if len(pol.Coverage) == 0 {
		return nil
	}
	outstanding, err := listObligationsTx(tx, t.Run, t.ID)
	if err != nil {
		return err
	}
	for _, o := range outstanding {
		if o.TargetRev == digest {
			continue
		}
		switch o.Status {
		case ObligationRequired, ObligationRunning, ObligationBlocked:
			if _, serr := supersedeObligationTx(tx, o.ID, digest, req); serr != nil {
				return serr
			}
		}
	}
	// Test-only fault injection fires AFTER the supersede writes, so
	// the injected failure exercises the error path with the
	// transaction holding its write lock — the exact condition under
	// which recording the diagnostic before rollback wedges.
	if generateCoverageTxFault != nil {
		if ferr := generateCoverageTxFault(t.ID); ferr != nil {
			return ferr
		}
	}
	return requireCoverageObligationsTx(tx, t, pol, digest, req)
}

// requireCoverageObligationsTx is requireCoverageObligations on
// tx: fresh duties for the filed evidence at digest, no supersede.
// The report checkpoint uses the full generate; this require-only
// spelling exists for checkpoints that file no new evidence.
func requireCoverageObligationsTx(tx *sql.Tx, t Task, pol teamPolicy, digest string, req OperationRequest) error {
	arts, err := currentAttemptArtifacts(t.ID, t.Attempt)
	if err != nil {
		return err
	}
	specs, err := evaluateCoveragePolicy(pol, arts)
	if err != nil {
		return err
	}
	for _, spec := range specs {
		if _, err := requireObligationTx(tx, t.Run, t.ID, spec.Kind, digest, spec.EvidenceRefs, req); err != nil {
			return err
		}
	}
	return nil
}

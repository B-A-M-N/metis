package main

// coverage_store.go: durable storage for the delegation-coverage
// obligation ledger (OCO-B).
//
// An obligation is a durable review duty bound to one task and the
// exact target revision it reviews (an artifact digest or report
// id): independent review, security verification, integration
// testing. The ledger records what coverage is owed; it never
// performs the review itself.
//
// A verified task does NOT satisfy an unrelated obligation: the
// verdict judges the task's claim, never the outstanding duty.
// Unchanged already-verified evidence does NOT cause
// re-delegation: requireObligation is idempotent on
// (task, kind, targetRev).
//
// Dispositions: required -> running -> satisfied is the happy
// path. Any non-terminal row may move to blocked or excepted with
// a recorded reason. satisfied, stale, and excepted are terminal:
// a satisfied row is never silently reopened, and stale is entered
// only via an explicit supersede carrying the new targetRev.
//
// This file owns records, schema, queries, compare-and-set
// transitions, and revision identity. It performs NO policy
// evaluation and decides NO verification outcomes.

import (
	"database/sql"
	"fmt"
	"strings"
)

// Obligation dispositions. required, running, and stale block a
// run; satisfied and excepted do not.
const (
	ObligationRequired  = "required"
	ObligationRunning   = "running"
	ObligationSatisfied = "satisfied"
	ObligationStale     = "stale"
	ObligationBlocked   = "blocked"
	ObligationExcepted  = "excepted"
)

// obligationKinds are the only admissible review duties. A
// free-form kind would let callers smuggle new coverage types
// past review.
var obligationKinds = map[string]bool{
	"independent_review":    true,
	"security_verification": true,
	"integration_testing":   true,
}

// coverageObligation is one durable review duty.
type coverageObligation struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	TaskID    string `json:"task_id"`
	Kind      string `json:"kind"`
	TargetRev string `json:"target_rev"`
	Evidence  string `json:"evidence_refs"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
	RequestID string `json:"request_id"`
}

func ensureObligationTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS coverage_obligations (
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
	)`)
	if err != nil {
		return err
	}
	// Idempotency boundary: one row per (task, kind, revision).
	// Unchanged evidence reuses the row; a new revision is new work.
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS coverage_obligations_identity
		ON coverage_obligations (task_id, kind, target_rev)`)
	return err
}

// newObligationID mirrors newRequestID: a stable collision-resistant
// ob- identity (P0-A).
func newObligationID() string {
	return newUniqueID("ob")
}

// requireObligation records the duty to review (task, kind) at
// targetRev, or returns the existing row when the same evidence
// was already required: unchanged evidence never re-delegates.
// Evidence refs default to an empty list.
func requireObligation(run, task, kind, targetRev, evidence string, req OperationRequest) (string, error) {
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
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureObligationTable(db); err != nil {
		return "", err
	}
	var id string
	qerr := db.QueryRow(`SELECT id FROM coverage_obligations
		WHERE task_id = ? AND kind = ? AND target_rev = ?`, task, kind, targetRev).Scan(&id)
	if qerr == nil {
		return id, nil
	}
	if qerr != sql.ErrNoRows {
		return "", qerr
	}
	now := nowUTC()
	id = newObligationID()
	if _, err := db.Exec(`INSERT INTO coverage_obligations
		(id, run_id, task_id, kind, target_rev, evidence_refs, status, created, updated, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, run, task, kind, targetRev, evidence, ObligationRequired, now, now, req.RequestID); err != nil {
		// Lost a concurrent require for the same identity: the
		// winner's row is the answer, never a second row.
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "constraint failed") {
			if rerr := db.QueryRow(`SELECT id FROM coverage_obligations
				WHERE task_id = ? AND kind = ? AND target_rev = ?`, task, kind, targetRev).Scan(&id); rerr == nil {
				return id, nil
			}
		}
		return "", err
	}
	recordReceipt(run, "obligation_required",
		fmt.Sprintf("actor=%s req=%s obligation=%s task=%s kind=%s rev=%s", req.Actor, req.RequestID, id, task, kind, targetRev))
	return id, nil
}

// getObligation reads one obligation by id.
func getObligation(id string) (coverageObligation, error) {
	var o coverageObligation
	db, err := openTasksDB()
	if err != nil {
		return o, err
	}
	defer db.Close()
	if err := ensureObligationTable(db); err != nil {
		return o, err
	}
	err = db.QueryRow(`SELECT id, run_id, task_id, kind, target_rev, evidence_refs,
		status, reason, created, updated, request_id
		FROM coverage_obligations WHERE id = ?`, id).Scan(
		&o.ID, &o.RunID, &o.TaskID, &o.Kind, &o.TargetRev, &o.Evidence,
		&o.Status, &o.Reason, &o.Created, &o.Updated, &o.RequestID)
	if err == sql.ErrNoRows {
		return o, fmt.Errorf("no such obligation: %s", id)
	}
	return o, err
}

// obligationTransitions are the legal moves out of each
// non-terminal disposition. satisfied, stale, and excepted are
// terminal: a satisfied row is never silently reopened. stale is
// absent here on purpose — it is entered only via
// supersedeObligation, which carries the new target revision.
var obligationTransitions = map[string]map[string]bool{
	ObligationRequired: {ObligationRunning: true, ObligationBlocked: true, ObligationExcepted: true},
	ObligationRunning:  {ObligationSatisfied: true, ObligationBlocked: true, ObligationExcepted: true},
	ObligationBlocked:  {ObligationRunning: true, ObligationExcepted: true},
}

// setObligationState moves one obligation along its legal
// transitions. Moves to blocked or excepted require a reason so
// the ledger says why coverage stopped. The optional reason is
// positional to keep the (id, state, req) call shape.
func setObligationState(id, state string, req OperationRequest, reason ...string) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	switch state {
	case ObligationRequired, ObligationRunning, ObligationSatisfied, ObligationBlocked, ObligationExcepted:
	default:
		if state == ObligationStale {
			return fmt.Errorf("obligation %s: stale is entered only via supersede with a new target revision", id)
		}
		return fmt.Errorf("unknown obligation state %q", state)
	}
	if state == ObligationBlocked || state == ObligationExcepted {
		if len(reason) == 0 || strings.TrimSpace(reason[0]) == "" {
			return fmt.Errorf("obligation %s: %s requires a reason", id, state)
		}
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureObligationTable(db); err != nil {
		return err
	}
	var cur string
	if err := db.QueryRow(`SELECT status FROM coverage_obligations WHERE id = ?`, id).Scan(&cur); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no such obligation: %s", id)
		}
		return err
	}
	// Independence (TD-095 applied to coverage): the worker agent
	// that produced the attempt under review cannot satisfy its own
	// review duty. The satisfier in req.Actor must differ from the
	// task's worker; operators waive via excepted, never by
	// self-satisfying.
	if state == ObligationSatisfied {
		ob, oerr := getObligation(id)
		if oerr != nil {
			return oerr
		}
		t, terr := getTask(ob.TaskID)
		if terr != nil {
			return fmt.Errorf("obligation %s: cannot prove independence without task %s: %v", id, ob.TaskID, terr)
		}
		if req.Actor == t.Agent {
			return fmt.Errorf("obligation %s: worker %q cannot satisfy its own %s duty; an independent reviewer must (TD-095)", id, req.Actor, ob.Kind)
		}
	}
	if cur == state {
		return nil
	}
	allowed, ok := obligationTransitions[cur]
	if !ok {
		return fmt.Errorf("obligation %s is %s (terminal); use supersede for a new target revision, never reopen", id, cur)
	}
	if !allowed[state] {
		return fmt.Errorf("obligation %s: %s -> %s is not a legal transition", id, cur, state)
	}
	note := ""
	if len(reason) > 0 {
		note = reason[0]
	}
	if _, err := db.Exec(`UPDATE coverage_obligations SET status = ?, reason = ?, updated = ? WHERE id = ?`,
		state, note, nowUTC(), id); err != nil {
		return err
	}
	cur2, _ := getObligation(id)
	recordReceipt(cur2.RunID, "obligation_"+state,
		fmt.Sprintf("actor=%s req=%s obligation=%s task=%s kind=%s", req.Actor, req.RequestID, id, cur2.TaskID, cur2.Kind))
	return nil
}

// supersedeObligation parks a non-terminal obligation as stale and
// opens its replacement at newTargetRev. This is the ONLY path to
// stale: a revision change is explicit new work, never a silent
// reopen of the satisfied row.
func supersedeObligation(id, newTargetRev string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(newTargetRev) == "" {
		return "", fmt.Errorf("supersede requires a new target revision")
	}
	cur, err := getObligation(id)
	if err != nil {
		return "", err
	}
	if cur.Status == ObligationSatisfied || cur.Status == ObligationStale || cur.Status == ObligationExcepted {
		return "", fmt.Errorf("obligation %s is %s (terminal); supersede applies only to outstanding work", id, cur.Status)
	}
	if newTargetRev == cur.TargetRev {
		return "", fmt.Errorf("obligation %s already reviews %q; supersede requires a new target revision", id, newTargetRev)
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureObligationTable(db); err != nil {
		return "", err
	}
	res, err := db.Exec(`UPDATE coverage_obligations SET status = ?, updated = ?
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
	next := newObligationID()
	now := nowUTC()
	if _, err := db.Exec(`INSERT INTO coverage_obligations
		(id, run_id, task_id, kind, target_rev, evidence_refs, status, created, updated, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		next, cur.RunID, cur.TaskID, cur.Kind, newTargetRev, cur.Evidence, ObligationRequired, now, now, req.RequestID); err != nil {
		return "", err
	}
	recordReceipt(cur.RunID, "obligation_superseded",
		fmt.Sprintf("actor=%s req=%s obligation=%s superseded-by=%s rev=%s", req.Actor, req.RequestID, id, next, newTargetRev))
	return next, nil
}

// listObligations reads a run's obligations; a non-empty task
// narrows the projection to one task.
func listObligations(run, task string) ([]coverageObligation, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureObligationTable(db); err != nil {
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
	rows, err := db.Query(q, args...)
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

// reportDigestFor returns the filed digest of a task's report at an
// attempt, or "" when nothing was filed: there is no revision to
// review and generation has nothing to bind.
func reportDigestFor(taskID string, attempt int) (string, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var digest string
	err = db.QueryRow(`SELECT digest FROM task_reports WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&digest)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return digest, err
}

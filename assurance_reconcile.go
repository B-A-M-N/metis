package main

// assurance_reconcile.go: attempt reconciliation.
//
// Inspects the latest attempt of a dispatched task before any
// replacement is claimed. An attempt whose worker may still be
// acting is reconciled by a human first; anything else is marked
// superseded and a fresh attempt number is claimed for the
// replacement. History is preserved, never rewritten (TD-015,
// TD-118).
//
// The swap is atomic (TD-027/TD-118): supersede, bump, and row
// creation land in ONE transaction, and the bump is predicated on
// the attempt this call reconciled. A concurrent retry passes the
// same pre-gates but its bump then matches zero rows and rolls
// back instead of double-claiming.

import (
	"database/sql"
	"fmt"
)

// checkRetryBudget enforces the explicit retry policy (TD-035): a
// task that has already consumed its dispatch-attempt budget is
// refused a replacement. History is preserved untouched; the
// orchestrator must decide deliberately.
func checkRetryBudget(t Task) error {
	if t.Attempt >= maxDispatchAttempts {
		return fmt.Errorf("retry budget exhausted: task %s already has %d attempts (max %d); start a new run or override explicitly (TD-035)", t.ID, t.Attempt, maxDispatchAttempts)
	}
	return nil
}

// reconcileForRetry inspects the latest attempt of a dispatched task.
// An attempt whose worker may still be acting must be reconciled by a
// human first; anything else is marked superseded and a fresh attempt
// number is claimed for the replacement. History is preserved, never
// rewritten (TD-015, TD-118).
func reconcileForRetry(db *sql.DB, id string, req OperationRequest) (retryNext TaskAttempt, opErr error) {
	defer func() { observeOps(opsPhaseReconciliation, req.RequestID, id, 0, "", opErr) }()
	t, err := getTask(id)
	if err != nil {
		return TaskAttempt{}, err
	}
	if err := checkRetryBudget(t); err != nil {
		return TaskAttempt{}, err
	}
	prev, err := latestAttempt(db, id)
	if err != nil {
		return TaskAttempt{}, err
	}
	dlv, err := latestDelivery(db, prev.ID)
	if err != nil {
		return TaskAttempt{}, err
	}
	// A possibly-live attempt refuses replacement first: it does not
	// matter whether the task row has moved on, the worker may still
	// be acting (TD-118).
	if liveDelivery(dlv.State) {
		// P3 metrics hook (additive only): the prior attempt stays
		// uncertain, so its age keeps accruing. Retry behavior is
		// untouched.
		observeRetryRefused(id, prev.AttemptNo)
		return TaskAttempt{}, fmt.Errorf("attempt %s may still be active (delivery %s); reconcile it before replacing (TD-118)", prev.ID, dlv.State)
	}
	if t.Status != TaskDispatched {
		return TaskAttempt{}, fmt.Errorf("nothing uncertain to reconcile: task %s is %s", id, t.Status)
	}
	// Atomic replacement (TD-027/TD-118): supersede, bump, and row
	// creation land in ONE transaction, and the bump is predicated on
	// the attempt this call reconciled. A concurrent retry passes the
	// same pre-gates but its bump then matches zero rows and rolls
	// back instead of double-claiming. The tx holds only writes, so
	// no statement reads a stale snapshot.
	tx, err := db.Begin()
	if err != nil {
		return TaskAttempt{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE task_attempts SET state = ?, updated = ? WHERE id = ?`,
		AttemptSuperseded, nowUTC(), prev.ID); err != nil {
		return TaskAttempt{}, err
	}
	res, err := tx.Exec(`UPDATE tasks SET status = ?, updated = ?, attempt = attempt + 1
		WHERE id = ? AND status = ? AND attempt = ?`,
		TaskDispatched, nowUTC(), id, TaskDispatched, prev.AttemptNo)
	if err != nil {
		return TaskAttempt{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return TaskAttempt{}, err
		}
		return TaskAttempt{}, fmt.Errorf("task %s already reconciled by a concurrent retry; re-read the latest attempt before replacing", id)
	}
	nextNo := prev.AttemptNo + 1
	nextID, err := insertAttempt(tx, id, nextNo, AttemptClaimed, req)
	if err != nil {
		return TaskAttempt{}, err
	}
	var next TaskAttempt
	if err := tx.QueryRow(`SELECT id, task_id, attempt_no, state, request_id, actor, created, updated
		FROM task_attempts WHERE id = ?`, nextID).Scan(
		&next.ID, &next.TaskID, &next.AttemptNo, &next.State, &next.RequestID, &next.Actor, &next.Created, &next.Updated); err != nil {
		return TaskAttempt{}, err
	}
	if err := tx.Commit(); err != nil {
		return TaskAttempt{}, err
	}
	recordReceipt(id, "retry-supersedes", fmt.Sprintf("actor=%s req=%s superseded=%s", req.Actor, req.RequestID, prev.ID))
	// P3 metrics hook (additive only): supersede-and-bump resolves
	// the prior attempt's identity via replacement. Retry behavior
	// is untouched.
	observeRetryReconciled(id, prev.AttemptNo)
	return next, nil
}

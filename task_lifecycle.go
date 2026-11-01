package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// task_lifecycle.go: task and attempt state transitions.
//
// Conditional status guards, attempt/delivery row writers, and
// execution-receipt recording.

func terminalTaskStatus(s string) bool {
	return s == TaskVerified || s == TaskFailed || s == TaskCancelled
}

// transitionTask moves a task to a new status with a single conditional
// UPDATE, so concurrent dispatchers cannot both win: exactly one row must
// change. from lists the statuses the transition may leave; nil means any
// non-terminal status. extraSet appends extra column assignments to the same
// atomic statement. A zero-row result is diagnosed with a fresh read so
// losers get a meaningful conflict instead of an opaque database error.
func transitionTask(id string, from []string, to string, extraSet string, extraArgs ...any) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	q := `UPDATE tasks SET status = ?, updated = ?`
	args := []any{to, nowUTC()}
	if extraSet != "" {
		q += ", " + extraSet
		args = append(args, extraArgs...)
	}
	q += ` WHERE id = ?`
	args = append(args, id)
	if len(from) > 0 {
		q += ` AND status IN (` + strings.Repeat("?,", len(from)-1) + `?)`
		for _, s := range from {
			args = append(args, s)
		}
	} else {
		q += ` AND status NOT IN ('verified','failed','cancelled','cancelling','termination_unknown')`
	}
	res, err := db.Exec(q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		// P0-A: execution ownership is conclusively settled only at
		// a terminal state, so the workspace reservation releases
		// here — never on uncertain outcomes. Best-effort: the
		// transition already committed, so a release failure is
		// receipted, not returned as a false transition error.
		if terminalTaskStatus(to) {
			if rerr := releaseWorkspace(id); rerr != nil {
				recordReceipt(id, "reservation_release_failed", rerr.Error())
			}
		}
		return nil
	}
	cur, gerr := getTask(id)
	if gerr != nil {
		return fmt.Errorf("no such task: %s", id)
	}
	if terminalTaskStatus(cur.Status) {
		return fmt.Errorf("task %s is terminal (%s); use followup for more work", id, cur.Status)
	}
	if len(from) > 0 {
		return fmt.Errorf("task %s is %s, expected %s", id, cur.Status, strings.Join(from, " or "))
	}
	return fmt.Errorf("task %s is %s, cannot move to %s", id, cur.Status, to)
}

// markTaskWorking records that the worker session picked the task up.
// The latest unconfirmed delivery advances to working alongside the task
// so transport, agent, and working states stay distinguishable.
// markTaskWorking records an attributed pickup claim for one exact
// attempt. The claim is bounded (task + attempt + requester) and
// single-shot: a stale attempt is refused, a duplicate claim on a
// live task is refused, and the ledger transition plus the delivery
// and attempt advances happen in one transaction. The receipt stays
// best-effort, consistent with the rest of the trail.
func markTaskWorking(id string, attempt int, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	t, err := getTask(id)
	if err != nil {
		return err
	}
	if t.Attempt != attempt {
		return fmt.Errorf("stale pickup for task %s: task is at attempt %d, claim names %d", id, t.Attempt, attempt)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return err
	}
	if err := ensureWorkspaceReservations(db); err != nil {
		return err
	}
	// Ensure BEFORE Begin: this handle allows one connection, so
	// DDL after Begin would wait on the tx's own connection
	// forever. The scope gate below runs inside the tx.
	if err := ensureWorkspaceAllocTable(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE tasks SET status = ?, updated = ? WHERE id = ? AND status = 'dispatched' AND attempt = ?`,
		TaskWorking, nowUTC(), id, attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var cur string
		if qerr := tx.QueryRow(`SELECT status FROM tasks WHERE id = ?`, id).Scan(&cur); qerr != nil {
			return fmt.Errorf("no such task: %s", id)
		} else if cur == TaskWorking {
			return fmt.Errorf("task %s attempt %d already picked up; duplicate claims refused", id, attempt)
		} else if terminalTaskStatus(cur) {
			return fmt.Errorf("task %s is terminal (%s); use followup for more work", id, cur)
		}
		return fmt.Errorf("task %s is %s; only a dispatched task can be picked up", id, cur)
	}
	// P0-A: pickup re-verifies workspace ownership inside the same
	// transaction. A task committed without its root reservation —
	// lost yield window, tampered row, or pre-reservation legacy —
	// cannot go working. Shared workspaces are explicitly
	// authorized to share and skip the fence.
	if t.SharedWorkspace == 0 {
		var holder string
		if qerr := tx.QueryRow(`SELECT task_id FROM workspace_reservations WHERE root = ?`, t.ExecutionRoot).Scan(&holder); qerr != nil {
			return fmt.Errorf("task %s has no workspace reservation for %s; refusing pickup", id, t.ExecutionRoot)
		} else if holder != id {
			return fmt.Errorf("workspace %s reservation held by %s, not %s; refusing pickup", t.ExecutionRoot, holder, id)
		}
	}
	// Worktree scope (P0): when this attempt carries a worktree
	// binding, pickup claims allocated -> in_use and requires the
	// task's execution root to BE the bound checkout. A foreign
	// root is cross-worktree execution and is refused inside the
	// same transaction, so the task can never go working elsewhere.
	if err := claimWorktreeUse(tx, id, attempt, t.ExecutionRoot, req.Actor, req.RequestID); err != nil {
		return err
	}
	for _, adv := range []struct {
		table string
		from  []string
		to    string
	}{
		{"task_deliveries", []string{DeliverySubmitted, DeliveryTransportAcked, DeliveryAgentAcked}, DeliveryWorking},
		{"task_attempts", []string{AttemptClaimed, AttemptSubmitted}, AttemptWorking},
	} {
		q := `UPDATE ` + adv.table + ` SET state = ?, updated = ? WHERE task_id = ? AND state IN (` +
			strings.Repeat("?,", len(adv.from)-1) + `?)`
		args := []any{adv.to, nowUTC(), id}
		for _, s := range adv.from {
			args = append(args, s)
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	recordReceipt(id, "working", fmt.Sprintf("actor=%s req=%s attempt=%d picked up task", req.Actor, req.RequestID, attempt))
	return nil
}

func insertAttempt(db sqlExecer, taskID string, attemptNo int, state string, req OperationRequest) (string, error) {
	id := newAttemptID()
	now := nowUTC()
	_, err := db.Exec(`INSERT INTO task_attempts (id, task_id, attempt_no, state, request_id, actor, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, attemptNo, state, req.RequestID, req.Actor, now, now)
	return id, err
}

// insertDelivery records one observable delivery step for an attempt.
func insertDelivery(db sqlExecer, taskID, attemptID, state string, req OperationRequest, detail string) (string, error) {
	id := newDeliveryID()
	now := nowUTC()
	_, err := db.Exec(`INSERT INTO task_deliveries (id, attempt_id, task_id, state, request_id, actor, detail, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, attemptID, taskID, state, req.RequestID, req.Actor, detail, now, now)
	return id, err
}

func setAttemptState(db *sql.DB, attemptID, state string) error {
	_, err := db.Exec(`UPDATE task_attempts SET state = ?, updated = ? WHERE id = ?`, state, nowUTC(), attemptID)
	return err
}

func setDeliveryState(db *sql.DB, deliveryID, state, detail string) error {
	_, err := db.Exec(`UPDATE task_deliveries SET state = ?, detail = ?, updated = ? WHERE id = ?`,
		state, detail, nowUTC(), deliveryID)
	return err
}

// liveDelivery reports whether a delivery state means the worker may
// still be acting on the attempt. Only settled-nonlive states may be
// replaced without human reconciliation.
func liveDelivery(state string) bool {
	switch state {
	case DeliveryWorking, DeliveryAgentAcked, DeliveryTransportAcked:
		return true
	}
	return false
}

// recordExecutionStart binds one provider launch to one attempt. The
// UNIQUE(task_id, attempt_no) row makes exactly-once enforceable: a
// second launch for the same attempt is refused, never recorded.
func recordExecutionStart(taskID string, attempt int, session, harness, argvJSON string, pid int, reporter string) error {
	start, _ := procStartTime(pid)
	return recordExecutionStartWithProc(taskID, attempt, session, harness, argvJSON, pid, start, reporter)
}

// recordExecutionStartWithProc binds one provider launch with an
// explicit process-start identity. Callers that did not observe the
// launch pass 0 and accept that cancellation will refuse to signal.
func recordExecutionStartWithProc(taskID string, attempt int, session, harness, argvJSON string, pid int, procStart uint64, reporter string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO execution_receipts
		(task_id, attempt_no, session, harness, argv, pid, proc_start, reporter, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, attempt, session, harness, argvJSON, pid, procStart, reporter, nowUTC(), nowUTC())
	if err != nil {
		if strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("task %s attempt %d already executed; duplicate launches refused", taskID, attempt)
		}
		return err
	}
	return nil
}

// recordExecutionResult files the native process outcome for one
// attempt. It only fills a still-open receipt; a second result for
// the same attempt is refused. Exit code zero is recorded, never
// interpreted — it is not proof the work succeeded.
func recordExecutionResult(taskID string, attempt, exitCode int, outputDigest string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE execution_receipts SET exit_code = ?, output_digest = ?, updated = ?
		WHERE task_id = ? AND attempt_no = ? AND exit_code IS NULL`,
		exitCode, outputDigest, nowUTC(), taskID, attempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("no open execution receipt for task %s attempt %d", taskID, attempt)
	}
	return nil
}

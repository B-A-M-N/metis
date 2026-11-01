package main

// dispatch_commit.go: the attempt-claim transaction (P0-A step 2).
//
// AttemptCommitter is the exclusive authority for claiming an
// attempt: the conditional claim, the attempt row, the delivery
// rows, the workspace reservation, and the authoritative delegation
// event land in ONE transaction where required. No waiter can
// observe dispatched without its attempt+delivery bindings, and a
// lost dispatch race rolls back cleanly instead of leaving a torn
// half-dispatch behind.
//
// The committer also revalidates every mutable-resource decision
// from admission inside the transaction (status, attempt sequence,
// execution-root binding, dependencies) — there is no TOCTOU
// between the admission check and the claim. Static inputs
// (eligibility, run/team ceilings) stay enforced by claim
// predicates evaluated under the write lock. Identical outcomes in
// the sequential path; racers converge on one winner.

import (
	"database/sql"
	"fmt"
	"strings"
)

// AttemptCommitter commits one admitted, prepared dispatch. The
// transaction is the one winner-maker: a lost race affects zero
// rows and performs no delivery bookkeeping.
type AttemptCommitter struct{}

// Commit claims the pending task for the admitted attempt with the
// prepared delivery binding. On any failure the transaction rolls
// back and the caller must reconcile the prepared files, which have
// no ledger binding.
func (AttemptCommitter) Commit(adm DispatchAdmission, prep PreparedDelivery, req OperationRequest, detail string) (opErr error) {
	defer func() {
		observeOps(opsPhaseDispatch, req.RequestID, adm.Plan.TaskID, adm.Plan.Attempt, adm.Plan.SessionID, opErr)
	}()
	id := adm.Plan.TaskID
	session := adm.Plan.SessionID
	pending := adm.Task
	nextAttempt := adm.Plan.Attempt
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureWorkspaceReservations(db); err != nil {
		return err
	}
	harness := adm.Harness
	// Commit boundary: the conditional claim, the attempt row, and
	// delivery rows land in ONE transaction. No waiter can
	// observe dispatched without its attempt+delivery bindings, and
	// a lost dispatch race rolls back cleanly instead of leaving a
	// torn half-dispatch behind.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Revalidate mutable-resource decisions inside the transaction:
	// anything another dispatcher may have changed between the
	// read-only admission snapshot and this claim is re-read here,
	// under the same connection that will claim. The sequential
	// path observes identical state and proceeds; only interleaved
	// writers are refused.
	if rerr := revalidateAdmissionTx(tx, adm); rerr != nil {
		return rerr
	}
	claimSet := `status = ?, updated = ?, agent = ?, attempt = ?`
	claimArgs := []any{TaskDispatched, nowUTC(), session, nextAttempt}
	if pending.ExecutionRoot == "" {
		claimSet += `, execution_root = ?`
		claimArgs = append(claimArgs, adm.Plan.ExecutionRoot)
	}
	claimWhere := `WHERE id = ? AND status = ?`
	claimArgs = append(claimArgs, id, TaskPending)
	if adm.RunLimitSet {
		// Atomic admission (TD-032): the pre-claim live count is a
		// fail-fast courtesy only. The single UPDATE below evaluates
		// the bound under the write lock against latest committed
		// state, so two racers that both passed the pre-check cannot
		// both claim: the loser matches zero rows and is refused.
		claimWhere += ` AND (SELECT COUNT(*) FROM tasks WHERE run = ? AND status IN ('dispatched','working')) < ?`
		claimArgs = append(claimArgs, pending.Run, adm.RunLimit)
	}
	if adm.Team != nil {
		// Atomic Team admission (P0-B): same shape as the run
		// bound, but project-wide — live tasks across every run
		// pinned to this revision count against the ceiling, so
		// racers that both passed the fail-fast precheck converge
		// on one winner at commit time.
		claimWhere += ` AND (? > (SELECT COUNT(*) FROM tasks t JOIN run_teams r ON r.run_id = t.run WHERE r.project = ? AND r.revision = ? AND t.status IN ('dispatched','working')))`
		claimArgs = append(claimArgs, adm.Team.Limits.MaxActiveWorkers, adm.Team.Project, adm.Team.Revision)
	}
	res, cerr := tx.Exec(`UPDATE tasks SET `+claimSet+` `+claimWhere, claimArgs...)
	if cerr != nil {
		return cerr
	}
	if n, cerr := res.RowsAffected(); cerr != nil || n != 1 {
		if cerr != nil {
			return cerr
		}
		cur, gerr := getTask(id)
		if gerr != nil {
			return fmt.Errorf("dispatch claim lost for task %s: %v", id, gerr)
		}
		if terminalTaskStatus(cur.Status) {
			return fmt.Errorf("task %s is terminal (%s); use followup for more work", id, cur.Status)
		}
		if adm.RunLimitSet {
			// A concurrent dispatcher may have filled the last
			// slot between the pre-check and the claim; report
			// the bound instead of a misleading state conflict.
			if live, lerr := liveTaskCount(pending.Run); lerr == nil && live >= adm.RunLimit {
				return fmt.Errorf("run %q at concurrency limit %d (%d live); resolve a task before dispatching %s (TD-032)", pending.Run, adm.RunLimit, live, id)
			}
		}
		if adm.Team != nil {
			// Same courtesy for the Team budget: a concurrent
			// dispatcher may have taken the last project-wide
			// slot between the precheck and the claim.
			db2, derr := openTasksDB()
			if derr == nil {
				active, aerr := teamActiveCount(db2, adm.Team.Project, adm.Team.Revision)
				db2.Close()
				if aerr == nil && active >= adm.Team.Limits.MaxActiveWorkers {
					return fmt.Errorf("team policy (rev %d): %d active workers at limit %d",
						adm.Team.Revision, active, adm.Team.Limits.MaxActiveWorkers)
				}
			}
		}
		return fmt.Errorf("task %s is %s, expected pending (concurrent dispatch won)", id, cur.Status)
	}
	attemptID, err := insertAttempt(tx, id, nextAttempt, AttemptClaimed, req)
	if err != nil {
		return err
	}
	if _, err := insertDelivery(tx, id, attemptID, DeliveryQueued, req, detail); err != nil {
		return err
	}
	if _, err := insertDelivery(tx, id, attemptID, DeliverySubmitted, req, detail); err != nil {
		return err
	}
	// P0-A: durable workspace ownership commits with the claim. The
	// pre-claim occupancy check is advisory; this INSERT is the
	// fence — a conflicting dispatch never commits at all, so no
	// post-commit yield can race a worker pickup. Shared workspaces
	// are explicitly authorized to share and skip the fence.
	if pending.SharedWorkspace == 0 {
		if err := reserveWorkspaceTx(tx, prep.ExecutionRoot, id, nextAttempt); err != nil {
			return err
		}
	}
	// The delegation event commits with the claim: the feed can never
	// show dispatched without its attempt/delivery bindings, nor the
	// bindings without the feed row. Payload carries the exact
	// instruction text (bounded by truncation).
	if _, err := insertOrchEventTx(tx, OrchEvent{
		RunID: pending.Run, OriginPrincipal: req.Actor, OriginHarness: harness,
		TargetAgent: session, TaskID: id, Attempt: nextAttempt,
		EventType: OrchDelegationDispatched, RequestID: req.RequestID,
		Payload: pending.Objective, Disposition: "committed",
	}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// revalidateAdmissionTx re-reads every mutable-resource decision the
// admission snapshot depends on, inside the claiming transaction. A
// concurrent dispatcher, authorizer, or dependency edit that landed
// between AdmitDispatch and Commit is visible here and refuses the
// claim before it becomes observable. The sequential path — state
// unchanged since admission — passes through untouched.
func revalidateAdmissionTx(tx *sql.Tx, adm DispatchAdmission) error {
	id := adm.Plan.TaskID
	var curStatus string
	var curAttempt int
	var curRoot string
	var curShared int
	if err := tx.QueryRow(`SELECT status, attempt, execution_root, shared_workspace FROM tasks WHERE id = ?`, id).
		Scan(&curStatus, &curAttempt, &curRoot, &curShared); err != nil {
		return err
	}
	if curStatus != TaskPending {
		return fmt.Errorf("task %s is %s, expected pending (concurrent dispatch won)", id, curStatus)
	}
	if curAttempt != adm.Plan.Attempt-1 {
		return fmt.Errorf("task %s moved to attempt %d after admission; refusing stale claim for attempt %d", id, curAttempt, adm.Plan.Attempt)
	}
	if curRoot != "" && curRoot != adm.Plan.ExecutionRoot {
		return fmt.Errorf("task %s authorized for %s after admission; session %s executes in %s (TD-050)", id, curRoot, adm.Plan.SessionID, adm.Plan.ExecutionRoot)
	}
	// Dependencies are mutable plan state: an edge added after the
	// pre-claim check must still gate the claim (TD-030).
	rows, err := tx.Query(`SELECT depends_on FROM task_deps WHERE task_id = ?`, id)
	if err != nil {
		return err
	}
	var deps []string
	for rows.Next() {
		var dep string
		if serr := rows.Scan(&dep); serr != nil {
			rows.Close()
			return serr
		}
		deps = append(deps, dep)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var blocking []string
	for _, depID := range deps {
		var st string
		if qerr := tx.QueryRow(`SELECT status FROM tasks WHERE id = ?`, depID).Scan(&st); qerr != nil {
			return qerr
		}
		if st != TaskVerified {
			blocking = append(blocking, fmt.Sprintf("%s (%s)", depID, st))
		}
	}
	if len(blocking) > 0 {
		return fmt.Errorf("task %s cannot start: unsatisfied dependencies: %s", id, strings.Join(blocking, ", "))
	}
	return nil
}

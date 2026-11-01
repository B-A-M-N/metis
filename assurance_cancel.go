package main

// assurance_cancel.go: cancellation coordination + descendant accounting.
//
// Cancelling a task cancels every active descendant linked by
// correction lineage. Cancellation is attributable and never claims
// more than it performed: already-terminal tasks are left alone and
// named, and only non-terminal tasks transition (TD-037). Unrelated
// run-mates are untouched (TD-040). This disposes orchestration
// state; it never asserts worker processes are dead — confirmed
// process termination is tracked separately (TD-038 residual), and a
// sent SIGTERM is requested, never confirmed.
//
// Settlement is two-stage: stage 1 records cancellation requested
// (no new work may start on the execution root), stage 2 routes the
// stop to the exact managed execution (assurance_terminate.go). An
// uncertain stop leaves the task in termination_unknown — and the
// workspace stays claimed — instead of reading as cancelled.

import (
	"fmt"
	"strings"
)

// collectCancelLineage returns the cancellation set for a root: the
// root plus every descendant linked by correction lineage (children
// have Parent == current), breadth-first. Terminal membership is
// decided later per task; this accounts the set, nothing more.
func collectCancelLineage(root string) ([]string, error) {
	ids := []string{root}
	seen := map[string]bool{root: true}
	for i := 0; i < len(ids); i++ {
		db, err := openTasksDB()
		if err != nil {
			return nil, err
		}
		rows, err := db.Query(`SELECT id FROM tasks WHERE parent = ?`, ids[i])
		if err != nil {
			db.Close()
			return nil, err
		}
		var kids []string
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				db.Close()
				return nil, err
			}
			kids = append(kids, k)
		}
		rows.Close()
		db.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, k := range kids {
			if !seen[k] {
				seen[k] = true
				ids = append(ids, k)
			}
		}
	}
	return ids, nil
}

func cancelTask(id string, req OperationRequest) (opErr error) {
	defer func() {
		attempt, session := 0, ""
		if t, gerr := getTask(id); gerr == nil {
			attempt, session = t.Attempt, t.Agent
		}
		observeOps(opsPhaseCancellation, req.RequestID, id, attempt, session, opErr)
	}()
	if err := requireRequest(req); err != nil {
		return err
	}
	root, err := getTask(id)
	if err != nil {
		return err
	}
	// Cancelling an already-terminal task is meaningless; the caller
	// wants followup, not a silent no-op.
	if terminalTaskStatus(root.Status) {
		return fmt.Errorf("task %s is terminal (%s); use followup for more work", id, root.Status)
	}
	// BFS over correction lineage: children have Parent == current.
	ids, err := collectCancelLineage(id)
	if err != nil {
		return err
	}
	var skipped []string
	cancelled := map[string]bool{}
	for _, tid := range ids {
		t, err := getTask(tid)
		if err != nil {
			return err
		}
		if terminalTaskStatus(t.Status) {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", tid, t.Status))
			continue
		}
		detail := fmt.Sprintf("actor=%s req=%s root=%s", req.Actor, req.RequestID, id)
		if tid != id {
			detail += fmt.Sprintf(" cascade_from=%s", id)
		}
		// Pending tasks never had a worker: nothing to stop,
		// so cancellation is immediate and complete.
		if t.Status == TaskPending {
			if err := transitionTask(tid, []string{TaskPending}, TaskCancelled, ""); err != nil {
				if cur, gerr := getTask(tid); gerr == nil && terminalTaskStatus(cur.Status) {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", tid, cur.Status))
					continue
				}
				return err
			}
			cancelled[tid] = true
			recordReceipt(tid, "cancelled", detail)
			continue
		}
		// Stage 1: record cancellation requested. From here on
		// no new work may start on this execution root; the
		// workspace stays occupied until the stop is proven.
		if err := transitionTask(tid, []string{TaskDispatched, TaskWorking, TaskReportReady}, TaskCancelling, ""); err != nil {
			// Lost a race with another transition: diagnose
			// the fresh state instead of assuming.
			if cur, gerr := getTask(tid); gerr == nil && terminalTaskStatus(cur.Status) {
				skipped = append(skipped, fmt.Sprintf("%s (%s)", tid, cur.Status))
				continue
			}
			return err
		}
		recordReceipt(tid, "cancel_requested", detail)
		// The task is under cancellation by this operation: exclude
		// it from sibling live-binding checks before settling its
		// worker, exactly as the old immediate path did.
		cancelled[tid] = true
		session := t.Agent
		if session != "" {
			settleBoundWorker(tid, session, cancelled, req)
		}
		// Stage 2: route cancellation to the exact managed
		// process bound to this attempt (WP1c). Session
		// settlement alone never counts as confirmed — the
		// disposition says what the process actually did.
		disp, derr := stopManagedExecution(tid, t.Attempt, req)
		if derr != nil {
			recordReceipt(tid, "cancel_execution_error", fmt.Sprintf("actor=%s req=%s attempt=%d %v", req.Actor, req.RequestID, t.Attempt, derr))
			disp = ExecutionCancelUncertain
		} else {
			recordReceipt(tid, "cancel_execution_"+disp, fmt.Sprintf("actor=%s req=%s attempt=%d", req.Actor, req.RequestID, t.Attempt))
		}
		switch disp {
		case ExecutionCancelConfirmed, ExecutionCancelNoBinding:
			// Confirmed dead, or nothing was ever bound to
			// stop: cancellation is complete.
			if err := transitionTask(tid, []string{TaskCancelling}, TaskCancelled, ""); err != nil {
				return err
			}
			recordReceipt(tid, "cancelled", detail)
		default:
			// The process may still be alive (a recycled PID,
			// a failed signal, a native turn with no OS
			// binding). The task must not read as cancelled
			// and must keep blocking its workspace.
			if err := transitionTask(tid, []string{TaskCancelling}, TaskTerminationUnknown, ""); err != nil {
				return err
			}
			recordReceipt(tid, "cancel_termination_unknown", detail+fmt.Sprintf(" attempt=%d disp=%s", t.Attempt, disp))
			if _, aerr := raiseAttention(t.Run, tid, t.Attempt, session, "uncertain_outcome",
				"execution stop unconfirmed; termination unknown",
				fmt.Sprintf("attempt=%d disp=%s", t.Attempt, disp)); aerr != nil {
				recordReceipt(tid, "attention_error", detail+fmt.Sprintf(" attempt=%d %v", t.Attempt, aerr))
			}
		}
	}
	if len(skipped) > 0 {
		recordReceipt(id, "cancel_skipped_terminal", fmt.Sprintf("actor=%s req=%s skipped=%s", req.Actor, req.RequestID, strings.Join(skipped, ", ")))
	}
	return nil
}

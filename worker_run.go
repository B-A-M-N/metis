package main

// worker_run.go: the managed worker loop (remediation Phase A).
//
// The loop is strictly mechanical: it finds one task the ledger
// committed to its own session, claims that exact attempt once,
// launches the provider as a child process, files the observed
// result. It never schedules, never judges, never switches
// providers. The external orchestrator decides; the loop executes.
//
// A session persists while individual one-shot invocations come and
// go. That does not make the underlying model conversation
// persistent, and adapter capabilities continue to say so.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Bounds for one managed execution.
const (
	// workerOutputCap caps the stored provider output per attempt.
	workerOutputCap = 1 << 20
	// workerReportTail is how much trailing output a filed report
	// carries inline; the full log stays attached as an artifact.
	workerReportTail = 4096
)

// dispatchedTasksForSession lists tasks the ledger committed to one
// session, oldest first. The loop never consults task files for
// authorization — only this committed state activates it.
func dispatchedTasksForSession(session string) ([]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return dispatchedTasksForSessionDB(db, session)
}

// dispatchedTasksForSessionDB is the handle-scoped query behind
// dispatchedTasksForSession. Long-lived waiters must reuse one
// handle (see waitForCommittedWork); one-shot callers use the
// wrapper above.
func dispatchedTasksForSessionDB(db *sql.DB, session string) ([]string, error) {
	rows, err := db.Query(`SELECT id FROM tasks WHERE agent = ? AND status = 'dispatched' ORDER BY rowid`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// criteriaFromAcceptance bridges the ledger's free-text acceptance
// to the envelope's criterion list: one non-empty line, one
// criterion, stable C<n> IDs in file order. Reviewers cite these
// IDs; an empty acceptance yields no criteria rather than a
// fabricated one.
func criteriaFromAcceptance(acceptance string) []Criterion {
	var out []Criterion
	for i, line := range strings.Split(acceptance, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, Criterion{
			ID:          fmt.Sprintf("C%d", len(out)+1),
			Requirement: strings.TrimSpace(line),
		})
		_ = i
	}
	return out
}

// attemptSidecarPath is the machine-readable attempt binding next
// to the human-readable task file.
func attemptSidecarPath(workdir, taskID string) string {
	return filepath.Join(workdir, "task-"+taskID+".attempt.json")
}

// writeAttemptSidecar records the claimed attempt beside the task
// instruction after a committed dispatch, so file readers see the
// same binding the ledger holds.
func writeAttemptSidecar(workdir string, t Task) error {
	raw, err := json.Marshal(map[string]any{
		"task_id": t.ID,
		"attempt": t.Attempt,
		"run_id":  t.Run,
	})
	if err != nil {
		return err
	}
	return os.WriteFile(attemptSidecarPath(workdir, t.ID), raw, 0o600)
}

// checkAttemptSidecar refuses a file/ledger divergence: when a
// sidecar exists it must name this task and its current attempt.
// A missing sidecar (tasks dispatched before sidecars existed)
// defers to the ledger, which stays authoritative.
func checkAttemptSidecar(workdir string, t Task) error {
	raw, err := os.ReadFile(attemptSidecarPath(workdir, t.ID))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("attempt file unreadable for task %s: %w", t.ID, err)
	}
	var side map[string]any
	if err := json.Unmarshal(raw, &side); err != nil {
		return fmt.Errorf("attempt file for task %s is corrupt; refusing (ledger authoritative)", t.ID)
	}
	id, _ := side["task_id"].(string)
	att, _ := side["attempt"].(float64)
	if id != t.ID || int(att) != t.Attempt {
		return fmt.Errorf("attempt file for task %s names %s attempt %v; ledger holds attempt %d: divergence refused",
			t.ID, id, side["attempt"], t.Attempt)
	}
	return nil
}

// workerStreamCap bounds what one attempt appends to the bound
// session's live stream. The TUI reads that file first, so the
// mirror keeps real execution visible without letting one burst
// evict the session's history (rotation still applies on top).
func reconcileLoopRestart(session string, req OperationRequest) (int, error) {
	if err := requireRequest(req); err != nil {
		return 0, err
	}
	reg := loadRegistry()
	if _, ok := reg[session]; !ok {
		return 0, fmt.Errorf("no such session: %s", session)
	}
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, attempt FROM tasks WHERE agent = ? AND status = 'working'`, session)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type open struct {
		id      string
		attempt int
	}
	var working []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.attempt); err != nil {
			return 0, err
		}
		working = append(working, o)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	named := 0
	for _, w := range working {
		var exitCode any
		err := db.QueryRow(`SELECT exit_code FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`,
			w.id, w.attempt).Scan(&exitCode)
		switch {
		case err == sql.ErrNoRows:
			recordReceipt(w.id, "loop_restart_observed", fmt.Sprintf("actor=%s req=%s attempt=%d claimed but never launched: outcome UNKNOWN, reconcile before replacing",
				req.Actor, req.RequestID, w.attempt))
			named++
		case err != nil:
			return named, err
		case exitCode == nil:
			recordReceipt(w.id, "loop_restart_observed", fmt.Sprintf("actor=%s req=%s attempt=%d execution open, no result: outcome UNKNOWN, reconcile before replacing",
				req.Actor, req.RequestID, w.attempt))
			named++
		default:
			// Result recorded; the report path (or its absence)
			// decides next — reconcileTask already covers it.
		}
	}
	return named, nil
}

// museTurnTimeout bounds one native turn wait. Native turns
// legitimately run for minutes; the default is generous and the
// operator can tighten it per environment. Unknown on expiry, never
// failure.
func refundClaim(id string, attempt int, reason string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	// The acknowledged-turn guard below reads the intent table, so
	// it must exist before the transaction opens: DDL on this
	// single-connection handle after Begin would wait on the tx's
	// own connection.
	if err := ensureTurnIntentTable(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Single owner for proven-not-submitted cleanup: the open
	// execution reservation, the claim, and the delivery/attempt
	// mirrors retire in one transaction fenced to the exact (task,
	// attempt), so a crash mid-cleanup commits nothing and creates
	// no second runnable owner. Only an open (result-less) receipt
	// is a reservation; a finished receipt is evidence and is never
	// deleted here. An acknowledged turn proves the server may have
	// executed, so it refuses the refund outright.
	var turnID string
	qerr := tx.QueryRow(`SELECT turn_id FROM muse_turn_intents WHERE task_id = ? AND attempt = ?`,
		id, attempt).Scan(&turnID)
	switch {
	case qerr == sql.ErrNoRows:
	case qerr != nil:
		return qerr
	case turnID != "":
		return fmt.Errorf("cannot refund task %s attempt %d: turn %s already acknowledged (possibly executed; reconcile instead)", id, attempt, turnID)
	}
	if _, err := tx.Exec(`DELETE FROM execution_receipts WHERE task_id = ? AND attempt_no = ? AND exit_code IS NULL`,
		id, attempt); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE tasks SET status = ?, updated = ? WHERE id = ? AND status = 'working' AND attempt = ?`,
		TaskDispatched, nowUTC(), id, attempt)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("cannot refund task %s attempt %d: not ours to refund", id, attempt)
	}
	// Mirrors fenced to the exact attempt: the attempt row names
	// its number directly, and deliveries join through their
	// attempt row, so other attempts' mirrors are untouched.
	if _, err := tx.Exec(`UPDATE task_attempts SET state = ?, updated = ? WHERE task_id = ? AND attempt_no = ? AND state = 'working'`,
		AttemptClaimed, nowUTC(), id, attempt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE task_deliveries SET state = ?, updated = ? WHERE task_id = ? AND state = 'working'
		AND attempt_id IN (SELECT id FROM task_attempts WHERE task_id = ? AND attempt_no = ?)`,
		DeliverySubmitted, nowUTC(), id, id, attempt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	recordReceipt(id, "claim_refunded", fmt.Sprintf("attempt=%d reason=%s", attempt, reason))
	return nil
}

// runWorkerOnce executes at most one ledger-committed task bound to
// session with adapter a. It returns true when it executed a task,
// false with nil error when idle. Every refusal names what stopped
// it and executes nothing.
var _ = sql.ErrNoRows

package main

// task_receipts.go: the durable delivery/verification acknowledgment
// trail for tasks: instruction delivered, report received, evidence
// checked, verified, operator-approved, or rejected. The receipt
// trail is what separates a worker's claim from an established
// outcome.
//
// RELIABILITY POLICY — read before depending on receipts:
//
//   - Receipts are ADVISORY best-effort. recordReceipt never returns
//     an error and never fails the operation it records: receipt
//     storage going down must not take down dispatch, reporting,
//     verification, or coverage with it.
//   - Authoritative evidence must NEVER silently depend on a
//     best-effort receipt. Task status, the report text, the
//     append-only task_reports history, obligation rows, verdicts,
//     and every admission/verification/coverage/finalize decision
//     are derived from their own tables, never from receipts. No
//     gate in this codebase reads the receipt trail to decide
//     anything; gap markers ("coverage_gap", "orch_gap") are
//     observability for the operator, not inputs to any checkpoint.
//   - recordReceiptTx is the same best-effort policy scoped to a
//     transaction: the receipt is durable exactly when the
//     transaction commits, and a receipt write never fails the
//     checkpoint — receipt errors are dropped, only ledger errors
//     propagate.

import (
	"database/sql"
)

// Receipt is a durable delivery/verification acknowledgment for a
// task: instruction delivered, report received, evidence checked,
// verified, operator-approved, or rejected. The receipt trail is what
// separates a worker's claim from an established outcome.
type Receipt struct {
	TaskID  string `json:"task_id"`
	Kind    string `json:"kind"`
	Detail  string `json:"detail"`
	Created string `json:"created"`
}

func ensureReceiptsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_receipts (
		task_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS task_receipts_task ON task_receipts(task_id)`)
	return err
}

// ensureReceiptsTableTx creates the receipt trail inside a
// transaction. DDL mirrors ensureReceiptsTable, which owns the
// canonical schema; this is its tx-scoped spelling.
func ensureReceiptsTableTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS task_receipts (
		task_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS task_receipts_task ON task_receipts(task_id)`)
	return err
}

// recordReceipt appends an acknowledgment to the task's trail. It is
// best-effort: receipt storage must never fail the operation it
// records.
func recordReceipt(taskID, kind, detail string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	if err := ensureReceiptsTable(db); err != nil {
		return
	}
	_, _ = db.Exec(`INSERT INTO task_receipts (task_id, kind, detail, created) VALUES (?, ?, ?, ?)`,
		taskID, kind, detail, nowUTC())
}

// recordReceiptTx appends an acknowledgment inside a transaction, so
// the receipt is durable exactly when the transaction commits. It
// is best-effort like recordReceipt: the returned error is for
// callers that can propagate it, but checkpoints must drop it — a
// receipt write never fails the operation it records.
func recordReceiptTx(tx *sql.Tx, taskID, kind, detail string) error {
	if err := ensureReceiptsTableTx(tx); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO task_receipts (task_id, kind, detail, created) VALUES (?, ?, ?, ?)`,
		taskID, kind, detail, nowUTC())
	return err
}

func listReceipts(taskID string) ([]Receipt, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureReceiptsTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT task_id, kind, detail, created FROM task_receipts WHERE task_id = ? ORDER BY created`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var r Receipt
		if err := rows.Scan(&r.TaskID, &r.Kind, &r.Detail, &r.Created); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

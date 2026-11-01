package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// run_finalization.go: closed-run state and completion.
//
// This file is the ONLY authority that closes a run: finalizeRun
// moves a run to closed when every task has reached a terminal
// disposition, and runClosed/runClosedTx report that state to
// admission and dependency gates. Nothing here admits work, edits
// edges, or counts capacity.

// finalizeRun closes a run only when every task in it has reached a
// terminal disposition. Anything still pending, dispatched, working,
// or awaiting a verdict blocks finalization by name (TD-041).
//
// Atomicity (P0-3): the terminal re-read, the audit receipt, and
// the closed-state commit happen in one BEGIN IMMEDIATE
// transaction. Admission takes the same write lock around its
// closed-check+insert, so no task can slip between the re-read and
// the close commit. The pre-lock listTasks read is advisory only.
func finalizeRun(run string, req OperationRequest) (RunSummary, error) {
	if err := requireRequest(req); err != nil {
		return RunSummary{}, err
	}
	if strings.TrimSpace(run) == "" {
		return RunSummary{}, fmt.Errorf("finalize requires a run")
	}
	if tasks, err := listTasks(run); err != nil {
		return RunSummary{}, err
	} else if len(tasks) == 0 {
		return RunSummary{}, fmt.Errorf("run %q has no tasks; nothing to finalize", run)
	}
	db, err := openTasksDB()
	if err != nil {
		return RunSummary{}, err
	}
	defer db.Close()
	if err := ensureRunStatesTable(db); err != nil {
		return RunSummary{}, err
	}
	if err := ensureObligationTable(db); err != nil {
		return RunSummary{}, err
	}
	// Advisory coverage read: fail fast when duties are owed. The
	// authoritative re-check under the lock below closes the race.
	if blocking, berr := obligationsBlocking(run); berr != nil {
		return RunSummary{}, berr
	} else if len(blocking) > 0 {
		return RunSummary{}, fmt.Errorf("run %q cannot finalize with unresolved coverage obligations: %s", run, obligationSummary(blocking))
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return RunSummary{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	// Authoritative gate: re-read every status under the lock.
	// Anything admitted after the advisory read above is visible
	// here and blocks the close.
	rows, err := db.Query(`SELECT id, status FROM tasks WHERE run = ?`, run)
	if err != nil {
		return RunSummary{}, err
	}
	sum := RunSummary{Run: run}
	var open []string
	for rows.Next() {
		var id, status string
		if serr := rows.Scan(&id, &status); serr != nil {
			rows.Close()
			return RunSummary{}, serr
		}
		sum.Total++
		switch status {
		case TaskVerified:
			sum.Verified++
		case TaskFailed:
			sum.Failed++
		case TaskCancelled:
			sum.Cancelled++
		default:
			open = append(open, fmt.Sprintf("%s (%s)", id, status))
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return RunSummary{}, err
	}
	if sum.Total == 0 {
		return RunSummary{}, fmt.Errorf("run %q has no tasks; nothing to finalize", run)
	}
	if len(open) > 0 {
		return RunSummary{}, fmt.Errorf("run %q cannot finalize with unresolved tasks: %s", run, strings.Join(open, ", "))
	}
	// Authoritative coverage gate under the lock: the advisory
	// read above is fail-fast only; a duty required between that
	// read and this lock still blocks the close.
	var owed int
	if qerr := db.QueryRow(`SELECT COUNT(*) FROM coverage_obligations WHERE run_id = ? AND status IN ('required','running','stale')`, run).Scan(&owed); qerr != nil {
		return RunSummary{}, qerr
	}
	if owed > 0 {
		blocking, berr := obligationsBlocking(run)
		if berr != nil {
			return RunSummary{}, berr
		}
		return RunSummary{}, fmt.Errorf("run %q cannot finalize with unresolved coverage obligations: %s", run, obligationSummary(blocking))
	}
	detail := fmt.Sprintf("actor=%s req=%s total=%d verified=%d failed=%d cancelled=%d",
		req.Actor, req.RequestID, sum.Total, sum.Verified, sum.Failed, sum.Cancelled)
	if _, err := db.Exec(`INSERT INTO run_receipts (run, kind, detail, request_id, actor, created)
		VALUES (?, 'finalized', ?, ?, ?, ?)`, run, detail, req.RequestID, req.Actor, nowUTC()); err != nil {
		return RunSummary{}, err
	}
	// Durable closed-run state commits with the receipt: admission
	// consults this row, not the append-only receipt log, so no
	// task can be added after finalization.
	if _, err := db.Exec(`INSERT INTO run_states (run, closed, updated) VALUES (?, 1, ?)
		ON CONFLICT(run) DO UPDATE SET closed = 1, updated = excluded.updated`, run, nowUTC()); err != nil {
		return RunSummary{}, err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return RunSummary{}, err
	}
	committed = true
	return sum, nil
}

// ensureRunStatesTable records one row per run with its lifecycle
// state. Closed runs refuse new admissions and dependency edits;
// the row is the authority, receipts are the audit trail.
func ensureRunStatesTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS run_states (
		run TEXT PRIMARY KEY,
		closed INTEGER NOT NULL DEFAULT 0,
		updated TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// runClosed reports whether the run was finalized. Unknown runs
// are open: finalization is explicit, never inferred.
func runClosed(run string) (bool, error) {
	db, err := openTasksDB()
	if err != nil {
		return false, err
	}
	defer db.Close()
	if err := ensureRunStatesTable(db); err != nil {
		return false, err
	}
	var closed int
	err = db.QueryRow(`SELECT closed FROM run_states WHERE run = ?`, run).Scan(&closed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return closed != 0, err
}

// runClosedTx is runClosed on an already-open handle: admission
// and dependency gates inside a transaction must not open a
// second connection mid-transaction.
func runClosedTx(db *sql.DB, run string) (bool, error) {
	var closed int
	err := db.QueryRow(`SELECT closed FROM run_states WHERE run = ?`, run).Scan(&closed)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return closed != 0, err
}

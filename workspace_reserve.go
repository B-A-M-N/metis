package main

import (
	"database/sql"
	"fmt"
	"time"
)

// workspace_reserve.go: durable workspace ownership (P0-A).
//
// The occupancy pre-check in dispatch is advisory: two dispatchers
// can both pass it, both commit, and then the loser must be yielded
// back — except a worker may pick the loser up in between, and the
// yield fails on a working task. The reservation closes that hole:
// the claim transaction inserts (root → task), so a conflicting
// dispatch never commits at all. Pickup re-verifies the holder, and
// only a terminal transition releases the root.

func ensureWorkspaceReservations(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS workspace_reservations (
		root       TEXT PRIMARY KEY,
		task_id    TEXT NOT NULL,
		attempt    INTEGER NOT NULL,
		created_ns INTEGER NOT NULL
	)`)
	return err
}

// reserveWorkspaceTx inserts the ownership row inside the caller's
// transaction. A held root refuses with the holder named; the caller
// rolls back, so the refused dispatch never becomes visible.
func reserveWorkspaceTx(tx *sql.Tx, root, taskID string, attempt int) error {
	if _, err := tx.Exec(`INSERT INTO workspace_reservations (root, task_id, attempt, created_ns)
		VALUES (?, ?, ?, ?)`, root, taskID, attempt, time.Now().UnixNano()); err != nil {
		var holder string
		if qerr := tx.QueryRow(`SELECT task_id FROM workspace_reservations WHERE root = ?`, root).Scan(&holder); qerr != nil {
			holder = "another task"
		}
		return fmt.Errorf("workspace %s reserved by %s; refusing %s (TD-051)", root, holder, taskID)
	}
	return nil
}

// reserveWorkspace is the standalone variant for tests and
// single-statement callers. One statement: atomic by itself.
func reserveWorkspace(root, taskID string, attempt int) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureWorkspaceReservations(db); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO workspace_reservations (root, task_id, attempt, created_ns)
		VALUES (?, ?, ?, ?)`, root, taskID, attempt, time.Now().UnixNano()); err != nil {
		holder := workspaceReservationHolder(root)
		if holder == "" {
			holder = "another task"
		}
		return fmt.Errorf("workspace %s reserved by %s; refusing %s (TD-051)", root, holder, taskID)
	}
	return nil
}

// releaseWorkspace frees the root held by a task. terminal-only by
// convention: callers release after execution ownership is
// conclusively settled, never on uncertain outcomes.
func releaseWorkspace(taskID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureWorkspaceReservations(db); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM workspace_reservations WHERE task_id = ?`, taskID)
	return err
}

// workspaceReservationHolder reports who holds root, or "" if free.
func workspaceReservationHolder(root string) string {
	db, err := openTasksDB()
	if err != nil {
		return ""
	}
	defer db.Close()
	if err := ensureWorkspaceReservations(db); err != nil {
		return ""
	}
	var holder string
	if err := db.QueryRow(`SELECT task_id FROM workspace_reservations WHERE root = ?`, root).Scan(&holder); err != nil {
		return ""
	}
	return holder
}

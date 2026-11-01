package main

import (
	"database/sql"
	"fmt"
)

// task_dependencies.go: dependency-edge admission and status.
//
// This file owns the task dependency graph: edge insertion (plain and
// idempotent), cycle detection, and the verified-gate read. Both edge
// entry points share ONE transaction-scoped admission implementation
// (admitDependEdgeTx): the callers differ only in key ownership and
// replay semantics, never in validation.

// admitDependEdgeTx validates and records one dependency edge inside
// the caller's already-open BEGIN IMMEDIATE transaction on db. It
// opens no second connection: gate re-reads, the closed-run check,
// cycle validation, and the edge insert all ride the same handle, so
// concurrent edge additions serialize on the write lock instead of
// both passing validation and inserting a cycle.
//
// The caller owns key claiming, replay convergence, COMMIT/ROLLBACK,
// and receipts; this function only admits and records the edge.
func admitDependEdgeTx(db *sql.DB, taskID, depID string, req OperationRequest) error {
	// Re-read the gate state inside the transaction: statuses
	// observed before the lock are advisory, not authoritative.
	var run, status string
	if err := db.QueryRow(`SELECT run, status FROM tasks WHERE id = ?`, taskID).Scan(&run, &status); err == sql.ErrNoRows {
		return fmt.Errorf("no such task: %s", taskID)
	} else if err != nil {
		return fmt.Errorf("task %s: %w", taskID, err)
	}
	var depRun string
	if err := db.QueryRow(`SELECT run FROM tasks WHERE id = ?`, depID).Scan(&depRun); err == sql.ErrNoRows {
		return fmt.Errorf("no such task: %s", depID)
	} else if err != nil {
		return fmt.Errorf("task %s: %w", depID, err)
	}
	if run != depRun {
		return fmt.Errorf("cross-run dependency refused: %s (run %s) on %s (run %s)", taskID, run, depID, depRun)
	}
	if status != TaskPending {
		return fmt.Errorf("task %s is %s, not pending; dependency edits require a pending task (explicit plan revision)", taskID, status)
	}
	if closed, cerr := runClosedTx(db, run); cerr != nil {
		return cerr
	} else if closed {
		return fmt.Errorf("run %q is closed (finalized); dependency edits refused", run)
	}
	if err := dependCycleTx(db, taskID, depID); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT INTO task_deps (task_id, depends_on, request_id, actor, created) VALUES (?, ?, ?, ?, ?)`,
		taskID, depID, req.RequestID, req.Actor, nowUTC()); err != nil {
		return fmt.Errorf("dependency %s-on-%s not recorded: %w", taskID, depID, err)
	}
	return nil
}

// dependCycleTx refuses the edge taskID-on-depID when depID already
// (transitively) depends on taskID. Runs on the caller's transaction
// handle: no second connection.
func dependCycleTx(db *sql.DB, taskID, depID string) error {
	seen := map[string]bool{taskID: true}
	stack := []string{depID}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == taskID {
			return fmt.Errorf("dependency cycle refused: %s already transitively depends on %s", depID, taskID)
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		rows, err := db.Query(`SELECT depends_on FROM task_deps WHERE task_id = ?`, cur)
		if err != nil {
			return err
		}
		var next []string
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				rows.Close()
				return err
			}
			next = append(next, o)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		stack = append(stack, next...)
	}
	return nil
}

// addTaskDep records that taskID must not start until depID is
// verified. Edges are same-run, acyclic, and attributable (TD-030).
func addTaskDep(taskID, depID string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	if taskID == depID {
		return fmt.Errorf("task %s cannot depend on itself", taskID)
	}
	t, err := getTask(taskID)
	if err != nil {
		return err
	}
	d, err := getTask(depID)
	if err != nil {
		return err
	}
	if t.Run != d.Run {
		return fmt.Errorf("cross-run dependency refused: %s (run %s) on %s (run %s)", taskID, t.Run, depID, d.Run)
	}
	if terminalTaskStatus(t.Status) {
		return fmt.Errorf("task %s is terminal (%s); dependencies cannot be added", taskID, t.Status)
	}
	if t.Status != TaskPending {
		// Plan changes to running work need an explicit revision,
		// not a silent edge edit: dispatch-time admission already
		// snapshotted the dependency set the worker was claimed
		// under.
		return fmt.Errorf("task %s is %s, not pending; dependency edits require a pending task (explicit plan revision)", taskID, t.Status)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureRunStatesTable(db); err != nil {
		return err
	}
	// BEGIN IMMEDIATE takes the write lock up front: two
	// concurrent edge additions serialize here instead of both
	// passing the cycle check and inserting a cycle. The handle
	// is single-connection, so every statement below rides the
	// same transaction; db.Close rolls back an uncommitted one.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	if err := admitDependEdgeTx(db, taskID, depID, req); err != nil {
		return err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	committed = true
	recordReceipt(taskID, "dependency_added", fmt.Sprintf("actor=%s req=%s depends_on=%s", req.Actor, req.RequestID, depID))
	return nil
}

// addTaskDepIdempotent records the dependency edge AND owns the
// caller's idempotency key in ONE transaction on ONE connection.
//
// BEGIN IMMEDIATE takes the write lock up front; the key row is
// inserted first, then the edge is validated and recorded via the
// shared admitDependEdgeTx, then both commit together. A concurrent
// same-key request either blocks on the lock and then reads the
// winner (converge on an identical edge, *dependKeyConflict on a
// different one) or loses the key race outright — either way it never
// mutates domain state. Any failure before COMMIT rolls back both
// rows.
//
// A fresh key whose edge already exists (recorded under another key
// or out of band) converges with Replay=true and records nothing,
// mirroring the legacy replay path.
func addTaskDepIdempotent(taskID, depID string, req OperationRequest, requestKey string) (dependIdemOutcome, error) {
	if err := requireRequest(req); err != nil {
		return dependIdemOutcome{}, err
	}
	db, err := openTasksDB()
	if err != nil {
		return dependIdemOutcome{}, err
	}
	defer db.Close()
	if err := ensureRunStatesTable(db); err != nil {
		return dependIdemOutcome{}, err
	}
	if err := ensureMcpDependTable(db); err != nil {
		return dependIdemOutcome{}, err
	}
	// BEGIN IMMEDIATE takes the write lock up front: two
	// concurrent same-key requests serialize here instead of both
	// mutating domain state before one loses the key race. The
	// handle is single-connection, so every statement below rides
	// the same transaction; db.Close rolls back an uncommitted one.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return dependIdemOutcome{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	// Claim the key first, carrying the payload. A lost key race
	// reads the winner below and converges or conflicts WITHOUT
	// mutating domain state.
	if _, err := db.Exec(`INSERT INTO mcp_depend (request_id, task_id, dep_id, created) VALUES (?, ?, ?, ?)`,
		requestKey, taskID, depID, nowUTC()); err != nil {
		var winnerTask, winnerDep string
		if qerr := db.QueryRow(`SELECT task_id, dep_id FROM mcp_depend WHERE request_id = ?`, requestKey).Scan(&winnerTask, &winnerDep); qerr != nil {
			return dependIdemOutcome{}, err
		}
		if winnerTask != taskID || winnerDep != depID {
			return dependIdemOutcome{}, &dependKeyConflict{RequestKey: requestKey, WinnerTask: winnerTask, WinnerDep: winnerDep}
		}
		// Identical replay: converge on the winner. The deferred
		// ROLLBACK ends the transaction (the losing INSERT staged
		// nothing); the winner's commit already holds the edge.
		return dependIdemOutcome{TaskID: winnerTask, DepID: winnerDep, Replay: true}, nil
	}
	if taskID == depID {
		return dependIdemOutcome{}, fmt.Errorf("task %s cannot depend on itself", taskID)
	}
	// An edge recorded under another key (or out of band) converges
	// without duplicating: roll back the claim and replay.
	var existing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on = ?`, taskID, depID).Scan(&existing); err != nil {
		return dependIdemOutcome{}, err
	}
	if existing > 0 {
		return dependIdemOutcome{TaskID: taskID, DepID: depID, Replay: true}, nil
	}
	if err := admitDependEdgeTx(db, taskID, depID, req); err != nil {
		return dependIdemOutcome{}, err
	}
	if mcpIdemInjectTxFailure != nil {
		return dependIdemOutcome{}, mcpIdemInjectTxFailure
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return dependIdemOutcome{}, err
	}
	committed = true
	recordReceipt(taskID, "dependency_added", fmt.Sprintf("actor=%s req=%s depends_on=%s", req.Actor, req.RequestID, depID))
	return dependIdemOutcome{TaskID: taskID, DepID: depID, Replay: false}, nil
}

// unsatisfiedDepsOn is the handle-scoped core of unsatisfiedDeps:
// same gate, shared handle, no per-task open. Readiness scans call
// this per task; the wrapper keeps one-shot semantics.
func unsatisfiedDepsOn(db *sql.DB, taskID string) ([]string, error) {
	rows, err := db.Query(`SELECT depends_on FROM task_deps WHERE task_id = ? ORDER BY depends_on`, taskID)
	if err != nil {
		return nil, err
	}
	var deps []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return nil, err
		}
		deps = append(deps, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var blocking []string
	for _, depID := range deps {
		d, err := getTaskOn(db, depID)
		if err != nil {
			return nil, err
		}
		if d.Status != TaskVerified {
			blocking = append(blocking, fmt.Sprintf("%s (%s)", depID, d.Status))
		}
	}
	return blocking, nil
}

// unsatisfiedDeps returns the dependencies of taskID that are not
// verified. Verification — not process exit, report, or any other
// non-terminal state — is the only satisfaction (TD-031).
func unsatisfiedDeps(taskID string) ([]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return unsatisfiedDepsOn(db, taskID)
}

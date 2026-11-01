package main

import (
	"database/sql"
	"fmt"
	"time"
)

// fleet_resources.go: host capacity and fair admission.
//
// This is the fourth fleet module. It accounts per-node execution
// slots so dispatch can refuse over-capacity placement before
// claiming work, mirroring the run_limits pattern for runs. It is
// accounting only: no cgroup, no preemption, no cross-node
// stealing. A reservation binds (node, task, attempt); release
// frees exactly that binding, and double-release is refused so a
// load count can never go negative through replay.

func ensureFleetResourceTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_capacity (
		node_id   TEXT PRIMARY KEY,
		max_slots INTEGER NOT NULL DEFAULT 1
	)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_reservations (
		node_id    TEXT NOT NULL,
		task_id    TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		created_ns INTEGER NOT NULL,
		PRIMARY KEY (node_id, task_id, attempt_id)
	)`)
	return err
}

// setFleetNodeCapacity pins a node's concurrency budget. Unknown
// nodes are refused: capacity follows enrollment.
func setFleetNodeCapacity(nodeID string, maxSlots int) error {
	if maxSlots < 1 {
		return fmt.Errorf("fleet: max slots must be >= 1, got %d", maxSlots)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetNodeTable(db); err != nil {
		return err
	}
	if err := ensureFleetResourceTables(db); err != nil {
		return err
	}
	var one int
	if err := db.QueryRow(`SELECT 1 FROM fleet_nodes WHERE node_id = ?`, nodeID).Scan(&one); err != nil {
		return fmt.Errorf("fleet: unknown node %q: enroll before setting capacity", nodeID)
	}
	_, err = db.Exec(`INSERT INTO fleet_capacity (node_id, max_slots) VALUES (?, ?)
		ON CONFLICT (node_id) DO UPDATE SET max_slots = excluded.max_slots`, nodeID, maxSlots)
	return err
}

// reserveFleetSlot admits one (task, attempt) onto the node when a
// free slot exists. P0-C: the capacity predicate and the insert
// commit in one BEGIN IMMEDIATE transaction, so concurrent racers
// cannot both observe a free slot. The request ID makes retries
// idempotent only for byte-identical arguments: a reused request ID
// with different (node, task, attempt) is a conflict, never a
// silent success. Unknown nodes and full nodes are refused.
func reserveFleetSlot(nodeID, taskID, attemptID string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetNodeTable(db); err != nil {
		return err
	}
	if err := ensureFleetResourceTables(db); err != nil {
		return err
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	if err := reserveFleetSlotLocked(db, nodeID, taskID, attemptID, req); err != nil {
		return err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// reserveFleetSlotLocked runs inside the caller's immediate write
// transaction: idempotency check, capacity predicate, and insert
// are one atomic unit.
func reserveFleetSlotLocked(db *sql.DB, nodeID, taskID, attemptID string, req OperationRequest) error {
	var one int
	if err := db.QueryRow(`SELECT 1 FROM fleet_nodes WHERE node_id = ?`, nodeID).Scan(&one); err != nil {
		return fmt.Errorf("fleet: unknown node %q: enroll before reserving", nodeID)
	}
	var prevNode, prevTask, prevAttempt string
	if err := db.QueryRow(`SELECT node_id, task_id, attempt_id FROM fleet_reservations WHERE request_id = ?`,
		req.RequestID).Scan(&prevNode, &prevTask, &prevAttempt); err == nil {
		if prevNode != nodeID || prevTask != taskID || prevAttempt != attemptID {
			return fmt.Errorf("fleet: request ID %q conflict: already reserves (%s,%s,%s), refusing (%s,%s,%s)",
				req.RequestID, prevNode, prevTask, prevAttempt, nodeID, taskID, attemptID)
		}
		return nil
	}
	var maxSlots int
	if err := db.QueryRow(`SELECT max_slots FROM fleet_capacity WHERE node_id = ?`, nodeID).Scan(&maxSlots); err != nil {
		maxSlots = 1
	}
	var used int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fleet_reservations WHERE node_id = ?`, nodeID).Scan(&used); err != nil {
		return err
	}
	if used >= maxSlots {
		return fmt.Errorf("fleet: node %q at capacity (%d/%d)", nodeID, used, maxSlots)
	}
	_, err := db.Exec(`INSERT INTO fleet_reservations (node_id, task_id, attempt_id, request_id, created_ns)
		VALUES (?, ?, ?, ?, ?)`, nodeID, taskID, attemptID, req.RequestID, time.Now().UnixNano())
	return err
}

// releaseFleetSlot frees one reservation. Releasing a binding that
// is not held is refused, so replayed releases cannot drive the
// load negative.
func releaseFleetSlot(nodeID, taskID, attemptID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetResourceTables(db); err != nil {
		return err
	}
	res, err := db.Exec(`DELETE FROM fleet_reservations WHERE node_id = ? AND task_id = ? AND attempt_id = ?`,
		nodeID, taskID, attemptID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("fleet: no reservation for node %q task %q attempt %q", nodeID, taskID, attemptID)
	}
	return nil
}

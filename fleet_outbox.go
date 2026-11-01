package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// fleet_outbox.go: durable node-bound operation log.
//
// This is the third fleet module. When the controller hands work to
// a node, the exact operation identity must survive process death,
// lost acknowledgments, and node reconnects: a reconnecting node
// replays its pending ops rather than receiving fresh duplicates,
// and an ack settles exactly one op. Retried enqueues reuse the
// caller's request ID and observe the original row, mirroring the
// guidance accept-before-delivery pattern (TD-083).
//
// Task-scoped delivery rows (task_deliveries, guidance) are
// untouched: this outbox covers node operations (Offer, Claim,
// Report, Heartbeat, Reconcile, Stop), not task lifecycle.

type fleetOp struct {
	OpID      string
	NodeID    string
	Op        string
	Payload   string
	State     string
	RequestID string
	Created   int64
	Acked     int64
}

const (
	fleetOpPending = "pending"
	fleetOpAcked   = "acked"
)

func ensureFleetOutboxTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_outbox (
		op_id       TEXT PRIMARY KEY,
		node_id     TEXT NOT NULL,
		op          TEXT NOT NULL,
		payload     TEXT NOT NULL DEFAULT '{}',
		state       TEXT NOT NULL DEFAULT 'pending',
		request_id  TEXT NOT NULL DEFAULT '',
		created_ns  INTEGER NOT NULL,
		acked_ns    INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_outbox_request ON fleet_outbox (request_id)`)
	return err
}

// enqueueFleetOp durably records one node operation before any
// transmission. The request ID is the idempotency key: a retried
// enqueue of the same operation returns the original op ID.
func enqueueFleetOp(nodeID, op, payload string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(op) == "" {
		return "", fmt.Errorf("fleet outbox: node and op must not be empty")
	}
	if payload == "" {
		payload = "{}"
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureFleetOutboxTable(db); err != nil {
		return "", err
	}
	var existing, prevNode, prevOp, prevPayload string
	if err := db.QueryRow(`SELECT op_id, node_id, op, payload FROM fleet_outbox WHERE request_id = ?`,
		req.RequestID).Scan(&existing, &prevNode, &prevOp, &prevPayload); err == nil {
		if prevNode != nodeID || prevOp != op || prevPayload != payload {
			return "", fmt.Errorf("fleet: request ID %q conflict: already enqueued (%s,%s), refusing (%s,%s)",
				req.RequestID, prevNode, prevOp, nodeID, op)
		}
		return existing, nil
	}
	id := newUniqueID("fop")
	if _, err := db.Exec(`INSERT INTO fleet_outbox (op_id, node_id, op, payload, state, request_id, created_ns)
		VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
		id, nodeID, op, payload, req.RequestID, time.Now().UnixNano()); err != nil {
		return "", err
	}
	return id, nil
}

// ackFleetOp settles one enqueued op. Unknown op IDs are refused,
// never absorbed: an ack for something never enqueued is a protocol
// error.
func ackFleetOp(opID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetOutboxTable(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE fleet_outbox SET state = 'acked', acked_ns = ? WHERE op_id = ? AND state = 'pending'`,
		time.Now().UnixNano(), opID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("fleet outbox: unknown or already-settled op %q", opID)
	}
	return nil
}

// listPendingFleetOps returns a node's unsettled ops in enqueue
// order: the exact replay set a reconnecting node must observe.
func listPendingFleetOps(nodeID string) ([]fleetOp, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureFleetOutboxTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT op_id, node_id, op, payload, state, request_id, created_ns, acked_ns
		FROM fleet_outbox WHERE node_id = ? AND state = 'pending' ORDER BY created_ns ASC`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []fleetOp
	for rows.Next() {
		var o fleetOp
		if err := rows.Scan(&o.OpID, &o.NodeID, &o.Op, &o.Payload, &o.State, &o.RequestID, &o.Created, &o.Acked); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

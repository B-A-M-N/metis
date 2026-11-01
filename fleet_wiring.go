package main

// fleet_wiring.go: node execution adapter (P0-E).
//
// Fleet placement records now drive the real worker loop. The
// controller places a committed attempt on a node with
// offerFleetExecution (a durable Offer op); the loop running that
// session drains its node's pending offers after pickup, claims the
// attempt under the node's identity, executes, and settles the same
// identity. Attempts with no offer run the legacy path untouched —
// fleet placement is opt-in, never a gate on unplaced work.
//
// Delivery and completion stay distinct throughout: acking the
// offer says the node took the work; only settle says how it
// ended. A reconnect replays the unacked offer and the reclaim
// converges on the node's own in-flight claim instead of forking.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// fleetOpOffer is the outbox op placing one committed attempt.
const fleetOpOffer = "Offer"

// fleetOffer names one placed attempt: which node should execute
// which task attempt in which session.
type fleetOffer struct {
	TaskID  string `json:"task_id"`
	Attempt int    `json:"attempt"`
	Session string `json:"session"`
}

// fleetAttemptID renders a Metis attempt number as the opaque fleet
// attempt identity. This rendering is the single mapping between
// ledger attempts and fleet rows; both sides must use it.
func fleetAttemptID(attempt int) string {
	return fmt.Sprintf("attempt-%d", attempt)
}

// localFleetNodeID resolves the stable node identity for this
// machine: the earliest node row enrolled for this hostname is
// reused (and heartbeated); only a machine never enrolled mints a
// new row. Earliest-wins keeps the identity stable when operators
// (or tests) enroll superseding rows for the same host.
func localFleetNodeID() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("fleet: cannot prove local host: %w", err)
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	if err := ensureFleetNodeTable(db); err != nil {
		db.Close()
		return "", err
	}
	var id string
	qerr := db.QueryRow(`SELECT node_id FROM fleet_nodes WHERE host = ? ORDER BY enrolled_ns ASC LIMIT 1`, host).Scan(&id)
	db.Close()
	if qerr == sql.ErrNoRows {
		id, err = enrollFleetNode(host, "")
		if err != nil {
			return "", err
		}
	} else if qerr != nil {
		return "", qerr
	}
	if err := heartbeatFleetNode(id); err != nil {
		return "", err
	}
	return id, nil
}

// offerFleetExecution places one committed attempt on an enrolled
// node. The offer is durable before any execution; the node's
// adapter drains it, claims, executes, and settles. Offers to
// unknown nodes are refused — work is never placed into the void.
func offerFleetExecution(nodeID, taskID string, attempt int, session string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(session) == "" {
		return "", fmt.Errorf("fleet offer: task and session must not be empty")
	}
	known := false
	nodes, err := listFleetNodes(10 * time.Minute)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.NodeID == nodeID {
			known = true
			break
		}
	}
	if !known {
		return "", fmt.Errorf("fleet offer: unknown node %q: enroll before placing work", nodeID)
	}
	payload, err := json.Marshal(fleetOffer{TaskID: taskID, Attempt: attempt, Session: session})
	if err != nil {
		return "", err
	}
	return enqueueFleetOp(nodeID, fleetOpOffer, string(payload), req)
}

// drainFleetOffer consumes this node's pending offer for the exact
// (task, attempt, session): claims the execution, then acks the op
// so a reconnect never replays it. A claim failure refuses loudly
// with the offer left pending — the attempt must not execute
// unowned, and the placement survives for reconcile. No matching
// offer means legacy placement (ok=false), never an error.
func drainFleetOffer(taskID string, attempt int, session string) (string, bool, error) {
	node, err := localFleetNodeID()
	if err != nil {
		return "", false, err
	}
	ops, err := listPendingFleetOps(node)
	if err != nil {
		return "", false, err
	}
	for _, o := range ops {
		if o.Op != fleetOpOffer {
			continue
		}
		var off fleetOffer
		if jerr := json.Unmarshal([]byte(o.Payload), &off); jerr != nil {
			continue
		}
		if off.TaskID != taskID || off.Attempt != attempt || off.Session != session {
			continue
		}
		claimReq := OperationRequest{Actor: "worker-loop/" + session, RequestID: o.RequestID}
		if cerr := claimFleetExecution(node, taskID, fleetAttemptID(attempt), claimReq); cerr != nil {
			return "", false, fmt.Errorf("fleet: offer %s cannot claim %s attempt %d: %w", o.OpID, taskID, attempt, cerr)
		}
		// Ack AFTER the claim commits: a crash between claim and
		// ack replays the offer, and the reclaim converges on the
		// node's own in-flight claim instead of duplicating work.
		if aerr := ackFleetOp(o.OpID); aerr != nil {
			return "", false, fmt.Errorf("fleet: claimed %s attempt %d but offer %s unackable: %w", taskID, attempt, o.OpID, aerr)
		}
		return node, true, nil
	}
	return "", false, nil
}

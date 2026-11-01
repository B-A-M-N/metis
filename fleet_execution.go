package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"
)

// fleet_execution.go: node-scoped execution ownership.
//
// This is the fifth fleet module. It binds one claimed attempt to
// one live local node, enforcing single ownership across the fleet:
// the same (task, attempt) can never be claimed by two nodes. The
// claim reserves a capacity slot; settlement (complete or fail)
// frees it. Stale or remote nodes cannot claim, and settling an
// execution the node does not hold is refused.
//
// Local execution itself still runs through the existing worker
// path (worker_run.go, execution_receipts); this module owns only
// the fleet binding, never the process.

const (
	fleetExecClaimed   = "claimed"
	fleetExecCompleted = "completed"
	fleetExecFailed    = "failed"
)

func ensureFleetExecutionTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_executions (
		task_id    TEXT NOT NULL,
		attempt_id TEXT NOT NULL,
		node_id    TEXT NOT NULL,
		state      TEXT NOT NULL DEFAULT 'claimed',
		detail     TEXT NOT NULL DEFAULT '',
		created_ns INTEGER NOT NULL,
		updated_ns INTEGER NOT NULL,
		PRIMARY KEY (task_id, attempt_id)
	)`)
	return err
}

// liveLocalNode verifies the node is enrolled, live, and this
// machine. Claims and settlements both gate on it.
func liveLocalNode(nodeID string, ttl time.Duration) error {
	nodes, err := listFleetNodes(ttl)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		if n.NodeID != nodeID {
			continue
		}
		if !n.Alive {
			return fmt.Errorf("fleet: node %q is stale: heartbeat lapsed", nodeID)
		}
		local, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("fleet: cannot prove local host: %w", err)
		}
		if n.Host != local {
			return fmt.Errorf("fleet: node %q is remote (host %q): no remote transport yet", nodeID, n.Host)
		}
		return nil
	}
	return fmt.Errorf("fleet: unknown node %q: enroll before claiming", nodeID)
}

// claimFleetExecution binds (task, attempt) to a live local node.
// P0-D: the ownership fence, the capacity reservation, and the
// execution row commit in one BEGIN IMMEDIATE transaction. A crash
// between them cannot strand a reservation, and concurrent claims
// for one attempt converge on exactly one owner: losers roll back
// with no orphan rows. The attempt primary key remains the fence
// against cross-node forks.
func claimFleetExecution(nodeID, taskID, attemptID string, req OperationRequest) error {
	if err := liveLocalNode(nodeID, 10*time.Minute); err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetExecutionTable(db); err != nil {
		return err
	}
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
	var holder, hstate string
	if err := db.QueryRow(`SELECT node_id, state FROM fleet_executions WHERE task_id = ? AND attempt_id = ?`,
		taskID, attemptID).Scan(&holder, &hstate); err == nil {
		// Replay convergence: a node reclaiming its own in-flight
		// claim (crashed between claim and ack, reconnect replay)
		// resumes the same ownership instead of forking. Terminal
		// states stay fenced — a settled execution is never
		// re-claimed; retries need new attempts.
		if holder == nodeID && hstate == fleetExecClaimed {
			return nil
		}
		return fmt.Errorf("fleet: attempt %q/%q already claimed by node %q", taskID, attemptID, holder)
	}
	if err := reserveFleetSlotLocked(db, nodeID, taskID, attemptID, req); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	if _, err := db.Exec(`INSERT INTO fleet_executions (task_id, attempt_id, node_id, state, created_ns, updated_ns)
		VALUES (?, ?, ?, 'claimed', ?, ?)`, taskID, attemptID, nodeID, now, now); err != nil {
		return err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// fleetSettleSkipped marks a settlement result for a claim that held
// no fleet ownership: there was nothing to settle.
const fleetSettleSkipped = "skipped"

// fleetSettleAbsent marks a reconcile with no fleet execution row:
// nothing was ever claimed, so there is nothing to repair.
const fleetSettleAbsent = "absent"

// fleetSettleUnsettled marks a settlement attempt that did not land:
// the fleet row keeps its durable claimed state and the capacity
// reservation is preserved. The caller must reconcile.
const fleetSettleUnsettled = "unsettled"

// fleetSettleResult is settlement's typed outcome: which attempt on
// which node reached which disposition, and the settlement error when
// it did not land. A non-nil Err never rewrites the task ledger —
// the attempt's report and status stand as filed; only the fleet
// binding still needs reconcileFleetSettlement.
type fleetSettleResult struct {
	TaskID      string
	Attempt     int
	AttemptID   string
	NodeID      string
	Disposition string
	Settled     bool
	Err         error
}

// fleetSettleProbe is a test-only hook invoked inside the settlement
// transaction before the state change. A non-nil return aborts the
// settlement exactly like a database failure: rollback, claimed
// state preserved. Production code never sets it.
var fleetSettleProbe func() error

// settleFleetExecution moves a held claim to a terminal state and
// frees its slot atomically: the state change and the capacity
// release commit together, so a crash cannot settle without freeing
// or free without settling. Only the holding node can settle, and
// only once.
func settleFleetExecution(nodeID, taskID, attemptID, state, detail string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetExecutionTable(db); err != nil {
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
	if fleetSettleProbe != nil {
		if perr := fleetSettleProbe(); perr != nil {
			return perr
		}
	}
	res, err := db.Exec(`UPDATE fleet_executions SET state = ?, detail = ?, updated_ns = ?
		WHERE task_id = ? AND attempt_id = ? AND node_id = ? AND state = 'claimed'`,
		state, detail, time.Now().UnixNano(), taskID, attemptID, nodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("fleet: no claimed execution for node %q task %q attempt %q", nodeID, taskID, attemptID)
	}
	if _, err := db.Exec(`DELETE FROM fleet_reservations WHERE node_id = ? AND task_id = ? AND attempt_id = ?`,
		nodeID, taskID, attemptID); err != nil {
		return err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

func completeFleetExecution(nodeID, taskID, attemptID, detail string) error {
	return settleFleetExecution(nodeID, taskID, attemptID, fleetExecCompleted, detail)
}

func failFleetExecution(nodeID, taskID, attemptID, detail string) error {
	return settleFleetExecution(nodeID, taskID, attemptID, fleetExecFailed, detail)
}

// fleetConfirmedAttemptOutcome independently confirms what one
// attempt established, from the attempt's own durable evidence —
// never from the fleet row being reconciled. A filed report or a
// recorded execution result proves the attempt ran to an outcome;
// absence of both proves nothing, and the caller must not settle.
func fleetConfirmedAttemptOutcome(taskID string, attempt int) (reported, recorded bool, err error) {
	db, err := openTasksDB()
	if err != nil {
		return false, false, err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return false, false, err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&n); err != nil {
		return false, false, err
	}
	reported = n > 0
	var exitCode sql.NullInt64
	qerr := db.QueryRow(`SELECT exit_code FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&exitCode)
	switch {
	case qerr == sql.ErrNoRows:
		recorded = false
	case qerr != nil:
		return false, false, qerr
	default:
		recorded = exitCode.Valid
	}
	return reported, recorded, nil
}

// reconcileFleetSettlement repairs a fleet binding left claimed by a
// failed settlement or a crash between evidence commit and
// settlement. It settles ONLY after independently confirming the
// attempt outcome from the attempt's own evidence (a filed report or
// a recorded execution result), and it never launches execution — it
// reads the ledger and settles the binding. Already-settled rows
// converge as no-ops; unknown outcomes and foreign holders are
// refused with the claimed state preserved.
func reconcileFleetSettlement(nodeID, taskID string, attempt int) fleetSettleResult {
	aid := fleetAttemptID(attempt)
	res := fleetSettleResult{
		TaskID:      taskID,
		Attempt:     attempt,
		AttemptID:   aid,
		NodeID:      nodeID,
		Disposition: fleetSettleUnsettled,
	}
	db, err := openTasksDB()
	if err != nil {
		res.Err = fmt.Errorf("fleet: reconcile %s/%s on %s: %w (claimed state untouched)", taskID, aid, nodeID, err)
		return res
	}
	defer db.Close()
	if err := ensureFleetExecutionTable(db); err != nil {
		res.Err = fmt.Errorf("fleet: reconcile %s/%s on %s: %w (claimed state untouched)", taskID, aid, nodeID, err)
		return res
	}
	if err := ensureFleetResourceTables(db); err != nil {
		res.Err = fmt.Errorf("fleet: reconcile %s/%s on %s: %w (claimed state untouched)", taskID, aid, nodeID, err)
		return res
	}
	var holder, state string
	if err := db.QueryRow(`SELECT node_id, state FROM fleet_executions WHERE task_id = ? AND attempt_id = ?`,
		taskID, aid).Scan(&holder, &state); err != nil {
		if err == sql.ErrNoRows {
			res.Disposition = fleetSettleAbsent
			res.Err = fmt.Errorf("fleet: nothing to reconcile for task %q attempt %q: no execution row", taskID, aid)
			return res
		}
		res.Err = fmt.Errorf("fleet: reconcile %s/%s on %s: %w (claimed state untouched)", taskID, aid, nodeID, err)
		return res
	}
	if holder != nodeID {
		recordReceipt(taskID, "fleet_reconcile_refused",
			fmt.Sprintf("attempt=%d node=%s holder=%s: only the holding node reconciles; claimed state preserved", attempt, nodeID, holder))
		res.Err = fmt.Errorf("fleet: execution %q/%q is held by node %q, not %q: refusing (claimed state preserved)", taskID, aid, holder, nodeID)
		return res
	}
	switch state {
	case fleetExecCompleted, fleetExecFailed:
		res.Disposition = state
		res.Settled = true
		return res
	case fleetExecClaimed:
	default:
		res.Err = fmt.Errorf("fleet: execution %q/%q in unknown state %q: refusing (claimed state preserved)", taskID, aid, state)
		return res
	}
	reported, recorded, cerr := fleetConfirmedAttemptOutcome(taskID, attempt)
	if cerr != nil {
		res.Err = fmt.Errorf("fleet: reconcile %s/%s on %s cannot confirm outcome: %v (claimed state preserved)", taskID, aid, nodeID, cerr)
		return res
	}
	if !reported && !recorded {
		recordReceipt(taskID, "fleet_reconcile_deferred",
			fmt.Sprintf("attempt=%d node=%s: outcome unconfirmed (no report or execution result); claimed state preserved", attempt, nodeID))
		res.Err = fmt.Errorf("fleet: attempt %q/%q outcome unconfirmed (no report or execution result): refusing to settle; reconcile after the attempt outcome is established", taskID, aid)
		return res
	}
	detail := fmt.Sprintf("reconcile: attempt outcome independently confirmed (report=%v result=%v)", reported, recorded)
	if serr := completeFleetExecution(nodeID, taskID, aid, detail); serr != nil {
		recordReceipt(taskID, "fleet_settle_failed",
			fmt.Sprintf("attempt=%d node=%s reconcile failed: %v (claimed state preserved)", attempt, nodeID, serr))
		res.Err = fmt.Errorf("fleet: reconcile of %q/%q failed: %v (claimed state preserved; retry reconcile)", taskID, aid, serr)
		return res
	}
	recordReceipt(taskID, "fleet_settle_reconciled",
		fmt.Sprintf("attempt=%d node=%s report=%v result=%v", attempt, nodeID, reported, recorded))
	res.Disposition = fleetExecCompleted
	res.Settled = true
	return res
}

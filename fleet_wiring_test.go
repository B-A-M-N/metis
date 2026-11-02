package main

import (
	"strings"
	"testing"
	"time"
)

// P0-E failing-first: the worker loop must execute fleet-placed
// attempts under an acquired claim and settle the same identity —
// and leave unplaced attempts on the legacy path untouched.

// An offered attempt runs to completion under its claim: the fleet
// row settles completed, the slot frees, and the report still files.
func TestFleetOfferExecutesUnderClaim(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fw1")
	seedOwnedSession(t, "w", "fw1")
	tk, err := createTask("run-fw1", "w", "print hello", "output says hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	node, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("local node: %v", err)
	}
	if err := setFleetNodeCapacity(node, 2); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if _, err := offerFleetExecution(node, tk.ID, 1, "w", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo model-output"), testReq("op"))
	if err != nil {
		t.Fatalf("loop failed: %v", err)
	}
	if !ran {
		t.Fatal("loop must report it executed the task")
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state, holder string
	if err := db.QueryRow(`SELECT state, node_id FROM fleet_executions WHERE task_id = ? AND attempt_id = ?`,
		tk.ID, "attempt-1").Scan(&state, &holder); err != nil {
		t.Fatalf("fleet execution row missing: %v", err)
	}
	if state != fleetExecCompleted || holder != node {
		t.Fatalf("fleet execution = (%s, %s), want (completed, %s)", state, holder, node)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_reservations WHERE node_id = ?`, node); n != 0 {
		t.Fatalf("settlement must free the slot, %d reservations remain", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_outbox WHERE node_id = ? AND state = 'pending'`, node); n != 0 {
		t.Fatalf("consumed offer must be acked, %d ops still pending", n)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("expected report_ready, got %s", got.Status)
	}
}

// A full node refuses the worker loudly: no unowned execution, no
// report, the offer stays pending for reconcile, and nothing is
// settled. Capacity exhaustion — not a rival claim on the same
// attempt — is the contention that one node can actually observe.
func TestFleetClaimContentionRefusesWorker(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fw2")
	seedOwnedSession(t, "w", "fw2")
	seedOwnedSession(t, "wx", "fw2")
	holder, err := createTask("run-fw2", "w", "holder", "acc")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-fw2", "wx", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(holder.ID, "w"); err != nil {
		t.Fatalf("dispatch holder: %v", err)
	}
	if err := dispatchTask(tk.ID, "wx"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	node, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("local node: %v", err)
	}
	if err := setFleetNodeCapacity(node, 1); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	// An unrelated attempt holds the node's only slot.
	if err := claimFleetExecution(node, holder.ID, fleetAttemptID(1), OperationRequest{Actor: "other", RequestID: newRequestID()}); err != nil {
		t.Fatalf("rival claim: %v", err)
	}
	if _, err := offerFleetExecution(node, tk.ID, 1, "wx", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if _, err := runWorkerOnce("wx", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op")); err == nil {
		t.Fatal("loop executed despite exhausted fleet capacity; must be refused")
	} else if !strings.Contains(err.Error(), "fleet") {
		t.Fatalf("refusal must name the fleet claim: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatal("refused worker must file no report")
	}
	// The offer survives unacked for reconcile, and nothing settled.
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_outbox WHERE node_id = ? AND state = 'pending'`, node); n != 1 {
		t.Fatalf("refused offer must stay pending, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_executions WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatal("refused claim must leave no execution row")
	}
}

// Reclaiming the node's own in-flight claim converges (crash
// between claim and ack, reconnect replay); a settled execution
// is never re-claimed — retries need new attempts.
func TestFleetSameNodeClaimConverges(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fw4")
	seedHarnessSession(t, "w", "fw4", "demo")
	tk, err := createTask("run-fw4", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	node, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("local node: %v", err)
	}
	if err := setFleetNodeCapacity(node, 2); err != nil {
		t.Fatalf("capacity: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(node, tk.ID, fleetAttemptID(1), req); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := claimFleetExecution(node, tk.ID, fleetAttemptID(1), req); err != nil {
		t.Fatalf("same-node reclaim must converge, got: %v", err)
	}
	if err := completeFleetExecution(node, tk.ID, fleetAttemptID(1), "done"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := claimFleetExecution(node, tk.ID, fleetAttemptID(1), OperationRequest{Actor: "test", RequestID: newRequestID()}); err == nil {
		t.Fatal("settled execution re-claimed; terminal states must stay fenced")
	}
}

// Without an offer the loop runs exactly as before: no fleet row,
// no slot consumed, report filed on the legacy path.
func TestFleetNoOfferLegacyPath(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "fw3")
	seedOwnedSession(t, "w", "fw3")
	tk, err := createTask("run-fw3", "w", "print hello", "output says hello")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op"))
	if err != nil || !ran {
		t.Fatalf("legacy loop must run unplaced work: ran=%v err=%v", ran, err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureFleetExecutionTable(db); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM fleet_executions WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatal("unplaced attempt must not create a fleet execution")
	}
}

// The local node identity is stable across calls and heartbeats
// stay fresh: one machine, one node row, always live.
func TestLocalFleetNodeStable(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	b, err := localFleetNodeID()
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if a != b {
		t.Fatalf("local node unstable: %q vs %q", a, b)
	}
	nodes, err := listFleetNodes(10 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.NodeID == a {
			found = true
			if !n.Alive {
				t.Fatal("local node must read live after heartbeat")
			}
		}
	}
	if !found {
		t.Fatalf("local node %q not enrolled", a)
	}
}

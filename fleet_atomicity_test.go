package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// P0-C: capacity check and reservation insert must be one atomic
// predicate. Twenty racers against two slots must leave exactly two
// reservations — never an over-admitted third.
func TestFleetReserveConcurrentCapacity(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	id, err := enrollFleetNode("host-race", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := setFleetNodeCapacity(id, 2); err != nil {
		t.Fatalf("setFleetNodeCapacity: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := OperationRequest{Actor: "test", RequestID: newRequestID()}
			if err := reserveFleetSlot(id, fmt.Sprintf("task-r%d", i), "atm-1", req); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if won != 2 {
		t.Fatalf("capacity race admitted %d, want exactly 2", won)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fleet_reservations WHERE node_id = ?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("reservation rows = %d, want 2", count)
	}
}

// P0-C: a reused request ID with different arguments is a conflict,
// not a silent success. Idempotent replay requires identical
// payload, not merely an identical key.
func TestFleetReserveRequestIDConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	id, err := enrollFleetNode("host-idem", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot(id, "task-a", "atm-1", req); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	if err := reserveFleetSlot(id, "task-B-DIFFERENT", "atm-1", req); err == nil {
		t.Fatal("conflicting reuse of request ID succeeded, want conflict")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error = %q, want idempotency conflict", err)
	}
}

// P0-D: concurrent claims for one attempt converge on a single
// owner with no stranded reservation on the loser.
func TestFleetClaimRaceNoOrphan(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	a, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enroll a: %v", err)
	}
	b, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enroll b: %v", err)
	}
	for _, n := range []string{a, b} {
		if err := heartbeatFleetNode(n); err != nil {
			t.Fatalf("heartbeat %q: %v", n, err)
		}
		if err := setFleetNodeCapacity(n, 4); err != nil {
			t.Fatalf("capacity %q: %v", n, err)
		}
	}
	tx := uniqueTaskKey("task-claim-race")
	var wg sync.WaitGroup
	for _, n := range []string{a, b} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			_ = claimFleetExecution(n, tx, "atm-1", OperationRequest{Actor: "test", RequestID: newRequestID()})
		}(n)
	}
	wg.Wait()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var owners int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fleet_executions WHERE task_id = ? AND attempt_id = 'atm-1'`, tx).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Fatalf("execution rows = %d, want exactly 1 owner", owners)
	}
	var orphans int
	if err := db.QueryRow(`SELECT COUNT(*) FROM fleet_reservations WHERE task_id = ? AND NOT EXISTS
		(SELECT 1 FROM fleet_executions WHERE fleet_executions.task_id = fleet_reservations.task_id
		AND fleet_executions.attempt_id = fleet_reservations.attempt_id)`, tx).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Fatalf("%d stranded reservations without execution rows", orphans)
	}
}

// P0-C (outbox): a reused request ID with different op/payload is a
// conflict, not a silent replay of the old operation.
func TestFleetOutboxRequestIDConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	id, err := enrollFleetNode("host-obx", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if _, err := enqueueFleetOp(id, "Offer", `{"task":"t1"}`, req); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := enqueueFleetOp(id, "Claim", `{"task":"t1-changed"}`, req); err == nil {
		t.Fatal("conflicting reuse of request ID succeeded, want conflict")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error = %q, want idempotency conflict", err)
	}
}

package main

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// P0-A: workspace exclusivity must hold at commit, not by post-commit
// yield. Fifty concurrent dispatches onto one root must produce
// exactly one owner with no loser ever observably dispatched: losers
// are refused, never committed-then-yielded.
func TestDispatchWorkspaceRaceSingleOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (concurrency stress); run without -short")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "wrace")
	seedOwnedSession(t, "w", "wrace")

	const n = 50
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		tk, err := createTask("run-race", "w", fmt.Sprintf("race %d", i), "done")
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = tk.ID
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			req := OperationRequest{Actor: "test", RequestID: newRequestID()}
			if err := dispatchTaskWithRequest(id, "w", req); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(id)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("race did not settle within 60s")
	}
	mu.Lock()
	defer mu.Unlock()
	if won != 1 {
		t.Fatalf("exactly one dispatch must win, got %d", won)
	}
	// No loser may have been committed-then-yielded: yield receipts
	// prove the old check-after-commit path ran.
	for _, id := range ids {
		rs, err := listReceipts(id)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Kind == "dispatch_yielded" {
				t.Fatalf("task %s was committed then yielded: %+v", id, r)
			}
		}
	}
}

// The reservation lifecycle: a held root refuses a second owner,
// and a terminal transition releases it for the next task.
func TestWorkspaceReservationReleaseOnTerminal(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "wrel")
	seedOwnedSession(t, "w", "wrel")
	root, err := sessionRoot("w")
	if err != nil {
		t.Fatal(err)
	}
	a, err := createTask("run-rel", "w", "holder", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("dispatch holder: %v", err)
	}
	if err := reserveWorkspace(root, a.ID, 1); err == nil {
		t.Fatal("second reservation on held root admitted, want refusal")
	}
	if err := transitionTask(a.ID, []string{TaskDispatched}, TaskFailed, ""); err != nil {
		t.Fatalf("fail holder: %v", err)
	}
	b, err := createTask("run-rel", "w", "next", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(b.ID, "w", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("dispatch after terminal release refused: %v", err)
	}
}

// Pickup enforces the reservation: a dispatched task whose root is
// held by someone else (or nothing at all) cannot go working.
func TestPickupRequiresWorkspaceReservation(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "wpick")
	seedOwnedSession(t, "w", "wpick")
	a, err := createTask("run-pick", "w", "holder", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	committed, err := getTask(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the reservation to another owner: pickup must refuse.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	root, err := sessionRoot("w")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE workspace_reservations SET task_id = 'task-impostor' WHERE root = ?`, root); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := markTaskWorking(a.ID, committed.Attempt, testReq("worker-loop/w")); err == nil {
		t.Fatal("pickup with foreign workspace reservation succeeded, want refusal")
	} else if !strings.Contains(err.Error(), "reservation") {
		t.Fatalf("error = %q, want reservation refusal", err)
	}
}

package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// uniqueTaskKey keeps execution tests hermetic against the shared
// test database: fixed IDs would collide with rows settled by an
// earlier run, and the single-owner fence correctly refuses those.
func uniqueTaskKey(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// Claiming an attempt on a live local node binds exactly one
// execution owner: a second node claiming the same attempt is
// refused, never forked into a duplicate execution.
func TestFleetExecutionSingleOwner(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
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
	for _, id := range []string{a, b} {
		if err := heartbeatFleetNode(id); err != nil {
			t.Fatalf("heartbeat %q: %v", id, err)
		}
	}
	tx := uniqueTaskKey("task-x")
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(a, tx, "atm-1", req); err != nil {
		t.Fatalf("claim on a: %v", err)
	}
	req2 := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(b, tx, "atm-1", req2); err == nil {
		t.Fatal("second node claimed same attempt, want refusal")
	} else if !strings.Contains(err.Error(), "already claimed") {
		t.Fatalf("error = %q, want already-claimed refusal", err)
	}
}

// Claims require a live node: stale liveness cannot take ownership,
// or a dead node would hold work nobody can see.
func TestFleetExecutionStaleNodeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	backdateFleetHeartbeat(t, id, time.Now().Add(-time.Hour).UnixNano())
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(id, uniqueTaskKey("task-y"), "atm-1", req); err == nil {
		t.Fatal("stale node claimed execution, want refusal")
	}
}

// Completing settles the binding and frees the slot: the node can
// take new work, and settling an unknown execution is refused.
func TestFleetExecutionCompleteSettles(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := heartbeatFleetNode(id); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	if err := setFleetNodeCapacity(id, 1); err != nil {
		t.Fatalf("setFleetNodeCapacity: %v", err)
	}
	tx := uniqueTaskKey("task-z")
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(id, tx, "atm-1", req); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := completeFleetExecution(id, tx, "atm-1", "done"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := completeFleetExecution(id, tx, "atm-1", "done"); err == nil {
		t.Fatal("double settle succeeded, want refusal")
	}
	// Slot freed: a new claim fits again.
	req2 := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(id, uniqueTaskKey("task-w"), "atm-1", req2); err != nil {
		t.Fatalf("post-complete claim refused: %v", err)
	}
}

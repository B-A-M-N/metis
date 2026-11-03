package main

import (
	"fmt"
	"os"
	"testing"
)

// TestTwelveWorkerFleet is the composed concurrency gate: twelve
// live workers stay independently addressable — distinct processes,
// distinct fleet execution owners, distinct tabs, and independent
// input leases — under one controller and one node capacity budget.
// NOTE: the twelve processes run `sleep 30` under fleet claims; this
// qualifies the concurrency/isolation shape, NOT twelve simultaneously
// inferring Muse agents.
func TestTwelveWorkerFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (fleet scale); run without -short")
	}
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()

	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	node, err := enrollFleetNode(host, `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := heartbeatFleetNode(node); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	if err := setFleetNodeCapacity(node, 12); err != nil {
		t.Fatalf("setFleetNodeCapacity: %v", err)
	}

	const n = 12
	var names []string
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("qw%d", i)
		names = append(names, name)
		if err := ptyStart(name, work, "sleep 30", "muse", "code"); err != nil {
			t.Fatalf("ptyStart %s: %v", name, err)
		}
		defer ptyStop(name)
		task := uniqueTaskKey(fmt.Sprintf("task-12-%d", i))
		req := OperationRequest{Actor: "test", RequestID: newRequestID()}
		if err := claimFleetExecution(node, task, "atm-1", req); err != nil {
			t.Fatalf("fleet claim %s: %v", name, err)
		}
		defer completeFleetExecution(node, task, "atm-1", "twelve-fleet done")
	}

	metas, err := ptyListAll()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ptyMeta{}
	for _, m := range metas {
		byName[m.Name] = m
	}
	seenWorker, seenSup := map[int]bool{}, map[int]bool{}
	for _, name := range names {
		m, ok := byName[name]
		if !ok {
			t.Fatalf("worker %s missing from registry", name)
		}
		if ptyClassify(m) != "running" {
			t.Fatalf("worker %s not running: %+v", name, m)
		}
		if seenWorker[m.WorkerPid] || seenSup[m.SupervisorPid] {
			t.Fatalf("worker %s shares an identity: %+v", name, m)
		}
		seenWorker[m.WorkerPid] = true
		seenSup[m.SupervisorPid] = true
	}
	tabs := combinedNames(loadRegistry(), metas)
	for _, name := range names {
		found := false
		for _, tab := range tabs {
			if tab == ptyDisplay(name) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no tab for %s in %v", name, tabs)
		}
	}
	// Thirteenth claim exceeds node capacity: concurrency budget
	// holds at twelve even with live workers waiting.
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(node, uniqueTaskKey("task-13"), "atm-1", req); err == nil {
		t.Fatal("thirteenth claim admitted over capacity 12, want refusal")
	}
	// Input isolation at scale: qw0's lease blocks a rival on qw0
	// but never touches qw11.
	if err := ptyAcquireLease("qw0", "ctl-A"); err != nil {
		t.Fatalf("lease qw0: %v", err)
	}
	if err := ptyAcquireLease("qw0", "ctl-B"); err == nil {
		t.Fatal("rival took the qw0 input lease")
	}
	if err := ptyAcquireLease("qw11", "ctl-B"); err != nil {
		t.Fatalf("lease qw11 should be independent: %v", err)
	}
}

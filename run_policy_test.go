package main

// run_policy_test.go: closed-run admission, pending-only
// dependency edits, concurrent edge serialization, and the remote
// ingress trust marker.

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// Finalization closes the run: no fresh task may be admitted
// afterwards, while idempotent replay of an existing key still
// returns its row (replay creates no new work).
func TestClosedRunRefusesAdmission(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTaskIdem("run-closed", "agent", "work", "done", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := finalizeRun("run-closed", testReq("orch")); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, err := createTask("run-closed", "agent", "late work", "done"); err == nil {
		t.Fatal("task admitted after run close")
	} else if !strings.Contains(err.Error(), "closed") {
		t.Fatalf("refusal must name the closed run, got: %v", err)
	}
	// Replay of the pre-close key still converges on its row.
	rt, err := createTaskIdem("run-closed", "agent", "work", "done", "key-1")
	if err != nil {
		t.Fatalf("idempotent replay must survive close: %v", err)
	}
	if rt.ID != tk.ID {
		t.Fatalf("replay must return the original row, got %s want %s", rt.ID, tk.ID)
	}
}

// Dependency edits are a planning-time operation: once a task
// leaves pending, its edges are frozen absent an explicit plan
// revision.
func TestDependencyEditRestrictedToPending(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "depp")
	seedOwnedSession(t, "w", "depp")
	a, err := createTask("run-depp", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-depp", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := addTaskDep(a.ID, b.ID, testReq("orch")); err == nil {
		t.Fatal("dependency edit on a dispatched task must be refused")
	} else if !strings.Contains(err.Error(), "pending") {
		t.Fatalf("refusal must name the pending requirement, got: %v", err)
	}
	// Pending tasks still accept edges.
	if err := addTaskDep(b.ID, a.ID, testReq("orch")); err != nil {
		t.Fatalf("pending edit must succeed: %v", err)
	}
}

// Two concurrent opposite edge additions must serialize: exactly
// one wins, and the graph never holds a cycle.
func TestConcurrentDepEdgesSerialize(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (concurrency stress); run without -short")
	}
	for i := 0; i < 10; i++ {
		func() {
			t.Setenv("METIS_STATE", t.TempDir())
			a, err := createTask(fmt.Sprintf("run-race-%d", i), "agent", "a", "")
			if err != nil {
				t.Fatal(err)
			}
			b, err := createTask(fmt.Sprintf("run-race-%d", i), "agent", "b", "")
			if err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				errs[0] = addTaskDep(a.ID, b.ID, testReq("o1"))
			}()
			go func() {
				defer wg.Done()
				errs[1] = addTaskDep(b.ID, a.ID, testReq("o2"))
			}()
			wg.Wait()
			won := 0
			for _, e := range errs {
				if e == nil {
					won++
				}
			}
			if won != 1 {
				t.Fatalf("trial %d: exactly one edge must win, errs=%v", i, errs)
			}
			// The loser must now fail as a cycle either way.
			if won == 1 && errs[0] == nil {
				if err := addTaskDep(b.ID, a.ID, testReq("o3")); err == nil {
					t.Fatalf("trial %d: reverse edge must be a cycle", i)
				}
			} else if err := addTaskDep(a.ID, b.ID, testReq("o3")); err == nil {
				t.Fatalf("trial %d: reverse edge must be a cycle", i)
			}
		}()
	}
}

// Declaring remote ingress without a token fails closed: a remote
// caller must not inherit local-operator trust merely because
// Metis runs locally.
func TestRemoteIngressDisablesLocalTrust(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_REMOTE_INGRESS", "1")
	os.Unsetenv("METIS_MCP_WRITE_TOKEN")
	raw := []byte(`{"run":"r","session":"s","objective":"o","actor":"mallory","request_id":"q1"}`)
	if _, err := mcpToolDelegate(raw); err == nil {
		t.Fatal("remote ingress without token must refuse mutation")
	} else if !strings.Contains(err.Error(), "REMOTE_INGRESS") && !strings.Contains(err.Error(), "local-operator trust") {
		t.Fatalf("refusal must name the ingress trust problem, got: %v", err)
	}
}

// P0-3: finalization and admission must serialize. Dozens of task
// creations race one finalization across many iterations; the only
// coherent outcomes are a closed run whose every task was terminal
// at commit, or a refused finalization on an open run. A closed run
// holding a non-terminal racer task proves admission slipped past
// the close commit (check-versus-commit race).
func TestFinalizeRacesTaskAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (concurrency stress); run without -short")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	for iter := 0; iter < 12; iter++ {
		run := fmt.Sprintf("run-race-%d", iter)
		seed, err := createTask(run, "agent", "seed", "done")
		if err != nil {
			t.Fatal(err)
		}
		if err := cancelTask(seed.ID, testReq("orch")); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = finalizeRun(run, testReq("orch"))
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_, _ = createTaskIdem(run, "agent", fmt.Sprintf("racer %d", i), "done", "")
			}
		}()
		wg.Wait()
		closed, err := runClosed(run)
		if err != nil {
			t.Fatal(err)
		}
		if !closed {
			continue
		}
		tasks, err := listTasks(run)
		if err != nil {
			t.Fatal(err)
		}
		for _, tk := range tasks {
			if !terminalTaskStatus(tk.Status) {
				t.Fatalf("iter %d: run %q closed with non-terminal task %s (%s): admission slipped past finalization",
					iter, run, tk.ID, tk.Status)
			}
		}
	}
}

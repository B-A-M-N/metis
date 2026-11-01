package main

// P0-A: doctor is genuinely read-only and reports structured
// conditions with evidence sources.

import (
	"os"
	"path/filepath"
	"testing"
)

// An uncertain execution (working task, open receipt) must show up
// as DEGRADED with the task named — not OK, not silent.
func TestDoctorFlagsUncertainExecution(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "doc1")
	seedOwnedSession(t, "w", "doc1")
	tk, err := createTask("run-doc1", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("worker-loop/w")); err != nil {
		t.Fatal(err)
	}
	if err := recordExecutionStart(tk.ID, got.Attempt, "w", "test-fake", `["sh"]`, 4242, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	rep, err := doctorReport()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range rep.Conditions {
		if c.Status == ConditionDegraded && contains(c.Detail, tk.ID) {
			found = true
		}
		if c.Source == "" {
			t.Fatalf("condition %q must name its evidence source", c.Name)
		}
	}
	if !found {
		t.Fatalf("uncertain execution must be DEGRADED naming %s: %+v", tk.ID, rep.Conditions)
	}
}

// Read-only proof: run doctor against a state dir, then show no
// migration or receipt writes happened — schema probe on a bare
// directory must report BLOCKED/UNKNOWN, never create a database.
func TestDoctorNeverMigrates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TUI_DASH_STATE", dir)
	entriesBefore, _ := os.ReadDir(dir)
	rep, err := doctorReport()
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Conditions) == 0 {
		t.Fatal("doctor must report conditions even on empty state")
	}
	entriesAfter, _ := os.ReadDir(dir)
	if len(entriesAfter) != len(entriesBefore) {
		t.Fatalf("doctor created state files: before=%d after=%d", len(entriesBefore), len(entriesAfter))
	}
	if _, err := os.Stat(filepath.Join(dir, "tasks.db")); !os.IsNotExist(err) {
		t.Fatal("doctor must not create tasks.db")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

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

// Backend-neutral session inventory: a PTY-only fleet must appear
// in doctor output even when the tmux registry is empty. The old
// early return hid the entire PTY scan behind a nonempty registry.
func TestDoctorSessionsPtyOnlyFleetListed(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedPtyRow(t, "pp", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var names []string
	doctorSessions(func(name, status, detail, source string) {
		names = append(names, name)
		if source == "" {
			t.Errorf("condition %q must name its evidence source", name)
		}
	}, db)
	for _, n := range names {
		if n == "pty:pp" {
			return
		}
	}
	t.Fatalf("PTY-only fleet missing from doctor sessions: %v", names)
}

// A native-only fleet (logical_sessions row, empty registry, no PTY
// rows) must appear as native:<name>, not vanish.
func TestDoctorSessionsNativeOnlyListed(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedLogicalRow(t, "nw")
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var names []string
	doctorSessions(func(name, status, detail, source string) {
		names = append(names, name)
	}, db)
	for _, n := range names {
		if n == "native:nw" {
			return
		}
	}
	t.Fatalf("native-only fleet missing from doctor sessions: %v", names)
}

// Mixed fleets report every backend independently.
func TestDoctorSessionsMixedBackends(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "doc-mix")
	seedOwnedSession(t, "tm", "doc-mix")
	seedPtyRow(t, "pp", t.TempDir())
	seedLogicalRow(t, "nw")
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seen := map[string]bool{}
	doctorSessions(func(name, status, detail, source string) {
		seen[name] = true
	}, db)
	for _, want := range []string{"session:tm", "pty:pp", "native:nw"} {
		if !seen[want] {
			t.Fatalf("mixed fleet missing %q: %v", want, seen)
		}
	}
}

// A missing database must surface UNKNOWN for DB-backed sections,
// not a silent OK and not a panic. The file-backed tmux section
// still reports from the registry.
func TestDoctorSessionsMissingDBUnknown(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	var unknowns []string
	doctorSessions(func(name, status, detail, source string) {
		if status == ConditionUnknown {
			unknowns = append(unknowns, name)
		}
	}, nil)
	if len(unknowns) == 0 {
		t.Fatal("nil database must yield UNKNOWN sections, got none")
	}
}

// A broken database (failed queries) must also surface UNKNOWN,
// never silently drop the section.
func TestDoctorSessionsFailedQueryUnknown(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedPtyRow(t, "pp", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // every subsequent query fails
	var unknowns []string
	doctorSessions(func(name, status, detail, source string) {
		if status == ConditionUnknown {
			unknowns = append(unknowns, name)
		}
	}, db)
	if len(unknowns) == 0 {
		t.Fatal("failed queries must yield UNKNOWN sections, got none")
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

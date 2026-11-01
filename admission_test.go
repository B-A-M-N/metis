package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// P0-B failing-first: concurrent policy publishers must serialize
// revision assignment. The current read-then-insert assigns the same
// next revision to racers; losers die on the primary key.
func TestSetTeamPolicyConcurrentPublish(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	project := uniqueTaskKey("proj-pub")
	pol := `{"workers":[{"id":"w-a","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":8},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	const n = 20
	revs := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev, err := setTeamPolicy(project, pol, OperationRequest{Actor: "test", RequestID: newRequestID()})
			revs[i], errs[i] = rev, err
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("publish race did not settle within 60s")
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("publisher %d failed: %v", i, err)
		}
	}
	seen := map[int]bool{}
	for _, r := range revs {
		if r < 1 || r > n || seen[r] {
			t.Fatalf("revisions not exactly 1..%d: %v", n, revs)
		}
		seen[r] = true
	}
}

// seedHarnessSession registers a session with an explicit harness
// label so admission tests can prove eligibility per harness.
func seedHarnessSession(t *testing.T, name, owner, harness string) {
	t.Helper()
	reg := loadRegistry()
	reg[name] = Session{Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: owner, Harness: harness}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
}

// P0-B failing-first: the Team capacity bound must hold at commit.
// Two tasks on one run pinned to max_active_workers=1, dispatched
// concurrently from separate workspaces (so the workspace fence
// cannot decide it), must yield exactly one owner.
func TestDispatchTeamCapacityRace(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "tcap")
	seedHarnessSession(t, "w1", "tcap", "demo")
	seedHarnessSession(t, "w2", "tcap", "demo")

	project := uniqueTaskKey("proj-cap")
	pol := `{"workers":[{"id":"w-a","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":1},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	if _, err := setTeamPolicy(project, pol, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	run := uniqueTaskKey("run-cap")
	if err := pinTeamPolicy(run, project); err != nil {
		t.Fatalf("pin: %v", err)
	}
	a, err := createTask(run, "w1", "cap a", "done")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask(run, "w2", "cap b", "done")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for _, tc := range []struct{ id, session string }{{a.ID, "w1"}, {b.ID, "w2"}} {
		wg.Add(1)
		go func(id, session string) {
			defer wg.Done()
			if err := dispatchTaskWithRequest(id, session, OperationRequest{Actor: "test", RequestID: newRequestID()}); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}(tc.id, tc.session)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("capacity race did not settle within 60s")
	}
	mu.Lock()
	defer mu.Unlock()
	if won != 1 {
		t.Fatalf("team capacity 1 must admit exactly one dispatch, got %d", won)
	}
}

// P0-B failing-first: a harness outside the pinned Team must be
// refused before any delivery bookkeeping, and the task must stay
// pending with no attempt or instruction file behind it.
func TestDispatchTeamEligibilityRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "telig")
	seedHarnessSession(t, "w", "telig", "codex")

	project := uniqueTaskKey("proj-elig")
	pol := `{"workers":[{"id":"w-a","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":4},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	if _, err := setTeamPolicy(project, pol, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	run := uniqueTaskKey("run-elig")
	if err := pinTeamPolicy(run, project); err != nil {
		t.Fatalf("pin: %v", err)
	}
	tk, err := createTask(run, "w", "elig", "done")
	if err != nil {
		t.Fatal(err)
	}
	err = dispatchTaskWithRequest(tk.ID, "w", OperationRequest{Actor: "test", RequestID: newRequestID()})
	if err == nil {
		t.Fatal("dispatch with ineligible harness succeeded, want refusal")
	}
	if !strings.Contains(err.Error(), "team policy") {
		t.Fatalf("error = %q, want team policy refusal", err)
	}
	cur, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != TaskPending {
		t.Fatalf("task status = %q, want %q", cur.Status, TaskPending)
	}
	if cur.Attempt != 0 {
		t.Fatalf("attempt = %d, want 0 (no attempt claimed)", cur.Attempt)
	}
	root, err := sessionRoot("w")
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := os.Stat(filepath.Join(root, "task-"+tk.ID+".md")); !os.IsNotExist(serr) {
		t.Fatalf("instruction file exists for refused dispatch (stat err %v); refusal must precede delivery", serr)
	}
}

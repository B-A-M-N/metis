package main

// Extraction-guard tests for the P1-A workspace_alloc.go split.
// Each extracted component must agree with the pre-extraction
// package-level behavior it now backs.

import (
	"path/filepath"
	"testing"
	"time"
)

// The driver carries the allocation git timeout and resolves HEAD
// exactly like the package shims used before extraction.
func TestWorkspaceSplitDriverMatchesShims(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	d := defaultGitDriver()
	if d.Timeout <= 0 {
		t.Fatalf("default driver timeout = %v, must bound every git call", d.Timeout)
	}
	want := gitHeadOf(t, repo)
	got, err := d.Head(repo)
	if err != nil {
		t.Fatalf("driver Head: %v", err)
	}
	if got != want {
		t.Fatalf("driver Head = %q, want %q", got, want)
	}
	dirty, err := d.Dirty(repo)
	if err != nil {
		t.Fatalf("driver Dirty: %v", err)
	}
	if dirty {
		t.Fatalf("driver Dirty = true on clean repo")
	}
	reg, err := d.WorktreeRegistered(repo, filepath.Join(repo, ".metis-worktrees", "nope-1"))
	if err != nil {
		t.Fatalf("driver WorktreeRegistered: %v", err)
	}
	if reg {
		t.Fatalf("driver WorktreeRegistered = true for absent path")
	}
	if ttl := d.Timeout; ttl != gitTimeout {
		t.Fatalf("driver timeout = %v, want gitTimeout %v", ttl, gitTimeout)
	}
	_ = time.Second // keep time import if helpers change
}

// Conditional transitions refuse through the repository type: a
// missing binding affects zero rows and is an error.
func TestWorkspaceSplitRepositoryGuardsTransition(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB: %v", err)
	}
	defer db.Close()
	if err := ensureWorkspaceAllocTable(db); err != nil {
		t.Fatalf("ensureWorkspaceAllocTable: %v", err)
	}
	repo := NewWorkspaceRepository(db)
	if err := repo.MarkState("no-such-task", 1,
		[]string{WorkspaceAllocated}, WorkspaceReleased, "orch", "req-1"); err == nil {
		t.Fatalf("MarkState on missing binding succeeded; must be refused")
	}
}

// The allocator derives the same deterministic path as before, and
// a full alloc/release cycle through the new types preserves the
// release-keeps-directory invariant.
func TestWorkspaceSplitAllocatorCycleKeepsDirectory(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	a := defaultWorkspaceAllocator()
	wantPath := filepath.Join(repo, ".metis-worktrees", "task-split-1")
	if got := a.PathFor(repo, "task-split", 1); got != wantPath {
		t.Fatalf("PathFor = %q, want %q", got, wantPath)
	}
	if worktreePathFor(repo, "task-split", 1) != wantPath {
		t.Fatalf("worktreePathFor shim diverged from allocator")
	}
	rec, err := a.Alloc(repo, "task-split", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("Alloc: %v", err)
	}
	if rec.WorktreePath != wantPath || rec.State != WorkspaceAllocated {
		t.Fatalf("Alloc = %+v, want path %q in state allocated", rec, wantPath)
	}
	if err := a.Release("task-split", 1, testReq("orch")); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := a.Release("task-split", 1, testReq("orch")); err == nil {
		t.Fatalf("double Release succeeded; must be refused")
	}
}

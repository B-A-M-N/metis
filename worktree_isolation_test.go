package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// P0 worktree isolation, failing-first: allocWorktree must create a
// REAL git worktree (not an empty directory), execution must be
// pinned to the bound path, and the ledger must distinguish live,
// executing, blocked, and retired bindings.

// gitWorktreesOf returns the porcelain worktree list of repo.
func gitWorktreesOf(t *testing.T, repo string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	return string(out)
}

// alloc must produce a real detached worktree at the recorded base
// revision, carrying the committed file — not an empty directory.
func TestWorktreeAllocCreatesRealWorktree(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	want := gitHeadOf(t, repo)
	rec, err := allocWorktree(repo, "task-real", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if !strings.Contains(gitWorktreesOf(t, repo), rec.WorktreePath) {
		t.Fatalf("path %s not registered in git worktree list", rec.WorktreePath)
	}
	got, err := exec.Command("git", "-C", rec.WorktreePath, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(got)) != want {
		t.Fatalf("worktree HEAD = %q (err %v), want %q", strings.TrimSpace(string(got)), err, want)
	}
	branch, err := exec.Command("git", "-C", rec.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(branch)) != "HEAD" {
		t.Fatalf("worktree not detached: %q (err %v)", strings.TrimSpace(string(branch)), err)
	}
	seed, err := os.ReadFile(filepath.Join(rec.WorktreePath, "seed.txt"))
	if err != nil || string(seed) != "seed\n" {
		t.Fatalf("worktree lacks committed file: %q (err %v)", seed, err)
	}
}

// Two bindings from one repo must not observe each other's
// uncommitted writes, and the base checkout stays untouched.
func TestWorktreesIsolateWrites(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	a, err := allocWorktree(repo, "task-iso-a", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	b, err := allocWorktree(repo, "task-iso-b", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if err := os.WriteFile(filepath.Join(a.WorktreePath, "same.txt"), []byte("from-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.WorktreePath, "same.txt"), []byte("from-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ra, _ := os.ReadFile(filepath.Join(a.WorktreePath, "same.txt"))
	rb, _ := os.ReadFile(filepath.Join(b.WorktreePath, "same.txt"))
	if string(ra) != "from-a\n" || string(rb) != "from-b\n" {
		t.Fatalf("worktrees cross-observed: a=%q b=%q", ra, rb)
	}
	if _, err := os.Stat(filepath.Join(repo, "same.txt")); !os.IsNotExist(err) {
		t.Fatalf("base checkout polluted by worktree writes")
	}
	// Each worktree attributes its own revision line.
	for _, rec := range []WorktreeAllocation{a, b} {
		head, err := exec.Command("git", "-C", rec.WorktreePath, "rev-parse", "HEAD").Output()
		if err != nil || strings.TrimSpace(string(head)) != rec.BaseRevision {
			t.Fatalf("worktree %s HEAD drifted: %q vs %q", rec.TaskID, strings.TrimSpace(string(head)), rec.BaseRevision)
		}
	}
}

// Pickup pins execution to the bound path: a task whose binding
// names worktree W cannot go working from any other root, and the
// bound pickup moves the binding allocated -> in_use exactly once.
func TestWorktreePickupScopeEnforced(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "tscope")
	repo := initGitRepo(t, false)

	// Negative: binding exists for attempt 1, but the task executes
	// from a foreign root.
	neg, err := createTask(uniqueTaskKey("run-neg"), "wneg", "neg work", "done")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocWorktree(repo, neg.ID, 1, testReq("orch")); err != nil {
		t.Fatalf("alloc neg: %v", err)
	}
	seedOwnedSession(t, "wneg", "tscope")
	if err := dispatchTaskWithRequest(neg.ID, "wneg", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("dispatch neg: %v", err)
	}
	if err := markTaskWorking(neg.ID, 1, testReq("worker-loop/wneg")); err == nil {
		t.Fatal("pickup from foreign root succeeded despite live worktree binding; must be refused")
	} else if !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("error = %q, want worktree scope refusal", err)
	}

	// Positive: execution root IS the bound worktree.
	pos, err := createTask(uniqueTaskKey("run-pos"), "wpos", "pos work", "done")
	if err != nil {
		t.Fatal(err)
	}
	bind, err := allocWorktree(repo, pos.ID, 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc pos: %v", err)
	}
	reg := loadRegistry()
	reg["wpos"] = Session{Name: "wpos", Tmux: tmuxName("wpos"), Workdir: bind.WorktreePath, Owner: "tscope"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(pos.ID, "wpos", OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("dispatch pos: %v", err)
	}
	if err := markTaskWorking(pos.ID, 1, testReq("worker-loop/wpos")); err != nil {
		t.Fatalf("bound pickup refused: %v", err)
	}
	got, err := worktreeFor(pos.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != WorkspaceInUse {
		t.Fatalf("binding state = %q after pickup, want %q", got.State, WorkspaceInUse)
	}
	// A second pickup for the same attempt is a duplicate claim.
	if err := markTaskWorking(pos.ID, 1, testReq("worker-loop/wpos")); err == nil {
		t.Fatal("duplicate pickup on in_use binding succeeded; must be refused")
	}
}

// A binding whose worktree vanished behind the ledger cannot be
// silently re-bound: re-alloc recreates it when git allows, and
// records uncertain when recovery is blocked.
func TestWorktreeReallocAfterManualRemove(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	rec, err := allocWorktree(repo, "task-gone", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	rm := exec.Command("git", "-C", repo, "worktree", "remove", "--force", rec.WorktreePath)
	if out, err := rm.CombinedOutput(); err != nil {
		t.Fatalf("manual remove: %v\n%s", err, out)
	}
	again, err := allocWorktree(repo, "task-gone", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("re-alloc after manual remove refused: %v", err)
	}
	if again.State != WorkspaceAllocated {
		t.Fatalf("re-alloc state = %q, want allocated", again.State)
	}
	if !strings.Contains(gitWorktreesOf(t, repo), again.WorktreePath) {
		t.Fatal("re-alloc did not recreate a registered worktree")
	}

	// Blocked recovery: an unregistered non-empty directory at the
	// deterministic path can be neither adopted nor deleted — the
	// binding goes uncertain and the operator must resolve it.
	rec2, err := allocWorktree(repo, "task-block", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc block: %v", err)
	}
	rm2 := exec.Command("git", "-C", repo, "worktree", "remove", "--force", rec2.WorktreePath)
	if out, err := rm2.CombinedOutput(); err != nil {
		t.Fatalf("manual remove 2: %v\n%s", err, out)
	}
	if err := os.MkdirAll(rec2.WorktreePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec2.WorktreePath, "stranger.txt"), []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := allocWorktree(repo, "task-block", 1, testReq("orch")); err == nil {
		t.Fatal("re-alloc over foreign content succeeded; must be refused")
	}
	got, err := worktreeFor("task-block", 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != WorkspaceUncertain {
		t.Fatalf("blocked binding state = %q, want %q", got.State, WorkspaceUncertain)
	}
}

package main

// Failing-first adversarial tests for worktree allocation and resource
// ownership (OCO-C). Written before workspace_alloc.go existed; the
// first run must be RED (undefined symbols), then GREEN after the
// implementation lands. Do NOT weaken these assertions: fix the
// production code instead.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// initGitRepo builds a git fixture: git init + one commit so HEAD
// resolves. When dirty is true, an uncommitted change is left behind.
func initGitRepo(t *testing.T, dirty bool) string {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME="+t.TempDir(),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "seed.txt")
	run("commit", "-qm", "seed")
	if dirty {
		if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("dirty\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func gitHeadOf(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// alloc records base rev + owner.
func TestWorktreeAllocRecordsBaseRev(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	want := gitHeadOf(t, repo)
	rec, err := allocWorktree(repo, "task-alpha", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if rec.BaseRevision != want {
		t.Fatalf("base rev = %q, want %q", rec.BaseRevision, want)
	}
	if rec.TaskID != "task-alpha" || rec.Attempt != 1 {
		t.Fatalf("owner = %s/%d, want task-alpha/1", rec.TaskID, rec.Attempt)
	}
	if rec.State != WorkspaceAllocated {
		t.Fatalf("state = %q, want allocated", rec.State)
	}
	if rec.Dirty {
		t.Fatalf("clean repo recorded dirty=true")
	}
	wantPath := filepath.Join(repo, ".metis-worktrees", "task-alpha-1")
	if rec.WorktreePath != wantPath {
		t.Fatalf("path = %q, want %q", rec.WorktreePath, wantPath)
	}
}

// Same owner re-alloc is idempotent: same record, no duplicate row.
func TestWorktreeAllocIdempotentSameOwner(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	first, err := allocWorktree(repo, "task-beta", 2, testReq("orch"))
	if err != nil {
		t.Fatalf("first alloc: %v", err)
	}
	second, err := allocWorktree(repo, "task-beta", 2, testReq("orch"))
	if err != nil {
		t.Fatalf("second alloc: %v", err)
	}
	if first.WorktreePath != second.WorktreePath || first.BaseRevision != second.BaseRevision {
		t.Fatalf("idempotent re-alloc diverged: %+v vs %+v", first, second)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM workspace_allocations WHERE task_id = ? AND attempt_no = ?`, "task-beta", 2); n != 1 {
		t.Fatalf("%d rows for same owner, want 1", n)
	}
}

// Different owner mapping to the same live path is refused.
func TestWorktreeAllocDifferentOwnerSamePathRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	if _, err := allocWorktree(repo, "task/x", 1, testReq("orch")); err != nil {
		t.Fatalf("first alloc: %v", err)
	}
	// "task_x" sanitizes to the same path component as "task/x".
	if _, err := allocWorktree(repo, "task_x", 1, testReq("orch")); err == nil {
		t.Fatalf("different owner over live path succeeded; must be refused")
	}
}

// Non-git dir is refused: never invent a revision.
func TestWorktreeAllocNonGitRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	plain := t.TempDir()
	if _, err := allocWorktree(plain, "task-nogit", 1, testReq("orch")); err == nil {
		t.Fatalf("alloc in non-git dir succeeded; must be refused")
	}
}

// Git repo with unresolvable HEAD (no commits) is refused.
func TestWorktreeAllocNoHeadRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := t.TempDir()
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if _, err := allocWorktree(repo, "task-nohead", 1, testReq("orch")); err == nil {
		t.Fatalf("alloc with unresolvable HEAD succeeded; must be refused")
	}
}

// Release frees the binding; re-alloc by the same owner is allowed.
func TestWorktreeReleaseThenRealloc(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	if _, err := allocWorktree(repo, "task-rel", 1, testReq("orch")); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if err := releaseWorktree("task-rel", 1, testReq("orch")); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, err := worktreeFor("task-rel", 1)
	if err != nil {
		t.Fatalf("lookup after release: %v", err)
	}
	if got.State != WorkspaceReleased {
		t.Fatalf("state after release = %q, want released", got.State)
	}
	if _, err := allocWorktree(repo, "task-rel", 1, testReq("orch")); err != nil {
		t.Fatalf("re-alloc after release: %v", err)
	}
	got, err = worktreeFor("task-rel", 1)
	if err != nil {
		t.Fatalf("lookup after re-alloc: %v", err)
	}
	if got.State != WorkspaceAllocated {
		t.Fatalf("state after re-alloc = %q, want allocated", got.State)
	}
}

// Double release is refused.
func TestWorktreeDoubleReleaseRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	if _, err := allocWorktree(repo, "task-dbl", 1, testReq("orch")); err != nil {
		t.Fatalf("alloc: %v", err)
	}
	if err := releaseWorktree("task-dbl", 1, testReq("orch")); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := releaseWorktree("task-dbl", 1, testReq("orch")); err == nil {
		t.Fatalf("double release succeeded; must be refused")
	}
	if err := releaseWorktree("task-never", 9, testReq("orch")); err == nil {
		t.Fatalf("release of unknown binding succeeded; must be refused")
	}
}

// Release never deletes the directory, even with owner content inside.
func TestWorktreeReleaseNeverDeletesDir(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, false)
	rec, err := allocWorktree(repo, "task-keep", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("alloc: %v", err)
	}
	marker := filepath.Join(rec.WorktreePath, "owner-output.txt")
	if err := os.WriteFile(marker, []byte("owner data\n"), 0o600); err != nil {
		t.Fatalf("seed owner file: %v", err)
	}
	if err := releaseWorktree("task-keep", 1, testReq("orch")); err != nil {
		t.Fatalf("release: %v", err)
	}
	fi, err := os.Stat(rec.WorktreePath)
	if err != nil || !fi.IsDir() {
		t.Fatalf("worktree dir gone after release: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("owner file gone after release: %v", err)
	}
}

// Dirty repo allocates but records dirty=true.
func TestWorktreeAllocDirtyRecorded(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repo := initGitRepo(t, true)
	rec, err := allocWorktree(repo, "task-dirty", 1, testReq("orch"))
	if err != nil {
		t.Fatalf("dirty repo alloc refused: %v", err)
	}
	if !rec.Dirty {
		t.Fatalf("dirty repo recorded dirty=false")
	}
}

// Lookup of unknown binding errors; list is scoped to the repo.
func TestWorktreeLookupAndList(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	repoA := initGitRepo(t, false)
	repoB := initGitRepo(t, false)
	if _, err := allocWorktree(repoA, "task-a", 1, testReq("orch")); err != nil {
		t.Fatalf("alloc a: %v", err)
	}
	if _, err := allocWorktree(repoB, "task-b", 1, testReq("orch")); err != nil {
		t.Fatalf("alloc b: %v", err)
	}
	if _, err := worktreeFor("task-missing", 1); err == nil {
		t.Fatalf("lookup of unknown binding succeeded; must fail")
	}
	list, err := listWorktrees(repoA)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].TaskID != "task-a" {
		t.Fatalf("list(repoA) = %+v, want exactly [task-a]", list)
	}
}

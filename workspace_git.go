package main

// workspace_git.go: git worktree subprocess driver.
//
// Extracted from workspace_alloc.go; behavior unchanged.
//
// GitWorktreeDriver owns every git invocation behind workspace
// allocation: subprocess execution with timeouts, worktree
// registration checks, and checkout verification. Registration —
// not directory existence — stays the source of truth: only git
// knows which paths are live checkouts.
//
// The package-level functions below are the pre-extraction API and
// delegate to the default driver so existing callers are unaffected.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitTimeout bounds every git invocation this module makes: a hung
// git must fail the allocation, never stall the controller loop.
const gitTimeout = 60 * time.Second

// GitWorktreeDriver runs the git subprocess operations behind
// workspace allocation. Timeout bounds every invocation.
type GitWorktreeDriver struct {
	Timeout time.Duration
}

// defaultGitDriver returns the driver matching the pre-extraction
// behavior: every git call bounded by gitTimeout.
func defaultGitDriver() GitWorktreeDriver {
	return GitWorktreeDriver{Timeout: gitTimeout}
}

// Run executes one git command, optionally inside repo.
func (d GitWorktreeDriver) Run(repo string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if repo != "" {
		cmd.Dir = repo
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return out, fmt.Errorf("git %v timed out after %s", args, d.Timeout)
	}
	if err != nil {
		return out, fmt.Errorf("git %v: %v\n%s", args, err, out)
	}
	return out, nil
}

// WorktreeRegistered reports whether repo's worktree list names
// path. Registration — not directory existence — is the source of
// truth: only git knows which paths are live checkouts.
func (d GitWorktreeDriver) WorktreeRegistered(repo, path string) (bool, error) {
	out, err := d.Run(repo, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	clean := filepath.Clean(path)
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "worktree "); ok {
			if filepath.Clean(strings.TrimSpace(rest)) == clean {
				return true, nil
			}
		}
	}
	return false, nil
}

// WorktreeHEAD resolves HEAD inside an existing worktree path.
func (d GitWorktreeDriver) WorktreeHEAD(path string) (string, error) {
	out, err := d.Run("", "-C", path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" {
		return "", fmt.Errorf("empty HEAD in worktree %s; refusing to invent a revision", path)
	}
	return rev, nil
}

// WorktreeAdd creates a detached worktree of rev at path. The
// parent prefix is created (empty dirs are harmless); the worktree
// itself is created by git, never by Mkdir.
func (d GitWorktreeDriver) WorktreeAdd(repo, path, rev string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("worktree add refused: cannot create prefix for %s: %v", path, err)
	}
	_, err := d.Run("", "-C", repo, "worktree", "add", "--detach", path, rev)
	return err
}

// Head resolves HEAD in repo without inventing anything: a non-git
// directory or an unresolvable HEAD (e.g. zero commits) is an error,
// never an empty or placeholder revision.
func (d GitWorktreeDriver) Head(repo string) (string, error) {
	cmd := exec.Command("git", "-C", repo, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("worktree alloc refused: cannot resolve HEAD in %s: %v (not a git checkout or no commits)", repo, err)
	}
	rev := strings.TrimSpace(string(out))
	if rev == "" {
		return "", fmt.Errorf("worktree alloc refused: empty HEAD in %s; refusing to invent a revision", repo)
	}
	return rev, nil
}

// Dirty reports whether the repo has uncommitted changes. Any
// failure to inspect is an error, never a silent clean bill.
func (d GitWorktreeDriver) Dirty(repo string) (bool, error) {
	cmd := exec.Command("git", "-C", repo, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("worktree alloc refused: cannot inspect dirtiness of %s: %v", repo, err)
	}
	return len(strings.TrimSpace(string(out))) > 0, nil
}

// VerifyCheckout confirms git registers path and its HEAD
// equals the recorded base revision.
func (d GitWorktreeDriver) VerifyCheckout(repo, path, rev string) error {
	reg, err := d.WorktreeRegistered(repo, path)
	if err != nil {
		return err
	}
	if !reg {
		return fmt.Errorf("worktree %s is not registered in %s", path, repo)
	}
	head, err := d.WorktreeHEAD(path)
	if err != nil {
		return err
	}
	if head != rev {
		return fmt.Errorf("worktree %s HEAD %s does not match recorded base %s", path, head, rev)
	}
	return nil
}

func runGit(repo string, args ...string) ([]byte, error) {
	return defaultGitDriver().Run(repo, args...)
}

func gitWorktreeRegistered(repo, path string) (bool, error) {
	return defaultGitDriver().WorktreeRegistered(repo, path)
}

func gitWorktreeHEAD(path string) (string, error) {
	return defaultGitDriver().WorktreeHEAD(path)
}

func gitWorktreeAdd(repo, path, rev string) error {
	return defaultGitDriver().WorktreeAdd(repo, path, rev)
}

func gitHead(repo string) (string, error) {
	return defaultGitDriver().Head(repo)
}

func gitDirty(repo string) (bool, error) {
	return defaultGitDriver().Dirty(repo)
}

func verifyWorktreeCheckout(repo, path, rev string) error {
	return defaultGitDriver().VerifyCheckout(repo, path, rev)
}

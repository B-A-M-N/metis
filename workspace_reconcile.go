package main

// workspace_reconcile.go: ledger-vs-git reconciliation.
//
// Extracted from workspace_alloc.go; behavior unchanged.
//
// WorkspaceReconciler compares the recorded ledger identity
// (registered path + base revision) against actual git state and
// classifies uncertainty: anything it cannot reconcile marks the
// binding uncertain and refuses, so the operator resolves it
// instead of the allocator guessing. It never deletes anything.
//
// The package-level functions below are the pre-extraction API and
// delegate to the reconciler so existing callers are unaffected.

import (
	"database/sql"
	"fmt"
)

// WorkspaceReconciler reconciles recorded ledger bindings against
// actual git state. Git performs inspection and recovery checkouts;
// Store persists the uncertain classifications.
type WorkspaceReconciler struct {
	Git   GitWorktreeDriver
	Store WorkspaceRepository
}

// NewWorkspaceReconciler binds reconciliation to one DB handle and
// one git driver.
func NewWorkspaceReconciler(db *sql.DB, git GitWorktreeDriver) WorkspaceReconciler {
	return WorkspaceReconciler{Git: git, Store: NewWorkspaceRepository(db)}
}

// defaultWorkspaceReconciler matches the pre-extraction behavior on
// a fresh handle.
func defaultWorkspaceReconciler(db *sql.DB) WorkspaceReconciler {
	return NewWorkspaceReconciler(db, defaultGitDriver())
}

// VerifyOrRecover reconciles a live ledger row with git. A
// registered checkout at the recorded revision is healthy. A
// missing checkout is recreated (unless live execution holds the
// binding — never rebuild under a running worker). Anything that
// blocks recovery marks the binding uncertain and refuses, so the
// operator resolves it instead of the allocator guessing.
func (c WorkspaceReconciler) VerifyOrRecover(rec WorktreeAllocation, req OperationRequest, liveExecution bool) error {
	if err := c.Git.VerifyCheckout(rec.RepoRoot, rec.WorktreePath, rec.BaseRevision); err == nil {
		return nil
	}
	reg, rerr := c.Git.WorktreeRegistered(rec.RepoRoot, rec.WorktreePath)
	if rerr != nil {
		return rerr
	}
	if liveExecution {
		_ = c.Store.MarkState(rec.TaskID, rec.Attempt, []string{WorkspaceAllocated, WorkspaceInUse}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("worktree binding for %s attempt %d diverged during live execution; recorded uncertain, operator must resolve %s",
			rec.TaskID, rec.Attempt, rec.WorktreePath)
	}
	if reg {
		// Registered but wrong revision: re-base is a content
		// change, not a recovery — refuse and mark uncertain.
		_ = c.Store.MarkState(rec.TaskID, rec.Attempt, []string{WorkspaceAllocated, WorkspaceUncertain}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("worktree %s registered at unexpected revision; binding for %s attempt %d recorded uncertain",
			rec.WorktreePath, rec.TaskID, rec.Attempt)
	}
	// Unregistered: recreate the checkout. A blocked path (foreign
	// content git will not touch) marks uncertain instead of
	// deleting anything.
	if err := c.Git.WorktreeAdd(rec.RepoRoot, rec.WorktreePath, rec.BaseRevision); err != nil {
		_ = c.Store.MarkState(rec.TaskID, rec.Attempt, []string{WorkspaceAllocated, WorkspaceUncertain}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("worktree %s missing and recreation blocked: %v (binding uncertain; operator must resolve)", rec.WorktreePath, err)
	}
	if err := c.Git.VerifyCheckout(rec.RepoRoot, rec.WorktreePath, rec.BaseRevision); err != nil {
		_ = c.Store.MarkState(rec.TaskID, rec.Attempt, []string{WorkspaceAllocated, WorkspaceUncertain}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("recreated worktree unverifiable: %v (binding uncertain)", err)
	}
	return nil
}

// RebaseRetained moves a retained checkout to a fresh base
// revision during re-bind after release. Untracked owner files do
// not block a detach; conflicting tracked changes refuse (and mark
// uncertain) instead of being discarded.
func (c WorkspaceReconciler) RebaseRetained(repo, path, rev, taskID string, attempt int, req OperationRequest) error {
	reg, err := c.Git.WorktreeRegistered(repo, path)
	if err != nil {
		return err
	}
	if !reg {
		// Retained binding whose checkout vanished: recreate it.
		if err := c.Git.WorktreeAdd(repo, path, rev); err != nil {
			_ = c.Store.MarkState(taskID, attempt, []string{WorkspaceReleased}, WorkspaceUncertain, req.Actor, req.RequestID)
			return fmt.Errorf("released worktree %s missing and recreation blocked: %v (binding uncertain)", path, err)
		}
		return c.Git.VerifyCheckout(repo, path, rev)
	}
	head, err := c.Git.WorktreeHEAD(path)
	if err != nil {
		_ = c.Store.MarkState(taskID, attempt, []string{WorkspaceReleased}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("retained worktree %s unreadable: %v (binding uncertain)", path, err)
	}
	if head == rev {
		return nil
	}
	if _, err := c.Git.Run("", "-C", path, "checkout", "--detach", rev); err != nil {
		_ = c.Store.MarkState(taskID, attempt, []string{WorkspaceReleased}, WorkspaceUncertain, req.Actor, req.RequestID)
		return fmt.Errorf("retained worktree %s cannot re-base to %s (owner changes?): %v (binding uncertain; operator must resolve)", path, rev, err)
	}
	return c.Git.VerifyCheckout(repo, path, rev)
}

func verifyOrRecoverWorktree(db *sql.DB, rec WorktreeAllocation, req OperationRequest, liveExecution bool) error {
	return defaultWorkspaceReconciler(db).VerifyOrRecover(rec, req, liveExecution)
}

func rebaseRetainedWorktree(db *sql.DB, repo, path, rev, taskID string, attempt int, req OperationRequest) error {
	return defaultWorkspaceReconciler(db).RebaseRetained(repo, path, rev, taskID, attempt, req)
}

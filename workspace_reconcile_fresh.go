package main

// workspace_reconcile_fresh.go: fresh-binding record-vs-git disagreements.
//
// The create-vs-register gap: `git worktree add` and the ledger
// insert are not atomic, so a fresh binding can disagree with git in
// two ways. Both route through WorkspaceReconciler here, next to the
// other disagreement classifiers, instead of being decided inline by
// the allocator:
//
//   - add failed but git registers the path: someone (a concurrent
//     alloc, or a stale registration) owns it. The directory is
//     neither deleted nor adopted — the attempt is refused and the
//     holder keeps the path.
//   - add succeeded but the checkout is unverifiable (e.g. HEAD
//     advanced mid-alloc): the binding is recorded uncertain as an
//     explicit recoverable intent, so a retry converges on the row
//     instead of adopting a half-built tree, and the operator (never
//     the allocator) resolves it.
//
// Nothing here deletes anything: unregistered foreign content blocks
// recovery into uncertain instead of being removed.

import (
	"fmt"
)

// ClassifyAddFailure reconciles a failed `git worktree add` with the
// registration that survived it. A registered path belongs to someone
// — the pre-check should have caught it, so this is a race — and the
// attempt is refused without touching the directory. Anything else is
// a transient git failure the caller surfaces for a retry.
func (c WorkspaceReconciler) ClassifyAddFailure(repo, path string, addErr error) error {
	reg, rerr := c.Git.WorktreeRegistered(repo, path)
	if rerr == nil && reg {
		return fmt.Errorf("worktree alloc lost a race: %s became a registered worktree during add: %v", path, addErr)
	}
	return addErr
}

// RecordUnverifiable persists an unverifiable fresh checkout as an
// explicit uncertain binding and refuses. The directory is left for
// the operator, never auto-removed; a later attempt with the same key
// converges on this row through the uncertain path instead of
// adopting the directory merely because it exists. If even the
// uncertain row cannot be recorded, the verification error is
// returned so the failure stays visible.
func (c WorkspaceReconciler) RecordUnverifiable(rec WorktreeAllocation, verr error) error {
	rec.State = WorkspaceUncertain
	if ierr := c.Store.Insert(rec); ierr != nil {
		return verr
	}
	return fmt.Errorf("worktree alloc unverifiable: %v (binding recorded uncertain; operator must resolve %s)", verr, rec.WorktreePath)
}

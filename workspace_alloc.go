package main

// workspace_alloc.go: worktree allocation decisions (OCO-C).
//
// ALLOCATION ONLY. This module binds (repo, task, attempt) to a
// deterministic worktree path and records the base revision in the
// workspace_allocations ledger. It NEVER merges, publishes, pushes,
// or commits: promotion and integration stay an explicitly authorized
// operator action outside this module.
//
// RELEASE NEVER DELETES THE DIRECTORY. releaseWorktree frees only
// the ledger binding (allocated -> released). The worktree directory
// and everything in it survive release unconditionally, so owner data
// can never be destroyed by a ledger transition. Cleanup is an
// explicit operator action.
//
// Dirty-repo allocation is allowed but recorded: a repo with
// uncommitted changes allocates normally with dirty=true so reviewers
// can see the worktree started from a dirty base.
//
// Structure (P1-A split): git subprocess work lives in
// GitWorktreeDriver (workspace_git.go), ledger persistence in
// WorkspaceRepository (workspace_repo.go), and ledger-vs-git
// reconciliation in WorkspaceReconciler (workspace_reconcile.go).
// This file keeps allocation decisions only: deterministic paths,
// ownership binding, and claim/release authorization. Sharing is
// never implicit — authorization is enforced at dispatch/claim time,
// never inferred from directory availability.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// worktreePrefix is the only directory this module ever creates inside
// a repo. Release never removes anything under it.
const worktreePrefix = ".metis-worktrees"

// WorkspaceAllocator binds (repo, task, attempt) to deterministic
// worktree paths and records base revisions. It makes no acceptance
// decisions and merges nothing.
type WorkspaceAllocator struct {
	Git GitWorktreeDriver
}

// defaultWorkspaceAllocator matches the pre-extraction behavior.
func defaultWorkspaceAllocator() WorkspaceAllocator {
	return WorkspaceAllocator{Git: defaultGitDriver()}
}

// sanitizeWorktreeComponent maps a task id to one safe path component.
// Anything outside [A-Za-z0-9_-] becomes '_', so owners can never
// escape the module prefix with separators or dot segments. The
// mapping is deterministic, which also makes collision refusal
// testable: two owners that sanitize alike share one live path.
func sanitizeWorktreeComponent(taskID string) string {
	var b strings.Builder
	for _, r := range taskID {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_"
	}
	return b.String()
}

// PathFor derives the deterministic binding path for
// (canonical repo root, task, attempt). The repo root must already be
// canonical (see canonicalRoot) so aliased spellings map alike.
func (a WorkspaceAllocator) PathFor(canonRepo, taskID string, attempt int) string {
	return filepath.Join(canonRepo, worktreePrefix, fmt.Sprintf("%s-%d", sanitizeWorktreeComponent(taskID), attempt))
}

// worktreePathFor derives the deterministic binding path for
// (canonical repo root, task, attempt). The repo root must already be
// canonical (see canonicalRoot) so aliased spellings map alike.
func worktreePathFor(canonRepo, taskID string, attempt int) string {
	return defaultWorkspaceAllocator().PathFor(canonRepo, taskID, attempt)
}

// allocWorktree binds (taskID, attempt) to the deterministic worktree
// path under repoRoot, creates a real detached `git worktree add`
// checkout of the recorded base revision, and verifies git reports
// it before returning. Same owner re-alloc is idempotent and
// returns the existing record (recreating the checkout when git
// allows). A different live owner of the same path is refused.
// The ledger row is written only after git succeeds — except for
// the uncertain case below — so a crash before verification leaves
// no row and a retry recovers cleanly instead of adopting a
// half-built tree.
func allocWorktree(repoRoot, taskID string, attempt int, req OperationRequest) (WorktreeAllocation, error) {
	return defaultWorkspaceAllocator().Alloc(repoRoot, taskID, attempt, req)
}

// Alloc implements allocWorktree.
func (a WorkspaceAllocator) Alloc(repoRoot, taskID string, attempt int, req OperationRequest) (WorktreeAllocation, error) {
	if err := requireRequest(req); err != nil {
		return WorktreeAllocation{}, err
	}
	if strings.TrimSpace(taskID) == "" {
		return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: task id must not be empty")
	}
	if attempt < 0 {
		return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: attempt must be non-negative")
	}
	canon, err := canonicalRoot(repoRoot)
	if err != nil {
		return WorktreeAllocation{}, err
	}
	if st, serr := os.Stat(canon); serr != nil || !st.IsDir() {
		return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: workspace root %q is not an accessible directory", repoRoot)
	}
	path := a.PathFor(canon, taskID, attempt)
	db, err := openTasksDB()
	if err != nil {
		return WorktreeAllocation{}, err
	}
	defer db.Close()
	if err := ensureWorkspaceAllocTable(db); err != nil {
		return WorktreeAllocation{}, err
	}
	// Authoritative record access goes through the repository;
	// record-vs-git disagreements go through the reconciler. Below
	// is orchestration only: deterministic path selection plus git,
	// store, and reconciler calls.
	store := NewWorkspaceRepository(db)
	reconcile := NewWorkspaceReconciler(db, a.Git)
	// Same-owner row for this exact (task, attempt) decides before
	// any git inspection. A live row is verified against git (a
	// checkout removed behind the ledger is recreated when git
	// allows, never silently adopted); a released row falls through
	// and re-binds below.
	rec, qerr := store.Find(taskID, attempt)
	if qerr == nil {
		switch rec.State {
		case WorkspaceAllocated, WorkspaceInUse:
			if rec.WorktreePath != path || rec.RepoRoot != canon {
				return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: binding for %s attempt %d names %s, deterministic path is %s; ledger divergence, refusing",
					taskID, attempt, rec.WorktreePath, path)
			}
			if verr := reconcile.VerifyOrRecover(rec, req, rec.State == WorkspaceInUse); verr != nil {
				return WorktreeAllocation{}, verr
			}
			return worktreeFor(taskID, attempt)
		case WorkspaceUncertain:
			if verr := reconcile.VerifyOrRecover(rec, req, false); verr != nil {
				return WorktreeAllocation{}, verr
			}
			return worktreeFor(taskID, attempt)
		case WorkspaceReleased:
			// Fall through and re-bind the same deterministic path
			// with a fresh revision.
		default:
			return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: binding for %s attempt %d is in unknown state %q",
				taskID, attempt, rec.State)
		}
	} else if qerr != sql.ErrNoRows {
		return WorktreeAllocation{}, qerr
	}
	// A different live or blocked owner of the same path refuses:
	// sharing is never implicit, and an uncertain binding holds its
	// path until an operator resolves it.
	if holder, herr := store.FindLiveByPath(path, taskID, attempt); herr == nil {
		return WorktreeAllocation{}, fmt.Errorf("worktree alloc refused: path %s is %s for %s attempt %d; refusing double-allocation to %s attempt %d",
			path, holder.State, holder.TaskID, holder.Attempt, taskID, attempt)
	} else if herr != sql.ErrNoRows {
		return WorktreeAllocation{}, herr
	}
	// Free: proceed.
	rev, err := a.Git.Head(canon)
	if err != nil {
		return WorktreeAllocation{}, err
	}
	isDirty, err := a.Git.Dirty(canon)
	if err != nil {
		return WorktreeAllocation{}, err
	}
	now := nowUTC()
	if qerr == nil {
		// Re-bind after release: same key, fresh revision. The
		// retained checkout is verified, recreated when it
		// vanished, or re-based to the new base when it drifted;
		// owner content that blocks recovery refuses instead of
		// being discarded.
		if err := reconcile.RebaseRetained(canon, path, rev, taskID, attempt, req); err != nil {
			return WorktreeAllocation{}, err
		}
		if err := store.Rebind(WorktreeAllocation{
			TaskID: taskID, Attempt: attempt, RepoRoot: canon, WorktreePath: path,
			BaseRevision: rev, Dirty: isDirty, State: WorkspaceAllocated,
			Actor: req.Actor, RequestID: req.RequestID, Updated: now,
		}); err != nil {
			return WorktreeAllocation{}, err
		}
		return worktreeFor(taskID, attempt)
	}
	// Fresh binding: git owns checkout creation. On failure the
	// path is inspected but never touched: a registered path belongs
	// to someone (the live-owner check above should have caught it,
	// so this is a race — surface it); anything else is a transient
	// git failure and a retry recovers.
	if err := a.Git.WorktreeAdd(canon, path, rev); err != nil {
		return WorktreeAllocation{}, reconcile.ClassifyAddFailure(canon, path, err)
	}
	if verr := a.Git.VerifyCheckout(canon, path, rev); verr != nil {
		// The checkout exists but does not match the recorded base
		// (e.g. HEAD advanced mid-alloc). The disagreement is
		// recorded uncertain — the directory is left for the
		// operator, never auto-removed.
		return WorktreeAllocation{}, reconcile.RecordUnverifiable(WorktreeAllocation{
			TaskID: taskID, Attempt: attempt, RepoRoot: canon, WorktreePath: path,
			BaseRevision: rev, Dirty: isDirty, State: WorkspaceUncertain,
			Actor: req.Actor, RequestID: req.RequestID, Created: now, Updated: now,
		}, verr)
	}
	rec = WorktreeAllocation{
		TaskID: taskID, Attempt: attempt, RepoRoot: canon, WorktreePath: path,
		BaseRevision: rev, Dirty: isDirty, State: WorkspaceAllocated,
		Actor: req.Actor, RequestID: req.RequestID, Created: now, Updated: now,
	}
	if err := store.Insert(rec); err != nil {
		// Lost a concurrent alloc with the same key: the winner's
		// row is authoritative. Same-owner convergence returns it;
		// anything else surfaces the insert error. The winner's
		// checkout already exists (rows postdate git success), so
		// convergence adopts nothing half-built.
		if winner, werr := store.Find(taskID, attempt); werr == nil {
			if (winner.State == WorkspaceAllocated || winner.State == WorkspaceInUse) && winner.WorktreePath == path && winner.RepoRoot == canon {
				return winner, nil
			}
		}
		return WorktreeAllocation{}, err
	}
	return rec, nil
}

// releaseWorktree retires the caller's binding: the row must be
// live (allocated or in_use) — or uncertain, giving the operator an
// escape hatch that retires the ledger claim while keeping the
// directory. Double release and unknown bindings are refused. The
// directory is never touched.
func releaseWorktree(taskID string, attempt int, req OperationRequest) error {
	return defaultWorkspaceAllocator().Release(taskID, attempt, req)
}

// Release implements releaseWorktree. The directory is never touched:
// only the ledger binding moves to released.
func (a WorkspaceAllocator) Release(taskID string, attempt int, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureWorkspaceAllocTable(db); err != nil {
		return err
	}
	// Ledger transition only, through the repository: the directory
	// is never touched, so owner data survives unconditionally.
	return NewWorkspaceRepository(db).ReleaseBinding(taskID, attempt, req.Actor, req.RequestID)
}

// claimWorktreeUse moves a live allocated binding to in_use for the
// exact execution root: the pickup that first executes under this
// attempt owns the checkout. A foreign root is cross-worktree
// execution and is refused; a duplicate claim, an uncertain
// binding, and a retired binding are refused. Absence of a binding
// is not an error — isolation is opt-in per attempt.
func claimWorktreeUse(db sqlExecer, taskID string, attempt int, execRoot, actor, reqID string) error {
	return defaultWorkspaceAllocator().Claim(db, taskID, attempt, execRoot, actor, reqID)
}

// Claim implements claimWorktreeUse: authorization at pickup time,
// never inferred from directory availability. The record read and
// transition run on the caller's exec through the repository, so the
// pickup claim stays inside the lifecycle transaction.
func (a WorkspaceAllocator) Claim(db sqlExecer, taskID string, attempt int, execRoot, actor, reqID string) error {
	return WorkspaceRepository{}.ClaimBinding(db, taskID, attempt, execRoot, actor, reqID)
}

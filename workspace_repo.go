package main

// workspace_repo.go: workspace allocation ledger persistence.
//
// Extracted from workspace_alloc.go; behavior unchanged.
//
// WorkspaceRepository owns the workspace_allocations ledger: table
// migration, row reads, and conditional state transitions. Every
// transition carries an exact from-state guard; a lost race affects
// zero rows and is an error, never a silent overwrite.
//
// The package-level functions below are the pre-extraction API and
// delegate to the repository so existing callers are unaffected.

import (
	"database/sql"
	"fmt"
	"path/filepath"
)

// Worktree binding states. Stored literally in the ledger.
//
//   - allocated: live binding backed by a verified registered git
//     worktree. Execution has not claimed it yet.
//   - in_use: one execution claimed the binding at pickup; a second
//     pickup for the same attempt is refused.
//   - uncertain: the ledger and git disagree (or recovery is
//     blocked) and an operator must resolve it. Nothing proceeds
//     from this state automatically.
//   - released: retired binding. The directory survives; re-alloc
//     by the same owner re-binds (and re-bases) it.
const (
	WorkspaceAllocated = "allocated"
	WorkspaceInUse     = "in_use"
	WorkspaceUncertain = "uncertain"
	WorkspaceReleased  = "released"
)

// WorktreeAllocation is one ledger binding of (task, attempt) to a
// deterministic worktree path inside a repo checkout.
type WorktreeAllocation struct {
	TaskID       string
	Attempt      int
	RepoRoot     string
	WorktreePath string
	BaseRevision string
	Dirty        bool
	State        string
	Actor        string
	RequestID    string
	Created      string
	Updated      string
}

const worktreeAllocationColumns = `task_id, attempt_no, repo_root, worktree_path, base_revision, dirty, state, actor, request_id, created, updated`

// WorkspaceRepository persists workspace_allocations bindings on one
// DB handle: row reads, listings, and conditional transitions.
type WorkspaceRepository struct {
	db *sql.DB
}

// NewWorkspaceRepository binds the ledger to db.
func NewWorkspaceRepository(db *sql.DB) WorkspaceRepository {
	return WorkspaceRepository{db: db}
}

// MarkState moves one binding between states with an exact
// from-state guard; a lost race affects zero rows and is an error,
// never a silent overwrite.
func (r WorkspaceRepository) MarkState(taskID string, attempt int, from []string, to, actor, reqID string) error {
	return markWorktreeState(r.db, taskID, attempt, from, to, actor, reqID)
}

// Find reads one ledger row in canonical column order.
func (r WorkspaceRepository) Find(taskID string, attempt int) (WorktreeAllocation, error) {
	var rec WorktreeAllocation
	var dirty int
	err := r.db.QueryRow(`SELECT `+worktreeAllocationColumns+` FROM workspace_allocations WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&rec.TaskID, &rec.Attempt, &rec.RepoRoot, &rec.WorktreePath,
		&rec.BaseRevision, &dirty, &rec.State, &rec.Actor, &rec.RequestID, &rec.Created, &rec.Updated)
	if err != nil {
		return WorktreeAllocation{}, err
	}
	rec.Dirty = dirty != 0
	return rec, nil
}

// List returns every ledger binding scoped to a canonical repo root
// in any state, ordered by task then attempt. Callers filter on
// State for live-only views.
func (r WorkspaceRepository) List(canonRepo string) ([]WorktreeAllocation, error) {
	rows, err := r.db.Query(`SELECT `+worktreeAllocationColumns+` FROM workspace_allocations WHERE repo_root = ? ORDER BY task_id, attempt_no`, canonRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorktreeAllocation
	for rows.Next() {
		rec, serr := scanWorktreeAllocation(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ensureWorkspaceAllocTable creates the ledger table on every handle
// that touches it. openTasksDB lives in task_schema.go (out of scope),
// so this module migrates its own table instead of the shared open.
func ensureWorkspaceAllocTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS workspace_allocations (
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		repo_root TEXT NOT NULL DEFAULT '',
		worktree_path TEXT NOT NULL DEFAULT '',
		base_revision TEXT NOT NULL DEFAULT '',
		dirty INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT 'allocated',
		actor TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL,
		PRIMARY KEY (task_id, attempt_no)
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS workspace_allocations_repo ON workspace_allocations(repo_root)`)
	if err != nil {
		return err
	}
	return ensureColumn(db, "workspace_allocations", "dirty", `INTEGER NOT NULL DEFAULT 0`)
}

// scanWorktreeAllocation reads one ledger row in canonical column order.
func scanWorktreeAllocation(rows *sql.Rows) (WorktreeAllocation, error) {
	var rec WorktreeAllocation
	var dirty int
	err := rows.Scan(&rec.TaskID, &rec.Attempt, &rec.RepoRoot, &rec.WorktreePath,
		&rec.BaseRevision, &dirty, &rec.State, &rec.Actor, &rec.RequestID,
		&rec.Created, &rec.Updated)
	rec.Dirty = dirty != 0
	return rec, err
}

// markWorktreeState moves one binding between states with an exact
// from-state guard; a lost race affects zero rows and is an error,
// never a silent overwrite. It takes any sqlExecer so transitions
// run inside the caller's transaction as well as on a direct handle.
func markWorktreeState(db sqlExecer, taskID string, attempt int, from []string, to, actor, reqID string) error {
	q := `UPDATE workspace_allocations SET state = ?, actor = ?, request_id = ?, updated = ? WHERE task_id = ? AND attempt_no = ? AND state IN (`
	args := []any{to, actor, reqID, nowUTC(), taskID, attempt}
	for i, s := range from {
		if i > 0 {
			q += `, `
		}
		q += `?`
		args = append(args, s)
	}
	q += `)`
	res, err := db.Exec(q, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("worktree state refused: binding for %s attempt %d is not in %v", taskID, attempt, from)
	}
	return nil
}

// worktreeFor returns the ledger binding for (taskID, attempt) in any
// state. Unknown bindings are an error.
func worktreeFor(taskID string, attempt int) (WorktreeAllocation, error) {
	db, err := openTasksDB()
	if err != nil {
		return WorktreeAllocation{}, err
	}
	defer db.Close()
	if err := ensureWorkspaceAllocTable(db); err != nil {
		return WorktreeAllocation{}, err
	}
	rec, err := NewWorkspaceRepository(db).Find(taskID, attempt)
	if err == sql.ErrNoRows {
		return WorktreeAllocation{}, fmt.Errorf("no worktree binding for %s attempt %d", taskID, attempt)
	}
	return rec, err
}

// listWorktrees returns every ledger binding scoped to repoRoot in any
// state, ordered by task then attempt. Callers filter on State for
// live-only views.
func listWorktrees(repoRoot string) ([]WorktreeAllocation, error) {
	canon, err := canonicalRoot(repoRoot)
	if err != nil {
		return nil, err
	}
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureWorkspaceAllocTable(db); err != nil {
		return nil, err
	}
	return NewWorkspaceRepository(db).List(canon)
}

// FindLiveByPath returns the live or blocked binding holding path,
// excluding the caller's own (task, attempt) key. A hit means the
// path is taken and the caller must refuse double-allocation;
// sql.ErrNoRows means the path is free. Sharing is never implicit,
// and an uncertain binding holds its path until an operator resolves
// it, so all three non-released states block.
func (r WorkspaceRepository) FindLiveByPath(path, excludeTask string, excludeAttempt int) (WorktreeAllocation, error) {
	var rec WorktreeAllocation
	var dirty int
	err := r.db.QueryRow(`SELECT `+worktreeAllocationColumns+` FROM workspace_allocations WHERE worktree_path = ? AND state IN (?, ?, ?) AND NOT (task_id = ? AND attempt_no = ?)`,
		path, WorkspaceAllocated, WorkspaceInUse, WorkspaceUncertain, excludeTask, excludeAttempt).Scan(
		&rec.TaskID, &rec.Attempt, &rec.RepoRoot, &rec.WorktreePath,
		&rec.BaseRevision, &dirty, &rec.State, &rec.Actor, &rec.RequestID, &rec.Created, &rec.Updated)
	if err != nil {
		return WorktreeAllocation{}, err
	}
	rec.Dirty = dirty != 0
	return rec, nil
}

// Insert records a fresh binding. Rows postdate git success: the
// checkout already exists when the row lands, so a UNIQUE conflict
// means a concurrent allocator won the same key and the caller
// converges on the winner via Find instead of overwriting it.
func (r WorkspaceRepository) Insert(rec WorktreeAllocation) error {
	_, err := r.db.Exec(`INSERT INTO workspace_allocations (`+worktreeAllocationColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.TaskID, rec.Attempt, rec.RepoRoot, rec.WorktreePath, rec.BaseRevision,
		boolToInt(rec.Dirty), rec.State, rec.Actor, rec.RequestID, rec.Created, rec.Updated)
	return err
}

// Rebind moves a released binding back to live with a fresh base
// revision at the same deterministic path. The retained checkout is
// reconciled (recreated or re-based) before this runs; this only
// swaps the ledger identity, never owner content.
func (r WorkspaceRepository) Rebind(rec WorktreeAllocation) error {
	_, err := r.db.Exec(`UPDATE workspace_allocations SET repo_root = ?, worktree_path = ?, base_revision = ?, dirty = ?, state = ?, actor = ?, request_id = ?, updated = ? WHERE task_id = ? AND attempt_no = ?`,
		rec.RepoRoot, rec.WorktreePath, rec.BaseRevision, boolToInt(rec.Dirty), rec.State,
		rec.Actor, rec.RequestID, rec.Updated, rec.TaskID, rec.Attempt)
	return err
}

// ReleaseBinding retires a live binding: only the ledger row moves
// to released. The directory is never touched — owner data survives
// unconditionally and cleanup stays an explicit operator action.
// Double release and unknown bindings are refused.
func (r WorkspaceRepository) ReleaseBinding(taskID string, attempt int, actor, reqID string) error {
	res, err := r.db.Exec(`UPDATE workspace_allocations SET state = ?, actor = ?, request_id = ?, updated = ? WHERE task_id = ? AND attempt_no = ? AND state IN (?, ?, ?)`,
		WorkspaceReleased, actor, reqID, nowUTC(), taskID, attempt,
		WorkspaceAllocated, WorkspaceInUse, WorkspaceUncertain)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var state string
		if serr := r.db.QueryRow(`SELECT state FROM workspace_allocations WHERE task_id = ? AND attempt_no = ?`, taskID, attempt).Scan(&state); serr == nil {
			return fmt.Errorf("worktree release refused: binding for %s attempt %d is %s, not live (double release?)", taskID, attempt, state)
		}
		return fmt.Errorf("worktree release refused: no binding for %s attempt %d", taskID, attempt)
	}
	return nil
}

// ClaimBinding moves a live allocated binding to in_use for the exact
// execution root: the pickup that first executes under this attempt
// owns the checkout. A foreign root is cross-worktree execution and
// is refused; a duplicate claim, an uncertain binding, and a retired
// binding are refused. Absence of a binding is not an error —
// isolation is opt-in per attempt. Authorization runs at pickup time,
// never inferred from directory availability.
//
// It runs on the caller's exec (a *sql.Tx in the pickup path) so the
// claim stays inside the lifecycle transaction; the receiver only
// scopes the method to the ledger.
func (r WorkspaceRepository) ClaimBinding(exec sqlExecer, taskID string, attempt int, execRoot, actor, reqID string) error {
	var state, path string
	err := exec.QueryRow(`SELECT state, worktree_path FROM workspace_allocations WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&state, &path)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	switch state {
	case WorkspaceReleased:
		return nil
	case WorkspaceUncertain:
		return fmt.Errorf("worktree scope refused: binding for %s attempt %d is uncertain; operator must resolve %s first",
			taskID, attempt, path)
	case WorkspaceInUse:
		return fmt.Errorf("worktree scope refused: binding for %s attempt %d is already in use at %s (duplicate pickup?)",
			taskID, attempt, path)
	case WorkspaceAllocated:
		// Claim below.
	default:
		return fmt.Errorf("worktree scope refused: binding for %s attempt %d is in unknown state %q", taskID, attempt, state)
	}
	if filepath.Clean(execRoot) != filepath.Clean(path) {
		return fmt.Errorf("worktree scope refused: task %s attempt %d executes in %s, bound to worktree %s; refusing cross-worktree execution",
			taskID, attempt, execRoot, path)
	}
	return markWorktreeState(exec, taskID, attempt, []string{WorkspaceAllocated}, WorkspaceInUse, actor, reqID)
}

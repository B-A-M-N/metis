package main

// Authority tests for the workspace separation: allocation-record
// SQL lives behind WorkspaceRepository, and fresh-binding
// record-vs-git disagreements route through WorkspaceReconciler.
// These cover the repository methods and gap handlers directly, with
// no git fixtures except where registration must be inspected.

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func openAuthorityTestDB(t *testing.T) *sql.DB {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := ensureWorkspaceAllocTable(db); err != nil {
		t.Fatalf("ensureWorkspaceAllocTable: %v", err)
	}
	return db
}

func authorityRec(taskID string, path string, state string) WorktreeAllocation {
	now := nowUTC()
	return WorktreeAllocation{
		TaskID: taskID, Attempt: 1, RepoRoot: "/repo", WorktreePath: path,
		BaseRevision: "rev-1", State: state,
		Actor: "orch", RequestID: "req-1", Created: now, Updated: now,
	}
}

// Insert/Find round-trip preserves the record.
func TestWorkspaceRepositoryInsertFind(t *testing.T) {
	db := openAuthorityTestDB(t)
	store := NewWorkspaceRepository(db)
	want := authorityRec("auth-ins", "/repo/.metis-worktrees/auth-ins-1", WorkspaceAllocated)
	if err := store.Insert(want); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	got, err := store.Find("auth-ins", 1)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != want {
		t.Fatalf("Find = %+v, want %+v", got, want)
	}
	if _, err := store.Find("auth-missing", 1); err != sql.ErrNoRows {
		t.Fatalf("Find missing = %v, want sql.ErrNoRows", err)
	}
}

// A live holder blocks the path; the owner's own key and released
// rows do not.
func TestWorkspaceRepositoryFindLiveByPath(t *testing.T) {
	db := openAuthorityTestDB(t)
	store := NewWorkspaceRepository(db)
	path := "/repo/.metis-worktrees/shared-1"
	if err := store.Insert(authorityRec("auth-a", path, WorkspaceAllocated)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	holder, err := store.FindLiveByPath(path, "auth-b", 1)
	if err != nil {
		t.Fatalf("FindLiveByPath: %v", err)
	}
	if holder.TaskID != "auth-a" || holder.State != WorkspaceAllocated {
		t.Fatalf("holder = %+v, want auth-a/allocated", holder)
	}
	// Own key excluded: free from the owner's perspective.
	if _, err := store.FindLiveByPath(path, "auth-a", 1); err != sql.ErrNoRows {
		t.Fatalf("own-key FindLiveByPath = %v, want sql.ErrNoRows", err)
	}
	// Released rows do not hold the path.
	if err := store.ReleaseBinding("auth-a", 1, "orch", "req-2"); err != nil {
		t.Fatalf("ReleaseBinding: %v", err)
	}
	if _, err := store.FindLiveByPath(path, "auth-b", 1); err != sql.ErrNoRows {
		t.Fatalf("released FindLiveByPath = %v, want sql.ErrNoRows", err)
	}
}

// Release retires live bindings and refuses double/unknown release
// with the established strings.
func TestWorkspaceRepositoryReleaseBinding(t *testing.T) {
	db := openAuthorityTestDB(t)
	store := NewWorkspaceRepository(db)
	if err := store.ReleaseBinding("auth-never", 9, "orch", "req-1"); err == nil ||
		!strings.Contains(err.Error(), "no binding for auth-never attempt 9") {
		t.Fatalf("release unknown = %v, want no-binding refusal", err)
	}
	if err := store.Insert(authorityRec("auth-rel", "/repo/.metis-worktrees/auth-rel-1", WorkspaceAllocated)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := store.ReleaseBinding("auth-rel", 1, "orch", "req-2"); err != nil {
		t.Fatalf("ReleaseBinding: %v", err)
	}
	got, err := store.Find("auth-rel", 1)
	if err != nil || got.State != WorkspaceReleased {
		t.Fatalf("after release = %+v (err %v), want released", got, err)
	}
	if err := store.ReleaseBinding("auth-rel", 1, "orch", "req-3"); err == nil ||
		!strings.Contains(err.Error(), "not live (double release?)") {
		t.Fatalf("double release = %v, want not-live refusal", err)
	}
}

// Claim authorizes pickup-time ownership: foreign roots, duplicate
// claims, and uncertain bindings refuse; absence is not an error.
func TestWorkspaceRepositoryClaimBinding(t *testing.T) {
	db := openAuthorityTestDB(t)
	store := NewWorkspaceRepository(db)
	if err := store.ClaimBinding(db, "auth-absent", 1, "/elsewhere", "w", "req-1"); err != nil {
		t.Fatalf("claim without binding refused: %v", err)
	}
	path := "/repo/.metis-worktrees/auth-claim-1"
	if err := store.Insert(authorityRec("auth-claim", path, WorkspaceAllocated)); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := store.ClaimBinding(db, "auth-claim", 1, "/foreign", "w", "req-2"); err == nil ||
		!strings.Contains(err.Error(), "cross-worktree") {
		t.Fatalf("foreign-root claim = %v, want cross-worktree refusal", err)
	}
	if err := store.ClaimBinding(db, "auth-claim", 1, path, "w", "req-2"); err != nil {
		t.Fatalf("bound claim refused: %v", err)
	}
	if err := store.ClaimBinding(db, "auth-claim", 1, path, "w", "req-3"); err == nil ||
		!strings.Contains(err.Error(), "already in use") {
		t.Fatalf("duplicate claim = %v, want already-in-use refusal", err)
	}
	uncPath := "/repo/.metis-worktrees/auth-unc-1"
	if err := store.Insert(authorityRec("auth-unc", uncPath, WorkspaceUncertain)); err != nil {
		t.Fatalf("Insert uncertain: %v", err)
	}
	if err := store.ClaimBinding(db, "auth-unc", 1, uncPath, "w", "req-4"); err == nil ||
		!strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("uncertain claim = %v, want uncertain refusal", err)
	}
}

// An unverifiable fresh checkout is recorded uncertain and refused;
// the error names the path for the operator.
func TestWorkspaceReconcilerRecordUnverifiable(t *testing.T) {
	db := openAuthorityTestDB(t)
	r := NewWorkspaceReconciler(db, defaultGitDriver())
	path := "/repo/.metis-worktrees/auth-unver-1"
	verr := fmt.Errorf("worktree HEAD mismatch")
	err := r.RecordUnverifiable(authorityRec("auth-unver", path, WorkspaceAllocated), verr)
	if err == nil || !strings.Contains(err.Error(), "unverifiable") ||
		!strings.Contains(err.Error(), path) {
		t.Fatalf("RecordUnverifiable = %v, want unverifiable refusal naming %s", err, path)
	}
	got, ferr := NewWorkspaceRepository(db).Find("auth-unver", 1)
	if ferr != nil || got.State != WorkspaceUncertain {
		t.Fatalf("recorded = %+v (err %v), want uncertain", got, ferr)
	}
}

// A failed add over an unregistered path surfaces the git error
// unchanged: nothing is deleted or adopted.
func TestWorkspaceReconcilerClassifyAddFailurePassthrough(t *testing.T) {
	db := openAuthorityTestDB(t)
	repo := initGitRepo(t, false)
	r := NewWorkspaceReconciler(db, defaultGitDriver())
	addErr := fmt.Errorf("git worktree add: transient failure")
	ghost := filepath.Join(repo, ".metis-worktrees", "ghost-1")
	if err := r.ClassifyAddFailure(repo, ghost, addErr); err != addErr {
		t.Fatalf("ClassifyAddFailure unregistered = %v, want the original git error", err)
	}
}

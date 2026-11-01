package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// team_policy_store.go: Team policy persistence, revision lookup,
// and run pinning.
//
// Revisions are append-only rows in team_policies; run_teams binds
// each run to the revision in force at admission. Pure definition
// and evaluation live in team_policy.go; live capacity accounting
// lives in placement_capacity.go. Nothing here judges eligibility —
// it only stores, retrieves, and pins immutable snapshots.

func ensureTeamPolicyTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS team_policies (
		project    TEXT NOT NULL,
		revision   INTEGER NOT NULL,
		policy     TEXT NOT NULL,
		created_ns INTEGER NOT NULL,
		PRIMARY KEY (project, revision)
	)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS run_teams (
		run_id   TEXT PRIMARY KEY,
		project  TEXT NOT NULL,
		revision INTEGER NOT NULL
	)`)
	return err
}

// setTeamPolicy publishes a new revision for a project. Revisions
// are append-only: publishing never mutates an earlier revision.
//
// Revision assignment serializes under BEGIN IMMEDIATE: the
// read-then-insert of MAX(revision)+1 raced under concurrent
// publishers, handing two revisions the same number so the loser
// died on the primary key. The immediate write lock makes the
// second publisher wait, re-read the winner's revision, and take
// the next one — concurrent publishes yield a dense sequence.
func setTeamPolicy(project, raw string, req OperationRequest) (int, error) {
	if err := requireRequest(req); err != nil {
		return 0, err
	}
	if strings.TrimSpace(project) == "" {
		return 0, fmt.Errorf("team policy: project must not be empty")
	}
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureTeamPolicyTables(db); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	var maxRev sql.NullInt64
	if err := db.QueryRow(`SELECT MAX(revision) FROM team_policies WHERE project = ?`, project).Scan(&maxRev); err != nil {
		return 0, err
	}
	next := 1
	if maxRev.Valid {
		next = int(maxRev.Int64) + 1
	}
	if _, err := parseTeamPolicy(project, next, raw); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`INSERT INTO team_policies (project, revision, policy, created_ns)
		VALUES (?, ?, ?, ?)`, project, next, raw, time.Now().UnixNano()); err != nil {
		return 0, err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return 0, err
	}
	committed = true
	return next, nil
}

// getTeamPolicy returns one revision; revision 0 means latest.
func getTeamPolicy(project string, revision int) (string, int, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", 0, err
	}
	defer db.Close()
	if err := ensureTeamPolicyTables(db); err != nil {
		return "", 0, err
	}
	var raw string
	var rev int
	if revision == 0 {
		err = db.QueryRow(`SELECT policy, revision FROM team_policies WHERE project = ?
			ORDER BY revision DESC LIMIT 1`, project).Scan(&raw, &rev)
	} else {
		err = db.QueryRow(`SELECT policy, revision FROM team_policies WHERE project = ? AND revision = ?`,
			project, revision).Scan(&raw, &rev)
	}
	if err != nil {
		return "", 0, fmt.Errorf("team policy: no such revision (project %q revision %d)", project, revision)
	}
	return raw, rev, nil
}

// latestTeamPolicy parses the newest revision for a project.
func latestTeamPolicy(project string) (teamPolicy, error) {
	raw, rev, err := getTeamPolicy(project, 0)
	if err != nil {
		return teamPolicy{}, err
	}
	return parseTeamPolicy(project, rev, raw)
}

// pinTeamPolicy binds a run to the project's current revision. A
// run pins once: re-pinning keeps the original revision, so
// admission always checks the contract the run started under.
func pinTeamPolicy(runID, project string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("team policy: run id must not be empty")
	}
	_, rev, err := getTeamPolicy(project, 0)
	if err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureTeamPolicyTables(db); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO run_teams (run_id, project, revision) VALUES (?, ?, ?)
		ON CONFLICT (run_id) DO NOTHING`, runID, project, rev)
	return err
}

// pinnedTeamPolicy resolves the contract a run was admitted under.
func pinnedTeamPolicy(runID string) (teamPolicy, error) {
	db, err := openTasksDB()
	if err != nil {
		return teamPolicy{}, err
	}
	defer db.Close()
	if err := ensureTeamPolicyTables(db); err != nil {
		return teamPolicy{}, err
	}
	var project string
	var rev int
	if err := db.QueryRow(`SELECT project, revision FROM run_teams WHERE run_id = ?`, runID).Scan(&project, &rev); err != nil {
		return teamPolicy{}, fmt.Errorf("team policy: run %q has no pinned policy", runID)
	}
	raw, _, err := getTeamPolicy(project, rev)
	if err != nil {
		return teamPolicy{}, err
	}
	return parseTeamPolicy(project, rev, raw)
}

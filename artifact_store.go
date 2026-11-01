package main

// artifact_store.go: content-addressed evidence backing task reports.
//
// An artifact is one file a task's report depends on. Each attached
// path is hashed at attach time, so a reviewer compares digests
// instead of trusting that a file was "created". Evidence binds to
// the attempt that files it (TD-088): a retry that re-attaches the
// same path gets its own row, and re-attaching the same path in the
// same attempt is a duplicate filing, not independent evidence.
// Terminal tasks refuse new evidence — nothing may be planted after
// the verdict. Path confinement and hashing live in
// artifact_integrity.go; this file owns records and persistence.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Artifact is one file backing a task's report.
type Artifact struct {
	TaskID    string `json:"task_id"`
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	AttemptNo int    `json:"attempt_no"`
	Created   string `json:"created"`
}

// VerificationEvidence is one verifier's per-artifact check at verdict
// time: what was re-hashed, what it hashed to, and whether it matched
// the digest recorded when the worker filed the evidence.
type VerificationEvidence struct {
	TaskID        string `json:"task_id"`
	AttemptNo     int    `json:"attempt_no"`
	Path          string `json:"path"`
	DigestAtCheck string `json:"digest_at_verify"`
	OK            bool   `json:"ok"`
	Verifier      string `json:"verifier"`
	RequestID     string `json:"request_id"`
	Created       string `json:"created"`
}

func ensureArtifactsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS artifacts (
		task_id TEXT NOT NULL,
		path TEXT NOT NULL,
		digest TEXT NOT NULL,
		attempt_no INTEGER NOT NULL DEFAULT 0,
		created TEXT NOT NULL,
		PRIMARY KEY (task_id, path, attempt_no)
	)`)
	if err != nil {
		return err
	}
	return migrateArtifactsTable(db)
}

// artifactHasColumn reports whether the artifacts table already
// carries a named column.
func artifactHasColumn(db *sql.DB, want string) bool {
	rows, err := db.Query(`PRAGMA table_info(artifacts)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	var cid int
	var name, ctype string
	var notnull, pk int
	var dflt any
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == want {
			return true
		}
	}
	return false
}

// migrateArtifactsTable rebuilds databases created before evidence
// was bound to its producing attempt. The old (task_id, path) key
// would let a retry overwrite attempt-1 evidence; the rebuild keeps
// every row, binding legacy rows to the attempt on the task row.
func migrateArtifactsTable(db *sql.DB) error {
	if artifactHasColumn(db, "attempt_no") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`ALTER TABLE artifacts RENAME TO artifacts_legacy`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE artifacts (
		task_id TEXT NOT NULL,
		path TEXT NOT NULL,
		digest TEXT NOT NULL,
		attempt_no INTEGER NOT NULL DEFAULT 0,
		created TEXT NOT NULL,
		PRIMARY KEY (task_id, path, attempt_no)
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO artifacts (task_id, path, digest, attempt_no, created)
		SELECT task_id, path, digest,
			COALESCE((SELECT attempt FROM tasks WHERE tasks.id = artifacts_legacy.task_id), 0),
			created FROM artifacts_legacy`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE artifacts_legacy`); err != nil {
		return err
	}
	return tx.Commit()
}

// attachArtifact records a file as evidence for a task. The file must
// exist inside the task's worker workspace; its sha256 is stored so
// later reads can detect replacement. Evidence binds to the attempt
// that files it (TD-088): a retry that re-attaches the same path gets
// its own row, and re-attaching the same path in the same attempt is a
// duplicate filing, not independent evidence. Terminal tasks refuse
// new evidence — nothing may be planted after the verdict.
func attachArtifact(taskID, path string) (Artifact, error) {
	t, err := getTask(taskID)
	if err != nil {
		return Artifact{}, err
	}
	if terminalTaskStatus(t.Status) {
		return Artifact{}, fmt.Errorf("task %s is %s; evidence cannot be attached after the verdict", taskID, t.Status)
	}
	root, err := artifactRoot(t)
	if err != nil {
		return Artifact{}, err
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, abs)
	}
	abs = filepath.Clean(abs)
	// Confine BEFORE reading (the old order read first), through
	// the fd-verified helper so a symlink swapped in between the
	// check and the read is refused, not followed.
	data, err := readConfinedFile(root, abs)
	if err != nil {
		return Artifact{}, err
	}
	sum := sha256Digest(data)
	a := Artifact{TaskID: taskID, Path: abs, Digest: sum, AttemptNo: t.Attempt, Created: nowUTC()}
	db, err := openTasksDB()
	if err != nil {
		return Artifact{}, err
	}
	defer db.Close()
	if err := ensureArtifactsTable(db); err != nil {
		return Artifact{}, err
	}
	var dup int
	if err := db.QueryRow(`SELECT COUNT(*) FROM artifacts WHERE task_id = ? AND path = ? AND attempt_no = ?`,
		a.TaskID, a.Path, a.AttemptNo).Scan(&dup); err != nil {
		return Artifact{}, err
	}
	if dup > 0 {
		return Artifact{}, fmt.Errorf("artifact %s already attached to task %s for attempt %d; attach a new path or retry first", a.Path, a.TaskID, a.AttemptNo)
	}
	if _, err := db.Exec(`INSERT INTO artifacts (task_id, path, digest, attempt_no, created) VALUES (?, ?, ?, ?, ?)`,
		a.TaskID, a.Path, a.Digest, a.AttemptNo, a.Created); err != nil {
		if strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE") {
			return Artifact{}, fmt.Errorf("artifact %s already attached to task %s for attempt %d", a.Path, a.TaskID, a.AttemptNo)
		}
		return Artifact{}, err
	}
	return a, nil
}

func listArtifacts(taskID string) ([]Artifact, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureArtifactsTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT task_id, path, digest, attempt_no, created FROM artifacts WHERE task_id = ? ORDER BY attempt_no, path`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var a Artifact
		if err := rows.Scan(&a.TaskID, &a.Path, &a.Digest, &a.AttemptNo, &a.Created); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// currentAttemptArtifacts returns only the evidence filed by the
// task's current attempt. Superseded attempts' artifacts stay in the
// table for audit but are out of scope for the verdict (TD-088).
func currentAttemptArtifacts(taskID string, attempt int) ([]Artifact, error) {
	all, err := listArtifacts(taskID)
	if err != nil {
		return nil, err
	}
	var out []Artifact
	for _, a := range all {
		if a.AttemptNo == attempt {
			out = append(out, a)
		}
	}
	return out, nil
}

// verifyArtifact re-hashes the file and reports whether it still
// matches the digest recorded at attach time.
func verifyArtifact(a Artifact) (bool, error) {
	data, err := os.ReadFile(a.Path)
	if err != nil {
		return false, fmt.Errorf("artifact unreadable: %w", err)
	}
	sum := sha256Digest(data)
	return sum == a.Digest, nil
}

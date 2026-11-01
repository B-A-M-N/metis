package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// fleet_artifacts.go: host-bound artifact manifests and retrieval.
//
// This is the sixth and last fleet module. It records manifests,
// not bytes: one row binds a content digest to the node, task,
// and attempt that produced it, plus the node-local path where the
// bytes live. The same bytes from another node are a different
// manifest, so cross-node confusion is impossible by construction.
// Recording the same row twice converges on one identity.
// Integrity verdicts stay with reports.go/verification.go
// (attachArtifact/verifyArtifact remain authoritative); this module
// owns only fleet attribution and lifecycle.
//
// GC removes only manifests older than the horizon that no live
// (claimed) fleet execution still references.

type fleetArtifact struct {
	ManifestID string
	NodeID     string
	TaskID     string
	AttemptID  string
	Path       string
	Digest     string
	Size       int64
	Created    int64
}

func ensureFleetArtifactTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_artifacts (
		manifest_id TEXT PRIMARY KEY,
		node_id     TEXT NOT NULL,
		task_id     TEXT NOT NULL,
		attempt_id  TEXT NOT NULL,
		path        TEXT NOT NULL,
		digest      TEXT NOT NULL,
		size        INTEGER NOT NULL DEFAULT 0,
		created_ns  INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_fleet_artifacts_binding
		ON fleet_artifacts (node_id, task_id, attempt_id, path, digest)`)
	return err
}

// recordFleetArtifact manifests node-local output. The binding
// columns are the identity: re-recording the same row returns the
// original manifest ID, never a duplicate.
func recordFleetArtifact(nodeID, taskID, attemptID, path, digest string, size int64) (string, error) {
	if strings.TrimSpace(nodeID) == "" || strings.TrimSpace(taskID) == "" ||
		strings.TrimSpace(attemptID) == "" || strings.TrimSpace(digest) == "" {
		return "", fmt.Errorf("fleet artifacts: node, task, attempt, and digest must not be empty")
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureFleetArtifactTable(db); err != nil {
		return "", err
	}
	var existing string
	if err := db.QueryRow(`SELECT manifest_id FROM fleet_artifacts
		WHERE node_id = ? AND task_id = ? AND attempt_id = ? AND path = ? AND digest = ?`,
		nodeID, taskID, attemptID, path, digest).Scan(&existing); err == nil {
		return existing, nil
	}
	id := newUniqueID("fart")
	if _, err := db.Exec(`INSERT INTO fleet_artifacts
		(manifest_id, node_id, task_id, attempt_id, path, digest, size, created_ns)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, nodeID, taskID, attemptID, path, digest, size, time.Now().UnixNano()); err != nil {
		return "", err
	}
	return id, nil
}

// getFleetArtifact retrieves one manifest by identity. Unknown
// identities are refused, never answered with empty content.
func getFleetArtifact(manifestID string) (fleetArtifact, error) {
	var a fleetArtifact
	db, err := openTasksDB()
	if err != nil {
		return a, err
	}
	defer db.Close()
	if err := ensureFleetArtifactTable(db); err != nil {
		return a, err
	}
	if err := db.QueryRow(`SELECT manifest_id, node_id, task_id, attempt_id, path, digest, size, created_ns
		FROM fleet_artifacts WHERE manifest_id = ?`, manifestID).Scan(
		&a.ManifestID, &a.NodeID, &a.TaskID, &a.AttemptID, &a.Path, &a.Digest, &a.Size, &a.Created); err != nil {
		return a, fmt.Errorf("fleet: unknown artifact manifest %q", manifestID)
	}
	return a, nil
}

// gcFleetArtifacts deletes manifests older than the horizon unless
// a live (claimed) fleet execution still references their
// (task, attempt). Returns the number removed.
func gcFleetArtifacts(olderThan time.Duration) (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureFleetArtifactTable(db); err != nil {
		return 0, err
	}
	if err := ensureFleetExecutionTable(db); err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan).UnixNano()
	res, err := db.Exec(`DELETE FROM fleet_artifacts WHERE created_ns < ?
		AND NOT EXISTS (SELECT 1 FROM fleet_executions
			WHERE fleet_executions.task_id = fleet_artifacts.task_id
			AND fleet_executions.attempt_id = fleet_artifacts.attempt_id
			AND fleet_executions.state = 'claimed')`, cutoff)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

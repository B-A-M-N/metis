package main

// schema_epoch.go: explicit schema epoch/version admission (F-15).
//
// The migration path owns one single-row table, schema_epoch, that
// records the durable schema version and the minimum reader version
// compatible with it. openTasksDB validates that row on every open
// that is not an identity-confirmed cache hit and refuses with a
// *SchemaVersionError (a migration/compatibility error, never a
// generic I/O error) when the durable schema is newer than this
// binary or requires a newer reader. Missing data (no file, empty
// file, no epoch row) migrates forward; incompatible data refuses.
//
// The in-process migration cache is keyed by file identity (device,
// inode), not by path string, so replacing the file at the same path
// is detected and re-migrated instead of skipped.
//
// doctor.go stays observe-only: it never writes this table and never
// migrates; it can only read the row.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

const (
	// tasksSchemaVersion is the durable schema version this binary
	// writes. Bump when the migration path changes table shape.
	tasksSchemaVersion = 1
	// tasksSchemaMinCompatible is the oldest durable version this
	// binary can read and migrate forward.
	tasksSchemaMinCompatible = 1
)

// SchemaVersionError refusal reasons.
const (
	// SchemaReasonNewer means the durable version exceeds the
	// binary's version: the data was written by a newer binary.
	SchemaReasonNewer = "unsupported newer schema"
	// SchemaReasonReader means the durable row demands a newer
	// minimum reader than this binary provides.
	SchemaReasonReader = "incompatible schema: newer reader required"
	// SchemaReasonDowngrade means a stamp attempted to move the
	// recorded version backward.
	SchemaReasonDowngrade = "downgrade refused"
)

// SchemaVersionError is the specific migration/compatibility error
// returned when the durable schema version cannot be admitted: a
// newer-than-binary schema, a newer-reader requirement, or a
// downgrade attempt. It distinguishes incompatible schema from
// missing data (which migrates forward without error).
type SchemaVersionError struct {
	Path          string
	Have          int
	Want          int
	MinCompatible int
	Reason        string
}

func (e *SchemaVersionError) Error() string {
	switch e.Reason {
	case SchemaReasonDowngrade:
		return fmt.Sprintf("tasks schema downgrade refused for %s: durable version %d is newer than version %d: migrate with a newer binary instead of downgrading",
			e.Path, e.Have, e.Want)
	case SchemaReasonReader:
		return fmt.Sprintf("incompatible tasks schema for %s: durable data requires reader version %d but binary supports version %d (durable version %d): incompatible schema",
			e.Path, e.MinCompatible, e.Want, e.Have)
	default:
		return fmt.Sprintf("unsupported newer tasks schema for %s: durable version %d (minimum compatible reader %d) exceeds binary version %d: refusing downgrade/migration; incompatible schema",
			e.Path, e.Have, e.MinCompatible, e.Want)
	}
}

// IsSchemaVersionError reports whether err is a schema
// epoch/compatibility refusal.
func IsSchemaVersionError(err error) bool {
	var target *SchemaVersionError
	return errors.As(err, &target)
}

// dbIdentity identifies a database file beyond its path string so a
// replacement at the same path is not mistaken for an already
// migrated database. Only device+inode participate: size and modtime
// change on every write (including the epoch stamp itself), so
// including them self-invalidates the admission cache — each write
// workload open would miss and re-run full DDL. The stamped epoch row
// is the authority for schema currency (re-read on every fast-path
// hit); the stat identity only detects file replacement, for which
// device+inode is the right signal.
type dbIdentity struct {
	dev uint64
	ino uint64
	ok  bool
}

// sameFile reports whether two identities name the same file. A
// failed stat (ok == false) never matches: callers fall through to
// full validation/migration.
func (id dbIdentity) sameFile(other dbIdentity) bool {
	return id.ok && other.ok && id.dev == other.dev && id.ino == other.ino
}

// statDBIdentity stats path. ok is false when the file cannot be
// statted (e.g. not yet created); callers fall through to full
// validation/migration in that case.
func statDBIdentity(path string) dbIdentity {
	fi, err := os.Stat(path)
	if err != nil {
		return dbIdentity{}
	}
	id := dbIdentity{ok: true}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st != nil {
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
	}
	return id
}

// readSchemaEpoch returns the stamped durable version. present is
// false when the epoch table (or its row) is absent: a fresh,
// legacy, or interrupted-migration database that must migrate
// forward rather than refuse.
func readSchemaEpoch(db *sql.DB) (have, minCompat int, present bool, err error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_epoch'`).Scan(&n); err != nil {
		return 0, 0, false, err
	}
	if n == 0 {
		return 0, 0, false, nil
	}
	var v, m int
	err = db.QueryRow(`SELECT version, min_compatible FROM schema_epoch WHERE id = 1`).Scan(&v, &m)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return v, m, true, nil
}

// checkSchemaCompatible refuses durable versions this binary cannot
// read. Anything at or below the binary version migrates forward;
// anything above, or demanding a newer reader, is a
// *SchemaVersionError, never silent acceptance.
func checkSchemaCompatible(path string, have, minCompat int) error {
	if minCompat > tasksSchemaVersion {
		return &SchemaVersionError{Path: path, Have: have, Want: tasksSchemaVersion, MinCompatible: minCompat, Reason: SchemaReasonReader}
	}
	if have > tasksSchemaVersion {
		return &SchemaVersionError{Path: path, Have: have, Want: tasksSchemaVersion, MinCompatible: minCompat, Reason: SchemaReasonNewer}
	}
	return nil
}

// stampSchemaEpoch records the migrated version after successful
// DDL. It owns the epoch row: the only writer is the migration path
// (plus tests simulating a newer writer). Stamping backward over a
// newer durable version is a downgrade attempt and is refused so a
// newer schema is never relabeled as older.
func stampSchemaEpoch(db *sql.DB, version, minCompat int) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_epoch (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		version INTEGER NOT NULL,
		min_compatible INTEGER NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		return err
	}
	var have int
	switch err := db.QueryRow(`SELECT version FROM schema_epoch WHERE id = 1`).Scan(&have); {
	case err == sql.ErrNoRows:
		// No stamp yet: insert below.
	case err != nil:
		return err
	default:
		if have > version {
			return &SchemaVersionError{Reason: SchemaReasonDowngrade, Have: have, Want: version, MinCompatible: minCompat}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`INSERT OR REPLACE INTO schema_epoch (id, version, min_compatible, updated) VALUES (1, ?, ?, ?)`,
		version, minCompat, now)
	return err
}

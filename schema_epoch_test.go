package main

// schema_epoch_test.go: failing-first tests for explicit schema
// epoch/version admission (F-15). Each test isolates state with
// TUI_DASH_STATE so the process-global migration cache cannot leak
// between tests.

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func openRawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", tasksDBPath())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func requireTableExists(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatalf("table inventory for %s: %v", name, err)
	}
	if n != 1 {
		t.Fatalf("expected table %s to exist after open", name)
	}
}

func requireEpoch(t *testing.T, db *sql.DB, wantVersion int) {
	t.Helper()
	have, _, present, err := readSchemaEpoch(db)
	if err != nil {
		t.Fatalf("readSchemaEpoch: %v", err)
	}
	if !present {
		t.Fatalf("schema_epoch row missing after open")
	}
	if have != wantVersion {
		t.Fatalf("schema epoch = %d, want %d", have, wantVersion)
	}
}

// Old fixture: a legacy database with only an early tasks table and
// no epoch row must migrate forward, preserve its rows, and stamp.
func TestSchemaEpochOldFixtureOpen(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	raw := openRawDB(t)
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY,
		run TEXT NOT NULL,
		agent TEXT NOT NULL,
		objective TEXT NOT NULL,
		created TEXT NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create legacy tasks: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tasks (id, run, agent, objective, created, updated)
		VALUES ('t1', 'run1', 'agent', 'legacy objective', 'c', 'u')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB on legacy fixture: %v", err)
	}
	defer db.Close()
	if IsSchemaVersionError(err) {
		t.Fatalf("legacy data misreported as incompatible schema")
	}
	var obj string
	if err := db.QueryRow(`SELECT objective FROM tasks WHERE id = 't1'`).Scan(&obj); err != nil || obj != "legacy objective" {
		t.Fatalf("legacy row lost: obj=%q err=%v", obj, err)
	}
	for _, tbl := range []string{"task_attempts", "task_deliveries", "task_reports", "execution_receipts", "task_verdicts", "guidance", "schema_epoch"} {
		requireTableExists(t, db, tbl)
	}
	if !taskHasColumn(db, "key") {
		t.Fatalf("legacy migration did not add key column")
	}
	requireEpoch(t, db, tasksSchemaVersion)
}

// Missing data is not incompatible data: a zero-byte file migrates
// forward without a compatibility error.
func TestSchemaEpochEmptyFileMigrates(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := os.WriteFile(tasksDBPath(), nil, 0o600); err != nil {
		t.Fatalf("write empty db: %v", err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB on empty file: %v", err)
	}
	defer db.Close()
	if IsSchemaVersionError(err) {
		t.Fatalf("empty file misreported as incompatible schema")
	}
	requireTableExists(t, db, "tasks")
	requireEpoch(t, db, tasksSchemaVersion)
}

// Interrupted migration: tables complete but no stamp (crash between
// the last DDL and the stamp), plus a dropped table, must re-run
// migration on the next open instead of trusting the cache.
func TestSchemaEpochInterruptedMigration(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	first, err := openTasksDB()
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = first.Close()

	raw := openRawDB(t)
	if _, err := raw.Exec(`DROP TABLE task_attempts`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := raw.Exec(`DELETE FROM schema_epoch`); err != nil {
		t.Fatalf("drop stamp: %v", err)
	}

	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB after interrupted migration: %v", err)
	}
	defer db.Close()
	requireTableExists(t, db, "task_attempts")
	requireEpoch(t, db, tasksSchemaVersion)
}

// Concurrent first-open: many goroutines on a fresh path all succeed
// with exactly one epoch row and a complete schema.
func TestSchemaEpochConcurrentFirstOpen(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	const n = 16
	errs := make([]error, n)
	dbs := make([]*sql.DB, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := openTasksDB()
			dbs[i], errs[i] = db, err
		}(i)
	}
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("opener %d: %v", i, errs[i])
		}
		_ = dbs[i].Close()
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("verify open: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_epoch WHERE id = 1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("epoch rows = %d, err = %v; want exactly 1", count, err)
	}
	requireEpoch(t, db, tasksSchemaVersion)
	requireTableExists(t, db, "task_attempts")
}

// File replacement at the same path: after a migrated open, swapping
// in a different file must be detected (identity, not path string)
// and re-migrated. A path-only cache would skip DDL and leave
// task_attempts missing.
func TestSchemaEpochFileReplacementSamePath(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	first, err := openTasksDB()
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = first.Close()
	if _, ok := migratedDBs.Load(tasksDBPath()); !ok {
		t.Fatalf("migration cache not populated after first open")
	}

	if err := os.Remove(tasksDBPath()); err != nil {
		t.Fatalf("remove db: %v", err)
	}
	raw := openRawDB(t)
	if _, err := raw.Exec(`CREATE TABLE tasks (
		id TEXT PRIMARY KEY,
		run TEXT NOT NULL,
		agent TEXT NOT NULL,
		objective TEXT NOT NULL,
		created TEXT NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create replacement tasks: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO tasks (id, run, agent, objective, created, updated)
		VALUES ('r1', 'runR', 'agent', 'replacement row', 'c', 'u')`); err != nil {
		t.Fatalf("seed replacement row: %v", err)
	}

	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB after file replacement: %v", err)
	}
	defer db.Close()
	requireTableExists(t, db, "task_attempts")
	requireTableExists(t, db, "schema_epoch")
	requireEpoch(t, db, tasksSchemaVersion)
	var obj string
	if err := db.QueryRow(`SELECT objective FROM tasks WHERE id = 'r1'`).Scan(&obj); err != nil || obj != "replacement row" {
		t.Fatalf("replacement row lost: obj=%q err=%v", obj, err)
	}
}

// Unsupported newer schema: durable data far beyond this binary must
// be refused with the specific compatibility error, not migrated or
// silently accepted.
func TestSchemaEpochUnsupportedNewer(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seed, err := openTasksDB()
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	_ = seed.Close()

	raw := openRawDB(t)
	future := tasksSchemaVersion + 1000
	// A newer writer that stays reader-compatible: the version check,
	// not the reader check, must refuse.
	if _, err := raw.Exec(`UPDATE schema_epoch SET version = ?, min_compatible = ? WHERE id = 1`, future, tasksSchemaMinCompatible); err != nil {
		t.Fatalf("stamp future version: %v", err)
	}

	db, err := openTasksDB()
	if err == nil {
		_ = db.Close()
		t.Fatalf("openTasksDB accepted unsupported schema version %d", future)
	}
	if !IsSchemaVersionError(err) {
		t.Fatalf("expected *SchemaVersionError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "unsupported newer") {
		t.Fatalf("error should name unsupported newer schema: %v", err)
	}
	var sve *SchemaVersionError
	if ok := errorAsSchema(err, &sve); !ok || sve.Have != future {
		t.Fatalf("error should carry durable version %d: %+v", future, err)
	}
}

// Newer reader required: durable data at a readable version that
// demands a newer minimum reader is refused without migration.
func TestSchemaEpochNewerReaderRequired(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seed, err := openTasksDB()
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	_ = seed.Close()

	raw := openRawDB(t)
	if _, err := raw.Exec(`UPDATE schema_epoch SET min_compatible = ? WHERE id = 1`, tasksSchemaVersion+1); err != nil {
		t.Fatalf("raise min_compatible: %v", err)
	}

	db, err := openTasksDB()
	if err == nil {
		_ = db.Close()
		t.Fatalf("openTasksDB accepted data requiring a newer reader")
	}
	if !IsSchemaVersionError(err) {
		t.Fatalf("expected *SchemaVersionError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "reader") {
		t.Fatalf("error should name the reader requirement: %v", err)
	}
}

// Downgrade attempt: data written by a newer binary (one version
// ahead) is refused, and stamping backward over a newer durable
// version is refused without touching the row.
func TestSchemaEpochDowngradeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seed, err := openTasksDB()
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	_ = seed.Close()

	raw := openRawDB(t)
	newer := tasksSchemaVersion + 1
	// Data from a one-version-newer writer, still reader-compatible:
	// opening it with this binary would be a downgrade.
	if _, err := raw.Exec(`UPDATE schema_epoch SET version = ?, min_compatible = ? WHERE id = 1`, newer, tasksSchemaMinCompatible); err != nil {
		t.Fatalf("stamp newer version: %v", err)
	}

	db, err := openTasksDB()
	if err == nil {
		_ = db.Close()
		t.Fatalf("openTasksDB accepted downgrade from version %d", newer)
	}
	if !IsSchemaVersionError(err) {
		t.Fatalf("expected *SchemaVersionError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("error should name the downgrade refusal: %v", err)
	}

	stampDB, err := sql.Open("sqlite", tasksDBPath())
	if err != nil {
		t.Fatalf("open for stamp guard: %v", err)
	}
	defer stampDB.Close()
	if err := stampSchemaEpoch(stampDB, tasksSchemaVersion, tasksSchemaMinCompatible); err == nil {
		t.Fatalf("stampSchemaEpoch allowed backward stamp over version %d", newer)
	} else if !IsSchemaVersionError(err) || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("stamp guard should refuse downgrade specifically: %v", err)
	}
	have, _, present, rerr := readSchemaEpoch(stampDB)
	if rerr != nil || !present || have != newer {
		t.Fatalf("failed downgrade must leave durable version %d intact (have=%d present=%v err=%v)", newer, have, present, rerr)
	}
}

func errorAsSchema(err error, target **SchemaVersionError) bool {
	return errors.As(err, target)
}

// Steady-state opens must not re-run migration DDL. After the first
// migration, write+reopen cycles must leave migrationDDLruns unchanged:
// the epoch-gated fast path admits the already-migrated file without
// DDL or re-stamp. Re-running DDL per open is the ~16-26ms/op overhead
// dominating write-heavy suites (TestFinalizeRacesTaskAdmission): the
// stamp write changes size/mtime, so the next open misses the cache
// and re-migrates, self-perpetuating on every write workload.
func TestReopenAfterWriteSkipsMigrationDDL(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_ = db.Close()
	const cycles = 10
	before := migrationDDLruns.Load()
	for i := 0; i < cycles; i++ {
		if _, err := createTask("r-reopen", "agent", fmt.Sprintf("obj-%d", i), ""); err != nil {
			t.Fatalf("createTask: %v", err)
		}
		d, err := openTasksDB()
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		_ = d.Close()
	}
	if got := migrationDDLruns.Load() - before; got != 0 {
		t.Fatalf("migration DDL ran %d times over %d write+reopen cycles, want 0 (steady-state opens must use the epoch-gated fast path)", got, cycles)
	}
}

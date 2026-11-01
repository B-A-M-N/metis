package main

// task_schema.go: store location, connection setup, and schema
// creation/migration. Reads and writes stay in task_store.go.
// Extracted from task_store.go; behavior unchanged.

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

func tasksDBPath() string { return stateDir() + "/tasks.db" }

func openTasksDB() (*sql.DB, error) {
	path := tasksDBPath()
	openMu.Lock()
	defer openMu.Unlock()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection per handle: PRAGMAs like busy_timeout are
	// per-connection, and a pooled second connection would lose
	// them and surface SQLITE_BUSY under concurrent use (a polling
	// worker loop racing a dispatch). WAL still allows
	// cross-process readers alongside one writer.
	db.SetMaxOpenConns(1)
	// busy_timeout FIRST: it needs no lock itself, and it must
	// already apply when the journal-mode upgrade below (or any
	// DDL after it) waits on another connection. Setting it after
	// the upgrade left a window where a momentary collision
	// failed instantly with SQLITE_BUSY instead of waiting.
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open tasks db busy_timeout: %w", err)
	}
	// Supervisors and CLI invocations write concurrently (heartbeats,
	// leases, transitions). WAL keeps readers out of writers' way and
	// the busy timeout turns momentary writer/writer collisions into
	// short waits instead of SQLITE_BUSY failures. The upgrade needs
	// an exclusive lock, so only attempt it when the database is not
	// already in WAL — a read that takes no write lock.
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open tasks db journal_mode read: %w", err)
	}
	if !strings.EqualFold(mode, "wal") {
		if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db journal_mode: %w", err)
		}
	}
	// Task state can contain objectives, reports, and findings:
	// owner-only, like the rest of the state tree.
	_ = os.Chmod(path, 0o600)
	// Explicit epoch admission (F-15): a cached path alone proves
	// nothing if the file was replaced, so a fast return additionally
	// requires the stat identity to match AND the stamped epoch to
	// still read back compatible. Anything else falls through to
	// full validation and migration below; a failed or refused open
	// never populates the cache.
	if v, ok := migratedDBs.Load(path); ok {
		if ident, ok := v.(dbIdentity); ok && ident.sameFile(statDBIdentity(path)) {
			if have, minCompat, present, rerr := readSchemaEpoch(db); rerr == nil && present &&
				have == tasksSchemaVersion && minCompat <= tasksSchemaVersion {
				dbOpenCount.Add(1)
				return db, nil
			}
		}
	}
	// Refuse incompatible durable schemas before touching DDL:
	// missing data (no epoch row) migrates forward, but a newer
	// schema or a newer-reader requirement is a specific
	// migration/compatibility error, never silent acceptance.
	have, minCompat, present, verr := readSchemaEpoch(db)
	if verr != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open tasks db read schema epoch: %w", verr)
	} else if present {
		if verr := checkSchemaCompatible(path, have, minCompat); verr != nil {
			_ = db.Close()
			return nil, verr
		}
	}
	// Epoch-gated DDL skip: when a previous open in any process
	// already migrated this file to this binary's exact stamp, DDL
	// is a no-op by construction (all statements are IF NOT EXISTS
	// or additive) and re-running it only costs round trips — plus
	// the stamp write below, which used to self-invalidate the
	// identity cache on every write workload. Fresh, legacy, or
	// version-skewed files still run the full path and stamp.
	if !present || have != tasksSchemaVersion || minCompat != tasksSchemaMinCompatible {
		migrationDDLruns.Add(1)
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS tasks (
		id TEXT PRIMARY KEY,
		run TEXT NOT NULL,
		agent TEXT NOT NULL,
		objective TEXT NOT NULL,
		acceptance TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		attempt INTEGER NOT NULL DEFAULT 0,
		parent TEXT NOT NULL DEFAULT '',
		report TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL,
		key TEXT NOT NULL DEFAULT '',
		correction_no INTEGER NOT NULL DEFAULT 0,
		parent_report TEXT NOT NULL DEFAULT '',
		execution_root TEXT NOT NULL DEFAULT '',
		shared_workspace INTEGER NOT NULL DEFAULT 0
	)`)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db create tasks: %w", err)
		}
		if err := ensureTaskColumn(db, "correction_no", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db migrate correction_no: %w", err)
		}
		if err := ensureTaskColumn(db, "parent_report", "TEXT NOT NULL DEFAULT ''"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db migrate parent_report: %w", err)
		}
		if err := ensureTaskColumn(db, "execution_root", "TEXT NOT NULL DEFAULT ''"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db migrate execution_root: %w", err)
		}
		if err := ensureTaskColumn(db, "shared_workspace", "INTEGER NOT NULL DEFAULT 0"); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db migrate shared_workspace: %w", err)
		}
		// Migrate databases created before the idempotency key existed.
		if !taskHasKeyColumn(db) {
			if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN key TEXT NOT NULL DEFAULT ''`); err != nil {
				_ = db.Close()
				return nil, fmt.Errorf("open tasks db migrate key: %w", err)
			}
		}
		// Non-empty keys are unique per run; empty keys never collide.
		if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS tasks_run_key ON tasks(run, key) WHERE key <> ''`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db create run_key index: %w", err)
		}
		// The dashboard titles each worker tab with its latest objective
		// (latestObjectiveByAgent); the (agent, created) index keeps that
		// projection indexed instead of scanning the whole task history
		// on every refresh.
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS tasks_agent_created ON tasks(agent, created)`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db create agent_created index: %w", err)
		}
		if err := ensureLifecycleTables(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db lifecycle tables: %w", err)
		}
		// The orchestration feed table migrates with the rest: the
		// dispatch commit inserts the delegation event inside its own
		// transaction, so the table must exist before any dispatch.
		if err := ensureOrchEventsTable(db); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db orch events table: %w", err)
		}
		// Only a fully migrated schema is cached: a concurrent
		// first-open that skips DDL on a half-built database reads
		// missing tables instead of waiting. Concurrent DDL on a
		// fresh path can still collide once; busy_timeout rides it
		// out, and IF NOT EXISTS keeps it idempotent. The epoch stamp
		// is the last write, so an interrupted migration leaves no
		// stamp and the next open re-runs DDL instead of trusting a
		// half-built database; the cached identity binds the stamp to
		// this exact file so a replacement at the same path
		// re-migrates.
		if err := stampSchemaEpoch(db, tasksSchemaVersion, tasksSchemaMinCompatible); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("open tasks db stamp schema epoch: %w", err)
		}
	}
	migratedDBs.Store(path, statDBIdentity(path))
	dbOpenCount.Add(1)
	return db, nil
}

// taskHasColumn reports whether the tasks table already carries a
// named column (fresh databases get new columns from CREATE TABLE).
func taskHasColumn(db *sql.DB, want string) bool {
	rows, err := db.Query(`PRAGMA table_info(tasks)`)
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

// taskHasKeyColumn reports whether the tasks table already carries the
// idempotency-key column (fresh databases get it from CREATE TABLE above).
func taskHasKeyColumn(db *sql.DB) bool { return taskHasColumn(db, "key") }

// ensureTaskColumn adds a column to pre-existing databases that were
// created before it existed. A concurrent first-open in another
// process can win the same ALTER between our check and our Exec;
// "duplicate column name" then means the column exists, not failure.
func ensureTaskColumn(db *sql.DB, name, ddl string) error {
	if taskHasColumn(db, name) {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE tasks ADD COLUMN ` + name + ` ` + ddl); err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// tableHasColumn reports whether any table already carries a named
// column (fresh databases get new columns from CREATE TABLE).
func tableHasColumn(db *sql.DB, table, want string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
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

// ensureColumn adds a column to any pre-existing table that was
// created before it existed.
func ensureColumn(db *sql.DB, table, name, ddl string) error {
	if tableHasColumn(db, table, name) {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + ddl); err != nil {
		if strings.Contains(err.Error(), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

func ensureLifecycleTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_attempts (
		id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		state TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL,
		UNIQUE (task_id, attempt_no)
	)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS task_attempts_task ON task_attempts(task_id)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_deliveries (
		id TEXT PRIMARY KEY,
		attempt_id TEXT NOT NULL,
		task_id TEXT NOT NULL,
		state TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS task_deliveries_task ON task_deliveries(task_id)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS task_deliveries_attempt ON task_deliveries(attempt_id)`)
	if err != nil {
		return err
	}
	// task_deps records mandatory dependency edges: a task must not
	// start until every task it depends on is verified (TD-030/031).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_deps (
		task_id TEXT NOT NULL,
		depends_on TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		PRIMARY KEY (task_id, depends_on)
	)`); err != nil {
		return err
	}
	// run_receipts records run-level dispositions such as
	// finalization, which belong to no single task (TD-041).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS run_receipts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		run TEXT NOT NULL,
		kind TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS run_receipts_run ON run_receipts(run)`)
	if err != nil {
		return err
	}
	// run_limits holds one explicit concurrency bound per run: the
	// maximum number of simultaneously live (dispatched/working)
	// tasks. No row means unbounded. This is a scheduler knob,
	// independent of UI panes, provider quotas, or host capacity
	// (TD-032, TD-034).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS run_limits (
		run TEXT PRIMARY KEY,
		max_live INTEGER NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL
	)`); err != nil {
		return err
	}
	// guidance records durable intervention requests: accepted first,
	// then settled to a terminal disposition (filed, delivered,
	// refused, failed). Accepted rows survive controller restart
	// until settled (TD-083); every row carries who/when/mode/
	// outcome for audit (TD-084).
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS guidance (
		id TEXT PRIMARY KEY,
		session TEXT NOT NULL,
		mode TEXT NOT NULL,
		state TEXT NOT NULL,
		text TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS guidance_session ON guidance(session)`)
	if err != nil {
		return err
	}
	// Idempotency for retried interventions: same request ID means
	// the same operation (TD-028 pattern). A lost race returns the
	// winner's row instead of duplicating the message.
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS guidance_request ON guidance(request_id) WHERE request_id <> ''`)
	if err != nil {
		return err
	}
	// Native steering reuses one server command per guidance row:
	// a retried delivery after a crash resubmits the same id, so
	// Muse dedups instead of running the instruction twice.
	if err := ensureColumn(db, "guidance", "command_id", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// task_reports is the append-only history of worker claims: one
	// row per (task, attempt), never updated, so a filed report
	// survives refused verifications and later attempts and no louder
	// re-report can overwrite it (TD-087). actor is the worker
	// session that owns the attempt (TD-092); reporter names who
	// actually filed the text and source labels what it is, so an
	// outside submission can never impersonate the worker.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS task_reports (
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		report TEXT NOT NULL,
		digest TEXT NOT NULL,
		actor TEXT NOT NULL DEFAULT '',
		reporter TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		PRIMARY KEY (task_id, attempt_no)
	)`); err != nil {
		return err
	}
	if err := ensureColumn(db, "task_reports", "reporter", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "task_reports", "source", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	// verification_evidence records every per-artifact check behind a
	// verdict: what the verifier re-hashed, what it hashed to at
	// check time, and whether it matched the filed digest (TD-098).
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS verification_evidence (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		path TEXT NOT NULL,
		digest_at_verify TEXT NOT NULL,
		ok INTEGER NOT NULL,
		verifier TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS verification_evidence_task ON verification_evidence(task_id)`)
	if err != nil {
		return err
	}
	// execution_receipts bind one provider execution to one attempt
	// (remediation Phase A/C): the actual invocation plus the native
	// process result, stored separately from delivery assertions. One
	// row per (task, attempt) — a duplicate launch is refused, never
	// recorded twice. exit_code NULL means the process has not been
	// observed to exit yet; it is never success evidence by itself.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS execution_receipts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id TEXT NOT NULL,
		attempt_no INTEGER NOT NULL,
		session TEXT NOT NULL,
		harness TEXT NOT NULL,
		argv TEXT NOT NULL,
		pid INTEGER NOT NULL DEFAULT 0,
		exit_code INTEGER,
		output_digest TEXT NOT NULL DEFAULT '',
		reporter TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL,
		UNIQUE (task_id, attempt_no)
	)`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS execution_receipts_task ON execution_receipts(task_id)`)
	if err != nil {
		return err
	}
	// proc_start binds the recorded PID to one specific process
	// incarnation (WP1c): cancellation verifies identity before
	// signaling, so a recycled PID is never killed. Zero means the
	// start time was unavailable at launch — cancel then refuses
	// rather than guessing.
	if err := ensureColumn(db, "execution_receipts", "proc_start", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err != nil {
		return err
	}
	// task_verdicts hold one external judgment per criterion per
	// attempt (remediation Phase B): requirement satisfaction,
	// distinct from evidence integrity and operator override.
	return ensureVerdictsTable(db)
}

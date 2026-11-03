package main

// provision_repository.go: durable provision persistence.
//
// This file owns the provision intent store: one row per
// request_id, migrated forward from older databases, read back for
// replay, and moved only by generation-fenced compare-and-sets. It
// owns no lifecycle policy beyond the fenced write itself: the
// transition gate (provisionTransitionAllowed) stays with the
// ProvisionService in provision_service.go, which keeps admission,
// replay, launch, and convergence as one cohesive lifecycle and
// calls back into this repository for every durable read/write.

import (
	"database/sql"
)

// ProvisionRepository is the durable provision store: one intent row
// per request_id. All writes are generation-fenced
// compare-and-sets; a stale (state, generation) never commits.
type ProvisionRepository struct {
	db *sql.DB
}

// NewProvisionRepository binds the store to an open database. The
// caller owns the handle lifetime.
func NewProvisionRepository(db *sql.DB) *ProvisionRepository {
	return &ProvisionRepository{db: db}
}

// Ensure creates the request_id -> provision intent table and
// migrates older databases forward without touching existing rows.
// Older databases carry only the original five columns; the
// generation column fences concurrent transitions: every state
// change is a compare-and-set on (state, generation).
func (r *ProvisionRepository) Ensure() error {
	if _, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS mcp_provision (
		request_id TEXT PRIMARY KEY,
		session TEXT NOT NULL,
		run TEXT NOT NULL DEFAULT '',
		objective TEXT NOT NULL DEFAULT '',
		workdir TEXT NOT NULL DEFAULT '',
		backend TEXT NOT NULL DEFAULT 'tmux',
		harness TEXT NOT NULL DEFAULT '',
		command TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'reserved',
		generation INTEGER NOT NULL DEFAULT 0,
		created TEXT NOT NULL
	)`); err != nil {
		return err
	}
	rows, err := r.db.Query(`PRAGMA table_info (mcp_provision)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	migrate := map[string]string{
		"workdir": "", "harness": "", "command": "", "model": "", "state": "",
		"backend": "'tmux'",
	}
	for col, dflt := range migrate {
		if present[col] {
			continue
		}
		if _, err := r.db.Exec(`ALTER TABLE mcp_provision ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ` + dflt); err != nil {
			return err
		}
	}
	if !present["generation"] {
		if _, err := r.db.Exec(`ALTER TABLE mcp_provision ADD COLUMN generation INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

// Load returns the recorded intent and its fencing generation.
func (r *ProvisionRepository) Load(requestID string) (mcpProvisionRecord, int64, bool) {
	var rec mcpProvisionRecord
	var generation sql.NullInt64
	err := r.db.QueryRow(`SELECT session, run, objective, workdir, backend, harness, command, model, state, generation
		FROM mcp_provision WHERE request_id = ?`, requestID).Scan(
		&rec.Session, &rec.Run, &rec.Objective, &rec.Workdir, &rec.Backend,
		&rec.Harness, &rec.Command, &rec.Model, &rec.State, &generation)
	if err != nil {
		return mcpProvisionRecord{}, 0, false
	}
	if rec.Backend == "" {
		rec.Backend = provisionBackendTMUX
	}
	if generation.Valid {
		rec.Generation = generation.Int64
	}
	return rec, rec.Generation, true
}

// InsertReserved durably records the intent before any workdir or
// process exists. A duplicate request_id is not an error to decide
// here: the caller loads the winner and replays/converges.
func (r *ProvisionRepository) InsertReserved(requestID string, want mcpProvisionRecord) (int64, error) {
	if _, err := r.db.Exec(`INSERT INTO mcp_provision
		(request_id, session, run, objective, workdir, backend, harness, command, model, state, generation, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'reserved', 0, ?)`,
		requestID, want.Session, want.Run, want.Objective, want.Workdir,
		want.Backend, want.Harness, want.Command, want.Model, nowUTC()); err != nil {
		return 0, err
	}
	return 0, nil
}

// OwnerOf reports the bound principal for session, if any.
func (r *ProvisionRepository) OwnerOf(session string) (string, bool, error) {
	return lookupProvisionOwner(r.db, session)
}

// Store exposes the intent handle as an OwnerStore for the
// single-statement replay path (grandfathering pre-policy rows);
// the fresh path binds inside ProvisionReservation instead.
func (r *ProvisionRepository) Store() OwnerStore {
	return r.db
}

// ClaimOwner binds session to principal when unbound and reports
// whether a different principal already owns it. One statement,
// inherently atomic; the fresh path uses the reservation
// transaction so intent and owner commit together.
func (r *ProvisionRepository) ClaimOwner(session, principal string) (bool, error) {
	return claimProvisionOwner(r.db, session, principal)
}

// ProvisionReservation is one atomic reserve-and-bind unit: the
// intent insert and the owner claim share a single SQL
// transaction. Commit makes both durable together; any failure
// or an explicit Rollback leaves neither behind. No worker may
// start until Commit succeeds — a crash mid-unit replays from
// zero, never from a half-written reservation.
type ProvisionReservation struct {
	tx *sql.Tx
}

// BeginReserve opens the reservation transaction. Intent DDL
// runs first via Ensure (idempotent, pre-existing pattern); the
// owner table is ensured INSIDE the transaction so a rollback
// also drops a half-created owner schema.
func (r *ProvisionRepository) BeginReserve() (*ProvisionReservation, error) {
	if err := r.Ensure(); err != nil {
		return nil, err
	}
	tx, err := r.db.Begin()
	if err != nil {
		return nil, err
	}
	if err := ensureProvisionOwnerTable(tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return &ProvisionReservation{tx: tx}, nil
}

// InsertReserved records the intent inside the transaction. A
// duplicate request_id errors here; the caller rolls back and
// converges on the committed winner via Load.
func (p *ProvisionReservation) InsertReserved(requestID string, want mcpProvisionRecord) error {
	_, err := p.tx.Exec(`INSERT INTO mcp_provision
		(request_id, session, run, objective, workdir, backend, harness, command, model, state, generation, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'reserved', 0, ?)`,
		requestID, want.Session, want.Run, want.Objective, want.Workdir,
		want.Backend, want.Harness, want.Command, want.Model, nowUTC())
	return err
}

// Exec delegates to the transaction so the reservation itself
// satisfies OwnerStore: the binding claim runs inside the unit.
func (p *ProvisionReservation) Exec(query string, args ...any) (sql.Result, error) {
	return p.tx.Exec(query, args...)
}

// QueryRow delegates to the transaction for same-unit reads.
func (p *ProvisionReservation) QueryRow(query string, args ...any) *sql.Row {
	return p.tx.QueryRow(query, args...)
}

// Commit makes the intent and the owner binding durable together.
func (p *ProvisionReservation) Commit() error {
	return p.tx.Commit()
}

// Rollback abandons the unit: neither intent nor owner persists.
func (p *ProvisionReservation) Rollback() error {
	return p.tx.Rollback()
}

// CompareAndSet moves request_id to newState only when the row still
// carries expectedState at expectedGeneration, bumping the
// generation on success. It reports (false, nil) when the fence or
// the lifecycle gate refuses — a stale holder learns it lost instead
// of clobbering the winner. Illegal lifecycle edges never commit.
func (r *ProvisionRepository) CompareAndSet(requestID, expectedState string, expectedGeneration int64, newState string) (bool, error) {
	if !provisionTransitionAllowed(expectedState, newState) {
		return false, nil
	}
	res, err := r.db.Exec(`UPDATE mcp_provision
		SET state = ?, generation = generation + 1
		WHERE request_id = ? AND state = ? AND generation = ?`,
		newState, requestID, expectedState, expectedGeneration)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

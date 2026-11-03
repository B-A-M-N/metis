package main

// provision_owner.go: durable session-owner bindings for the
// principal-scoped controller policy.
//
// A provision records which verified principal owns the worker.
// Remote session-targeting tools (terminal read, terminal input,
// send, session stop) admit only the owning principal; anything
// else is refused before leases, generations, or backends are
// touched. A lease is arbitration, not authorization: holding one
// never confers access. Local (non-remote) transport bypasses the
// check — the machine owner retains full control — and
// fleet-visible discovery (status, agents listing) never exposes
// session content.
//
// Binding happens BEFORE worker launch, inside ONE transaction
// with the provision intent: both commit together or roll back
// together. A failed binding fails the provision with no worker
// started, never a false success. First writer wins; replays and
// reconciliations never touch the row.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// OwnerStore is the write surface the owner binding needs. Both
// *sql.DB and *sql.Tx satisfy it, so the same claim runs alone
// (single-statement, inherently atomic) or inside the reservation
// transaction (committing with the intent or rolling back with it).
type OwnerStore interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func ensureProvisionOwnerTable(db OwnerStore) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS provision_owner (
		session TEXT PRIMARY KEY,
		principal TEXT NOT NULL,
		created TEXT NOT NULL
	)`)
	return err
}

// claimProvisionOwner binds session to principal when unbound and
// reports whether a different principal already owns it. First
// writer wins via insert-if-absent: a conflicting admission never
// overwrites, so a lost race refuses instead of silently
// reassigning ownership. An empty principal binds nothing (legacy
// local callers carry none) and never reports a conflict.
func claimProvisionOwner(db OwnerStore, session, principal string) (ownedByOther bool, err error) {
	session = ptyBare(strings.TrimSpace(session))
	principal = strings.TrimSpace(principal)
	if session == "" {
		return false, fmt.Errorf("provision owner: empty session")
	}
	if principal == "" {
		return false, nil
	}
	if err := ensureProvisionOwnerTable(db); err != nil {
		return false, err
	}
	if _, err := db.Exec(`INSERT INTO provision_owner(session, principal, created)
		VALUES(?, ?, ?) ON CONFLICT(session) DO NOTHING`,
		session, principal, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return false, err
	}
	current, found, err := lookupProvisionOwner(db, session)
	if err != nil {
		return false, err
	}
	if found && current != principal {
		return true, nil
	}
	return false, nil
}

// lookupProvisionOwner returns the owning principal and whether a
// binding exists. No row means the session predates owner
// tracking (or was never provisioned through the tool): remote
// callers are refused fail-closed, never grandfathered in.
func lookupProvisionOwner(db OwnerStore, session string) (string, bool, error) {
	session = ptyBare(strings.TrimSpace(session))
	if session == "" {
		return "", false, nil
	}
	if err := ensureProvisionOwnerTable(db); err != nil {
		return "", false, err
	}
	var principal string
	err := db.QueryRow(`SELECT principal FROM provision_owner WHERE session = ?`, session).Scan(&principal)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return principal, true, nil
}

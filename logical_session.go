package main

// logical_session.go: one logical session record with a typed backend.
//
// Metis executes sessions on three backends with different lifecycles
// (tmux registry rows, managed PTY supervisor rows, Muse-native
// bindings), but every session — however it executes — is one logical
// session: a name, a backend identity, and an authorized workspace
// root. This file owns that record.
//
// The record is the only durable home a Muse-native session needs:
// native provisioning creates it (authorized root, provider, model,
// profile) without starting any tmux session or PTY supervisor, and
// the bind path attaches the Muse native session to it later. Shell
// sessions keep their existing registries untouched — a shell-backed
// name always resolves from its own store first, so this record can
// never shadow or stale a live tmux/PTY session; it is only the
// fallback that lets native-only sessions be
// created/discovered/used/observed/recovered with no dummy shell
// session behind them.

import (
	"database/sql"
	"fmt"
	"strings"
)

// Logical session states. active means the session is established
// and eligible for adoption/binding; stopped is terminal (recorded
// only after the backend confirms the stop) and never adopted.
const (
	logicalActive  = "active"
	logicalStopped = "stopped"
)

// logicalSession is one named session on one backend. Workspace is
// the canonical authorized root the session executes in. NativeID is
// set for muse-native sessions once bound (empty until then);
// shell-backed sessions never carry a logical row.
type logicalSession struct {
	Name       string
	Backend    string
	Workspace  string
	Harness    string
	Model      string
	Provider   string
	Profile    string
	Run        string
	Objective  string
	NativeID   string
	State      string
	Generation int64
	Created    string
	Updated    string
}

// ensureLogicalSessionTable creates the logical session store and
// migrates older databases forward without touching existing rows.
func ensureLogicalSessionTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS logical_sessions (
		name TEXT PRIMARY KEY,
		backend TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		harness TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		run TEXT NOT NULL DEFAULT '',
		objective TEXT NOT NULL DEFAULT '',
		native_id TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'active',
		generation INTEGER NOT NULL DEFAULT 0,
		created TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	for _, col := range [][2]string{
		{"backend", `TEXT NOT NULL DEFAULT ''`},
		{"workspace", `TEXT NOT NULL DEFAULT ''`},
		{"harness", `TEXT NOT NULL DEFAULT ''`},
		{"model", `TEXT NOT NULL DEFAULT ''`},
		{"provider", `TEXT NOT NULL DEFAULT ''`},
		{"profile", `TEXT NOT NULL DEFAULT ''`},
		{"run", `TEXT NOT NULL DEFAULT ''`},
		{"objective", `TEXT NOT NULL DEFAULT ''`},
		{"native_id", `TEXT NOT NULL DEFAULT ''`},
		{"state", `TEXT NOT NULL DEFAULT 'active'`},
		{"generation", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if err := ensureColumn(db, "logical_sessions", col[0], col[1]); err != nil {
			return err
		}
	}
	return nil
}

// loadLogicalSession returns the logical record for one session
// name, or found=false when no logical session was ever established
// under it. Shell-backed sessions normally have no row; that is not
// an error — callers fall back to the shell stores.
func loadLogicalSession(name string) (logicalSession, bool, error) {
	var ls logicalSession
	db, err := openTasksDB()
	if err != nil {
		return ls, false, err
	}
	defer db.Close()
	if err := ensureLogicalSessionTable(db); err != nil {
		return ls, false, err
	}
	var generation sql.NullInt64
	qerr := db.QueryRow(`SELECT name, backend, workspace, harness, model, provider, profile,
		run, objective, native_id, state, generation, created, updated
		FROM logical_sessions WHERE name = ?`, name).Scan(
		&ls.Name, &ls.Backend, &ls.Workspace, &ls.Harness, &ls.Model, &ls.Provider,
		&ls.Profile, &ls.Run, &ls.Objective, &ls.NativeID, &ls.State,
		&generation, &ls.Created, &ls.Updated)
	if qerr == sql.ErrNoRows {
		return logicalSession{}, false, nil
	}
	if qerr != nil {
		return logicalSession{}, false, qerr
	}
	if generation.Valid {
		ls.Generation = generation.Int64
	}
	return ls, true, nil
}

// claimLogicalSession records one logical session. It reports
// (false, nil) when the name is already established: the caller
// loads the winner and converges or conflicts, exactly like the
// provision intent store. A stopped row never un-stops through a
// claim — re-provision under a new name instead.
func claimLogicalSession(want logicalSession) (bool, error) {
	if strings.TrimSpace(want.Name) == "" {
		return false, fmt.Errorf("logical session needs a name")
	}
	if want.Backend != TargetTMUX && want.Backend != TargetPTY && want.Backend != TargetMuseNative {
		return false, fmt.Errorf("unknown logical backend %q (want tmux|pty|muse-native)", want.Backend)
	}
	if strings.TrimSpace(want.Workspace) == "" {
		return false, fmt.Errorf("logical session %q needs an authorized workspace root", want.Name)
	}
	db, err := openTasksDB()
	if err != nil {
		return false, err
	}
	defer db.Close()
	if err := ensureLogicalSessionTable(db); err != nil {
		return false, err
	}
	state := want.State
	if state == "" {
		state = logicalActive
	}
	res, err := db.Exec(`INSERT INTO logical_sessions
		(name, backend, workspace, harness, model, provider, profile, run, objective, native_id, state, generation, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(name) DO NOTHING`,
		want.Name, want.Backend, want.Workspace, want.Harness, want.Model, want.Provider,
		want.Profile, want.Run, want.Objective, want.NativeID, state, nowUTC(), nowUTC())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// recordLogicalNativeID attaches the bound Muse native session to an
// established logical record. It is a no-op when no logical row
// exists (shell-backed sessions bind without one), so the bind path
// behaves identically with or without a logical session. A different
// native ID already recorded is refused, never silently swapped.
func recordLogicalNativeID(session, nativeID string) error {
	if strings.TrimSpace(nativeID) == "" {
		return fmt.Errorf("logical native identity must not be empty")
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLogicalSessionTable(db); err != nil {
		return err
	}
	var existing string
	qerr := db.QueryRow(`SELECT native_id FROM logical_sessions WHERE name = ?`, session).Scan(&existing)
	if qerr == sql.ErrNoRows {
		return nil
	}
	if qerr != nil {
		return qerr
	}
	if existing != "" && existing != nativeID {
		return fmt.Errorf("logical session %s already names native session %s; refusing silent swap to %s", session, existing, nativeID)
	}
	if existing == nativeID {
		return nil
	}
	_, err = db.Exec(`UPDATE logical_sessions SET native_id = ?, updated = ? WHERE name = ?`,
		nativeID, nowUTC(), session)
	return err
}

// stopLogicalSession marks one logical session stopped. The write is
// fenced on (active, generation): only the current active record
// commits, and the generation bumps so a stale holder's late write
// can never resurrect it. A stopped session is never adopted as
// live, by provisioning or by dispatch.
func stopLogicalSession(name string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLogicalSessionTable(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE logical_sessions
		SET state = ?, generation = generation + 1, updated = ?
		WHERE name = ? AND state = ?`, logicalStopped, nowUTC(), name, logicalActive)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("logical session %q is not active; refusing stop", name)
	}
	return nil
}

// nativeIdentityMatches reports whether an established logical
// record is the session a native provision intent names: same typed
// backend, same canonical workspace, same provider/profile scope.
// Model follows the binding rule — a declared model that changed is
// a different execution environment, while an undeclared one stays
// unscoped — so empty on either side matches.
func nativeIdentityMatches(cur logicalSession, want mcpProvisionRecord) bool {
	if cur.Backend != provisionBackendNative || want.Backend != provisionBackendNative {
		return false
	}
	if !provisionWorkspaceMatches(cur.Workspace, want.Workdir) {
		return false
	}
	if cur.Provider != want.Provider || cur.Profile != want.Profile {
		return false
	}
	if cur.Model != "" && want.Model != "" && cur.Model != want.Model {
		return false
	}
	return true
}

// resolveSessionWorkspace is the workdir-decoupling resolver: the
// workspace a session executes in, however the session is backed. A
// shell-backed name (tmux registry or PTY row) always resolves from
// its own store first — the live store stays authoritative over any
// record, so a re-homed shell session is observed, never masked. A
// name with no shell backing resolves from its logical record, which
// is what lets a Muse-native session bind, turn, and resume with no
// tmux session or PTY supervisor behind it. A name with neither is
// an error, never an empty root a caller could mistake for "here".
func resolveSessionWorkspace(session string) (string, error) {
	if wd, err := sessionWorkdir(session); err == nil {
		return wd, nil
	} else {
		serr := err
		ls, found, lerr := loadLogicalSession(session)
		if lerr != nil {
			return "", serr
		}
		if !found {
			return "", serr
		}
		if strings.TrimSpace(ls.Workspace) == "" {
			return "", fmt.Errorf("logical session %q names no workspace", session)
		}
		return ls.Workspace, nil
	}
}

// resolveLogicalModelFallback serves the model-decoupling edit in
// muse_bindings.go: the logical record behind a name with no shell
// backing, or ("", false) when there is none. Shell misses stay
// errors — an undeclared model reads back empty (server default),
// never a refusal.
func resolveLogicalModelFallback(session string) (string, bool) {
	ls, found, lerr := loadLogicalSession(session)
	if lerr != nil || !found {
		return "", false
	}
	return ls.Model, true
}

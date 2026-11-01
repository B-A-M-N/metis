package main

// muse_bindings.go: the durable map between a Metis worker session
// and its native Muse session. One row per Metis session: the
// server-minted native session ID (immutable once bound — a rebind
// to a different native ID is refused, never silently swapped), the
// latest observed view cursor (opaque, replay position), and the
// provider the host was started with.
//
// The native session itself lives in muse's store; this table is
// only Metis's pointer to it plus the replay position. Losing the
// row never orphans execution (the native session persists), but
// the cursor position is lost — resume then serves without a cursor.

import (
	"database/sql"
	"fmt"
)

type museBinding struct {
	Session  string `json:"session"`
	NativeID string `json:"native_id"`
	Cursor   string `json:"view_cursor"`
	Provider string `json:"provider"`
	// Profile names the isolated muse home the native session lives
	// in ("": ambient). It selects the store exactly like the
	// provider does; resuming under another profile is refused.
	Profile string `json:"profile"`
	// Workspace is the canonical execution root the native
	// session was bound to. Turns execute only while the Metis
	// session still resolves to this root; a re-homed session
	// is refused, never silently followed.
	Workspace string `json:"workspace"`
	// Model is the operator-declared harness model at bind time
	// ("": undeclared). A declared model that no longer matches
	// the session is refused; an undeclared one stays unscoped.
	Model string `json:"model"`
	// CursorTurn attributes the committed replay position to the
	// terminal turn that produced it. Cursors are opaque, so
	// recency cannot be compared; attribution keeps every advance
	// accountable to the turn that observed it.
	CursorTurn string `json:"cursor_turn"`
	Created    string `json:"created"`
	Updated    string `json:"updated"`
}

func ensureMuseTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_bindings (
		session TEXT PRIMARY KEY,
		native_id TEXT NOT NULL,
		view_cursor TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL,
		updated TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if !museBindingHasColumn(db, "profile") {
		// Pre-profile rows predate the column; backfill ambient ("").
		if _, err := db.Exec(`ALTER TABLE muse_bindings ADD COLUMN profile TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	// Older rows predate workspace/model scoping; the columns
	// backfill empty (unscoped) and new binds always set them.
	if err := ensureColumn(db, "muse_bindings", "workspace", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	if err := ensureColumn(db, "muse_bindings", "model", `TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}
	return ensureColumn(db, "muse_bindings", "cursor_turn", `TEXT NOT NULL DEFAULT ''`)
}

// museBindingHasColumn reports whether muse_bindings already
// carries a named column (same shape as artifactHasColumn).
func museBindingHasColumn(db *sql.DB, want string) bool {
	rows, err := db.Query(`PRAGMA table_info(muse_bindings)`)
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

// bindMuseSession records a Metis session's native counterpart.
// Binding is an attributed state change. Rebinding the same native
// ID refreshes the cursor (idempotent); binding a different native
// ID is refused — identity swaps must be explicit, never silent.
func bindMuseSession(session, nativeID, cursor, provider, profile string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	if session == "" || nativeID == "" {
		return fmt.Errorf("muse bind requires a session and a native session id")
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseTables(db); err != nil {
		return err
	}
	// The binding names the execution scope, not just the native
	// session: a session with no resolvable workspace cannot be
	// bound, because turns would have no authorized root. The
	// workspace resolves decoupled from shell backing — a
	// native-only logical session binds with no tmux session or PTY
	// supervisor behind it.
	workdir, werr := resolveSessionWorkspace(session)
	if werr != nil {
		return fmt.Errorf("muse bind refused for %s: %v", session, werr)
	}
	workspace, werr := canonicalRoot(workdir)
	if werr != nil {
		return fmt.Errorf("muse bind refused for %s: %v", session, werr)
	}
	model, merr := resolveSessionModel(session)
	if merr != nil {
		return fmt.Errorf("muse bind refused for %s: %v", session, merr)
	}
	var existing, existingProfile, existingProvider, existingWorkspace, existingModel string
	err = db.QueryRow(`SELECT native_id, profile, provider, workspace, model FROM muse_bindings WHERE session = ?`, session).Scan(&existing, &existingProfile, &existingProvider, &existingWorkspace, &existingModel)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.Exec(`INSERT INTO muse_bindings (session, native_id, view_cursor, provider, profile, workspace, model, created, updated)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, session, nativeID, cursor, provider, profile, workspace, model, nowUTC(), nowUTC())
		if err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if existing != nativeID {
			return fmt.Errorf("session %s already bound to native session %s; refusing silent rebind to %s", session, existing, nativeID)
		}
		if existingProfile != profile {
			return fmt.Errorf("session %s bound under muse profile %q, not %q; refusing cross-profile rebind", session, existingProfile, profile)
		}
		if existingProvider != provider {
			return fmt.Errorf("session %s bound with provider %q, not %q; refusing silent provider switch (explicit migration required)", session, existingProvider, provider)
		}
		if existingWorkspace != "" && existingWorkspace != workspace {
			return fmt.Errorf("session %s bound to workspace %s, not %s; refusing silent re-home (explicit migration required)", session, existingWorkspace, workspace)
		}
		if existingModel != "" && model != "" && existingModel != model {
			return fmt.Errorf("session %s bound with model %q, not %q; refusing silent model switch (explicit migration required)", session, existingModel, model)
		}
		if _, err := db.Exec(`UPDATE muse_bindings SET view_cursor = ?, workspace = ?, model = ?, updated = ? WHERE session = ?`,
			cursor, workspace, model, nowUTC(), session); err != nil {
			return err
		}
	}
	// A logical record behind this name learns the bound native
	// session, completing the one-record lifecycle (provision →
	// bind → turn). Shell-backed sessions carry no logical row and
	// skip this untouched.
	if err := recordLogicalNativeID(session, nativeID); err != nil {
		return err
	}
	recordReceipt(session, "muse_bound", fmt.Sprintf("actor=%s req=%s native=%s provider=%s profile=%s workspace=%s model=%s", req.Actor, req.RequestID, nativeID, provider, profile, workspace, model))
	return nil
}

// resolveSessionModel is the one authoritative model source for
// every session kind: PTY sessions read their metadata, registry
// sessions read the registry, and native-only sessions read their
// logical record. A name with no backing anywhere is an error —
// never a silent empty model that later code could mistake for
// "server default".
func resolveSessionModel(session string) (string, error) {
	if isPtySession(session) {
		m, err := ptyGet(ptyBare(session))
		if err != nil {
			return "", err
		}
		return m.Model, nil
	}
	if e, ok := loadRegistry()[session]; ok {
		return e.Model, nil
	}
	if model, found := resolveLogicalModelFallback(session); found {
		return model, nil
	}
	return "", fmt.Errorf("no such session: %s", session)
}

func getMuseBinding(session string) (museBinding, error) {
	var b museBinding
	db, err := openTasksDB()
	if err != nil {
		return b, err
	}
	defer db.Close()
	if err := ensureMuseTables(db); err != nil {
		return b, err
	}
	err = db.QueryRow(`SELECT session, native_id, view_cursor, provider, profile, workspace, model, cursor_turn, created, updated
		FROM muse_bindings WHERE session = ?`, session).Scan(
		&b.Session, &b.NativeID, &b.Cursor, &b.Provider, &b.Profile, &b.Workspace, &b.Model, &b.CursorTurn, &b.Created, &b.Updated)
	if err == sql.ErrNoRows {
		return b, fmt.Errorf("no muse binding for session %s", session)
	}
	return b, err
}

// updateMuseCursor advances the stored replay position after the
// terminal turn that observed it. Only turn-terminal writers may
// call it (managed turns, direct turn commands); point-in-time
// reads never advance it. An empty cursor is a no-op, never a
// blank: blanking would flip resume to from-now mode and silently
// drop unobserved history. The advance is attributed to its turn
// because cursors are opaque and cannot be ordered.
func updateMuseCursor(session, cursor, turnID string) error {
	if cursor == "" {
		return nil
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseTables(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE muse_bindings SET view_cursor = ?, cursor_turn = ?, updated = ? WHERE session = ?`,
		cursor, turnID, nowUTC(), session)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("no muse binding for session %s", session)
	}
	return nil
}

package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// muse_session_store.go: persistent native session-intent record
// (create / claim / replay-load).
//
// One row per caller-minted command identity binds a session/start
// command to the native session the server created for it. The
// command identity is persisted BEFORE transmitting session/start,
// exactly like turn intents: an acknowledgment loss leaves the
// intent submitted, and a retry with the same caller key returns
// the already-created session instead of minting another.
//
// State machine: submitted -> acknowledged. Only the ack moves the
// row out of submitted. A lost ack must NEVER create a second
// native session on replay: the replay path re-sends the SAME
// command identity and the server deduplicates it to the original
// session (proven by TestMuseEchoLifecycle's commandId-replay
// section; hermetic refusal coverage in TestStartRefusesLyingServer).
//
// Native protocol adaptation (command allocation, wire calls, ack
// parsing, turn lifecycle) stays in muse_sessions.go; this file
// owns durability and nothing else.

// museSessionIntent is the durable identity of one session/start
// command: the caller-minted command ID plus, once acknowledged,
// the server-minted native session it created.
//
// The bind path keys retry identity one level up: RequestID is the
// MCP request_id the bind was admitted under (one durable command
// per request key, claimed before Muse is ever called), and the
// frozen scope (session/workspace/provider/profile/model) plus the
// payload fingerprint pin what that command may create. A reused
// request key with a different payload is a conflict, never a
// silent convergence; an acknowledged intent with no binding row is
// adoptable by reconcile instead of minting a second session.
type museSessionIntent struct {
	CommandID  string
	NativeID   string
	ViewCursor string
	State      string // submitted | acknowledged
	// Bind retry identity: empty for intents minted directly by the
	// lower-level session/start path, which keys only on CommandID.
	RequestID          string
	Principal          string
	Session            string
	Workspace          string
	Provider           string
	Profile            string
	Model              string
	PayloadFingerprint string
	Created            string
	Updated            string
}

// Session-intent states. Only an ack enters acknowledged.
const (
	museSessionIntentSubmitted    = "submitted"
	museSessionIntentAcknowledged = "acknowledged"
)

// ensureMuseSessionIntentTable records one session/start command
// identity per caller key so a retried creation reuses the same
// server command instead of minting a second native session.
func ensureMuseSessionIntentTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_session_intents (
		command_id TEXT NOT NULL PRIMARY KEY,
		session TEXT NOT NULL DEFAULT '',
		native_id TEXT NOT NULL DEFAULT '',
		view_cursor TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'submitted',
		request_id TEXT NOT NULL DEFAULT '',
		principal TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		payload_fingerprint TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return err
	}
	// Pre-extension rows predate bind retry identity; backfill the
	// columns so frozen-identity comparison has a value on both
	// sides. request_id stays the lookup key via the index below
	// (ADD COLUMN cannot carry a UNIQUE constraint).
	for _, col := range [][2]string{
		{"request_id", `TEXT NOT NULL DEFAULT ''`},
		{"principal", `TEXT NOT NULL DEFAULT ''`},
		{"workspace", `TEXT NOT NULL DEFAULT ''`},
		{"provider", `TEXT NOT NULL DEFAULT ''`},
		{"profile", `TEXT NOT NULL DEFAULT ''`},
		{"model", `TEXT NOT NULL DEFAULT ''`},
		{"payload_fingerprint", `TEXT NOT NULL DEFAULT ''`},
	} {
		if err := ensureColumn(db, "muse_session_intents", col[0], col[1]); err != nil {
			return err
		}
	}
	// Partial: lower-level intents minted directly by the
	// session/start path carry no request key (''), and must never
	// collide with each other — only bind-claimed rows are unique
	// per request key.
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_muse_session_intents_request ON muse_session_intents(request_id) WHERE request_id != ''`)
	return err
}

// loadSessionIntent returns the durable record for one command
// identity, or found=false when this command was never persisted.
// A transported/acquire failure is an error; an unknown command is
// not — the caller mints it via createSessionIntent.
func loadSessionIntent(commandID string) (intent museSessionIntent, found bool, err error) {
	db, err := openTasksDB()
	if err != nil {
		return museSessionIntent{}, false, err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return museSessionIntent{}, false, err
	}
	intent = museSessionIntent{CommandID: commandID}
	qerr := db.QueryRow(`SELECT native_id, view_cursor, state, request_id, principal,
		session, workspace, provider, profile, model, payload_fingerprint, created, updated
		FROM muse_session_intents WHERE command_id = ?`,
		commandID).Scan(&intent.NativeID, &intent.ViewCursor, &intent.State,
		&intent.RequestID, &intent.Principal, &intent.Session, &intent.Workspace,
		&intent.Provider, &intent.Profile, &intent.Model, &intent.PayloadFingerprint,
		&intent.Created, &intent.Updated)
	if qerr == sql.ErrNoRows {
		return museSessionIntent{}, false, nil
	}
	if qerr != nil {
		return museSessionIntent{}, false, qerr
	}
	return intent, true, nil
}

// createSessionIntent persists a fresh submitted intent for one
// command identity BEFORE the session/start transmission. A
// conflicting row (same command issued concurrently) surfaces the
// constraint error unchanged — same as the inline insert this
// replaces — so concurrent creators converge on one record.
func createSessionIntent(commandID string) error {
	if strings.TrimSpace(commandID) == "" {
		return fmt.Errorf("session intent needs a caller command identity; refusing to mint inside the retry path")
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO muse_session_intents
		(command_id, native_id, view_cursor, state, created, updated)
		VALUES (?, '', '', 'submitted', ?, ?)`,
		commandID, nowUTC(), nowUTC())
	return err
}

// bindPayloadFingerprint freezes what one bind command may create:
// the Metis session, the authorized scope (workspace/provider/
// profile/model), and the title handed to session/start. A reused
// request key naming anything else is a conflict, never a silent
// convergence.
func bindPayloadFingerprint(session, provider, profile, title, workspace, model string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		session, provider, profile, title, workspace, model,
	}, "\x00")))
	return fmt.Sprintf("sha256:%x", sum)
}

// museBindIntentConflictError marks a frozen-identity refusal: the
// request key already owns a bind command with a different payload
// or principal. That command may already have created a native
// session server-side, so a conflict is never proof nothing was
// sent — the caller must reconcile, never mint a fresh command.
type museBindIntentConflictError struct{ err error }

func (e *museBindIntentConflictError) Error() string { return e.err.Error() }
func (e *museBindIntentConflictError) Unwrap() error { return e.err }

func museBindIntentConflict(format string, args ...any) error {
	return &museBindIntentConflictError{err: fmt.Errorf(format, args...)}
}

// isBindIntentConflict reports whether err is a bind frozen-identity
// refusal (possibly-transmitted), as opposed to a proven-unsent
// failure that is safe to retry under a new key.
func isBindIntentConflict(err error) bool {
	var c *museBindIntentConflictError
	return errors.As(err, &c)
}

// loadBindIntentByRequest returns the durable bind command for one
// MCP request key, or found=false when this key was never claimed.
// A transported/acquire failure is an error; an unknown key is not —
// the caller claims it via claimBindIntent.
func loadBindIntentByRequest(requestID string) (intent museSessionIntent, found bool, err error) {
	db, err := openTasksDB()
	if err != nil {
		return museSessionIntent{}, false, err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return museSessionIntent{}, false, err
	}
	intent = museSessionIntent{RequestID: requestID}
	qerr := db.QueryRow(`SELECT command_id, native_id, view_cursor, state, principal,
		session, workspace, provider, profile, model, payload_fingerprint, created, updated
		FROM muse_session_intents WHERE request_id = ?`,
		requestID).Scan(&intent.CommandID, &intent.NativeID, &intent.ViewCursor, &intent.State,
		&intent.Principal, &intent.Session, &intent.Workspace, &intent.Provider,
		&intent.Profile, &intent.Model, &intent.PayloadFingerprint,
		&intent.Created, &intent.Updated)
	if qerr == sql.ErrNoRows {
		return museSessionIntent{}, false, nil
	}
	if qerr != nil {
		return museSessionIntent{}, false, qerr
	}
	return intent, true, nil
}

// checkBindIntentPayload refuses a reused request key whose frozen
// payload or principal no longer matches: the stored command may
// already be running server-side, so the retry must reconcile
// through it instead of forking a duplicate.
func checkBindIntentPayload(intent museSessionIntent, principal, fingerprint string) error {
	if intent.PayloadFingerprint != "" && intent.PayloadFingerprint != fingerprint {
		return museBindIntentConflict("bind_intent_conflict: request_id %q already owns bind command %s with a different payload; refusing changed-payload replay (retry with the same arguments to converge, or a new request_id for different work)",
			intent.RequestID, intent.CommandID)
	}
	if intent.Principal != "" && principal != "" && intent.Principal != principal {
		return museBindIntentConflict("bind_intent_conflict: request_id %q was claimed by a different principal; refusing cross-principal replay", intent.RequestID)
	}
	return nil
}

// claimBindIntent is the claim-or-recover gate for the bind path:
// exactly one durable session/start command identity per MCP
// request key, frozen with the payload it may create. The first
// claim mints the UUIDv7 command (the server refuses non-v7
// commandIds) and persists it BEFORE Muse is ever called; a retry
// with the same key and payload recovers the same command, so the
// lower-level dedup in muse_session_rpc.go replays the already
// created session instead of minting a second one. A retry with a
// different payload or principal is a conflict. A submitted
// (unacked) intent stays recoverable: the retry re-sends the same
// command, which the server deduplicates.
func claimBindIntent(req OperationRequest, session, provider, profile, title, workspace, model string) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(req.RequestID) == "" {
		return "", fmt.Errorf("bind intent needs a caller request identity; refusing to mint inside the retry path")
	}
	fingerprint := bindPayloadFingerprint(session, provider, profile, title, workspace, model)
	if intent, found, err := loadBindIntentByRequest(req.RequestID); err != nil {
		return "", err
	} else if found {
		if err := checkBindIntentPayload(intent, req.Principal, fingerprint); err != nil {
			return "", err
		}
		return intent.CommandID, nil
	}
	commandID, err := newMuseCommandID()
	if err != nil {
		return "", err
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return "", err
	}
	_, err = db.Exec(`INSERT INTO muse_session_intents
		(command_id, native_id, view_cursor, state, request_id, principal,
			session, workspace, provider, profile, model, payload_fingerprint, created, updated)
		VALUES (?, '', '', 'submitted', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		commandID, req.RequestID, req.Principal,
		session, workspace, provider, profile, model, fingerprint, nowUTC(), nowUTC())
	if err != nil {
		// A concurrent claim won the key: converge on the winner
		// when it carries the same payload, conflict when not.
		if strings.Contains(err.Error(), "UNIQUE") {
			if intent, found, lerr := loadBindIntentByRequest(req.RequestID); lerr != nil {
				return "", lerr
			} else if found {
				if cerr := checkBindIntentPayload(intent, req.Principal, fingerprint); cerr != nil {
					return "", cerr
				}
				return intent.CommandID, nil
			}
		}
		return "", err
	}
	recordReceipt(req.RequestID, "bind_intent",
		fmt.Sprintf("actor=%s req=%s cmd=%s session=%s state=submitted", req.Actor, req.RequestID, commandID, session))
	return commandID, nil
}

// findAckedUnboundBindIntent is the reconcile lookup for the
// acked-but-unbound gap: session/start answered (intent
// acknowledged) but the crash landed before the binding row
// committed. It returns the newest acknowledged intent for the
// same session and authorized scope whose native session has no
// binding row yet, so the bind path adopts it instead of minting
// a second native session. A bound session, or an intent for a
// different scope, never matches.
func findAckedUnboundBindIntent(session, provider, profile, workspace, model string) (museSessionIntent, bool, error) {
	var out museSessionIntent
	db, err := openTasksDB()
	if err != nil {
		return out, false, err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return out, false, err
	}
	if err := ensureMuseTables(db); err != nil {
		return out, false, err
	}
	var bound string
	if err := db.QueryRow(`SELECT native_id FROM muse_bindings WHERE session = ?`, session).Scan(&bound); err != nil && err != sql.ErrNoRows {
		return out, false, err
	} else if err == nil {
		// Already bound: nothing to reconcile. A rebind to a
		// different native ID stays refused at the binding row,
		// never silently swapped.
		return out, false, nil
	}
	qerr := db.QueryRow(`SELECT command_id, request_id, native_id, view_cursor, principal,
		provider, profile, model, payload_fingerprint, created, updated
		FROM muse_session_intents
		WHERE session = ? AND workspace = ? AND provider = ? AND profile = ? AND model = ?
			AND state = 'acknowledged' AND native_id != ''
		ORDER BY updated DESC LIMIT 1`,
		session, workspace, provider, profile, model).Scan(
		&out.CommandID, &out.RequestID, &out.NativeID, &out.ViewCursor, &out.Principal,
		&out.Provider, &out.Profile, &out.Model, &out.PayloadFingerprint,
		&out.Created, &out.Updated)
	if qerr == sql.ErrNoRows {
		return out, false, nil
	}
	if qerr != nil {
		return out, false, qerr
	}
	out.Session = session
	out.Workspace = workspace
	out.State = museSessionIntentAcknowledged
	return out, true, nil
}

// acknowledgeSessionIntent claims the intent with the server's
// answer: the minted native session plus the starting view cursor.
// Idempotent on (command_id): replaying an ack for an already
// acknowledged intent rewrites the same values.
func acknowledgeSessionIntent(commandID, nativeID, viewCursor string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseSessionIntentTable(db); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE muse_session_intents SET native_id = ?, view_cursor = ?,
		state = 'acknowledged', updated = ? WHERE command_id = ?`,
		nativeID, viewCursor, nowUTC(), commandID)
	return err
}

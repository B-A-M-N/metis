package main

// muse_turn_intent.go: durable native-turn execution identity.
//
// The commandId sent with turn/start is minted and persisted BEFORE
// the submit, bound to the exact task attempt, session, native
// session, profile, provider, and prompt digest. A submit error
// therefore leaves a `submitted` intent row instead of a refunded
// attempt: the outcome is unknown, and a retry reuses the same
// commandId with the identical payload, which the server
// deduplicates instead of executing twice.
//
// The intent is the execution identity reconciliation needs: kill
// Metis after Muse accepts but before the ack arrives, and the
// recovered intent still names the same command.

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

// turnIntent binds one turn/start command to the attempt it serves.
type turnIntent struct {
	CommandID    string `json:"command_id"`
	TaskID       string `json:"task_id"`
	Attempt      int    `json:"attempt"`
	Session      string `json:"session"`
	NativeID     string `json:"native_session_id"`
	Profile      string `json:"profile"`
	Provider     string `json:"provider"`
	Workspace    string `json:"workspace"`
	PromptDigest string `json:"prompt_digest"`
	TurnID       string `json:"turn_id"`
	State        string `json:"state"` // submitted | acknowledged
	Created      string `json:"created"`
	Updated      string `json:"updated"`
}

func ensureTurnIntentTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_turn_intents (
		task_id TEXT NOT NULL,
		attempt INTEGER NOT NULL DEFAULT 0,
		session TEXT NOT NULL DEFAULT '',
		native_id TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		command_id TEXT NOT NULL DEFAULT '',
		prompt_digest TEXT NOT NULL DEFAULT '',
		turn_id TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL DEFAULT 'submitted',
		created TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (task_id, attempt)
	)`)
	if err != nil {
		return err
	}
	// Older rows predate workspace scoping; backfill the column so
	// frozen-identity comparison has a value on both sides.
	return ensureColumn(db, "muse_turn_intents", "workspace", `TEXT NOT NULL DEFAULT ''`)
}

func turnPromptDigest(prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return fmt.Sprintf("sha256:%x", sum)
}

// museIntentConflictError marks a frozen-identity refusal: the
// attempt already owns a submitted command with a different binding
// or payload digest. That command may already be running
// server-side, so a conflict is never proof nothing was sent and
// must never trigger a pre-submit refund — the attempt stays fenced
// for reconciliation.
type museIntentConflictError struct{ err error }

func (e *museIntentConflictError) Error() string { return e.err.Error() }
func (e *museIntentConflictError) Unwrap() error { return e.err }

func museIntentConflict(format string, args ...any) error {
	return &museIntentConflictError{err: fmt.Errorf(format, args...)}
}

// isTurnIntentConflict reports whether err is a frozen-identity
// refusal (possibly-transmitted), as opposed to a proven-unsent
// failure that is safe to refund.
func isTurnIntentConflict(err error) bool {
	var c *museIntentConflictError
	return errors.As(err, &c)
}

// ensureTurnIntent returns the commandId this attempt must submit
// with. The first call freezes the execution identity AND the
// payload: the stored command may already be running server-side,
// so a retry reuses it byte-for-byte and the server dedups.
// A changed prompt, session, native session, profile, provider, or
// workspace on the same attempt is an explicit turn_intent_conflict —
// never a silent replacement. To run different work, create an
// authorized replacement attempt or correction task.
func ensureTurnIntent(taskID string, attempt int, session, nativeID, profile, provider, workspace, prompt string, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureTurnIntentTable(db); err != nil {
		return "", err
	}
	digest := turnPromptDigest(prompt)
	var cmdID, oldSession, oldNative, oldProfile, oldProvider, oldWorkspace, oldDigest, state string
	qerr := db.QueryRow(`SELECT command_id, session, native_id, profile, provider, workspace, prompt_digest, state FROM muse_turn_intents
		WHERE task_id = ? AND attempt = ?`, taskID, attempt).Scan(&cmdID, &oldSession, &oldNative, &oldProfile, &oldProvider, &oldWorkspace, &oldDigest, &state)
	if qerr == nil {
		if oldSession != session || oldNative != nativeID || oldProfile != profile || oldProvider != provider || oldWorkspace != workspace {
			return "", museIntentConflict("turn_intent_conflict: task %s attempt %d is frozen to session=%s native=%s profile=%s provider=%s workspace=%s; refusing rebind (create a replacement attempt or correction task)",
				taskID, attempt, oldSession, oldNative, oldProfile, oldProvider, oldWorkspace)
		}
		if oldDigest != digest {
			return "", museIntentConflict("turn_intent_conflict: task %s attempt %d owns submitted command %s with a different payload digest; refusing changed-payload replay (create a replacement attempt or correction task)",
				taskID, attempt, cmdID)
		}
		return cmdID, nil
	}
	if qerr != sql.ErrNoRows {
		return "", qerr
	}
	cmdID, err = newMuseCommandID()
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`INSERT INTO muse_turn_intents
		(task_id, attempt, session, native_id, profile, provider, workspace, command_id, prompt_digest, state, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'submitted', ?, ?)`,
		taskID, attempt, session, nativeID, profile, provider, workspace, cmdID, digest, nowUTC(), nowUTC()); err != nil {
		return "", err
	}
	recordReceipt(taskID, "turn_intent",
		fmt.Sprintf("actor=%s req=%s attempt=%d cmd=%s state=submitted", req.Actor, req.RequestID, attempt, cmdID))
	return cmdID, nil
}

// recordTurnAck marks the intent acknowledged with the server's
// turn id. Only an ack moves the intent out of submitted.
func recordTurnAck(commandID, turnID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureTurnIntentTable(db); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE muse_turn_intents SET turn_id = ?, state = 'acknowledged', updated = ?
		WHERE command_id = ?`, turnID, nowUTC(), commandID)
	return err
}

// getTurnIntent loads the execution identity for one attempt,
// or sql.ErrNoRows when the attempt never reached submit.
func getTurnIntent(taskID string, attempt int) (turnIntent, error) {
	var in turnIntent
	db, err := openTasksDB()
	if err != nil {
		return in, err
	}
	defer db.Close()
	if err := ensureTurnIntentTable(db); err != nil {
		return in, err
	}
	err = db.QueryRow(`SELECT command_id, task_id, attempt, session, native_id,
		profile, provider, workspace, prompt_digest, turn_id, state, created, updated
		FROM muse_turn_intents WHERE task_id = ? AND attempt = ?`,
		taskID, attempt).Scan(&in.CommandID, &in.TaskID, &in.Attempt, &in.Session,
		&in.NativeID, &in.Profile, &in.Provider, &in.Workspace, &in.PromptDigest,
		&in.TurnID, &in.State, &in.Created, &in.Updated)
	return in, err
}

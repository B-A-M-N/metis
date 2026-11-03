package main

// muse_message_service.go: direct Muse message operation.
//
// A bound Muse-native session can receive one structured message as
// a native MSP turn without any terminal attached. This service is
// the smallest durable path for "message a specific Muse Code
// session": it resolves the existing active binding, freezes the
// idempotency identity BEFORE submitting, submits through the
// existing MSP turn path with a stable command ID, and reports only
// what was actually observed.
//
// Delivery vs execution stay distinct by construction:
//   - refused: pre-submit validation failed (no binding, stopped or
//     re-homed session, missing daemon, auth). Nothing was sent.
//   - accepted: Muse admitted the turn (turn/start ack) but the
//     terminal outcome was not waited for (wait <= 0).
//   - unknown: the turn was admitted but its terminal outcome could
//     not be observed (timeout, host death, overflow gap). A retry
//     with the same request ID reconciles instead of resubmitting.
//   - <terminal>: the server-observed terminal value (completed,
//     failed, cancelled, unqueued). Only this proves execution.
//
// Admission is never completion: the ack is not an outcome.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// MuseMessageRequest is the caller-supplied message identity. The
// Actor is an asserted label only; authority comes from the
// authenticated OperationRequest (principal) passed alongside.
type MuseMessageRequest struct {
	SessionID string
	Text      string
	RequestID string
	Actor     string
}

// MuseMessageResult is the observed outcome of one message.
// State is refused, accepted, unknown, or the server-observed
// terminal value. TurnID is empty exactly when nothing was
// submitted (refused before submit).
type MuseMessageResult struct {
	RequestID string
	SessionID string
	TurnID    string
	State     string
	Detail    string
}

// Message states. Terminal values (completed/failed/cancelled/
// unqueued/...) pass through verbatim from the server; the three
// below are Metis's own admission bookkeeping.
const (
	museMessageRefused  = "refused"
	museMessageAccepted = "accepted"
	museMessageUnknown  = "unknown"
)

// museMessageConflictError marks a reused request ID with a
// different payload identity (session, native session, scope, or
// text digest). The stored command may already be running
// server-side, so a conflict never triggers a fresh submit.
type museMessageConflictError struct{ err error }

func (e *museMessageConflictError) Error() string { return e.err.Error() }

func museMessageConflict(format string, args ...any) error {
	return &museMessageConflictError{err: fmt.Errorf(format, args...)}
}

func ensureMuseMessageTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_message_intents (
		request_id TEXT PRIMARY KEY,
		session TEXT NOT NULL DEFAULT '',
		native_id TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		workspace TEXT NOT NULL DEFAULT '',
		prompt_digest TEXT NOT NULL DEFAULT '',
		principal TEXT NOT NULL DEFAULT '',
		command_id TEXT NOT NULL DEFAULT '',
		turn_id TEXT NOT NULL DEFAULT '',
		terminal TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL DEFAULT '',
		updated TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_muse_message_command ON muse_message_intents(command_id)`); err != nil {
		return err
	}
	// Migration for pre-existing tables: additive columns only.
	if _, err := db.Exec(`ALTER TABLE muse_message_intents ADD COLUMN principal TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}

func museMessageDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("sha256:%x", sum)
}

// museMessageOutcome is one persisted intent row: the frozen
// execution identity (command) plus the admission receipt
// (turn) plus the observed terminal outcome. Terminal holds an
// OBSERVED terminal value only and stays empty while the outcome
// is unresolved; unresolved is reported as unknown, never stored
// as a terminal, so a transient failure can never overwrite a
// completed result.
type museMessageOutcome struct {
	Session   string
	NativeID  string
	Command   string
	TurnID    string
	Terminal  string
	Detail    string
	Principal string
}

func isUniqueConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func claimMuseMessageRow(msg MuseMessageRequest, b museBinding, oldSession, oldNative, oldProfile, oldProvider, oldWorkspace, oldDigest, oldCmd, oldTurn, oldPrincipal, newPrincipal string) (commandID, priorTurnID string, replay bool, err error) {
	if oldSession != msg.SessionID || oldNative != b.NativeID || oldProfile != b.Profile ||
		oldProvider != b.Provider || oldWorkspace != b.Workspace {
		return "", "", false, museMessageConflict(
			"muse message conflict: request_id %q is frozen to session=%s native=%s profile=%s provider=%s workspace=%s; refusing rebind",
			msg.RequestID, oldSession, oldNative, oldProfile, oldProvider, oldWorkspace)
	}
	if oldDigest != museMessageDigest(msg.Text) {
		return "", "", false, museMessageConflict(
			"muse message conflict: request_id %q already carries a different payload digest; refusing changed-payload replay",
			msg.RequestID)
	}
	// A frozen intent belongs to the principal that created it. A
	// different non-empty principal replaying the same key is a
	// conflict, never a shared turn. Empty principals are the
	// pre-principal local-operator rows: they stay replayable.
	if oldPrincipal != "" && newPrincipal != "" && oldPrincipal != newPrincipal {
		return "", "", false, museMessageConflict(
			"muse message conflict: request_id %q is owned by another principal; refusing cross-principal replay",
			msg.RequestID)
	}
	return oldCmd, oldTurn, true, nil
}

func lookupMuseMessageRow(db *sql.DB, query, key string) (museMessageOutcome, bool, error) {
	var out museMessageOutcome
	qerr := db.QueryRow(`SELECT session, native_id, command_id, turn_id, terminal, detail, principal
		FROM muse_message_intents WHERE `+query+` = ?`, key).Scan(
		&out.Session, &out.NativeID, &out.Command, &out.TurnID, &out.Terminal, &out.Detail, &out.Principal)
	if qerr == sql.ErrNoRows {
		return museMessageOutcome{}, false, nil
	}
	if qerr != nil {
		return museMessageOutcome{}, false, qerr
	}
	return out, true, nil
}

func loadMuseMessageOutcome(requestID string) (museMessageOutcome, error) {
	db, err := openTasksDB()
	if err != nil {
		return museMessageOutcome{}, err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return museMessageOutcome{}, err
	}
	out, found, err := lookupMuseMessageRow(db, "request_id", requestID)
	if err != nil {
		return museMessageOutcome{}, err
	}
	if !found {
		return museMessageOutcome{}, fmt.Errorf("no such muse message request %q", requestID)
	}
	return out, nil
}

// claimMuseMessageIntent freezes the execution identity for one
// request ID BEFORE anything is submitted: the stable command ID
// plus the exact payload digest. A retry with the same identity
// reuses the stored command (server dedups); a reused key with a
// different identity is a conflict, never a silent replacement.
//
// Simultaneous identical claims race on the primary key; the
// loser reconciles the winner's row instead of surfacing a
// database failure.
// inboxMessageRequestPrefix reserves the request-ID namespace the
// M2M dispatcher owns. Ordinary callers through this claim path
// (MCP, CLI) cannot mint it: that would let a forged request
// collide with a later legitimate M2M delivery. The dispatcher
// uses claimMuseMessageIntentWithCommand directly and never
// passes through this check.
const inboxMessageRequestPrefix = "inbox:"

func claimMuseMessageIntent(msg MuseMessageRequest, b museBinding, principal string) (commandID, priorTurnID string, replay bool, err error) {
	if strings.HasPrefix(msg.RequestID, inboxMessageRequestPrefix) {
		return "", "", false, fmt.Errorf("muse message request_id %q uses the reserved M2M dispatch namespace", msg.RequestID)
	}
	commandID, err = newMuseCommandID()
	if err != nil {
		return "", "", false, err
	}
	return claimMuseMessageIntentWithCommand(msg, b, commandID, principal)
}

// claimMuseMessageIntentWithCommand is claimMuseMessageIntent with
// a caller-supplied command identity, so the M2M dispatch path and
// the message path share one authoritative command instead of
// minting two. An empty command mints one.
func claimMuseMessageIntentWithCommand(msg MuseMessageRequest, b museBinding, commandID, principal string) (string, string, bool, error) {
	if strings.TrimSpace(msg.RequestID) == "" {
		return "", "", false, fmt.Errorf("muse message requires a request ID")
	}
	if strings.TrimSpace(msg.SessionID) == "" {
		return "", "", false, fmt.Errorf("muse message requires a session")
	}
	if strings.TrimSpace(msg.Text) == "" {
		return "", "", false, fmt.Errorf("muse message requires text")
	}
	if strings.TrimSpace(commandID) == "" {
		var err error
		commandID, err = newMuseCommandID()
		if err != nil {
			return "", "", false, err
		}
	}
	db, err := openTasksDB()
	if err != nil {
		return "", "", false, err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return "", "", false, err
	}
	if _, err := db.Exec(`INSERT INTO muse_message_intents
		(request_id, session, native_id, profile, provider, workspace, prompt_digest, principal, command_id, turn_id, terminal, detail, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', '', ?, ?)`,
		msg.RequestID, msg.SessionID, b.NativeID, b.Profile, b.Provider, b.Workspace,
		museMessageDigest(msg.Text), principal, commandID, nowUTC(), nowUTC()); err == nil {
		recordReceipt(msg.SessionID, "muse_message_intent",
			fmt.Sprintf("actor=%s req=%s cmd=%s state=submitted", msg.Actor, msg.RequestID, commandID))
		return commandID, "", false, nil
	} else if !isUniqueConflict(err) {
		return "", "", false, err
	}
	// Lost the insert race (or replaying): reconcile the winner.
	var oldSession, oldNative, oldProfile, oldProvider, oldWorkspace, oldDigest, oldCmd, oldTurn, oldPrincipal string
	qerr := db.QueryRow(`SELECT session, native_id, profile, provider, workspace, prompt_digest, command_id, turn_id, principal
		FROM muse_message_intents WHERE request_id = ?`, msg.RequestID).Scan(
		&oldSession, &oldNative, &oldProfile, &oldProvider, &oldWorkspace, &oldDigest, &oldCmd, &oldTurn, &oldPrincipal)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return "", "", false, fmt.Errorf("muse message claim lost a command-identity race for request %q", msg.RequestID)
		}
		return "", "", false, qerr
	}
	return claimMuseMessageRow(msg, b, oldSession, oldNative, oldProfile, oldProvider, oldWorkspace, oldDigest, oldCmd, oldTurn, oldPrincipal, principal)
}

// recordMuseMessageAck stores the admission receipt for one
// command. The first ack wins: a later ack for the same command
// names the same turn through server dedup, so a conflicting name
// is returned as an error instead of silently replacing history.
func recordMuseMessageAck(commandID, turnID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE muse_message_intents SET turn_id = ?, updated = ? WHERE command_id = ? AND turn_id = ''`,
		turnID, nowUTC(), commandID); err != nil {
		return err
	}
	var stored string
	if err := db.QueryRow(`SELECT turn_id FROM muse_message_intents WHERE command_id = ?`, commandID).Scan(&stored); err != nil {
		return err
	}
	if stored != turnID {
		return fmt.Errorf("muse message ack conflict: command %s already admitted as turn %s", commandID, stored)
	}
	return nil
}

// recordMuseMessageNote stores an unresolved-delivery note
// (unknown) without touching the terminal column: only an
// observed server terminal may set terminal. The conditional
// write keeps a note from clobbering a terminal that landed
// between the observation and the note.
func recordMuseMessageNote(commandID, detail string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return err
	}
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	_, err = db.Exec(`UPDATE muse_message_intents SET detail = ?, updated = ? WHERE command_id = ? AND terminal = ''`,
		detail, nowUTC(), commandID)
	return err
}

// recordMuseMessageTerminal stores one OBSERVED server terminal
// value and returns the durable winner. The write is monotonic:
// WHERE terminal = ” keeps the first observed outcome, so a
// transient reconnect failure can never overwrite a persisted
// completion. Unknown is never passed here; see
// recordMuseMessageNote.
func recordMuseMessageTerminal(commandID, terminal, detail string) (string, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return "", err
	}
	if strings.TrimSpace(terminal) == "" {
		return "", fmt.Errorf("muse message terminal requires an observed value")
	}
	if len(detail) > 2000 {
		detail = detail[:2000]
	}
	if _, err := db.Exec(`UPDATE muse_message_intents SET terminal = ?, detail = ?, updated = ? WHERE command_id = ? AND terminal = ''`,
		terminal, detail, nowUTC(), commandID); err != nil {
		return "", err
	}
	var stored string
	if err := db.QueryRow(`SELECT terminal FROM muse_message_intents WHERE command_id = ?`, commandID).Scan(&stored); err != nil {
		return "", err
	}
	return stored, nil
}

// loadMuseMessageByCommand returns the persisted outcome for one
// claimed command identity.
func loadMuseMessageByCommand(commandID string) (museMessageOutcome, error) {
	db, err := openTasksDB()
	if err != nil {
		return museMessageOutcome{}, err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return museMessageOutcome{}, err
	}
	out, found, err := lookupMuseMessageRow(db, "command_id", commandID)
	if err != nil {
		return museMessageOutcome{}, err
	}
	if !found {
		return museMessageOutcome{}, fmt.Errorf("no such muse message command %q", commandID)
	}
	return out, nil
}

// musePersistedTerminal returns the durable observed terminal for
// one claimed command, or "" while unresolved. A completed
// request replays its stored terminal immediately instead of
// waiting for another terminal notification.
//
// Storage failure is an error, never an empty terminal: a
// database fault must never make an already-completed request
// look unsubmitted (which would resubmit a second turn). Only a
// missing row or an empty terminal column means "not completed".
func musePersistedTerminal(commandID string) (terminal, turnID, detail string, err error) {
	db, err := openTasksDB()
	if err != nil {
		return "", "", "", err
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		return "", "", "", err
	}
	out, found, err := lookupMuseMessageRow(db, "command_id", commandID)
	if err != nil || !found {
		return "", "", "", err
	}
	if strings.TrimSpace(out.Terminal) == "" {
		return "", "", "", nil
	}
	return out.Terminal, out.TurnID, out.Detail, nil
}

// traceMuseMessage appends one activity line to the targeted
// session's own events tail: request ID, turn ID, and the
// admitted/terminal state with delivery errors. The refresh path
// already tails that file, so the activity view surfaces message
// lifecycle with no new polling. Unknown polls never trace (only
// genuine transitions do), so a poll storm cannot spam the tail.
func traceMuseMessage(b museBinding, format string, args ...any) {
	if strings.TrimSpace(b.Workspace) == "" {
		return
	}
	appendEvent(b.Workspace, "muse_message", fmt.Sprintf(format, args...))
}

// unknownMuseMessage records an unresolved-delivery note (never a
// terminal) and reports it. Persistence failures surface in the
// detail: an unrecorded note means a later retry reconciles from
// the server instead of trusting local state.
func unknownMuseMessage(res *MuseMessageResult, commandID, turnID, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	if perr := recordMuseMessageNote(commandID, detail); perr != nil {
		detail = fmt.Sprintf("%s (note unrecorded: %v)", detail, perr)
	}
	res.TurnID = turnID
	res.State = museMessageUnknown
	res.Detail = detail
}

// terminalMuseMessage stores one observed server terminal and
// reports the durable winner (monotonic: the first persisted
// outcome survives concurrent or repeated observations). Every
// observed terminal appends one activity line: only a genuine
// transition reaches this function (replays return earlier, and
// unknown polls never observe), so the tail stays truthful
// without poll spam.
func terminalMuseMessage(res *MuseMessageResult, b museBinding, commandID, turnID, cursor, terminal, note string) {
	stored, perr := recordMuseMessageTerminal(commandID, terminal, note)
	if perr != nil {
		unknownMuseMessage(res, commandID, turnID, "turn %s observed %s but unrecorded: %v", turnID, terminal, perr)
		return
	}
	if cursor != "" {
		// Cursor persistence is reconciliation state, not a side
		// quest: a failure is recorded explicitly against the
		// session so history observation can see the gap. The
		// observed terminal still stands — a cursor gap must not
		// downgrade a completion to unknown.
		if cerr := updateMuseCursor(res.SessionID, cursor, turnID); cerr != nil {
			recordReceipt(res.SessionID, "muse_cursor_gap",
				fmt.Sprintf("turn %s observed %s but cursor unrecorded: %v", turnID, stored, cerr))
		}
	}
	traceMuseMessage(b, "req=%s turn=%s terminal=%s (%s)", res.RequestID, turnID, stored, note)
	res.TurnID = turnID
	res.State = stored
	res.Detail = fmt.Sprintf("turn %s terminal=%s", turnID, stored)
}

// sendMuseMessageOnHost runs one claimed message against an
// already-joined host: idempotent submit under the claimed command
// identity (or a pure wait when the turn ID is already known from
// a prior admitted attempt), then the terminal wait. A replay of
// an already-completed request returns the durable terminal
// without touching the host. Split out so tests drive the exact
// production path against a stub host.
func sendMuseMessageOnHost(ctx context.Context, h *museHost, msg MuseMessageRequest, b museBinding, commandID, priorTurnID string, wait time.Duration) (MuseMessageResult, error) {
	res := MuseMessageResult{RequestID: msg.RequestID, SessionID: msg.SessionID}
	if terminal, turnID, _, terr := musePersistedTerminal(commandID); terr != nil {
		// Storage is unavailable: the request may already be
		// completed, so report unknown (retry to reconcile) —
		// never proceed as if unsubmitted.
		unknownMuseMessage(&res, commandID, "", "terminal lookup failed: %v (outcome unknown; retry with the same request_id)", terr)
		return res, nil
	} else if terminal != "" {
		res.TurnID = turnID
		res.State = terminal
		res.Detail = fmt.Sprintf("replayed durable terminal=%s for turn %s (cmd %s)", terminal, turnID, commandID)
		return res, nil
	}
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		unknownMuseMessage(&res, commandID, "", "resume %s: %v (outcome unknown; cursor %q retained)", b.NativeID, err, b.Cursor)
		return res, nil
	}
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		res.State = museMessageRefused
		res.Detail = verr.Error()
		traceMuseMessage(b, "req=%s refused: %s", msg.RequestID, verr.Error())
		return res, nil
	}
	// A concurrent winner may have admitted the turn after our
	// claim: reload and reconcile instead of submitting twice.
	if strings.TrimSpace(priorTurnID) == "" {
		if fresh, ferr := loadMuseMessageByCommand(commandID); ferr == nil && strings.TrimSpace(fresh.TurnID) != "" {
			priorTurnID = fresh.TurnID
		}
	}
	// A prior admitted attempt names its turn: reconcile by waiting
	// for that exact turn, never by submitting a second one.
	if strings.TrimSpace(priorTurnID) != "" {
		res.TurnID = priorTurnID
		if wait <= 0 {
			res.State = museMessageAccepted
			res.Detail = fmt.Sprintf("reconciled admitted turn %s (cmd %s); terminal not waited for", priorTurnID, commandID)
			return res, nil
		}
		wctx, wcancel := context.WithTimeout(ctx, wait)
		defer wcancel()
		terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, priorTurnID, nil)
		if err != nil {
			unknownMuseMessage(&res, commandID, priorTurnID, "turn %s outcome unknown (%v); retry with the same request_id to reconcile", priorTurnID, err)
			return res, nil
		}
		terminalMuseMessage(&res, b, commandID, priorTurnID, cursor, terminal, "reconciled")
		return res, nil
	}
	ack, err := museSubmitTurnWithID(ctx, h, b.NativeID, msg.Text, "", commandID)
	if err != nil {
		unknownMuseMessage(&res, commandID, "", "turn/start %s: %v (submission uncertain; retry with the same request_id)", commandID, err)
		return res, nil
	}
	if err := recordMuseMessageAck(commandID, ack.TurnID); err != nil {
		unknownMuseMessage(&res, commandID, ack.TurnID, "turn %s acknowledged but intent unrecorded: %v (reconcile by command %s)", ack.TurnID, err, commandID)
		return res, nil
	}
	res.TurnID = ack.TurnID
	if ack.Status != "" && ack.Status != "accepted" {
		unknownMuseMessage(&res, commandID, ack.TurnID, "turn not admitted: status=%q disposition=%q", ack.Status, ack.Disposition)
		return res, nil
	}
	if wait <= 0 {
		res.State = museMessageAccepted
		res.Detail = fmt.Sprintf("admitted disposition=%s turn=%s cmd=%s (terminal not waited for)", ack.Disposition, ack.TurnID, commandID)
		traceMuseMessage(b, "req=%s admitted turn=%s cmd=%s", msg.RequestID, ack.TurnID, commandID)
		return res, nil
	}
	var finalText string
	observe := chainTurnObserver(ctx, h, msg.SessionID, b.NativeID, func(s string) { finalText = s })
	wctx, wcancel := context.WithTimeout(ctx, wait)
	defer wcancel()
	terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, ack.TurnID, observe)
	_ = finalText
	if err != nil {
		unknownMuseMessage(&res, commandID, ack.TurnID, "turn %s outcome unknown (%v); retry with the same request_id to reconcile", ack.TurnID, err)
		return res, nil
	}
	terminalMuseMessage(&res, b, commandID, ack.TurnID, cursor, terminal, "observed")
	return res, nil
}

// museMessageAllowedTargets is the operator-configured allowlist
// for direct muse messaging (METIS_MUSE_MESSAGE_TARGETS, comma
// separated). Empty means any in-scope bound session may be
// messaged by the LOCAL operator; remote callers are always
// refused without an explicit list naming their target (the gate
// enforces that split, so default-allow never leaks to remote).
// A validated Ocliv identity arrives at this boundary as the
// configured token and becomes the fixed remote principal
// server-side in mcpAuthCaller; the caller-supplied actor string
// is display-only and never confers authority.
func museMessageAllowedTargets() []string {
	raw := strings.TrimSpace(os.Getenv("METIS_MUSE_MESSAGE_TARGETS"))
	if raw == "" {
		return nil
	}
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// authorizeMuseMessageTarget is the ONE shared domain gate for
// every path that submits a turn for a message: the MCP op, the
// status reconciler, and the M2M inbox dispatcher. auth carries
// transport identity (principal); the self-declared msg.Actor never
// confers authority. bindKey pins auth.RequestID to msg.RequestID
// on the submit path so a retry owns its key; status passes false
// (a poll carries its own transport key and submits nothing). The
// returned refusal is empty exactly when submission may proceed.
func authorizeMuseMessageTarget(msg MuseMessageRequest, auth OperationRequest, bindKey bool) (b museBinding, refusal string) {
	if err := requireRequest(auth); err != nil {
		return museBinding{}, err.Error()
	}
	if strings.TrimSpace(auth.Principal) == "" {
		return museBinding{}, "muse message requires an authenticated principal; self-declared actor confers no authority"
	}
	if strings.TrimSpace(msg.RequestID) == "" || strings.TrimSpace(msg.SessionID) == "" || strings.TrimSpace(msg.Text) == "" {
		return museBinding{}, "muse message requires session, text, and request_id"
	}
	// The submit path binds the transport request key to the
	// message identity so a retry owns its key; the status path
	// (no submit) authenticates by principal only.
	if bindKey && auth.RequestID != "" && auth.RequestID != msg.RequestID {
		return museBinding{}, "muse message request_id must match the authenticated request key"
	}
	b, err := getMuseBinding(msg.SessionID)
	if err != nil || b.NativeID == "" {
		return museBinding{}, fmt.Sprintf("refused: no muse binding for session %s; bind it first", msg.SessionID)
	}
	if err := requireNativeUseScope(msg.SessionID, b); err != nil {
		return museBinding{}, fmt.Sprintf("refused: %v", err)
	}
	if allow := museMessageAllowedTargets(); len(allow) > 0 {
		ok := false
		for _, name := range allow {
			if name == msg.SessionID {
				ok = true
				break
			}
		}
		if !ok {
			return museBinding{}, fmt.Sprintf("refused: session %s is not an authorized muse message target", msg.SessionID)
		}
	} else if auth.Remote {
		// No default-allow for remote callers: a validated remote
		// principal reaches only sessions the operator named
		// explicitly. Local-operator default-allow is unchanged.
		return museBinding{}, "refused: remote message callers need an explicit METIS_MUSE_MESSAGE_TARGETS policy naming this session"
	}
	// A stopped shell-backed entry with no logical record is
	// history, never a message target.
	if _, found, lerr := loadLogicalSession(msg.SessionID); lerr == nil && !found {
		if entry, ok := loadRegistry()[msg.SessionID]; ok && entry.Stopped {
			return museBinding{}, fmt.Sprintf("refused: session %s is stopped", msg.SessionID)
		}
	}
	return b, ""
}

// sendMuseMessage is the production entry: authorize through the
// shared gate, freeze the idempotency identity, submit through MSP
// and return a prompt admission receipt (accepted + turn ID).
// Terminal observation is NEVER waited for here: the MCP exchange
// budget (30s) cannot cover an unbounded turn, so callers poll
// metis_muse_message_status for the durable outcome. A replay of
// an already-completed request returns its stored terminal.
// isMuseTerminalState reports a server-observed terminal: anything
// that is not Metis's own admission bookkeeping (accepted, refused,
// unknown) or empty.
func isMuseTerminalState(s string) bool {
	switch s {
	case "", museMessageAccepted, museMessageRefused, museMessageUnknown:
		return false
	}
	return true
}

// emitMuseMessageOutcome records one message submit/status outcome
// in the controller timeline: admission as message.sent (accepted
// or refused), an observed terminal as message.delivered. Origin is
// the server-established principal. Unknown polls emit nothing —
// a retry that later resolves emits then. (request_id, event_type)
// idempotency collapses repeats to the first row, so polls and
// replays never spam the feed. Best-effort: a feed-write failure is
// a gap receipt, never a message failure.
func emitMuseMessageOutcome(auth OperationRequest, res MuseMessageResult, recordRefusals bool) {
	var typ, disp, payload string
	switch {
	case res.State == museMessageAccepted:
		typ, disp = OrchMessageSent, museMessageAccepted
		payload = "turn=" + res.TurnID
	case res.State == museMessageRefused:
		// Submit-path refusals are admission results worth one
		// timeline row. Status-path refusals (stranger polls,
		// lapsed scope) are routine auth noise: recording them
		// would spam the feed and trip the blocking signal.
		if !recordRefusals {
			return
		}
		typ, disp = OrchMessageSent, museMessageRefused
		payload = res.Detail
	case isMuseTerminalState(res.State):
		typ, disp = OrchMessageDelivered, res.State
		payload = "turn=" + res.TurnID + " " + res.Detail
	default:
		return
	}
	if _, err := recordOrchEvent(OrchEvent{
		OriginPrincipal: auth.Principal,
		TargetAgent:     res.SessionID,
		EventType:       typ,
		RequestID:       res.RequestID,
		Payload:         payload,
		Disposition:     disp,
	}); err != nil {
		recordReceipt("", "orch_gap", "muse message feed write failed: "+err.Error())
	}
}

func sendMuseMessage(msg MuseMessageRequest, auth OperationRequest) (res MuseMessageResult, err error) {
	defer func() { emitMuseMessageOutcome(auth, res, true) }()
	res = MuseMessageResult{RequestID: msg.RequestID, SessionID: msg.SessionID}
	b, refusal := authorizeMuseMessageTarget(msg, auth, true)
	if refusal != "" {
		res.State = museMessageRefused
		res.Detail = refusal
		return res, nil
	}
	commandID, priorTurnID, _, cerr := claimMuseMessageIntent(msg, b, auth.Principal)
	if cerr != nil {
		res.State = museMessageRefused
		res.Detail = cerr.Error()
		return res, nil
	}
	// A completed request replays its durable terminal without
	// touching the host: no resubmit, no new wait. Storage failure
	// is unknown, never a license to submit.
	if terminal, turnID, _, terr := musePersistedTerminal(commandID); terr != nil {
		unknownMuseMessage(&res, commandID, "", "terminal lookup failed: %v (submission uncertain; retry with the same request_id)", terr)
		return res, nil
	} else if terminal != "" {
		res.TurnID = turnID
		res.State = terminal
		res.Detail = fmt.Sprintf("replayed durable terminal=%s for turn %s (cmd %s)", terminal, turnID, commandID)
		return res, nil
	}
	c, cerr := resolveMuseCommander(b.Profile)
	if cerr != nil {
		res.State = museMessageRefused
		res.Detail = cerr.Error()
		return res, nil
	}
	if derr := museRequireDaemonFor(c); derr != nil {
		res.State = museMessageRefused
		res.Detail = derr.Error()
		return res, nil
	}
	// museMessageSubmitBudget bounds the WHOLE submit RPC —
	// host startup plus admission — below the 30s Ocliv exchange
	// budget with headroom for serialization. A stall yields
	// unknown (retry with the same request_id), never a hang.
	ctx, cancel := context.WithTimeout(context.Background(), museMessageSubmitBudget)
	defer cancel()
	h, herr := startMuseHost(ctx, c.Binary, b.Provider, nil, "--join")
	if herr != nil {
		unknownMuseMessage(&res, commandID, "", "host: %v (submission uncertain; retry with the same request_id)", herr)
		return res, nil
	}
	defer h.Close()
	res, err = sendMuseMessageOnHost(ctx, h, msg, b, commandID, priorTurnID, 0)
	return res, err
}

// museMessageSubmitBudget bounds one message submit RPC — host
// startup plus admission — below the 30s Ocliv exchange budget
// with headroom for serialization and network overhead.
const museMessageSubmitBudget = 20 * time.Second

// messageStatusDefaultWait bounds one status poll. It must stay
// well under the MCP exchange budget so a reconcile never fails
// the caller while the turn is still running server-side.
const messageStatusDefaultWait = 5 * time.Second

// museMessageStatusMaxWait caps a status poll below the MCP
// exchange budget.
const museMessageStatusMaxWait = 25 * time.Second

// clampMessageStatusWait applies the default and the cap.
func clampMessageStatusWait(wait time.Duration) time.Duration {
	if wait <= 0 {
		return messageStatusDefaultWait
	}
	if wait > museMessageStatusMaxWait {
		return museMessageStatusMaxWait
	}
	return wait
}

// reconcileMuseMessageTurn observes one admitted turn to its
// terminal without resubmitting: the host-side twin of the
// accepted prompt receipt. Split out so tests drive the exact
// production path against a stub host.
func reconcileMuseMessageTurn(ctx context.Context, h *museHost, b museBinding, out museMessageOutcome, wait time.Duration) MuseMessageResult {
	res := MuseMessageResult{RequestID: "", SessionID: out.Session, TurnID: out.TurnID}
	if out.Terminal != "" {
		res.State = out.Terminal
		res.Detail = fmt.Sprintf("durable terminal=%s for turn %s (cmd %s)", out.Terminal, out.TurnID, out.Command)
		return res
	}
	if strings.TrimSpace(out.TurnID) == "" {
		res.State = museMessageUnknown
		res.Detail = fmt.Sprintf("turn not yet admitted for cmd %s (submission uncertain; retry the message request)", out.Command)
		return res
	}
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		unknownMuseMessage(&res, out.Command, out.TurnID, "resume %s: %v (outcome unknown)", b.NativeID, err)
		return res
	}
	wctx, wcancel := context.WithTimeout(ctx, clampMessageStatusWait(wait))
	defer wcancel()
	terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, out.TurnID, nil)
	if err != nil {
		unknownMuseMessage(&res, out.Command, out.TurnID, "turn %s outcome unknown (%v); poll again to reconcile", out.TurnID, err)
		return res
	}
	terminalMuseMessage(&res, b, out.Command, out.TurnID, cursor, terminal, "status-reconciled")
	return res
}

// queryMuseMessageStatus is the production status entry: load the
// durable outcome for one request ID and, while unresolved,
// reconcile it against the live host with a bounded wait. It never
// submits a turn; a request with no admission receipt stays
// unknown so the caller retries the message op.
func queryMuseMessageStatus(requestID string, auth OperationRequest, wait time.Duration) (res MuseMessageResult, err error) {
	defer func() { emitMuseMessageOutcome(auth, res, false) }()
	out, err := loadMuseMessageOutcome(requestID)
	if err != nil {
		return MuseMessageResult{}, err
	}
	// res carries no row-derived identity until the caller proves
	// ownership: a refused caller learns nothing beyond the
	// request it already named — no session, no turn, no
	// terminal. Authorized returns attach row data below.
	res = MuseMessageResult{RequestID: requestID}
	if strings.TrimSpace(auth.Principal) == "" {
		res.State = museMessageRefused
		res.Detail = "muse message status requires an authenticated principal; a bare request ID establishes no ownership"
		return res, nil
	}
	// Ownership precedes scope: a stranger who passes the
	// session allowlist must still not read another principal's
	// intent. Empty stored principals are pre-principal
	// local-operator rows and stay readable under local policy.
	if out.Principal != "" && auth.Principal != out.Principal {
		res.State = museMessageRefused
		res.Detail = "muse message request is owned by another principal"
		return res, nil
	}
	res.SessionID = out.Session
	// The status poll carries its own transport request key, so
	// the gate sees only principal + capability; the stored row
	// (not this call's key) owns the message identity. The gate
	// runs BEFORE any cached terminal is returned: a status
	// caller passes the same target-scope checks as a submitter.
	msg := MuseMessageRequest{SessionID: out.Session, RequestID: requestID, Text: "status-reconcile"}
	bound, refusal := authorizeMuseMessageTarget(msg, OperationRequest{
		Actor:     auth.Actor,
		RequestID: auth.RequestID,
		Principal: auth.Principal,
		Remote:    auth.Remote,
	}, false)
	if refusal != "" {
		// Historical read: the initiating principal may still
		// inspect a completed message after its session stopped,
		// lost its binding, or left the allowlist — but only the
		// completed terminal, never a live reconcile, and never
		// for a stranger. Possession of the request ID alone
		// confers nothing.
		if out.Terminal != "" && out.Principal != "" && auth.Principal == out.Principal {
			res.State = out.Terminal
			res.TurnID = out.TurnID
			res.Detail = fmt.Sprintf("durable terminal=%s for turn %s (historical read; session scope lapsed)", out.Terminal, out.TurnID)
			return res, nil
		}
		res.State = museMessageRefused
		res.Detail = refusal
		return res, nil
	}
	if out.Terminal != "" {
		res.State = out.Terminal
		res.TurnID = out.TurnID
		res.Detail = fmt.Sprintf("durable terminal=%s for turn %s", out.Terminal, out.TurnID)
		return res, nil
	}
	c, cerr := resolveMuseCommander(bound.Profile)
	if cerr != nil {
		res.State = museMessageRefused
		res.Detail = cerr.Error()
		return res, nil
	}
	if derr := museRequireDaemonFor(c); derr != nil {
		res.State = museMessageUnknown
		res.Detail = fmt.Sprintf("daemon unavailable: %v (turn %s may still complete; poll again)", derr, out.TurnID)
		return res, nil
	}
	// One total budget — host startup AND the poll share it —
	// capped below the 30s Ocliv exchange budget. The old
	// clamp(wait)+30s could reach 55s on a stalled daemon and
	// wedge the serial MCP connection past the exchange timeout.
	ctx, cancel := context.WithTimeout(context.Background(), museMessageStatusMaxWait)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, bound.Provider, nil, "--join")
	if err != nil {
		unknownMuseMessage(&res, out.Command, out.TurnID, "host: %v (outcome unknown; poll again)", err)
		res.RequestID = requestID
		return res, nil
	}
	defer h.Close()
	res = reconcileMuseMessageTurn(ctx, h, bound, out, wait)
	res.RequestID = requestID
	return res, nil
}

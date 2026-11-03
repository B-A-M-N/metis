package main

// m2m_bridge.go: thin Metis adapter over the existing M2M Python
// implementation (Drive: muse-to-muse).
//
// The M2M project already owns endpoint identities, encryption,
// signing, the relay store, the transport client, the durable
// outbox, and the Muse skill driver. This file does NOT reimplement
// any of that: it invokes the existing Python client through
// controlled subprocess arguments with payload bytes on stdin, and
// it keeps Metis's own execution-tracking state in SQLite.
//
// Security boundaries (load bearing, not cosmetic):
//   - Plaintext, tokens, and keys never appear on the command line:
//     argv carries only the subcommand and opaque references; every
//     sensitive byte travels on stdin.
//   - A decrypted M2M message is untrusted data. Execution is NEVER
//     sender-conferred: the adapter labels verified mail
//     "m2m.message" (ordinary message, no turn intent), and only the
//     operator's METIS_M2M_SENDERS + METIS_M2M_TURN_TYPES allowlists
//     confer executability. Everything else stays data and never
//     becomes a command.
//   - Delivery and execution stay distinct: the sender-visible
//     signed transport acknowledgment proves the key holder
//     processed the message, nothing more. TURN_ADMITTED and
//     TURN_COMPLETED are Metis's execution tracking, recorded in
//     the dispatch ledger, and only the native turn path may set
//     them. An ack echo never reports execution completion.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Bridge delivery/execution states. The QUEUED..INBOX_STORED prefix
// mirrors the M2M transport's own vocabulary and is never renamed;
// the TURN_* suffix is Metis's execution tracking.
const (
	m2mQueued          = "QUEUED"
	m2mRelayAccepted   = "RELAY_ACCEPTED"
	m2mAckedByEndpoint = "ACKNOWLEDGED_BY_ENDPOINT"
	m2mInboxStored     = "INBOX_STORED"
	m2mTurnAdmitted    = "TURN_ADMITTED"
	m2mTurnCompleted   = "TURN_COMPLETED"
	m2mRefused         = "REFUSED"
)

// m2mClientPath names the M2M Python client entry point. The Drive
// project ships its own layout; the operator points Metis at it
// explicitly via METIS_M2M_CLIENT. There is deliberately no PATH
// fallback: a bare-name lookup could select an impostor binary, so
// an unset variable means no client is configured and every bridge
// invocation refuses instead of executing something unexpected.
func m2mClientPath() string {
	return strings.TrimSpace(os.Getenv("METIS_M2M_CLIENT"))
}

// validateM2MClient resolves the configured client to an explicit
// executable file. Directories, non-regular files, and
// non-executable files are refused: the bridge only ever execs a
// file the operator installed and marked executable.
func validateM2MClient(path string) error {
	if path == "" {
		return fmt.Errorf("m2m bridge has no client configured (set METIS_M2M_CLIENT to the Python client entry point)")
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("m2m bridge requires an absolute client path, got %q", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("m2m bridge cannot stat client %q: %v", path, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("m2m bridge refuses non-regular client %q", path)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("m2m bridge refuses non-executable client %q", path)
	}
	return nil
}

// m2mAuthorizedSenders lists sender fingerprints allowed to request
// a Muse turn, comma-separated. Empty means no sender is authorized:
// fail closed, general inbox stays data.
func m2mAuthorizedSenders() map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("METIS_M2M_SENDERS"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// m2mTurnMessageTypes lists inbox message types allowed to request
// a Muse turn, comma-separated. Empty means no type may trigger
// execution: fail closed.
func m2mTurnMessageTypes() map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(os.Getenv("METIS_M2M_TURN_TYPES"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// m2mEnvelope is the decrypted-but-untrusted inbox item the M2M
// client hands Metis: identity, type, expiry, and opaque payload
// reference. The ciphertext itself is never stored here.
type m2mEnvelope struct {
	MessageID string
	SenderFP  string
	Type      string
	ExpiresAt time.Time
	Payload   []byte
}

func ensureMuseInboxDispatchTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_inbox_dispatch (
		message_id TEXT PRIMARY KEY,
		sender_fp TEXT NOT NULL,
		target_session TEXT NOT NULL,
		command_id TEXT NOT NULL UNIQUE,
		state TEXT NOT NULL,
		payload_digest TEXT NOT NULL DEFAULT '',
		terminal TEXT NOT NULL DEFAULT '',
		received_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`); err != nil {
		return err
	}
	// Migration for pre-existing tables: additive columns only,
	// never a rewrite.
	for _, col := range []string{
		`ALTER TABLE muse_inbox_dispatch ADD COLUMN payload_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE muse_inbox_dispatch ADD COLUMN terminal TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.Exec(col); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// m2mPayloadDigest freezes what the turn will say: the SHA-256 of
// the verified envelope payload. A reused message ID with
// different bytes is a conflict, never a silent substitution.
func m2mPayloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("sha256:%x", sum)
}

// claimInboxDispatch freezes the execution identity for one inbox
// message BEFORE anything is submitted: the same message ID always
// resumes the same command ID (crash recovery), while a reused ID
// naming a different sender, target, or payload digest is a
// conflict, never a silent retarget or substitution.
//
// Simultaneous identical claims race on the primary key; the loser
// reconciles the winner's row instead of surfacing a database
// failure.
func claimInboxDispatch(messageID, senderFP, targetSession, payloadDigest string) (commandID string, replay bool, err error) {
	if strings.TrimSpace(messageID) == "" || strings.TrimSpace(targetSession) == "" {
		return "", false, fmt.Errorf("inbox dispatch requires a message id and a target session")
	}
	db, err := openTasksDB()
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	if err := ensureMuseInboxDispatchTable(db); err != nil {
		return "", false, err
	}
	commandID, err = newMuseCommandID()
	if err != nil {
		return "", false, err
	}
	if _, err := db.Exec(`INSERT INTO muse_inbox_dispatch
		(message_id, sender_fp, target_session, command_id, state, payload_digest, terminal, received_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)`,
		messageID, senderFP, targetSession, commandID, m2mInboxStored, payloadDigest, nowUTC(), nowUTC()); err == nil {
		return commandID, false, nil
	} else if !isUniqueConflict(err) {
		return "", false, err
	}
	var oldSender, oldTarget, oldCmd, oldDigest string
	qerr := db.QueryRow(`SELECT sender_fp, target_session, command_id, payload_digest FROM muse_inbox_dispatch WHERE message_id = ?`,
		messageID).Scan(&oldSender, &oldTarget, &oldCmd, &oldDigest)
	if qerr != nil {
		if qerr == sql.ErrNoRows {
			return "", false, fmt.Errorf("inbox dispatch claim lost a command-identity race for message %q", messageID)
		}
		return "", false, qerr
	}
	if oldSender != senderFP || oldTarget != targetSession {
		return "", false, fmt.Errorf("inbox dispatch conflict: message %q already targets session %q from %q; refusing retarget to %q/%q",
			messageID, oldTarget, oldSender, targetSession, senderFP)
	}
	if oldDigest != "" && oldDigest != payloadDigest {
		return "", false, fmt.Errorf("inbox dispatch conflict: message %q already carries a different payload digest; refusing substitution", messageID)
	}
	return oldCmd, true, nil
}

// m2mDispatchSettled reports whether a dispatch row is settled
// history. REFUSED and any row carrying an observed terminal are
// final; TURN_ADMITTED is explicitly NON-terminal — admission only
// means Muse accepted the turn, and a later completion, failure,
// or cancellation must still advance the row. A later retry,
// revocation, or reconnect must never rewrite settled history.
func m2mDispatchSettled(state, terminal string) bool {
	return state == m2mRefused || terminal != ""
}

// recordInboxDispatchOutcome advances one dispatch row with
// conditional SQLite transitions — no read-then-update — so
// concurrent admission, completion, refusal, and retry cannot
// overwrite one another. The first terminal write wins (a turn
// completes exactly once server-side, so concurrent observers
// agree); a zero-rows update means a concurrent writer settled
// first and the winner stands. The terminal holds the
// server-observed value verbatim, so a failed or cancelled turn
// is never represented as a success.
func recordInboxDispatchOutcome(messageID, state, terminal string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseInboxDispatchTable(db); err != nil {
		return err
	}
	if terminal != "" {
		_, err := db.Exec(`UPDATE muse_inbox_dispatch SET state = ?, terminal = ?, updated_at = ?
			WHERE message_id = ? AND terminal = ''`, state, terminal, nowUTC(), messageID)
		return err
	}
	// Non-terminal advance (admission, refusal): only a row with
	// no observed terminal and no standing refusal moves.
	_, err = db.Exec(`UPDATE muse_inbox_dispatch SET state = ?, updated_at = ?
		WHERE message_id = ? AND terminal = '' AND state != ?`,
		state, nowUTC(), messageID, m2mRefused)
	return err
}

func recordInboxDispatchState(messageID, state string) error {
	return recordInboxDispatchOutcome(messageID, state, "")
}

// authorizeInboxTurn gates decrypted-but-untrusted inbox content:
// expiry first, then sender registration, then message type. Any
// failure keeps the item data (INBOX_STORED) and refuses execution.
// Only a nil error authorizes a turn request.
//
// Expiry is mandatory: an envelope without an explicit future
// expiry never authorizes a turn.
func authorizeInboxTurn(env m2mEnvelope, now time.Time) error {
	if env.ExpiresAt.IsZero() || !now.Before(env.ExpiresAt) {
		return fmt.Errorf("inbox message %q has no valid future expiry; refusing execution", env.MessageID)
	}
	if !m2mAuthorizedSenders()[env.SenderFP] {
		return fmt.Errorf("inbox sender %q is not registered for turn requests; message stays data", env.SenderFP)
	}
	if !m2mTurnMessageTypes()[env.Type] {
		return fmt.Errorf("inbox message type %q may not request a turn; message stays data", env.Type)
	}
	return nil
}

// m2mDispatchTerminalState maps an observed native terminal to
// the dispatch state. Only a completed turn records
// TURN_COMPLETED; every other disposition keeps its own name so a
// failed or cancelled turn is never represented as a success.
func m2mDispatchTerminalState(terminal string) string {
	if terminal == "completed" {
		return m2mTurnCompleted
	}
	return "TURN_" + strings.ToUpper(terminal)
}

// dispatchInboxToTurnOnHost is the single choke point from an
// authorized inbox item to exactly one correlated native turn. It
// claims the durable dispatch identity (freezing sender, target,
// and payload digest), enforces the fail-closed inbox policy
// (expiry, sender, type), derives the submitted text ONLY from the
// verified envelope payload, authorizes through the shared muse
// message gate (principal, binding, scope, allowlist), and submits
// under the ONE dispatch command identity — the message-intent
// claim reuses it instead of minting a second one.
//
// A refused envelope submits nothing and stays stored as data. The
// dispatch row advances to TURN_ADMITTED when the turn is admitted
// and to the observed terminal disposition when it completes;
// anything less stays INBOX_STORED. Settled rows are monotonic and
// are never rewritten by a later retry or policy change. Split out
// so tests drive the exact production path against a stub host.
func dispatchInboxToTurnOnHost(ctx context.Context, h *museHost, env m2mEnvelope, targetSession string, wait time.Duration) (MuseMessageResult, error) {
	requestID := "inbox:" + env.MessageID
	var res MuseMessageResult
	refuse := func(detail string) (MuseMessageResult, error) {
		// A refusal before the claim leaves no row: nothing was
		// reserved, which is exactly the point, so a missing row
		// is not an error here.
		if err := recordInboxDispatchOutcome(env.MessageID, m2mRefused, ""); err != nil && err != sql.ErrNoRows {
			detail = fmt.Sprintf("%s (dispatch state unrecorded: %v)", detail, err)
		}
		return MuseMessageResult{RequestID: requestID, SessionID: targetSession,
			State: museMessageRefused, Detail: detail}, nil
	}
	// Verification and policy checks run BEFORE any executable
	// identity is claimed: a forged or unauthorized envelope must
	// neither submit a turn nor reserve a legitimate sender's
	// message identity. Only the verified envelope payload below
	// ever becomes executable text, and only after every check.
	if err := authorizeInboxTurn(env, time.Now()); err != nil {
		return refuse(err.Error())
	}
	// Executable content comes only from the authenticated and
	// verified envelope payload — never from a caller-supplied
	// text argument (there is none).
	text := strings.TrimSpace(string(env.Payload))
	if text == "" {
		return refuse(fmt.Sprintf("inbox message %q carries an empty payload; message stays data", env.MessageID))
	}
	digest := m2mPayloadDigest(env.Payload)
	commandID, _, err := claimInboxDispatch(env.MessageID, env.SenderFP, targetSession, digest)
	if err != nil {
		return res, err
	}
	msg := MuseMessageRequest{SessionID: targetSession, Text: text,
		RequestID: requestID, Actor: "m2m:" + env.SenderFP}
	auth := OperationRequest{Actor: msg.Actor, RequestID: requestID, Principal: msg.Actor}
	b, refusal := authorizeMuseMessageTarget(msg, auth, true)
	if refusal != "" {
		return refuse(refusal)
	}
	// One authoritative command identity: the dispatch claim owns
	// it, the message-intent claim reuses it. A mismatch against a
	// historical row refuses instead of submitting another turn.
	storedCmd, priorTurnID, _, cerr := claimMuseMessageIntentWithCommand(msg, b, commandID, auth.Principal)
	if cerr != nil {
		return refuse(cerr.Error())
	}
	if storedCmd != commandID {
		return refuse(fmt.Sprintf("inbox dispatch identity drift: dispatch %s vs intent %s; refusing",
			commandID, storedCmd))
	}
	res, err = sendMuseMessageOnHost(ctx, h, msg, b, commandID, priorTurnID, wait)
	if err != nil {
		return res, err
	}
	switch {
	case res.TurnID != "" && res.State != museMessageAccepted && res.State != museMessageUnknown && res.State != museMessageRefused:
		if rerr := recordInboxDispatchOutcome(env.MessageID, m2mDispatchTerminalState(res.State), res.State); rerr != nil {
			res.Detail = fmt.Sprintf("%s (dispatch outcome unrecorded: %v)", res.Detail, rerr)
		}
	case res.TurnID != "":
		_ = recordInboxDispatchState(env.MessageID, m2mTurnAdmitted)
	}
	return res, nil
}

// m2mBridgeResult is one M2M client invocation outcome: the
// transport's own message ID plus the transport's own recorded
// state, verbatim. Metis never invents a transport state.
type m2mBridgeResult struct {
	MessageID string `json:"message_id"`
	State     string `json:"state"`
}

// m2mBridgeOutputCap bounds one client invocation's captured
// output. The client speaks a small JSON result, never a bulk
// transfer; anything larger is a runaway, not a result.
const m2mBridgeOutputCap = 1 << 20

// m2mBridgeDefaultTimeout bounds one client invocation when the
// caller supplies no deadline. The bridge never runs an
// unbounded subprocess.
const m2mBridgeDefaultTimeout = 30 * time.Second

// runM2MBridge invokes the M2M Python client: argv carries only the
// subcommand plus opaque references, and every sensitive byte
// (plaintext, tokens, keys) travels on stdin. The client reports
// its result as JSON on stdout: {"message_id": "...", "state":
// "..."}. The state is returned verbatim: it is the transport's
// recorded state, never Metis's interpretation.
//
// The client path must be an explicitly configured executable file
// (no PATH fallback); output is bounded; failures name the
// subcommand, the exit, and byte counts only — subprocess stderr
// may carry sensitive relay content and is never copied into the
// error.
// execM2MClient runs one client subcommand and returns its raw
// stdout. All bridge hardening lives here — explicit executable,
// no empty or control-character arguments, bounded runtime and
// output, stderr never quoted — so every subcommand inherits it.
// The client speaks small JSON results, never a bulk transfer;
// anything larger than the cap is a runaway, not a result.
func execM2MClient(ctx context.Context, subcommand string, refs []string, stdin []byte) ([]byte, error) {
	client := m2mClientPath()
	if err := validateM2MClient(client); err != nil {
		return nil, err
	}
	for _, s := range append([]string{subcommand}, refs...) {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("m2m bridge refuses an empty argument")
		}
		if strings.ContainsAny(s, "\x00\n\r") {
			return nil, fmt.Errorf("m2m bridge refuses control characters in arguments")
		}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m2mBridgeDefaultTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, client, append([]string{subcommand}, refs...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdoutBuf, stderrBuf bytes.Buffer
	stdout := &cappedWriter{w: &stdoutBuf, limit: m2mBridgeOutputCap}
	stderr := &cappedWriter{w: &stderrBuf, limit: m2mBridgeOutputCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("m2m client %s failed: %v (stdout %d bytes+%d discarded, stderr %d bytes+%d discarded)",
			subcommand, err, stdout.stored, stdout.discarded, stderr.stored, stderr.discarded)
	}
	if stdout.discarded > 0 {
		return nil, fmt.Errorf("m2m client %s output exceeded %d bytes; refusing runaway result",
			subcommand, m2mBridgeOutputCap)
	}
	return stdoutBuf.Bytes(), nil
}

// runM2MBridge invokes one client subcommand and parses its small
// JSON result. argv carries only the subcommand and opaque refs;
// every sensitive byte travels on stdin.
func runM2MBridge(ctx context.Context, subcommand string, refs []string, stdin []byte) (m2mBridgeResult, error) {
	var out m2mBridgeResult
	raw, err := execM2MClient(ctx, subcommand, refs, stdin)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("m2m client %s returned unparseable output (%d bytes): %v",
			subcommand, len(raw), err)
	}
	if out.MessageID == "" || out.State == "" {
		return out, fmt.Errorf("m2m client %s omitted message_id or state", subcommand)
	}
	return out, nil
}

// Client inbox protocol (the contract the Drive Python client
// satisfies; Metis never reimplements transport, identity, or
// crypto, it only invokes):
//   - `receive` (no refs): the client prints one JSON array of
//     verified inbox items: [{message_id, sender_fp, type,
//     expires_at (RFC3339), payload_b64}]. Every item the client
//     emits is authenticated and verified client-side; Metis
//     still enforces its own fail-closed turn policy
//     (authorizeInboxTurn) before anything executes, and treats
//     the payload as untrusted data throughout.
//   - `ack <message_id> <state>` (no stdin): the client records
//     durable acknowledgment and prints {message_id, state}.
//
// Metis acknowledges exactly what it has durably decided: the
// settled dispatch state for executed turns, REFUSED for
// policy-refused items (deterministic, so redelivery would refuse
// identically). Dispatch errors are never acknowledged — the item
// stays pending and a later run reconciles it via the dispatch
// claim. Claim conflicts (same ID, different sender/target/
// payload) are evidence of forgery or operator error: they are
// logged, left unacknowledged, and surfaced nonzero.
type m2mInboxItem struct {
	MessageID string `json:"message_id"`
	SenderFP  string `json:"sender_fp"`
	Type      string `json:"type"`
	ExpiresAt string `json:"expires_at"`
	Payload   string `json:"payload_b64"`
}

// receiveInboxViaBridge fetches the client's verified inbox and
// converts it to envelopes. Malformed items are reported and
// skipped — never executed — while well-formed items still flow.
func receiveInboxViaBridge(ctx context.Context) ([]m2mEnvelope, []string, error) {
	raw, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var items []m2mInboxItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, fmt.Errorf("m2m client receive returned unparseable output (%d bytes): %v", len(raw), err)
	}
	var envs []m2mEnvelope
	var malformed []string
	for i, it := range items {
		env, merr := m2mInboxEnvelope(it)
		if merr != nil {
			malformed = append(malformed, fmt.Sprintf("item %d: %v", i, merr))
			continue
		}
		envs = append(envs, env)
	}
	return envs, malformed, nil
}

// m2mInboxEnvelope validates one client inbox item. Every field
// is required; the payload must decode and the expiry must parse.
// A bad item is data, never a turn.
// museReceiptTTL bounds recovered items (no sender expiry
// observed) to receipt time plus this operator TTL. Empty or
// invalid values fall back to one hour; non-positive values refuse
// to widen the window silently by falling back as well.
func museReceiptTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("METIS_M2M_RECEIPT_TTL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

func m2mInboxEnvelope(it m2mInboxItem) (m2mEnvelope, error) {
	if strings.TrimSpace(it.MessageID) == "" {
		return m2mEnvelope{}, fmt.Errorf("omitted message_id")
	}
	if strings.TrimSpace(it.SenderFP) == "" {
		return m2mEnvelope{}, fmt.Errorf("message %q omitted sender_fp", it.MessageID)
	}
	if strings.TrimSpace(it.Type) == "" {
		return m2mEnvelope{}, fmt.Errorf("message %q omitted type", it.MessageID)
	}
	rawExp := strings.TrimSpace(it.ExpiresAt)
	var exp time.Time
	if rawExp == "" {
		// Receipt-bounded recovery: the client consumed the
		// envelope (verified, acked, seen) before Metis observed
		// it — crash between consume and emit, or arrival after
		// the adapter's header snapshot. The sender TTL is gone
		// with the queue entry, and fabricating one would forge
		// sender intent. Bound executability to receipt time
		// plus the operator's METIS_M2M_RECEIPT_TTL instead.
		exp = time.Now().Add(museReceiptTTL())
	} else {
		var err error
		exp, err = time.Parse(time.RFC3339, rawExp)
		if err != nil {
			return m2mEnvelope{}, fmt.Errorf("message %q has unparseable expires_at %q", it.MessageID, it.ExpiresAt)
		}
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(it.Payload))
	if err != nil {
		return m2mEnvelope{}, fmt.Errorf("message %q has undecodable payload: %v", it.MessageID, err)
	}
	if len(payload) == 0 {
		return m2mEnvelope{}, fmt.Errorf("message %q carries an empty payload; message stays data", it.MessageID)
	}
	return m2mEnvelope{MessageID: it.MessageID, SenderFP: it.SenderFP, Type: it.Type, ExpiresAt: exp, Payload: payload}, nil
}

// m2mAckViaBridge confirms the sender-visible signed TRANSPORT
// acknowledgment exists for this exact message ID and records the
// dispatch state Metis durably decided. The echo proves transport
// receipt, never execution: execution completion is observed from
// the dispatch ledger and the turn path, not from this call.
func m2mAckViaBridge(ctx context.Context, messageID, state string) error {
	got, err := runM2MBridge(ctx, "ack", []string{messageID, state}, nil)
	if err != nil {
		return err
	}
	if got.MessageID != messageID || got.State != state {
		return fmt.Errorf("m2m client ack echo mismatch: sent %q/%q, got %q/%q",
			messageID, state, got.MessageID, got.State)
	}
	return nil
}

// m2mAdapterAckCap bounds the parent-confirmed cursor. Old entries
// age out FIFO; if a pruned ID is ever re-emitted, the dispatch
// claim (the real authority) converges it, so the cap cannot lose
// a message, only re-deliver one.
const m2mAdapterAckCap = 10000

// m2mAdapterAckPath is the parent-confirmed cursor the Drive
// adapter treats as its ONLY skip-set. The adapter records nothing
// itself: a crash after the child flushed stdout but before Metis
// commits the dispatch must redeliver, and only an entry written
// here — after the durable claim AND the recorded decision —
// suppresses re-emission.
func m2mAdapterAckPath() string {
	home := strings.TrimSpace(os.Getenv("METIS_M2M_HOME"))
	if home == "" {
		return ""
	}
	return filepath.Join(home, "m2m_acked.json")
}

// m2mRecordAdapterAck confirms one message to the adapter: the
// dispatch claim is durable and the outcome (executed terminal
// state or deterministic refusal, both relay-acknowledged by the
// caller) is recorded. Callers must invoke it only after success;
// any failure is returned so the item stays unacked and a later
// run reconciles it instead of assuming it.
func m2mRecordAdapterAck(messageID string) error {
	path := m2mAdapterAckPath()
	if path == "" {
		return fmt.Errorf("m2m adapter ack: METIS_M2M_HOME not set")
	}
	if strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("m2m adapter ack: empty message id")
	}
	var acked []string
	if raw, err := os.ReadFile(path); err == nil {
		var doc struct {
			Acked []string `json:"acked"`
		}
		if jerr := json.Unmarshal(raw, &doc); jerr == nil {
			acked = doc.Acked
		}
	}
	for _, id := range acked {
		if id == messageID {
			return nil
		}
	}
	acked = append(acked, messageID)
	if len(acked) > m2mAdapterAckCap {
		acked = acked[len(acked)-m2mAdapterAckCap:]
	}
	doc, err := json.Marshal(map[string]any{"acked": acked})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(doc, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadInboxDispatchState returns the durable dispatch state for
// one inbox message, or sql.ErrNoRows when no claim exists yet
// (a pre-claim refusal reserves nothing by design).
func loadInboxDispatchState(messageID string) (state, terminal string, err error) {
	db, err := openTasksDB()
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	if err := ensureMuseInboxDispatchTable(db); err != nil {
		return "", "", err
	}
	err = db.QueryRow(`SELECT state, terminal FROM muse_inbox_dispatch WHERE message_id = ?`,
		messageID).Scan(&state, &terminal)
	return state, terminal, err
}

// M2MReceiveItemOutcome is the per-item disposition of one
// receive/dispatch run: dispatched (with the settled ack state),
// refused (policy, acked REFUSED), error (unacknowledged, needs a
// later run), or unacked (dispatched but the ack itself failed).
type M2MReceiveItemOutcome struct {
	MessageID string
	Outcome   string
	State     string
	Detail    string
}

// M2MReceiveReport summarizes one bounded receive/dispatch run
// for the operator: every fetched item is accounted exactly once.
type M2MReceiveReport struct {
	Session      string
	Received     int
	Outcomes     []M2MReceiveItemOutcome
	Malformed    []string
	Unacked      int
	ClientFailed string
}

// m2mReceiveDispatch is the production M2M intake: fetch the
// client's verified inbox, dispatch each item through the single
// choke point (dispatchInboxToTurnOnHost, which owns claims,
// policy, and the one command identity), and acknowledge what
// Metis durably decided. Items are processed in client order, one
// at a time — replay converges on the dispatch claim, so there is
// no concurrent-execution hazard to parallelize away. The ctx
// bounds the whole run; a cancelled run simply leaves later items
// for the next invocation.
func m2mReceiveDispatch(ctx context.Context, h *museHost, targetSession string, wait time.Duration) M2MReceiveReport {
	rep := M2MReceiveReport{Session: targetSession}
	envs, malformed, err := receiveInboxViaBridge(ctx)
	if err != nil {
		rep.ClientFailed = err.Error()
		return rep
	}
	rep.Received = len(envs)
	rep.Malformed = malformed
	for _, env := range envs {
		if ctx.Err() != nil {
			rep.Outcomes = append(rep.Outcomes, M2MReceiveItemOutcome{
				MessageID: env.MessageID, Outcome: "deferred",
				Detail: fmt.Sprintf("run budget expired: %v; left for the next run", ctx.Err())})
			rep.Unacked++
			continue
		}
		item := M2MReceiveItemOutcome{MessageID: env.MessageID}
		res, derr := dispatchInboxToTurnOnHost(ctx, h, env, targetSession, wait)
		switch {
		case derr != nil:
			// Host/claim failure or claim conflict: nothing was
			// durably decided, so nothing is acknowledged. A
			// legitimate retry reconciles on the claim; a
			// conflict keeps refusing loudly until resolved.
			item.Outcome = "error"
			item.Detail = derr.Error()
			rep.Unacked++
		case res.State == museMessageRefused:
			item.Outcome = "refused"
			item.State = m2mRefused
			item.Detail = res.Detail
			if aerr := m2mAckViaBridge(ctx, env.MessageID, m2mRefused); aerr != nil {
				item.Outcome = "unacked"
				item.Detail = fmt.Sprintf("%s (ack failed: %v)", res.Detail, aerr)
				rep.Unacked++
			} else if perr := m2mRecordAdapterAck(env.MessageID); perr != nil {
				// Relay-acknowledged but the adapter cursor is not
				// confirmed: stay unacked so a later run reconciles
				// instead of assuming the handoff.
				item.Outcome = "unacked"
				item.Detail = fmt.Sprintf("%s (adapter ack failed: %v)", res.Detail, perr)
				rep.Unacked++
			}
		default:
			state, _, serr := loadInboxDispatchState(env.MessageID)
			if serr != nil {
				item.Outcome = "unacked"
				item.Detail = fmt.Sprintf("turn %s dispatched but dispatch state unreadable: %v", res.TurnID, serr)
				rep.Unacked++
				break
			}
			item.State = state
			item.Outcome = "dispatched"
			item.Detail = fmt.Sprintf("turn %s -> %s", res.TurnID, res.State)
			if aerr := m2mAckViaBridge(ctx, env.MessageID, state); aerr != nil {
				item.Outcome = "unacked"
				item.Detail = fmt.Sprintf("%s (ack failed: %v)", item.Detail, aerr)
				rep.Unacked++
			} else if perr := m2mRecordAdapterAck(env.MessageID); perr != nil {
				item.Outcome = "unacked"
				item.Detail = fmt.Sprintf("%s (adapter ack failed: %v)", item.Detail, perr)
				rep.Unacked++
			}
		}
		rep.Outcomes = append(rep.Outcomes, item)
	}
	return rep
}

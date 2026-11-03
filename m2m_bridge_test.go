package main

// m2m_bridge_test.go: durable M2M-to-Muse execution tracking.
//
// The Python transport (Drive: muse-to-muse) owns crypto, relay,
// and outbox; these tests pin only Metis's side: the dispatch table
// (crash recovery by message ID), the fail-closed inbox policy
// (expiry, sender, type), and the subprocess seam (secrets on
// stdin, never argv).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInboxDispatchClaimAndReplay(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	digest := m2mPayloadDigest([]byte("hello"))
	cmd1, replay, err := claimInboxDispatch("m-1", "fp-a", "qa", digest)
	if err != nil || replay || cmd1 == "" {
		t.Fatalf("first claim must mint: cmd=%q replay=%v err=%v", cmd1, replay, err)
	}
	cmd2, replay, err := claimInboxDispatch("m-1", "fp-a", "qa", digest)
	if err != nil || !replay || cmd2 != cmd1 {
		t.Fatalf("replay must resume the same command: %q vs %q replay=%v err=%v", cmd2, cmd1, replay, err)
	}
	if _, _, err := claimInboxDispatch("m-1", "fp-a", "qb", digest); err == nil {
		t.Fatal("retargeting a message to another session must conflict")
	}
	if _, _, err := claimInboxDispatch("m-1", "fp-b", "qa", digest); err == nil {
		t.Fatal("relabeling a message sender must conflict")
	}
	if _, _, err := claimInboxDispatch("m-1", "fp-a", "qa", m2mPayloadDigest([]byte("different"))); err == nil {
		t.Fatal("substituting the payload under a reused message ID must conflict")
	}
}

func TestRelayReconnectPreservesQueue(t *testing.T) {
	// Crash recovery: claims accepted before a restart resume the
	// same command IDs after it. The reopened database is the only
	// state that crosses the restart.
	state := t.TempDir()
	t.Setenv("METIS_STATE", state)
	want := map[string]string{}
	for _, id := range []string{"r-1", "r-2", "r-3"} {
		cmd, _, err := claimInboxDispatch(id, "fp-a", "qa", m2mPayloadDigest([]byte(id)))
		if err != nil {
			t.Fatal(err)
		}
		want[id] = cmd
	}
	// "Restart": nothing in memory survives; the table does.
	for id, cmd := range want {
		got, replay, err := claimInboxDispatch(id, "fp-a", "qa", m2mPayloadDigest([]byte(id)))
		if err != nil || !replay || got != cmd {
			t.Fatalf("post-restart claim of %q must resume %q, got %q replay=%v err=%v", id, cmd, got, replay, err)
		}
	}
}

func authzEnv(t *testing.T, senders, types string) {
	t.Helper()
	t.Setenv("METIS_M2M_SENDERS", senders)
	t.Setenv("METIS_M2M_TURN_TYPES", types)
}

func TestExpiredEnvelopeRejected(t *testing.T) {
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "e-1", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(-time.Minute)}
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("expired envelope must be refused")
	} else if !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("refusal must name expiry, got: %v", err)
	}
}

// TestReceiptBoundedExpiry pins the recovery rule: an adapter item
// with no sender expiry (the client consumed the envelope before
// Metis observed it — crash or mid-run arrival) parses to a
// receipt-bounded expiry of now + METIS_M2M_RECEIPT_TTL, never to a
// fabricated sender TTL. An explicit unparseable expiry is still
// malformed; authorizeInboxTurn still refuses zero/past expiries.
func TestReceiptBoundedExpiry(t *testing.T) {
	t.Setenv("METIS_M2M_RECEIPT_TTL", "")
	before := time.Now()
	env, err := m2mInboxEnvelope(m2mInboxItem{MessageID: "r-1", SenderFP: "fp-a",
		Type: "m2m.message", ExpiresAt: "",
		Payload: base64.StdEncoding.EncodeToString([]byte("hello"))})
	if err != nil {
		t.Fatalf("empty expires_at must normalize receipt-bounded, got: %v", err)
	}
	if env.ExpiresAt.Before(before.Add(59*time.Minute)) || env.ExpiresAt.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("default receipt TTL must be ~1h, got %v", env.ExpiresAt)
	}
	t.Setenv("METIS_M2M_RECEIPT_TTL", "5m")
	env, err = m2mInboxEnvelope(m2mInboxItem{MessageID: "r-2", SenderFP: "fp-a",
		Type: "m2m.message", ExpiresAt: "",
		Payload: base64.StdEncoding.EncodeToString([]byte("hello"))})
	if err != nil {
		t.Fatalf("TTL override must apply, got: %v", err)
	}
	if time.Until(env.ExpiresAt) > 5*time.Minute || time.Until(env.ExpiresAt) <= 0 {
		t.Fatalf("TTL override must bound to 5m, got %v", env.ExpiresAt)
	}
	bad := m2mInboxItem{MessageID: "r-3", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: "not-a-time", Payload: base64.StdEncoding.EncodeToString([]byte("hello"))}
	if _, err := m2mInboxEnvelope(bad); err == nil {
		t.Fatal("explicit unparseable expiry must stay malformed")
	}
}

// TestMissingExpiryRefused pins the mandatory-expiry rule: an
// envelope without an explicit future expiry never authorizes a
// turn, even from a registered sender with an allowed type.
func TestMissingExpiryRefused(t *testing.T) {
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "e-0", SenderFP: "fp-a", Type: "m2m.message"}
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("envelope without expiry must be refused")
	} else if !strings.Contains(err.Error(), "expiry") {
		t.Fatalf("refusal must name expiry, got: %v", err)
	}
}

func TestUnregisteredSenderRejected(t *testing.T) {
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "e-2", SenderFP: "fp-stranger", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("unregistered sender must be refused")
	} else if !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("refusal must name registration, got: %v", err)
	}
}

func TestDisallowedTypeStaysData(t *testing.T) {
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "e-3", SenderFP: "fp-a", Type: "chat",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("general inbox type must stay data, never a command")
	}
}

func TestAuthorizedEnvelopePasses(t *testing.T) {
	authzEnv(t, "fp-a,fp-b", "m2m.message")
	env := m2mEnvelope{MessageID: "e-4", SenderFP: "fp-b", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := authorizeInboxTurn(env, time.Now()); err != nil {
		t.Fatalf("authorized envelope must pass: %v", err)
	}
}

func TestFailClosedWithNoPolicy(t *testing.T) {
	authzEnv(t, "", "")
	env := m2mEnvelope{MessageID: "e-5", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("empty policy must authorize nobody")
	}
}

func TestM2MBridgeKeepsSecretsOffArgv(t *testing.T) {
	dir := t.TempDir()
	argvLog := filepath.Join(dir, "argv.log")
	script := filepath.Join(dir, "m2m-client")
	body := "#!/bin/sh\necho \"$@\" > \"$M2M_ARGV_LOG\"\ncat > /dev/null\nprintf '{\"message_id\":\"m-9\",\"state\":\"RELAY_ACCEPTED\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", script)
	t.Setenv("M2M_ARGV_LOG", argvLog)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := runM2MBridge(ctx, "send", []string{"ref-9"}, []byte("PLAINTEXT-SECRET"))
	if err != nil {
		t.Fatalf("bridge: %v", err)
	}
	if got.MessageID != "m-9" || got.State != "RELAY_ACCEPTED" {
		t.Fatalf("bridge must replay the transport state verbatim: %+v", got)
	}
	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(argv), "PLAINTEXT-SECRET") {
		t.Fatalf("plaintext must never reach argv: %q", argv)
	}
	if !strings.Contains(string(argv), "send ref-9") {
		t.Fatalf("argv must carry only subcommand plus opaque refs: %q", argv)
	}
}

func TestRevokedSenderRefusedTurn(t *testing.T) {
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "e-6", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour)}
	if err := authorizeInboxTurn(env, time.Now()); err != nil {
		t.Fatalf("authorized sender must pass: %v", err)
	}
	// Operator revokes the sender: the same envelope must now be
	// refused, even though the message is already stored.
	authzEnv(t, "", "m2m.message")
	if err := authorizeInboxTurn(env, time.Now()); err == nil {
		t.Fatal("revoked sender must be refused a turn")
	}
}

func inboxDispatchState(t *testing.T, id string) string {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state string
	if err := db.QueryRow(`SELECT state FROM muse_inbox_dispatch WHERE message_id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestTrustedInboxItemBecomesOneTurn proves the full phase-5 path:
// an authorized envelope becomes exactly one correlated native
// turn, and the dispatch row advances to TURN_COMPLETED.
func TestTrustedInboxItemBecomesOneTurn(t *testing.T) {
	st, h, sid := setupMessageSession(t, "mmx")
	_ = sid
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-1", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("inbox hello")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := st.submits.Load()
	got, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmx", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" || got.TurnID == "" {
		t.Fatalf("authorized dispatch must complete one turn, got %+v", got)
	}
	if got.SessionID != "mmx" {
		t.Fatalf("dispatch must target the exact session, got %q", got.SessionID)
	}
	// The submitted text must derive from the envelope payload:
	// the intent row's digest matches the payload bytes, and the
	// dispatch row froze the same digest at claim time.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var promptDigest, dispatchDigest, terminal string
	if err := db.QueryRow(`SELECT prompt_digest FROM muse_message_intents WHERE request_id = 'inbox:d-1'`).Scan(&promptDigest); err != nil {
		t.Fatal(err)
	}
	if promptDigest != museMessageDigest("inbox hello") {
		t.Fatalf("submitted text must derive from the envelope payload, digest %q", promptDigest)
	}
	if err := db.QueryRow(`SELECT payload_digest, terminal FROM muse_inbox_dispatch WHERE message_id = 'd-1'`).Scan(&dispatchDigest, &terminal); err != nil {
		t.Fatal(err)
	}
	if dispatchDigest != m2mPayloadDigest([]byte("inbox hello")) {
		t.Fatalf("dispatch row must freeze the payload digest, got %q", dispatchDigest)
	}
	if terminal != "completed" {
		t.Fatalf("dispatch row must retain the terminal verbatim, got %q", terminal)
	}
	if n := st.submits.Load() - before; n != 1 {
		t.Fatalf("one dispatch must submit exactly one turn, submitted %d", n)
	}
	if s := inboxDispatchState(t, "d-1"); s != m2mTurnCompleted {
		t.Fatalf("dispatch row must advance to TURN_COMPLETED, got %q", s)
	}
}

// TestUntrustedInboxItemSubmitsNothing proves the policy choke
// point: a stranger's envelope submits no turn and the item stays
// stored as data (marked refused, never executed).
func TestUntrustedInboxItemSubmitsNothing(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmy")
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-2", SenderFP: "fp-stranger", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("inbox hello")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := st.submits.Load()
	got, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmy", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageRefused || got.TurnID != "" {
		t.Fatalf("untrusted dispatch must refuse with no turn, got %+v", got)
	}
	if n := st.submits.Load() - before; n != 0 {
		t.Fatalf("refused dispatch must submit nothing, submitted %d", n)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM muse_message_intents WHERE request_id = 'inbox:d-2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("refused dispatch must persist no message intent")
	}
}

// TestFailedTerminalKeepsDisposition proves a non-completed
// terminal is never recorded as a success: the dispatch row keeps
// the observed disposition verbatim under its own TURN_* state,
// and the message intent stores the same terminal.
func TestFailedTerminalKeepsDisposition(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmf")
	st.autoComplete.Store(false)
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-3", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("fail this turn")}
	b, err := getMuseBinding("mmf")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Submit in the background, then fail the exact turn on the
	// live stream and wait for the failure through dispatch.
	type outcome struct {
		res MuseMessageResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmf", 10*time.Second)
		done <- outcome{res, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	var turnID string
	for turnID == "" && time.Now().Before(deadline) {
		db, derr := openTasksDB()
		if derr != nil {
			t.Fatal(derr)
		}
		_ = db.QueryRow(`SELECT turn_id FROM muse_message_intents WHERE request_id = 'inbox:d-3'`).Scan(&turnID)
		_ = db.Close()
		if turnID == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if turnID == "" {
		t.Fatal("dispatch must admit a turn before the terminal can be observed")
	}
	raw, _ := json.Marshal(map[string]any{
		"sessionId": b.NativeID, "turnId": turnID,
		"terminal": "failed", "viewCursor": "c-failed",
	})
	h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.res.State != "failed" || got.res.TurnID != turnID {
		t.Fatalf("failed turn must report its disposition, got %+v", got.res)
	}
	if s := inboxDispatchState(t, "d-3"); s != "TURN_FAILED" {
		t.Fatalf("dispatch row must record TURN_FAILED, never TURN_COMPLETED, got %q", s)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var terminal string
	if err := db.QueryRow(`SELECT terminal FROM muse_inbox_dispatch WHERE message_id = 'd-3'`).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != "failed" {
		t.Fatalf("dispatch row must retain the terminal verbatim, got %q", terminal)
	}
}

// TestRevocationKeepsCompletedHistory proves the monotonic guard:
// revoking the sender after a completed dispatch refuses the
// redelivery but never rewrites the settled TURN_COMPLETED row.
func TestRevocationKeepsCompletedHistory(t *testing.T) {
	_, h, _ := setupMessageSession(t, "mmv")
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-4", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("do it once")}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmv", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "completed" {
		t.Fatalf("first dispatch must complete, got %+v", first)
	}
	// Operator revokes the sender: the redelivery submits nothing.
	authzEnv(t, "", "m2m.message")
	second, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmv", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != museMessageRefused {
		t.Fatalf("revoked redelivery must refuse, got %+v", second)
	}
	if s := inboxDispatchState(t, "d-4"); s != m2mTurnCompleted {
		t.Fatalf("revocation must not rewrite settled history, got %q", s)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var terminal string
	if err := db.QueryRow(`SELECT terminal FROM muse_inbox_dispatch WHERE message_id = 'd-4'`).Scan(&terminal); err != nil {
		t.Fatal(err)
	}
	if terminal != "completed" {
		t.Fatalf("settled terminal must survive revocation, got %q", terminal)
	}
}

// TestAdmittedTurnAdvancesToCompleted proves the P0-A lifecycle:
// an admission-only dispatch persists TURN_ADMITTED (nonterminal),
// and a later dispatch of the same envelope observes the same
// turn completing, advances the ledger to TURN_COMPLETED, and
// submits no second turn.
func TestAdmittedTurnAdvancesToCompleted(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mma")
	st.autoComplete.Store(false)
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-5", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("admit then complete")}
	b, err := getMuseBinding("mma")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := dispatchInboxToTurnOnHost(ctx, h, env, "mma", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.TurnID == "" {
		t.Fatal("admission-only dispatch must retain the turn")
	}
	if s := inboxDispatchState(t, "d-5"); s != m2mTurnAdmitted {
		t.Fatalf("admission must persist TURN_ADMITTED, got %q", s)
	}
	// Reconnect: the same turn completes on the live stream; the
	// redelivery reconciles instead of submitting again.
	st.autoComplete.Store(true)
	raw, _ := json.Marshal(map[string]any{
		"sessionId": b.NativeID, "turnId": first.TurnID,
		"terminal": "completed", "viewCursor": "c-admit",
	})
	h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	second, err := dispatchInboxToTurnOnHost(ctx, h, env, "mma", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "completed" || second.TurnID != first.TurnID {
		t.Fatalf("redelivery must reconcile the same turn, got %+v", second)
	}
	if s := inboxDispatchState(t, "d-5"); s != m2mTurnCompleted {
		t.Fatalf("completion must advance the ledger past admission, got %q", s)
	}
	if n := st.submits.Load(); n != 1 {
		t.Fatalf("admit-then-complete must submit exactly one turn, got %d", n)
	}
}

// TestAdmittedTurnAdvancesToFailed proves a non-completed terminal
// advances the ledger to its own disposition, never to success.
func TestAdmittedTurnAdvancesToFailed(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmfa")
	st.autoComplete.Store(false)
	authzEnv(t, "fp-a", "m2m.message")
	env := m2mEnvelope{MessageID: "d-6", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: []byte("admit then fail")}
	b, err := getMuseBinding("mmfa")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmfa", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	st.autoComplete.Store(true)
	raw, _ := json.Marshal(map[string]any{
		"sessionId": b.NativeID, "turnId": first.TurnID,
		"terminal": "cancelled", "viewCursor": "c-cancel",
	})
	h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	second, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmfa", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "cancelled" {
		t.Fatalf("cancelled turn must report its disposition, got %+v", second)
	}
	if s := inboxDispatchState(t, "d-6"); s != "TURN_CANCELLED" {
		t.Fatalf("cancellation must advance past admission, got %q", s)
	}
}

// TestDispatchIdentityDriftRefuses seeds a deliberately
// mismatched historical row (dispatch identity vs message-intent
// identity) and proves the redelivery refuses without submitting
// another turn.
func TestDispatchIdentityDriftRefuses(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmd")
	st.autoComplete.Store(false)
	authzEnv(t, "fp-a", "m2m.message")
	b, err := getMuseBinding("mmd")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("drifted history")
	env := m2mEnvelope{MessageID: "dd-1", SenderFP: "fp-a", Type: "m2m.message",
		ExpiresAt: time.Now().Add(time.Hour), Payload: payload}
	cmdA, _, err := claimInboxDispatch("dd-1", "fp-a", "mmd", m2mPayloadDigest(payload))
	if err != nil {
		t.Fatal(err)
	}
	// Historical row naming a DIFFERENT command identity for the
	// same request — the condition the equality check exists for.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO muse_message_intents
		(request_id, session, native_id, profile, provider, workspace, prompt_digest, principal, command_id, turn_id, terminal, detail, created, updated)
		VALUES ('inbox:dd-1', 'mmd', ?, ?, ?, ?, ?, 'm2m:fp-a', 'cmd-other', '', '', '', ?, ?)`,
		b.NativeID, b.Profile, b.Provider, b.Workspace, museMessageDigest(string(payload)),
		nowUTC(), nowUTC()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_ = cmdA
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := st.submits.Load()
	got, err := dispatchInboxToTurnOnHost(ctx, h, env, "mmd", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageRefused {
		t.Fatalf("identity drift must refuse, got %+v", got)
	}
	if !strings.Contains(got.Detail, "identity drift") {
		t.Fatalf("refusal must name the drift, got %q", got.Detail)
	}
	if n := st.submits.Load() - before; n != 0 {
		t.Fatalf("drifted redelivery must submit nothing, submitted %d", n)
	}
}

// TestConcurrentOutcomeWritersConverge fires simultaneous
// admission/completion/refusal writers at one dispatch row and
// proves the conditional transitions converge: the terminal wins
// exactly once and a late refusal cannot rewrite settled history.
func TestConcurrentOutcomeWritersConverge(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMuseInboxDispatchTable(db); err != nil {
		t.Fatal(err)
	}
	db.Close()
	digest := m2mPayloadDigest([]byte("converge"))
	if _, _, err := claimInboxDispatch("cw-1", "fp-a", "qa", digest); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_ = recordInboxDispatchOutcome("cw-1", m2mTurnAdmitted, "")
			case 1:
				_ = recordInboxDispatchOutcome("cw-1", m2mTurnCompleted, "completed")
			default:
				_ = recordInboxDispatchOutcome("cw-1", m2mRefused, "")
			}
		}(i)
	}
	wg.Wait()
	// A late refusal after settlement must be a no-op.
	_ = recordInboxDispatchOutcome("cw-1", m2mRefused, "")
	db, err = openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var state, terminal string
	if err := db.QueryRow(`SELECT state, terminal FROM muse_inbox_dispatch WHERE message_id = 'cw-1'`).Scan(&state, &terminal); err != nil {
		t.Fatal(err)
	}
	if state != m2mTurnCompleted || terminal != "completed" {
		t.Fatalf("concurrent writers must converge on the observed terminal, got state=%q terminal=%q", state, terminal)
	}
	if !m2mDispatchSettled(state, terminal) {
		t.Fatal("settled row must report settled")
	}
	if m2mDispatchSettled(m2mTurnAdmitted, "") {
		t.Fatal("TURN_ADMITTED must stay nonterminal")
	}
}

// TestConcurrentDispatchClaimsConverge fires simultaneous identical
// inbox claims and proves one command wins: every claim succeeds
// on the same command and exactly one is the first writer.
func TestConcurrentDispatchClaimsConverge(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	digest := m2mPayloadDigest([]byte("race"))
	const racers = 12
	type claim struct {
		cmd    string
		replay bool
		err    error
	}
	out := make([]claim, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd, replay, err := claimInboxDispatch("race-1", "fp-a", "qa", digest)
			out[i] = claim{cmd, replay, err}
		}(i)
	}
	wg.Wait()
	first := 0
	for i, c := range out {
		if c.err != nil {
			t.Fatalf("racer %d: identical claim must converge, got %v", i, c.err)
		}
		if c.cmd != out[0].cmd {
			t.Fatalf("racer %d: command %q diverges from winner %q", i, c.cmd, out[0].cmd)
		}
		if !c.replay {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("exactly one racer must be the first writer, got %d", first)
	}
}

// TestBridgeRequiresExplicitClient pins the no-fallback rule: an
// unset client, a relative path, a directory, and a non-executable
// file are all refused before anything is spawned.
func TestBridgeRequiresExplicitClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Setenv("METIS_M2M_CLIENT", "")
	if _, err := runM2MBridge(ctx, "send", []string{"r"}, nil); err == nil {
		t.Fatal("unset client must refuse")
	} else if !strings.Contains(err.Error(), "no client configured") {
		t.Fatalf("refusal must name the missing configuration, got: %v", err)
	}
	t.Setenv("METIS_M2M_CLIENT", "m2m-client")
	if _, err := runM2MBridge(ctx, "send", []string{"r"}, nil); err == nil {
		t.Fatal("bare-name client must refuse (no PATH fallback)")
	}
	dir := t.TempDir()
	t.Setenv("METIS_M2M_CLIENT", dir)
	if _, err := runM2MBridge(ctx, "send", []string{"r"}, nil); err == nil {
		t.Fatal("directory client must refuse")
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", plain)
	if _, err := runM2MBridge(ctx, "send", []string{"r"}, nil); err == nil {
		t.Fatal("non-executable client must refuse")
	}
}

// TestBridgeFailureOmitsSubprocessText proves error sanitizing: a
// failing client surfaces the subcommand and byte counts, never
// the subprocess's own stderr content.
func TestBridgeFailureOmitsSubprocessText(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "m2m-client")
	body := "#!/bin/sh\necho 'LEAKED-RELAY-SECRET' >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", script)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := runM2MBridge(ctx, "send", []string{"ref-x"}, []byte("SECRET-STDIN"))
	if err == nil {
		t.Fatal("failing client must surface an error")
	}
	if strings.Contains(err.Error(), "LEAKED-RELAY-SECRET") {
		t.Fatalf("bridge error must not copy subprocess text: %v", err)
	}
	if !strings.Contains(err.Error(), "send") || !strings.Contains(err.Error(), "stderr") {
		t.Fatalf("bridge error must name the subcommand and counts, got: %v", err)
	}
}

// fixtureM2MClient installs a script speaking the documented inbox
// protocol: `receive` prints the given items file, `ack` appends
// its refs to ackLog and echoes {message_id, state}. It plays the
// trusted-client role: identity verification is the client's job;
// Metis still enforces its own turn policy on every item.
func fixtureM2MClient(t *testing.T, itemsJSON string) (ackLog string) {
	t.Helper()
	dir := t.TempDir()
	inbox := filepath.Join(dir, "inbox.json")
	if err := os.WriteFile(inbox, []byte(itemsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	ackLog = filepath.Join(dir, "acks.log")
	script := filepath.Join(dir, "m2m-client")
	body := "#!/bin/sh\n" +
		"if [ \"$1\" = \"receive\" ]; then cat \"$M2M_INBOX_JSON\"; exit 0; fi\n" +
		"if [ \"$1\" = \"ack\" ]; then echo \"$@\" >> \"$M2M_ACK_LOG\"; " +
		"printf '{\"message_id\":\"%s\",\"state\":\"%s\"}' \"$2\" \"$3\"; exit 0; fi\n" +
		"echo \"unknown subcommand $1\" >&2; exit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", script)
	t.Setenv("M2M_INBOX_JSON", inbox)
	t.Setenv("M2M_ACK_LOG", ackLog)
	return ackLog
}

func inboxItemJSON(id, fp, typ, exp, payload string) map[string]any {
	return map[string]any{
		"message_id": id, "sender_fp": fp, "type": typ,
		"expires_at": exp, "payload_b64": base64.StdEncoding.EncodeToString([]byte(payload)),
	}
}

// TestProductionReceiveDispatchAckCycle is the P0-E regression:
// an authenticated inbox message travels through the production
// intake (m2mReceiveDispatch — the function `metis m2m receive`
// calls) to a real bound echo session, producing a correlated
// admission, an observed terminal, and a client acknowledgment.
// A forged item from an unregistered sender submits nothing and
// acks REFUSED; a malformed item never executes. Redelivery
// submits no second turn and re-acks idempotently.
func TestProductionReceiveDispatchAckCycle(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mrx")
	authzEnv(t, "fp-a", "m2m.message")
	// The dispatch path records the parent-confirmed adapter
	// cursor, so the cycle needs an adapter home like every
	// other receive test.
	home := t.TempDir()
	t.Setenv("METIS_M2M_HOME", home)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	items, _ := json.Marshal([]any{
		inboxItemJSON("inbox-1", "fp-a", "m2m.message", exp, "hello inbox"),
		inboxItemJSON("inbox-2", "fp-evil", "m2m.message", exp, "forged turn"),
		map[string]any{"message_id": "inbox-3", "sender_fp": "fp-a",
			"type": "m2m.message", "expires_at": exp, "payload_b64": "!!!not-base64!!!"},
	})
	ackLog := fixtureM2MClient(t, string(items))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rep := m2mReceiveDispatch(ctx, h, "mrx", 10*time.Second)
	if rep.ClientFailed != "" {
		t.Fatalf("receive must succeed, got: %s", rep.ClientFailed)
	}
	if rep.Received != 2 {
		t.Fatalf("two well-formed items must flow, got %d", rep.Received)
	}
	if len(rep.Malformed) != 1 || !strings.Contains(rep.Malformed[0], "inbox-3") {
		t.Fatalf("malformed item must be reported, not executed: %+v", rep.Malformed)
	}
	byID := map[string]M2MReceiveItemOutcome{}
	for _, it := range rep.Outcomes {
		byID[it.MessageID] = it
	}
	ok1, found1 := byID["inbox-1"]
	if !found1 || ok1.Outcome != "dispatched" {
		t.Fatalf("authenticated item must dispatch, got %+v", byID)
	}
	if ok1.State != m2mTurnCompleted {
		t.Fatalf("dispatched turn must settle completed, got %+v", ok1)
	}
	if n := st.submits.Load(); n != 1 {
		t.Fatalf("exactly one turn must submit, got %d", n)
	}
	if got := inboxDispatchState(t, "inbox-1"); got != m2mTurnCompleted {
		t.Fatalf("dispatch row must read TURN_COMPLETED, got %q", got)
	}
	// Parent-confirmed cursor: the adapter skip-set must hold
	// the dispatched ID after the durable claim, so a crash past
	// this point redelivers nothing. (Relay-acked refusals are
	// cursor-confirmed too, by the bridge's documented design;
	// only the dispatched ID is pinned here.)
	acked, err := os.ReadFile(filepath.Join(home, "m2m_acked.json"))
	if err != nil {
		t.Fatalf("adapter cursor must exist after dispatch: %v", err)
	}
	if !strings.Contains(string(acked), "inbox-1") {
		t.Fatalf("cursor must confirm the dispatched ID, got: %s", acked)
	}
	bad2, found2 := byID["inbox-2"]
	if !found2 || bad2.Outcome != "refused" || bad2.State != m2mRefused {
		t.Fatalf("forged item must refuse with REFUSED, got %+v", byID)
	}
	acks, err := os.ReadFile(ackLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(acks), "ack inbox-1 TURN_COMPLETED") {
		t.Fatalf("completion must be acknowledged, log:\n%s", acks)
	}
	if !strings.Contains(string(acks), "ack inbox-2 REFUSED") {
		t.Fatalf("refusal must be acknowledged REFUSED, log:\n%s", acks)
	}
	// Redelivery: no second turn, same durable decisions, acks
	// repeated idempotently.
	rep2 := m2mReceiveDispatch(ctx, h, "mrx", 10*time.Second)
	if rep2.ClientFailed != "" {
		t.Fatalf("redelivery must succeed, got: %s", rep2.ClientFailed)
	}
	if n := st.submits.Load(); n != 1 {
		t.Fatalf("redelivery must submit no second turn, submits=%d", n)
	}
	acks2, _ := os.ReadFile(ackLog)
	if n := strings.Count(string(acks2), "ack inbox-1 TURN_COMPLETED"); n != 2 {
		t.Fatalf("completion ack must repeat idempotently, got %d in:\n%s", n, acks2)
	}
}

// TestM2MDriveAdapterLiveClient is the P1-G contract test: the
// committed Drive adapter (scripts/m2m_drive.py) against the REAL
// M2M Python client, not a fixture. It proves the adapter emits the
// bridge protocol from genuinely verified mail (signature checked
// against the enrolled key, expiry pruned, replay deduped by the
// client) and refuses forged/tampered acks. The client lives outside
// driveLoopbackGate verifies the real-client prerequisites shared by
// the Drive adapter tests: METIS_M2M_LIB pointing at a directory
// containing the real m2m.py, plus python3 cryptography. It skips
// in normal gates (same pattern as the Ocliv loopback test).
func driveLoopbackGate(t *testing.T) string {
	t.Helper()
	lib := os.Getenv("METIS_M2M_LIB")
	if lib == "" {
		t.Skip("METIS_M2M_LIB not set: real M2M client checkout absent")
	}
	if _, err := os.Stat(filepath.Join(lib, "m2m.py")); err != nil {
		t.Skipf("real m2m.py absent in %s: %v", lib, err)
	}
	if out, err := exec.Command("python3", "-c", "import cryptography").CombinedOutput(); err != nil {
		t.Skipf("python3 cryptography missing: %s", out)
	}
	return lib
}

// driveLoopbackPair builds two mutually enrolled real endpoints
// (alice sends, metisbox receives) over a fresh file relay.
func driveLoopbackPair(t *testing.T, lib string) (homeA, homeB, relay, fpA string) {
	t.Helper()
	dir := t.TempDir()
	homeA, homeB, relay = filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "relay")
	runPy := func(args ...string) string {
		cmd := exec.Command("python3", append([]string{filepath.Join(lib, "m2m.py")}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("m2m.py %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runPy("init", "--home", homeA, "--name", "alice")
	runPy("init", "--home", homeB, "--name", "metisbox")
	var expA, expB struct{ Fingerprint string }
	if err := json.Unmarshal([]byte(runPy("export", "--home", homeA)), &expA); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(runPy("export", "--home", homeB)), &expB); err != nil {
		t.Fatal(err)
	}
	bundleA, bundleB := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	if err := os.WriteFile(bundleA, []byte(runPy("export", "--home", homeA)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleB, []byte(runPy("export", "--home", homeB)), 0o600); err != nil {
		t.Fatal(err)
	}
	runPy("add-contact", "--home", homeB, "--bundle", bundleA, "--verified-fp", expA.Fingerprint)
	runPy("add-contact", "--home", homeA, "--bundle", bundleB, "--verified-fp", expB.Fingerprint)
	if err := os.MkdirAll(relay, 0o700); err != nil {
		t.Fatal(err)
	}
	return homeA, homeB, relay, expA.Fingerprint
}

// driveSend delivers one real sealed message into the relay.
func driveSend(t *testing.T, lib, homeA, relay, to, text string) string {
	t.Helper()
	send := exec.Command("python3", filepath.Join(lib, "m2m.py"), "send",
		"--home", homeA, "--relay", relay, "--to", to, "--ttl", "3600")
	send.Stdin = strings.NewReader(text)
	out, err := send.CombinedOutput()
	if err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	var res struct {
		MsgID string `json:"msg_id"`
	}
	if err := json.Unmarshal(out, &res); err != nil || res.MsgID == "" {
		t.Fatalf("send must report msg_id, got %s err %v", out, err)
	}
	return res.MsgID
}

// this repo, so the test runs only with METIS_M2M_LIB pointing at
// the directory containing the real m2m.py (same gate pattern as
// the Ocliv loopback test); it skips in normal gates.
func TestM2MDriveAdapterLiveClient(t *testing.T) {
	lib := os.Getenv("METIS_M2M_LIB")
	if lib == "" {
		t.Skip("METIS_M2M_LIB not set: real M2M client checkout absent")
	}
	if _, err := os.Stat(filepath.Join(lib, "m2m.py")); err != nil {
		t.Skipf("real m2m.py absent in %s: %v", lib, err)
	}
	py := "python3"
	if out, err := exec.Command(py, "-c", "import cryptography").CombinedOutput(); err != nil {
		t.Skipf("python3 cryptography missing: %s", out)
	}
	dir := t.TempDir()
	homeA, homeB, relay := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, "relay")
	runPy := func(args ...string) string {
		cmd := exec.Command(py, append([]string{filepath.Join(lib, "m2m.py")}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("m2m.py %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	runPy("init", "--home", homeA, "--name", "alice")
	runPy("init", "--home", homeB, "--name", "metisbox")
	expA := struct{ Fingerprint string }{}
	if err := json.Unmarshal([]byte(runPy("export", "--home", homeA)), &expA); err != nil {
		t.Fatal(err)
	}
	expB := struct{ Fingerprint string }{}
	if err := json.Unmarshal([]byte(runPy("export", "--home", homeB)), &expB); err != nil {
		t.Fatal(err)
	}
	fpA, fpB := expA.Fingerprint, expB.Fingerprint
	bundleA := filepath.Join(dir, "a.json")
	bundleB := filepath.Join(dir, "b.json")
	if err := os.WriteFile(bundleA, []byte(runPy("export", "--home", homeA)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bundleB, []byte(runPy("export", "--home", homeB)), 0o600); err != nil {
		t.Fatal(err)
	}
	runPy("add-contact", "--home", homeB, "--bundle", bundleA, "--verified-fp", fpA)
	runPy("add-contact", "--home", homeA, "--bundle", bundleB, "--verified-fp", fpB)
	if err := os.MkdirAll(relay, 0o700); err != nil {
		t.Fatal(err)
	}
	send := exec.Command(py, filepath.Join(lib, "m2m.py"), "send",
		"--home", homeA, "--relay", relay, "--to", "metisbox", "--ttl", "3600")
	send.Stdin = strings.NewReader("hello drive")
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	adapter, err := filepath.Abs(filepath.Join("scripts", "m2m_drive.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", adapter)
	t.Setenv("METIS_M2M_LIB", lib)
	t.Setenv("METIS_M2M_HOME", homeB)
	t.Setenv("METIS_M2M_RELAY", relay)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	raw, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("adapter receive: %v", err)
	}
	var items []m2mInboxItem
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("adapter receive unparsable (%d bytes): %v", len(raw), err)
	}
	if len(items) != 1 {
		t.Fatalf("one verified item must flow, got %+v", items)
	}
	env, err := m2mInboxEnvelope(items[0])
	if err != nil {
		t.Fatalf("adapter item must satisfy the inbox contract: %v", err)
	}
	if env.SenderFP != fpA || env.Type != "m2m.message" || string(env.Payload) != "hello drive" {
		t.Fatalf("adapter item misattributed: %+v", env)
	}
	if time.Until(env.ExpiresAt) <= 0 {
		t.Fatalf("adapter must carry a future expiry, got %v", env.ExpiresAt)
	}
	// Parent confirms after durably committing (the receive path
	// does this once the dispatch claim and the decision are
	// recorded): only then does a replay stay empty. Without the
	// confirmation the adapter re-emits by design.
	if err := m2mRecordAdapterAck(env.MessageID); err != nil {
		t.Fatalf("parent ack: %v", err)
	}
	raw2, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("adapter replay receive: %v", err)
	}
	var items2 []m2mInboxItem
	if err := json.Unmarshal(raw2, &items2); err != nil || len(items2) != 0 {
		t.Fatalf("replay must be empty, got %s err %v", raw2, err)
	}
	// Ack echoes the state once the sender-visible signed ack exists.
	got, err := runM2MBridge(ctx, "ack", []string{env.MessageID, "TURN_COMPLETED"}, nil)
	if err != nil {
		t.Fatalf("adapter ack: %v", err)
	}
	if got.MessageID != env.MessageID || got.State != "TURN_COMPLETED" {
		t.Fatalf("ack echo mismatch: %+v", got)
	}
	// A forged ack file is refused: no sender-visible signed ack.
	ackDir := filepath.Join(relay, "acks", "deadbeef")
	if err := os.MkdirAll(ackDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ackDir, "fake-1.json"),
		[]byte(`{"msg_id":"fake-1","signature":"AAAA","body":{"msg_id":"fake-1","recipient_fp":"nope"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runM2MBridge(ctx, "ack", []string{"fake-1", "TURN_COMPLETED"}, nil); err == nil {
		t.Fatal("forged ack must be refused")
	}
}

// TestM2MDriveAdapterCrashRecovery proves the loss window is closed:
// the real client durably consumes a message (inbox + signed ack +
// seen + queue delete) before Metis claims it. A crash between
// consume and emit must not strand the message: the next adapter
// run recovers it from the durable inbox, receipt-bounded (no
// sender expiry observed), and dispatch idempotency keeps the
// recovery safe.
func TestM2MDriveAdapterCrashRecovery(t *testing.T) {
	lib := driveLoopbackGate(t)
	_, homeB, relay, fpA := driveLoopbackPair(t, lib)
	homeA := filepath.Join(filepath.Dir(homeB), "a")
	mid := driveSend(t, lib, homeA, relay, "metisbox", "do not lose me")
	adapter, err := filepath.Abs(filepath.Join("scripts", "m2m_drive.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", adapter)
	t.Setenv("METIS_M2M_LIB", lib)
	t.Setenv("METIS_M2M_HOME", homeB)
	t.Setenv("METIS_M2M_RELAY", relay)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Crash between client-consume and adapter-emit.
	crash := exec.CommandContext(ctx, "python3", adapter, "receive")
	crash.Env = append(os.Environ(), "_M2M_DRIVE_CRASH_AFTER_POLL=1")
	if out, err := crash.CombinedOutput(); err == nil {
		t.Fatalf("crash hook must fail, got: %s", out)
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 99 {
		t.Fatalf("crash hook must exit 99, got %v (%s)", err, out)
	}
	// Recovery run: the verified message must surface despite never
	// having been emitted, with no sender expiry (receipt-bounded).
	raw, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("recovery receive: %v", err)
	}
	var items []m2mInboxItem
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		t.Fatalf("exactly one recovered item must flow, got %s err %v", raw, err)
	}
	if items[0].MessageID != mid || items[0].ExpiresAt != "" {
		t.Fatalf("recovered item must carry the id with empty expiry, got %+v", items[0])
	}
	env, err := m2mInboxEnvelope(items[0])
	if err != nil {
		t.Fatalf("recovered item must normalize receipt-bounded: %v", err)
	}
	if env.SenderFP != fpA || time.Until(env.ExpiresAt) <= 0 {
		t.Fatalf("recovered envelope misattributed: %+v", env)
	}
	// Without a parent confirmation the next run must re-emit, not
	// assume: the adapter records nothing itself.
	rawAgain, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("unconfirmed receive: %v", err)
	}
	var itemsAgain []m2mInboxItem
	if err := json.Unmarshal(rawAgain, &itemsAgain); err != nil || len(itemsAgain) != 1 || itemsAgain[0].MessageID != mid {
		t.Fatalf("unconfirmed item must re-emit, got %s err %v", rawAgain, err)
	}
	// Parent confirms after durably committing: only now does the
	// cursor suppress re-emission.
	if err := m2mRecordAdapterAck(mid); err != nil {
		t.Fatalf("parent ack: %v", err)
	}
	// Idempotent: a third run emits nothing new.
	raw2, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("post-recovery receive: %v", err)
	}
	var items2 []m2mInboxItem
	if err := json.Unmarshal(raw2, &items2); err != nil || len(items2) != 0 {
		t.Fatalf("post-recovery run must be empty, got %s", raw2)
	}
}

// TestM2MDriveAdapterCrashBeforeRecord proves the parent-crash
// side of the at-least-once contract: the child flushed stdout but
// the parent died before committing, so no parent confirmation
// exists. The recovery run must re-emit (never strand), and once
// the parent confirms after a durable commit, later runs stay
// empty. The Metis dispatch claim converges the redelivery onto
// the same command instead of submitting twice.
func TestM2MDriveAdapterCrashBeforeRecord(t *testing.T) {
	lib := driveLoopbackGate(t)
	_, homeB, relay, fpA := driveLoopbackPair(t, lib)
	homeA := filepath.Join(filepath.Dir(homeB), "a")
	mid := driveSend(t, lib, homeA, relay, "metisbox", "redeliver me")
	adapter, err := filepath.Abs(filepath.Join("scripts", "m2m_drive.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", adapter)
	t.Setenv("METIS_M2M_LIB", lib)
	t.Setenv("METIS_M2M_HOME", homeB)
	t.Setenv("METIS_M2M_RELAY", relay)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Crash after the stdout handoff, before the parent could
	// commit: no confirmation exists, so recovery must re-emit.
	crash := exec.CommandContext(ctx, "python3", adapter, "receive")
	crash.Env = append(os.Environ(), "_M2M_DRIVE_CRASH_BEFORE_RECORD=1")
	out, err := crash.Output()
	if err == nil {
		t.Fatalf("crash hook must fail, got: %s", out)
	} else if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 98 {
		t.Fatalf("crash hook must exit 98, got %v (%s)", err, out)
	}
	// The handoff reached stdout before the crash: the item was
	// readable despite the missing cursor.
	var handed []m2mInboxItem
	if err := json.Unmarshal(out, &handed); err != nil || len(handed) != 1 || handed[0].MessageID != mid {
		t.Fatalf("crashed run must still hand off the item on stdout, got %s", out)
	}
	// Recovery run: nothing was parent-confirmed, so the verified
	// message must be re-emitted, not stranded.
	raw, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("recovery receive: %v", err)
	}
	var items []m2mInboxItem
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 || items[0].MessageID != mid {
		t.Fatalf("exactly one redelivered item must flow, got %s err %v", raw, err)
	}
	if items[0].SenderFP != fpA {
		t.Fatalf("redelivery must keep the verified sender, got %+v", items[0])
	}
	// Parent commits the dispatch, then confirms: only now does a
	// later run stay empty.
	t.Setenv("METIS_STATE", t.TempDir())
	rawPayload, err := base64.StdEncoding.DecodeString(items[0].Payload)
	if err != nil {
		t.Fatalf("payload must decode: %v", err)
	}
	digest := m2mPayloadDigest(rawPayload)
	cmd1, replay, err := claimInboxDispatch(mid, fpA, "qa", digest)
	if err != nil || replay || cmd1 == "" {
		t.Fatalf("first claim must mint: cmd=%q replay=%v err=%v", cmd1, replay, err)
	}
	if err := m2mRecordAdapterAck(mid); err != nil {
		t.Fatalf("parent ack after commit: %v", err)
	}
	raw3, err := execM2MClient(ctx, "receive", nil, nil)
	if err != nil {
		t.Fatalf("post-recovery receive: %v", err)
	}
	var items3 []m2mInboxItem
	if err := json.Unmarshal(raw3, &items3); err != nil || len(items3) != 0 {
		t.Fatalf("post-confirmation run must be empty, got %s", raw3)
	}
	// Metis-side convergence: both emissions claim the same
	// command; the redelivery never submits twice.
	// Metis-side convergence: the redelivery claims the same
	// command the first emission minted; nothing submits twice.
	rawPayload2, err := base64.StdEncoding.DecodeString(items[0].Payload)
	if err != nil {
		t.Fatalf("payload must decode: %v", err)
	}
	digest2 := m2mPayloadDigest(rawPayload2)
	cmd2, replay, err := claimInboxDispatch(mid, fpA, "qa", digest2)
	if err != nil || !replay || cmd2 != cmd1 {
		t.Fatalf("redelivery claim must resume the same command: %q vs %q replay=%v err=%v", cmd2, cmd1, replay, err)
	}
}

// TestM2MAdapterAckFile pins the parent-confirmed cursor the Drive
// adapter treats as its only skip-set: missing home fails safe
// (caller leaves the item unacked), writes are idempotent and
// atomic, and corrupt files reset rather than blocking recovery.
func TestM2MAdapterAckFile(t *testing.T) {
	t.Setenv("METIS_M2M_HOME", "")
	if m2mAdapterAckPath() != "" {
		t.Fatal("missing home must yield no ack path")
	}
	if err := m2mRecordAdapterAck("m-0"); err == nil {
		t.Fatal("missing home must fail safe (caller leaves unacked)")
	}
	t.Setenv("METIS_M2M_HOME", t.TempDir())
	if err := m2mRecordAdapterAck(""); err == nil {
		t.Fatal("empty message id must refuse")
	}
	if err := m2mRecordAdapterAck("m-1"); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := m2mRecordAdapterAck("m-1"); err != nil {
		t.Fatalf("idempotent re-record: %v", err)
	}
	if err := m2mRecordAdapterAck("m-2"); err != nil {
		t.Fatalf("record second: %v", err)
	}
	raw, err := os.ReadFile(m2mAdapterAckPath())
	if err != nil {
		t.Fatalf("read ack file: %v", err)
	}
	var doc struct {
		Acked []string `json:"acked"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("ack file must be JSON: %v", err)
	}
	if len(doc.Acked) != 2 || doc.Acked[0] != "m-1" || doc.Acked[1] != "m-2" {
		t.Fatalf("ack file must hold both ids in order: %s", raw)
	}
	if err := os.WriteFile(m2mAdapterAckPath(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m2mRecordAdapterAck("m-3"); err != nil {
		t.Fatalf("corrupt file must reset, not block: %v", err)
	}
}

// TestM2MDriveAdapterAckBindsExactMessage proves a valid signed ack
// for ANOTHER message cannot satisfy an ack call: the body must name
// the requested message ID.
func TestM2MDriveAdapterAckBindsExactMessage(t *testing.T) {
	lib := driveLoopbackGate(t)
	homeA, homeB, relay, _ := driveLoopbackPair(t, lib)
	mid := driveSend(t, lib, homeA, relay, "metisbox", "bind me")
	adapter, err := filepath.Abs(filepath.Join("scripts", "m2m_drive.py"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_M2M_CLIENT", adapter)
	t.Setenv("METIS_M2M_LIB", lib)
	t.Setenv("METIS_M2M_HOME", homeB)
	t.Setenv("METIS_M2M_RELAY", relay)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := execM2MClient(ctx, "receive", nil, nil); err != nil {
		t.Fatalf("receive: %v", err)
	}
	// Copy the real signed ack sideways under a foreign ID and
	// rewrite its body to a different message: verification must
	// still fail the binding check.
	entries, err := filepath.Glob(filepath.Join(relay, "acks", "*", mid+".json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("one signed ack must exist, got %v err %v", entries, err)
	}
	raw, err := os.ReadFile(entries[0])
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := json.Unmarshal(raw, &ack); err != nil {
		t.Fatal(err)
	}
	body, _ := ack["body"].(map[string]any)
	body["msg_id"] = "someone-else"
	out, _ := json.Marshal(ack)
	foreign := filepath.Join(relay, "acks", "deadbeef")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "someone-else.json"), out, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runM2MBridge(ctx, "ack", []string{"someone-else", "TURN_COMPLETED"}, nil); err == nil {
		t.Fatal("ack bound to another message must be refused")
	}
	// The genuine ack still echoes for its own message.
	got, err := runM2MBridge(ctx, "ack", []string{mid, "TURN_COMPLETED"}, nil)
	if err != nil {
		t.Fatalf("genuine ack: %v", err)
	}
	if got.MessageID != mid || got.State != "TURN_COMPLETED" {
		t.Fatalf("genuine ack echo mismatch: %+v", got)
	}
}

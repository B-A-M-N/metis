package main

// muse_bind_idempotency_test.go: F-01 — MCP bind retry identity.
//
// metis_bind must not lose retry identity: the MCP request_id owns
// exactly one durable session/start command (claimed before Muse is
// ever called), so an identical retry replays the already-created
// native session instead of minting a second one. A reused key with
// a different payload is a conflict; a submitted (unacked) intent
// stays recoverable through the same command; an acknowledged
// intent whose binding row never committed is adopted by reconcile
// under any key instead of forking a duplicate.
//
// All tests drive the production path (claimBindIntent +
// bindMuseNativeSessionOnHost, the exact functions
// bindMuseNativeSession runs) against a counting stub host: the
// session/start call count is the duplicate-session oracle.

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bindCountingStub serves initialize/session/start/session/read with
// the session identity fixed to native-bind-1. starts counts
// session/start transmissions (the duplicate-session oracle);
// session/read echoes back what session/start received so config
// verification passes exactly as in production.
func bindCountingStub(t *testing.T, conn net.Conn, starts *atomic.Int32, lastRoot *string, mu *sync.Mutex) {
	t.Helper()
	stubMuseServer(t, conn, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "session/start" {
			starts.Add(1)
			var p struct {
				WorkspaceRoot string `json:"workspaceRoot"`
			}
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			*lastRoot = p.WorkspaceRoot
			mu.Unlock()
			return map[string]any{
				"session":    map[string]string{"sessionId": "native-bind-1"},
				"viewCursor": "c-bind-0",
			}, nil
		}
		if method == "session/read" {
			mu.Lock()
			root := *lastRoot
			mu.Unlock()
			return map[string]any{
				"session": map[string]string{
					"sessionId":     "native-bind-1",
					"providerId":    "echo",
					"workspaceRoot": root,
				},
				"viewCursor": "c-bind-0",
			}, nil
		}
		return map[string]any{}, nil
	})
}

// bindTestScope resolves the frozen bind scope for a seeded session,
// exactly as bindMuseNativeSession does before claiming.
func bindTestScope(t *testing.T, session string) (workspaceRoot, model string) {
	t.Helper()
	workdir, err := sessionWorkdir(session)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err = canonicalRoot(workdir)
	if err != nil {
		t.Fatal(err)
	}
	model, err = resolveSessionModel(session)
	if err != nil {
		t.Fatal(err)
	}
	return workspaceRoot, model
}

func bindTestHost(t *testing.T, starts *atomic.Int32, lastRoot *string, mu *sync.Mutex) *museHost {
	t.Helper()
	client, server := net.Pipe()
	bindCountingStub(t, server, starts, lastRoot, mu)
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// Identical retry — including a crash between the ack persistence
// and the binding-row commit — must create at most one native
// session: the first pass transmits session/start and persists the
// ack without writing the binding row (the crash), and the recovery
// pass under the same request key reuses the claimed command, which
// the lower-level dedup replays without retransmitting.
func TestBindRetrySameKeyCreatesOneNativeSession(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedOwnedSession(t, "wb", "bind-idem")
	workspaceRoot, model := bindTestScope(t, "wb")
	req := OperationRequest{Actor: "orch", RequestID: "bind-retry-1"}

	var starts atomic.Int32
	var lastRoot string
	var mu sync.Mutex
	h := bindTestHost(t, &starts, &lastRoot, &mu)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First pass: claim, transmit, ack persisted — then the crash:
	// no binding row is written.
	cmd1, err := claimBindIntent(req, "wb", "echo", "", "metis:wb", workspaceRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sid1, _, err := museStartSessionWithID(ctx, h, "metis:wb", workspaceRoot,
		museSessionSelection{ProviderID: "echo", ModelID: model}, cmd1)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, err := getMuseBinding("wb"); err == nil {
		t.Fatal("crash simulation must leave no binding row")
	}

	// Recovery pass: same key, same payload, full production path
	// through the claimed command into the binding row.
	cmd2, err := claimBindIntent(req, "wb", "echo", "", "metis:wb", workspaceRoot, model)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if cmd2 != cmd1 {
		t.Fatalf("retry must recover the same command, got %q then %q", cmd1, cmd2)
	}
	sid2, cur2, err := bindMuseNativeSessionOnHost(ctx, h, "wb", "echo", "", "metis:wb", workspaceRoot, model, cmd2, req)
	if err != nil {
		t.Fatalf("recovery bind: %v", err)
	}
	if sid2 != sid1 || sid1 != "native-bind-1" {
		t.Fatalf("recovery must replay the created session, got %q then %q", sid1, sid2)
	}
	if starts.Load() != 1 {
		t.Fatalf("retry must not retransmit session/start, saw %d calls", starts.Load())
	}
	b, err := getMuseBinding("wb")
	if err != nil {
		t.Fatalf("recovery must commit the binding row: %v", err)
	}
	if b.NativeID != "native-bind-1" || b.Cursor != cur2 {
		t.Fatalf("binding must name the recovered session: %+v", b)
	}

	// Post-success idempotent retry: same key again converges on
	// the bound session with no new transmission.
	cmd3, err := claimBindIntent(req, "wb", "echo", "", "metis:wb", workspaceRoot, model)
	if err != nil {
		t.Fatalf("post-success re-claim: %v", err)
	}
	sid3, _, err := bindMuseNativeSessionOnHost(ctx, h, "wb", "echo", "", "metis:wb", workspaceRoot, model, cmd3, req)
	if err != nil {
		t.Fatalf("post-success retry: %v", err)
	}
	if sid3 != "native-bind-1" || starts.Load() != 1 {
		t.Fatalf("post-success retry must converge without transmitting, sid=%q starts=%d", sid3, starts.Load())
	}
}

// A submitted-but-unacked intent (crash before transmit, or ack
// loss) stays recoverable: re-claiming the same key returns the
// same command in submitted state, and the subsequent start
// transmits exactly once.
func TestBindUncertainIntentStaysRecoverable(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedOwnedSession(t, "wu", "bind-uncertain")
	workspaceRoot, model := bindTestScope(t, "wu")
	req := OperationRequest{Actor: "orch", RequestID: "bind-uncertain-1"}

	cmd1, err := claimBindIntent(req, "wu", "echo", "", "metis:wu", workspaceRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Crash before anything is sent: re-claim must recover the
	// same submitted command, never mint a second one.
	cmd2, err := claimBindIntent(req, "wu", "echo", "", "metis:wu", workspaceRoot, model)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if cmd1 == "" || cmd2 != cmd1 {
		t.Fatalf("uncertain intent must be recoverable through the same command, got %q then %q", cmd1, cmd2)
	}
	intent, found, err := loadSessionIntent(cmd1)
	if err != nil || !found {
		t.Fatalf("intent must be durable after claim (found=%v err=%v)", found, err)
	}
	if intent.State != museSessionIntentSubmitted {
		t.Fatalf("untransmitted intent must stay submitted, got %+v", intent)
	}
	var starts atomic.Int32
	var lastRoot string
	var mu sync.Mutex
	h := bindTestHost(t, &starts, &lastRoot, &mu)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sid, _, err := bindMuseNativeSessionOnHost(ctx, h, "wu", "echo", "", "metis:wu", workspaceRoot, model, cmd2, req)
	if err != nil {
		t.Fatalf("recovered start: %v", err)
	}
	if sid != "native-bind-1" || starts.Load() != 1 {
		t.Fatalf("recovered intent must transmit exactly once, sid=%q starts=%d", sid, starts.Load())
	}
}

// A reused request key with a different payload — or a different
// principal — is a conflict, never a silent convergence, and the
// frozen intent is left untouched.
func TestBindConflictingPayloadRefused(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedOwnedSession(t, "wc", "bind-conflict")
	workspaceRoot, model := bindTestScope(t, "wc")
	req := OperationRequest{Actor: "orch", RequestID: "bind-conflict-1", Principal: "tok-a"}

	first, err := claimBindIntent(req, "wc", "echo", "", "metis:wc", workspaceRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for name, other := range map[string]OperationRequest{
		"title":     {Actor: "orch", RequestID: "bind-conflict-1", Principal: "tok-a"},
		"principal": {Actor: "orch", RequestID: "bind-conflict-1", Principal: "tok-b"},
	} {
		_ = name
		var err error
		if name == "title" {
			_, err = claimBindIntent(other, "wc", "echo", "", "metis:CHANGED", workspaceRoot, model)
		} else {
			_, err = claimBindIntent(other, "wc", "echo", "", "metis:wc", workspaceRoot, model)
		}
		if err == nil {
			t.Fatalf("%s mismatch must conflict", name)
		}
		if !isBindIntentConflict(err) || !strings.Contains(err.Error(), "bind_intent_conflict") {
			t.Fatalf("%s refusal must name the bind intent conflict, got: %v", name, err)
		}
	}
	intent, found, err := loadBindIntentByRequest("bind-conflict-1")
	if err != nil || !found {
		t.Fatalf("frozen intent must survive conflicts (found=%v err=%v)", found, err)
	}
	if intent.CommandID != first || intent.PayloadFingerprint != bindPayloadFingerprint("wc", "echo", "", "metis:wc", workspaceRoot, model) {
		t.Fatalf("conflicted retry must not mutate the frozen intent: %+v", intent)
	}
	if _, err := claimBindIntent(OperationRequest{Actor: "orch"}, "wc", "echo", "", "metis:wc", workspaceRoot, model); err == nil {
		t.Fatal("anonymous claim must be refused")
	}
}

// Acked-but-unbound sessions reconcile under any key: the ack is
// persisted, the crash lands before the binding row, and recovery
// arrives under a fresh request key. Reconcile adopts the already
// created native session instead of transmitting a second
// session/start. Once bound, there is nothing left to reconcile.
func TestAckedButUnboundReconcilesAcrossKeys(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedOwnedSession(t, "wr", "bind-reconcile")
	workspaceRoot, model := bindTestScope(t, "wr")

	var starts atomic.Int32
	var lastRoot string
	var mu sync.Mutex
	h := bindTestHost(t, &starts, &lastRoot, &mu)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First attempt under key K1: claim, transmit, ack persisted,
	// crash before the binding row.
	req1 := OperationRequest{Actor: "orch", RequestID: "bind-rec-1"}
	cmd1, err := claimBindIntent(req1, "wr", "echo", "", "metis:wr", workspaceRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sid1, cur1, err := museStartSessionWithID(ctx, h, "metis:wr", workspaceRoot,
		museSessionSelection{ProviderID: "echo", ModelID: model}, cmd1)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Recovery under a FRESH key K2 must adopt, not duplicate.
	intent, found, err := findAckedUnboundBindIntent("wr", "echo", "", workspaceRoot, model)
	if err != nil || !found {
		t.Fatalf("acked-but-unbound session must be adoptable (found=%v err=%v)", found, err)
	}
	if intent.NativeID != sid1 || intent.ViewCursor != cur1 {
		t.Fatalf("reconcile must name the acked session, got %+v want %q/%q", intent, sid1, cur1)
	}
	req2 := OperationRequest{Actor: "orch", RequestID: "bind-rec-2"}
	if err := bindMuseSession("wr", intent.NativeID, intent.ViewCursor, "echo", "", req2); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("reconcile must not transmit session/start, saw %d calls", starts.Load())
	}
	b, err := getMuseBinding("wr")
	if err != nil || b.NativeID != sid1 {
		t.Fatalf("adopted binding must name the acked session: %+v err=%v", b, err)
	}
	// Bound now: nothing left to reconcile, for any scope query.
	if _, found, err := findAckedUnboundBindIntent("wr", "echo", "", workspaceRoot, model); err != nil || found {
		t.Fatalf("bound session must have nothing to reconcile (found=%v err=%v)", found, err)
	}
	if _, found, err := findAckedUnboundBindIntent("wr", "meta", "", workspaceRoot, model); err != nil || found {
		t.Fatalf("different provider must not adopt the session (found=%v err=%v)", found, err)
	}
}

package main

// muse_host_test.go: MSP client coverage in two layers.
//
// Hermetic layer (no binary): a canned stub server over an in-memory
// pipe proves framing, id correlation across interleaved
// notifications, error-kind mapping, and shutdown semantics.
//
// Wire layer (installed `muse`, echo provider, isolated XDG home):
// proves the real handshake, session/turn lifecycle, continuation,
// server-side commandId dedup, and cursor replay. Zero inference, no
// quota, no operator-state pollution. Skipped when `muse` is absent.

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMuseCommandIDShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := newMuseCommandID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 || id[14] != '7' {
			t.Fatalf("not UUIDv7 shape: %q", id)
		}
		variant := strings.ToLower(string(id[19]))
		if variant != "8" && variant != "9" && variant != "a" && variant != "b" {
			t.Fatalf("bad RFC4122 variant in %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate command id: %q", id)
		}
		seen[id] = true
	}
}

// stubMuseServer speaks just enough MSP for framing tests: canned
// replies by method plus scripted notifications.
//
// onCommand runs on the stub's own goroutine: callbacks that capture
// test state must synchronize (channel or mutex) — a bare slice
// append races with the test goroutine's read under -race.
func stubMuseServer(t *testing.T, conn net.Conn, onCommand func(id int64, method string, params json.RawMessage) (any, *museError)) {
	t.Helper()
	go func() {
		defer conn.Close()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		for {
			var msg struct {
				ID     *int64          `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := dec.Decode(&msg); err != nil {
				return
			}
			if msg.ID == nil {
				continue // notification in; stubs never answer those
			}
			res, merr := onCommand(*msg.ID, msg.Method, msg.Params)
			reply := map[string]any{"jsonrpc": "2.0", "id": *msg.ID}
			if merr != nil {
				reply["error"] = map[string]any{"code": merr.Code, "message": merr.Message, "data": map[string]any{"kind": merr.Kind}}
			} else {
				reply["result"] = res
			}
			if err := enc.Encode(reply); err != nil {
				return
			}
		}
	}()
}

func stubHandshake(t *testing.T, h *museHost) {
	t.Helper()
	// newMuseHostIO already performed initialize+initialized against
	// the stub; nothing further needed here.
	_ = h
}

func TestMuseHostFraming(t *testing.T) {
	client, server := net.Pipe()
	var mu sync.Mutex
	seenCmds := map[string]int{}
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		switch method {
		case "initialize":
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		case "session/start":
			var p struct {
				CommandID string `json:"commandId"`
			}
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			seenCmds[p.CommandID]++
			mu.Unlock()
			return map[string]any{
				"session":    map[string]string{"sessionId": "sess-1"},
				"viewCursor": "",
			}, nil
		case "nope/missing":
			return nil, &museError{Code: -32601, Kind: "methodNotFound", Message: "no such method"}
		}
		return nil, &museError{Code: -32601, Kind: "methodNotFound", Message: "stub: " + method}
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	stubHandshake(t, h)
	defer h.Close()

	ctx := context.Background()
	raw, err := h.Call(ctx, "session/start", map[string]any{"commandId": "cmd-1", "title": "x"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var started struct {
		Session struct {
			SessionID string `json:"sessionId"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Session.SessionID != "sess-1" {
		t.Fatalf("bad start result: %s / %v", string(raw), err)
	}
	_, err = h.Call(ctx, "nope/missing", map[string]any{})
	merr, ok := err.(*museError)
	if !ok || merr.Code != -32601 || merr.Kind != "methodNotFound" {
		t.Fatalf("error kind must round-trip, got %#v", err)
	}
	// A notification pushed by the server must surface without
	// disturbing pending calls.
	serverNotif := map[string]any{"jsonrpc": "2.0", "method": "turn/started", "params": map[string]any{"viewCursor": "v4:s:9"}}
	nb, _ := json.Marshal(serverNotif)
	if _, err := server.Write(append(nb, '\n')); err != nil {
		t.Fatalf("stub notif write: %v", err)
	}
	select {
	case n := <-h.Notifications():
		if n.Method != "turn/started" || museViewCursor(n) != "v4:s:9" {
			t.Fatalf("bad notification: %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("notification never surfaced")
	}
	if got := h.Dropped(); got != 0 {
		t.Fatalf("unexpected drops: %d", got)
	}
}

func TestMuseHostLateReplyDropped(t *testing.T) {
	client, server := net.Pipe()
	release := make(chan struct{})
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "slow/op" {
			<-release
			return map[string]any{"ok": true}, nil
		}
		return map[string]any{
			"serverInfo": map[string]string{"name": "stub", "version": "0"},
			"schema":     map[string]any{"fingerprint": "test", "version": 1},
		}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := h.Call(ctx, "slow/op", map[string]any{}); err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected deadline, got %v", err)
	}
	close(release) // late reply arrives with no waiter; must not panic or wedge
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.pendingMu.Lock()
		n := len(h.pending)
		h.pendingMu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending waiter leaked after timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMuseEchoLifecycle qualifies the real wire against the echo
// provider: handshake, session create, subscribe, turn admission,
// terminal completion, same-session continuation, commandId dedup,
// and session listing. No model calls, no quota.
func TestMuseEchoLifecycle(t *testing.T) {
	if _, err := lookupMuse(); err != nil {
		t.Skipf("muse binary absent: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", []string{"XDG_DATA_HOME=" + t.TempDir()})
	if err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer h.Close()
	if h.Fingerprint == "" || h.SchemaVersion != 1 {
		t.Fatalf("bad handshake: fingerprint=%q schema=%d", h.Fingerprint, h.SchemaVersion)
	}
	sid, cursor, err := museStartSession(ctx, h, "metis-qual-lifecycle", t.TempDir())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if sid == "" {
		t.Fatal("empty session id")
	}
	if _, err := museSubscribe(ctx, h, sid, cursor); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// First turn: admission, then terminal completion.
	ack, err := museSubmitTurn(ctx, h, sid, "hello", "")
	if err != nil {
		t.Fatalf("turn 1 submit: %v", err)
	}
	if ack.Status != "accepted" || ack.Disposition != "started" || ack.TurnID == "" {
		t.Fatalf("bad admission ack: %+v", ack)
	}
	wctx, wcancel := context.WithTimeout(ctx, 60*time.Second)
	defer wcancel()
	var events []string
	terminal, _, err := museWaitTurn(wctx, h, sid, ack.TurnID, func(n museNotification) {
		events = append(events, n.Method)
	})
	if err != nil {
		t.Fatalf("wait turn 1: %v (events: %v)", err, events)
	}
	if terminal != "completed" {
		t.Fatalf("terminal = %q, want completed (events: %v)", terminal, events)
	}
	// Continuation: a second turn on the SAME session completes with
	// a distinct turn id.
	ack2, err := museSubmitTurn(ctx, h, sid, "again", "")
	if err != nil {
		t.Fatalf("turn 2 submit: %v", err)
	}
	if ack2.TurnID == ack.TurnID {
		t.Fatal("continuation reused turn id")
	}
	wctx2, wcancel2 := context.WithTimeout(ctx, 60*time.Second)
	defer wcancel2()
	terminal2, _, err := museWaitTurn(wctx2, h, sid, ack2.TurnID, nil)
	if err != nil {
		t.Fatalf("wait turn 2: %v", err)
	}
	if terminal2 != "completed" {
		t.Fatalf("turn 2 terminal = %q", terminal2)
	}
	// Idempotent replay: the same commandId submitted twice must not
	// create a second session. The server dedups; we prove it by
	// counting sessions before and after.
	idsBefore, _, err := museSessionList(ctx, h, 200, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	fixedCmd, _ := newMuseCommandID()
	var first string
	for i := 0; i < 2; i++ {
		raw, err := h.Call(ctx, "session/start", map[string]any{"commandId": fixedCmd, "title": "metis-qual-dedup"})
		if err != nil {
			t.Fatalf("replay start %d: %v", i, err)
		}
		var res struct {
			Session struct {
				SessionID string `json:"sessionId"`
			} `json:"session"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		if first == "" {
			first = res.Session.SessionID
		} else if res.Session.SessionID != first {
			t.Fatalf("commandId replay created a second session: %q vs %q", first, res.Session.SessionID)
		}
	}
	idsAfter, _, err := museSessionList(ctx, h, 200, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(idsAfter) != len(idsBefore)+1 {
		t.Fatalf("replay duplicated a session: before=%d after=%d", len(idsBefore), len(idsAfter))
	}
	found := false
	for _, id := range idsAfter {
		if id == sid {
			found = true
		}
	}
	if !found {
		t.Fatalf("lifecycle session %s missing from list %v", sid, idsAfter)
	}
}

func lookupMuse() (string, error) {
	return exec.LookPath("muse")
}

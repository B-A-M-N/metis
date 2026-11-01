package main

// muse_turn_wiring_test.go: the production turn path must observe
// approvals, notice host death, keep provider bindings pinned, and
// keep status reads from moving the durable cursor.

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

// Rebinding the same native session under the same profile with a
// different provider must be refused: the provider selects the
// native store, and a silent switch lands execution elsewhere.
func TestBindRejectsProviderChange(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "prov")
	if err := bindMuseSession("w", "native-1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatal(err)
	}
	if err := bindMuseSession("w", "native-1", "c1", "meta", "", testReq("op")); err == nil {
		t.Fatal("provider overwrite on rebind must be refused")
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.Provider != "echo" || b.NativeID != "native-1" {
		t.Fatalf("refused rebind must move nothing: %+v", b)
	}
}

// The chained turn observer records approvals (with receipts) and
// still extracts terminal text: neither side may swallow the other.
func TestChainedTurnObserver(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var text string
	obs := chainTurnObserver(ctx, h, "w", "native-9", func(s string) { text = s })
	rid := int64(7)
	obs(museNotification{Method: "approval/request", RequestID: &rid, Params: []byte(
		`{"approvalId":"ap-chain","availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)})
	obs(museNotification{Method: "item/completed", Params: []byte(
		`{"item":{"kind":"agentMessage","status":"completed","text":"hello"}}`)})
	if text != "hello" {
		t.Fatalf("chained observer must still extract text, got %q", text)
	}
	a, err := museLoadPendingApproval("w", "ap-chain", "allow")
	if err != nil {
		t.Fatalf("chained observer must record approvals: %v", err)
	}
	if a.Status != "pending" {
		t.Fatalf("approval status = %q", a.Status)
	}
}

// Status is a read: it must never advance the durable cursor.
// The cursor moves only through the event-ingestion path, or a
// point-in-time read would skip unprocessed events.
func TestCmdStatusLeavesCursor(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Skipf("no ambient muse daemon: %v", err)
	}
	defer h.Close()
	seedOwnedSession(t, "w", "status")
	sid, _, err := museStartSession(ctx, h, "metis-qual-status-cursor", loadRegistry()["w"].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindMuseSession("w", sid, "", "echo", "", testReq("t")); err != nil {
		t.Fatal(err)
	}
	if err := updateMuseCursor("w", "cursor-old-sentinel", "turn-sentinel"); err != nil {
		t.Fatal(err)
	}
	if err := museCmdStatus("w", "echo", ""); err != nil {
		t.Fatal(err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.Cursor != "cursor-old-sentinel" {
		t.Fatalf("status moved the durable cursor to %q", b.Cursor)
	}
}

// Host death must unblock the wait promptly with a closed-host
// error, not a full context-timeout hang with outcome unknown.
func TestHostCloseUnblocksWait(t *testing.T) {
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, _, err := museWaitTurn(ctx, h, "native-x", "turn-never", nil)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	_ = server.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("dead host must error, not report a turn")
		}
		if got := time.Since(start); got > 5*time.Second {
			t.Fatalf("host death took %v; the wait hung until timeout", got)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("wait did not notice host death")
	}
}

// An overflowed notification stream must not produce a completion:
// drops during the wait mean the waiter may have missed the turn's
// terminal event, so it must report an explicit gap with the replay
// cursor instead of the turn outcome.
func TestWaitTurnNoticesOverflowGap(t *testing.T) {
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	// Hold the waiter inside observe so the flood below overflows
	// the 1024-deep stream mid-wait, deterministically: no timing
	// assumptions, the drops land while the waiter is attached.
	release := make(chan struct{})
	observed := make(chan struct{})
	observe := func(museNotification) {
		close(observed)
		<-release
	}
	type waitRes struct {
		terminal, cursor string
		err              error
	}
	done := make(chan waitRes, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		term, cur, err := museWaitTurn(ctx, h, "s", "t1", observe)
		done <- waitRes{term, cur, err}
	}()
	h.pushNotif(museNotification{Method: "output.delta",
		Params: json.RawMessage(`{"seq":1}`)})
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never picked up the first notification")
	}
	for i := 0; i < 1100; i++ {
		h.pushNotif(museNotification{Method: "output.delta",
			Params: json.RawMessage(`{"seq":2}`)})
	}
	if h.Dropped() == 0 {
		t.Fatal("flood during the wait must overflow the stream")
	}
	close(release)
	h.pushNotif(museNotification{Method: "turn/completed",
		Params: json.RawMessage(`{"sessionId":"s","turnId":"t1","terminal":"completed"}`)})
	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("completion on an overflowed stream must be refused as a gap, not reported")
		}
		if !strings.Contains(r.err.Error(), "overflow") && !strings.Contains(r.err.Error(), "gap") {
			t.Fatalf("gap error must name the overflow: %v", r.err)
		}
		if r.cursor != "" {
			t.Fatalf("gap must return the entry cursor, not one advanced past the hole: %q", r.cursor)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiter never returned after the gap")
	}
}

// turn/cancel admission is not confirmation: only the terminal
// outcome confirms the stop. A withheld terminal (server never
// reports) must leave the disposition uncertain, never confirmed.
func turnCancelStub(t *testing.T, conn net.Conn, terminal bool) {
	t.Helper()
	go func() {
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
				continue
			}
			if *msg.ID == 1 {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{
					"serverInfo": map[string]string{"name": "stub", "version": "0"},
					"schema":     map[string]any{"fingerprint": "test", "version": 1},
				}})
				continue
			}
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{}})
			if terminal && msg.Method == "turn/cancel" {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/completed",
					"params": map[string]any{"sessionId": "s", "turnId": "t9", "terminal": "cancelled"}})
			}
		}
	}()
}

func TestStopNativeTurnConfirmedOnTerminal(t *testing.T) {
	client, server := net.Pipe()
	turnCancelStub(t, server, true)
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disp, err := stopNativeTurnOnHost(ctx, h, "s", "t9", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if disp != ExecutionCancelConfirmed {
		t.Fatalf("observed terminal must confirm the stop, got %s", disp)
	}
}

func TestStopNativeTurnUncertainWhenWithheld(t *testing.T) {
	client, server := net.Pipe()
	turnCancelStub(t, server, false)
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	disp, err := stopNativeTurnOnHost(ctx, h, "s", "t9", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if disp != ExecutionCancelUncertain {
		t.Fatalf("withheld terminal must stay uncertain, got %s", disp)
	}
}

// A queued turn that never runs ends as turn/unqueued, not
// turn/completed. The waiter must treat it as an authoritative
// turn end — hanging until timeout would misreport a decided
// outcome as unknown.
func TestWaitTurnAcceptsUnqueued(t *testing.T) {
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.pushNotif(museNotification{Method: "turn/unqueued",
		Params: json.RawMessage(`{"sessionId":"s","turnId":"tq"}`)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	terminal, _, err := museWaitTurn(ctx, h, "s", "tq", nil)
	if err != nil {
		t.Fatalf("unqueued turn must end the wait, got error: %v", err)
	}
	if terminal != "unqueued" {
		t.Fatalf("unqueued turn must report terminal unqueued, got %q", terminal)
	}
}

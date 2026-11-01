package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// A lost session/start ack must NEVER mint a second native session:
// once the first attempt is durably recorded, a retry with the same
// caller command id replays the created session from the intent
// store without touching the wire — even when the server would now
// refuse or create something else. Companion to
// TestSessionStartDedupsOnCommandID (muse_bind_test.go), which
// proves the retry sends exactly one session/start.
func TestSessionRetryAfterAckLossNeverDuplicatesSession(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	starts := 0
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "session/start" {
			starts++
			if starts > 1 {
				// The wire is now hostile: a retransmit would either
				// fail or mint a second session. The retry must send
				// nothing, so this branch is unreachable.
				return nil, &museError{Code: -32000, Message: "must not retransmit"}
			}
			return map[string]any{
				"session":    map[string]string{"sessionId": "native-ack-loss"},
				"viewCursor": "c0",
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatal(err)
	}
	sid1, _, err := museStartSessionWithID(ctx, h, "t", "", museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	// The durable intent must record the ack: a lost ack leaves the
	// row submitted, a received one acknowledges it.
	intent, found, err := loadSessionIntent(cmdID)
	if err != nil || !found {
		t.Fatalf("intent must be durable after start (found=%v err=%v)", found, err)
	}
	if intent.State != museSessionIntentAcknowledged || intent.NativeID != "native-ack-loss" {
		t.Fatalf("intent must acknowledge the created session, got %+v", intent)
	}
	// The "lost ack" retry: same caller key, hostile wire. The
	// durable replay must win without transmitting.
	sid2, _, err := museStartSessionWithID(ctx, h, "t", "", museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("retry after ack loss: %v", err)
	}
	if sid1 != sid2 || sid1 != "native-ack-loss" {
		t.Fatalf("retry must replay the created session, got %q %q", sid1, sid2)
	}
	if starts != 1 {
		t.Fatalf("retry must not retransmit session/start, saw %d calls", starts)
	}
}

// Distinct caller keys must mint distinct sessions: the dedup above
// must not collapse independent creations into one.
func TestSessionDistinctCommandIDsMintDistinctSessions(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	starts := 0
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "session/start" {
			starts++
			return map[string]any{
				"session":    map[string]string{"sessionId": "native-n"},
				"viewCursor": "c0",
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 2; i++ {
		cmdID, err := newMuseCommandID()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := museStartSessionWithID(ctx, h, "t", "", museSessionSelection{}, cmdID); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if starts != 2 {
		t.Fatalf("distinct commands must each transmit session/start, saw %d calls", starts)
	}
}

// The retry path must never mint its own identity: an empty caller
// key is refused instead of creating an unkeyed (unduplicatable)
// session.
func TestSessionStartRefusesEmptyCommandID(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := museStartSessionWithID(ctx, h, "t", "", museSessionSelection{}, ""); err == nil {
		t.Fatal("empty command id must be refused")
	}
}

package main

// muse_config_verify_test.go (P0-4a): provider/model selection is
// explicit at session creation and verified against the
// server-reported Session record. A requested model the server did
// not load blocks admission — never a silent default.

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSessionStartSelectsAndEchoesConfig(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	defer h.Close()
	root := t.TempDir()
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := museStartSessionWithID(ctx, h, "metis-qual-sel", root,
		museSessionSelection{ProviderID: "echo", ModelID: "sel-model"}, cmdID)
	if err != nil {
		t.Fatalf("start with explicit selection: %v", err)
	}
	prov, model, ws, err := museReadSessionConfig(ctx, h, sid)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if prov != "echo" || model != "sel-model" || ws != root {
		t.Fatalf("server must echo the selection, got provider=%q model=%q workspace=%q", prov, model, ws)
	}
}

func TestVerifySessionConfigRefusesMismatch(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	defer h.Close()
	root := t.TempDir()
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatal(err)
	}
	sid, _, err := museStartSessionWithID(ctx, h, "metis-qual-mismatch", root, museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("start with defaults: %v", err)
	}
	if _, _, _, err := museVerifySessionConfig(ctx, h, sid, "echo", "model-that-was-not-loaded", root); err == nil {
		t.Fatal("demanded model the server did not load must be refused")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("refusal must name the mismatch, got: %v", err)
	}
	if _, _, _, err := museVerifySessionConfig(ctx, h, sid, "echo", "", "/definitely/elsewhere"); err == nil {
		t.Fatal("foreign workspace must be refused")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("refusal must name the mismatch, got: %v", err)
	}
	if _, _, _, err := museVerifySessionConfig(ctx, h, sid, "echo", "", root); err != nil {
		t.Fatalf("agreeing config must verify: %v", err)
	}
}

func TestBindBlocksUnverifiedModel(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	seedOwnedSession(t, "w", "bindmodel")
	reg := loadRegistry()
	e := reg["w"]
	e.Model = "model-that-was-not-loaded"
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	// The echo provider substitutes nothing: it echoes the requested
	// model, so an authorized selection the registry names is
	// confirmed, not defaulted. The block path (server reports
	// something else) is proven by TestStartRefusesLyingServer.
	if err := museCmdBind("w", "echo", "", "bind-model-qual", "test"); err != nil {
		t.Fatalf("bind with a confirmed selection must succeed: %v", err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatalf("binding must persist: %v", err)
	}
	if b.Model != "model-that-was-not-loaded" {
		t.Fatalf("binding must persist the authorized model, got %+v", b)
	}
}

// A server that reports a different provider/model/workspace than
// selected must cause session creation to fail before any binding:
// Metis never adopts a session whose effective config contradicts
// the authorized selection.
func TestStartRefusesLyingServer(t *testing.T) {
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
				"session":    map[string]string{"sessionId": "native-lying"},
				"viewCursor": "c0",
			}, nil
		}
		if method == "session/read" {
			return map[string]any{
				"session": map[string]string{
					"sessionId":     "native-lying",
					"providerId":    "other",
					"modelId":       "other-model",
					"workspaceRoot": "/elsewhere",
				},
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
	sel := museSessionSelection{ProviderID: "echo", ModelID: "want-model"}
	if _, _, err := museStartSessionWithID(ctx, h, "t", t.TempDir(), sel, cmdID); err == nil {
		t.Fatal("creation must refuse a session whose effective config contradicts the selection")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("refusal must name the mismatch, got: %v", err)
	}
	if starts != 1 {
		t.Fatalf("expected exactly one start attempt, got %d", starts)
	}
}

// P0-4b: revalidation before execution. A binding whose recorded
// model no longer matches the server-reported effective config
// refuses the turn before anything is submitted.
func TestManagedTurnRefusesModelDrift(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	seedOwnedSession(t, "w", "drift")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	nativeID, cursor, err := museStartSession(ctx, h, "metis-qual-drift", loadRegistry()["w"].Workdir)
	h.Close()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := bindMuseSession("w", nativeID, cursor, "echo", "", testReq("op")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// Simulate drift: the binding now claims a model the server
	// never loaded.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE muse_bindings SET model = 'model-never-loaded' WHERE session = 'w'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := museManagedTurn(ctx, "w", "hi", 60*time.Second, cmdID); err == nil {
		t.Fatal("drifted model must refuse the turn before submission")
	} else if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("refusal must name the mismatch, got: %v", err)
	}
}

// P0-5: the committed cursor is never blanked and every advance is
// attributed to the terminal turn that produced it. Blanking would
// flip resume to from-now mode and silently drop history.
func TestCursorEmptyWriteRetainsPosition(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "cur")
	if err := bindMuseSession("w", "native-1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := updateMuseCursor("w", "c0", "turn-1"); err != nil {
		t.Fatal(err)
	}
	if err := updateMuseCursor("w", "", "turn-2"); err != nil {
		t.Fatal(err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.Cursor != "c0" {
		t.Fatalf("empty write must never blank the committed cursor, got %q", b.Cursor)
	}
	if b.CursorTurn != "turn-1" {
		t.Fatalf("advance must stay attributed to its terminal turn, got %+v", b)
	}
}

func TestCursorWriteAttributesTurn(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "cur2")
	if err := bindMuseSession("w", "native-1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := updateMuseCursor("w", "c9", "turn-9"); err != nil {
		t.Fatal(err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.Cursor != "c9" || b.CursorTurn != "turn-9" {
		t.Fatalf("cursor advance must carry its terminal turn, got %+v", b)
	}
}

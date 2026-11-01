package main

// native_session_test.go: F-06 failing-first.
//
// One logical session record with a typed backend lets a
// Muse-native session be created, discovered, used, observed, and
// recovered with NO tmux session and NO PTY supervisor behind it:
// provisioning establishes the logical record (authorized root plus
// provider/model/profile scope), the bind path attaches the native
// session through the decoupled workspace/model resolvers, turns
// execute and observe purely through Muse session/turn events, and
// a fresh host resumes by stored identity. Every test below drives
// the production path against a scripted stub host and asserts the
// shell stores stay empty throughout.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// nativeStubSession is one scripted Muse-side session: the config
// session/start received (echoed back by session/read for the
// production config verification) plus the latest event cursor.
type nativeStubSession struct {
	id            string
	workspaceRoot string
	providerID    string
	modelID       string
	cursor        string
}

// nativeStub is a scripted MSP server: session lifecycle plus turn
// admission, with completion notifications emitted synchronously so
// waits are deterministic. autoComplete gates whether turn/start
// completes on its own (turns) or waits for turn/cancel (cancel
// test). starts counts session/start transmissions: the
// duplicate-session oracle for the whole lifecycle.
type nativeStub struct {
	mu           sync.Mutex
	sessions     map[string]*nativeStubSession
	turns        int
	starts       atomic.Int32
	autoComplete atomic.Bool
}

func newNativeStub() *nativeStub {
	st := &nativeStub{sessions: map[string]*nativeStubSession{}}
	st.autoComplete.Store(true)
	return st
}

func (st *nativeStub) sessionCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

// serveNativeStub speaks just enough MSP for the native lifecycle:
// initialize, session/start|read|resume|list, turn/start|cancel.
// It runs on the stub's own goroutine; all state funnels through
// the stub mutex, so no caller-side synchronization is needed
// beyond the atomic autoComplete flag.
func serveNativeStub(t *testing.T, conn net.Conn, st *nativeStub) {
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
				continue
			}
			id := *msg.ID
			reply := func(v any) {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": v})
			}
			notify := func(method string, params any) {
				_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
			}
			switch msg.Method {
			case "initialize":
				reply(map[string]any{
					"serverInfo": map[string]string{"name": "stub", "version": "0"},
					"schema":     map[string]any{"fingerprint": "test", "version": 1},
				})
			case "session/start":
				var p struct {
					WorkspaceRoot string `json:"workspaceRoot"`
					ProviderID    string `json:"providerId"`
					ModelID       string `json:"modelId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				st.mu.Lock()
				st.turns++
				sid := fmt.Sprintf("native-lc-%d", len(st.sessions)+1)
				cursor := fmt.Sprintf("c-start-%d", len(st.sessions)+1)
				st.sessions[sid] = &nativeStubSession{
					id: sid, workspaceRoot: p.WorkspaceRoot,
					providerID: p.ProviderID, modelID: p.ModelID, cursor: cursor,
				}
				st.mu.Unlock()
				st.starts.Add(1)
				reply(map[string]any{
					"session":    map[string]string{"sessionId": sid},
					"viewCursor": cursor,
				})
			case "session/read":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				st.mu.Lock()
				s := st.sessions[p.SessionID]
				st.mu.Unlock()
				if s == nil {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
						"code": -32002, "message": "no such session",
						"data": map[string]any{"kind": "sessionNotFound"}}})
					continue
				}
				reply(map[string]any{
					"session": map[string]string{
						"sessionId": s.id, "status": "active",
						"providerId": s.providerID, "modelId": s.modelID,
						"workspaceRoot": s.workspaceRoot,
					},
					"viewCursor": s.cursor,
				})
			case "session/resume":
				var p struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				st.mu.Lock()
				s := st.sessions[p.SessionID]
				st.mu.Unlock()
				if s == nil {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
						"code": -32002, "message": "no such session",
						"data": map[string]any{"kind": "sessionNotFound"}}})
					continue
				}
				reply(map[string]any{
					"session":    map[string]string{"sessionId": s.id, "status": "active"},
					"viewCursor": s.cursor,
				})
			case "session/list":
				st.mu.Lock()
				var ids []map[string]string
				for sid := range st.sessions {
					ids = append(ids, map[string]string{"sessionId": sid})
				}
				st.mu.Unlock()
				reply(map[string]any{"sessions": ids, "nextCursor": ""})
			case "turn/start":
				var p struct {
					SessionID string `json:"sessionId"`
					Input     []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				text := ""
				for _, in := range p.Input {
					if in.Text != "" {
						text = in.Text
					}
				}
				st.mu.Lock()
				st.turns++
				turnID := fmt.Sprintf("turn-%d", st.turns)
				s := st.sessions[p.SessionID]
				st.mu.Unlock()
				if s == nil {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
						"code": -32002, "message": "no such session",
						"data": map[string]any{"kind": "sessionNotFound"}}})
					continue
				}
				reply(map[string]any{
					"commandId": "cmd", "disposition": "started",
					"startedNewTurn": true, "status": "accepted", "turnId": turnID,
				})
				if st.autoComplete.Load() {
					cursor := "c-" + turnID
					st.mu.Lock()
					s.cursor = cursor
					st.mu.Unlock()
					notify("item/completed", map[string]any{
						"sessionId": p.SessionID, "viewCursor": cursor,
						"item": map[string]any{
							"kind": "agentMessage", "status": "completed",
							"text": "echo: " + text,
						},
					})
					notify("turn/completed", map[string]any{
						"sessionId": p.SessionID, "turnId": turnID,
						"terminal": "completed", "viewCursor": cursor,
					})
				}
			case "turn/cancel":
				var p struct {
					SessionID string `json:"sessionId"`
					TurnID    string `json:"turnId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				reply(map[string]any{})
				cursor := "c-cancel-" + p.TurnID
				st.mu.Lock()
				if s := st.sessions[p.SessionID]; s != nil {
					s.cursor = cursor
				}
				st.mu.Unlock()
				notify("turn/completed", map[string]any{
					"sessionId": p.SessionID, "turnId": p.TurnID,
					"terminal": "cancelled", "viewCursor": cursor,
				})
			default:
				reply(map[string]any{})
			}
		}
	}()
}

func newNativeStubHost(t *testing.T, st *nativeStub) *museHost {
	t.Helper()
	client, server := net.Pipe()
	serveNativeStub(t, server, st)
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatalf("stub host handshake: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// assertNoShellBacking fails when any tmux registry row or PTY
// supervisor row exists: the native lifecycle must never mint
// shell state, not even as scaffolding.
func assertNoShellBacking(t *testing.T, what string) {
	t.Helper()
	if n := len(loadRegistry()); n != 0 {
		t.Fatalf("%s: native lifecycle must not create tmux registry rows, got %d", what, n)
	}
	metas, err := ptyListAll()
	if err != nil {
		t.Fatalf("%s: pty list: %v", what, err)
	}
	if len(metas) != 0 {
		t.Fatalf("%s: native lifecycle must not create PTY rows, got %d", what, len(metas))
	}
}

// The full native-only lifecycle with no tmux/PTY anywhere:
// provision (logical record) -> discover (unbound) -> bind ->
// discover (dispatchable) -> two turns -> observe -> cancel ->
// host restart -> resume -> turn again. One session/start
// transmission total: nothing ever duplicates the native session.
func TestNativeOnlySessionLifecycle(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// CREATE: provisioning establishes the logical record only.
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rnc", "session": "nlc", "objective": "answer things",
		"actor": "orch", "request_id": "nlc-1", "backend": "muse-native",
		"workdir": work, "provider": "echo",
	}))
	if err != nil {
		t.Fatalf("native provision: %v", err)
	}
	if res.IsError {
		t.Fatalf("native provision isError: %s", res.Content[0].Text)
	}
	if got := mcpTaskText(t, res); got["state"] != "running" || got["session"] != "nlc" {
		t.Fatalf("native provision must report the running session, got %v", got)
	}
	wsRoot, err := canonicalRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	ls, found, err := loadLogicalSession("nlc")
	if err != nil || !found {
		t.Fatalf("provision must establish the logical record (found=%v err=%v)", found, err)
	}
	if ls.Backend != TargetMuseNative || ls.Workspace != wsRoot || ls.Provider != "echo" || ls.State != logicalActive {
		t.Fatalf("logical record must carry the authorized scope: %+v", ls)
	}
	assertNoShellBacking(t, "provision")

	// The daemon rendezvous for the ambient profile: a bare socket
	// in the isolated store (no daemon behind it — readiness here
	// is only the socket the tests below never execute through).
	sockDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "muse", "session-server")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(sockDir, "session-server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// DISCOVER: the session is known but unbound — refused as
	// unbound, never as unknown, and never via a shell fallback.
	if _, err := resolveExecTarget("nlc"); err == nil || !strings.Contains(err.Error(), "no muse binding") {
		t.Fatalf("unbound native session must be refused as unbound, got %v", err)
	}

	// BIND through the production claim path on a stub host. The
	// scope resolves from the logical record: no shell session was
	// ever seeded, so sessionWorkdir alone would refuse.
	st := newNativeStub()
	h1 := newNativeStubHost(t, st)
	model, err := resolveSessionModel("nlc")
	if err != nil {
		t.Fatalf("model must resolve from the logical record: %v", err)
	}
	if model != "" {
		t.Fatalf("undeclared model must read back empty, got %q", model)
	}
	req := OperationRequest{Actor: "orch", RequestID: "nlc-bind-1"}
	cmdID, err := claimBindIntent(req, "nlc", "echo", "", "metis:nlc", wsRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sid, _, err := bindMuseNativeSessionOnHost(ctx, h1, "nlc", "echo", "", "metis:nlc", wsRoot, model, cmdID, req)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if sid == "" || st.starts.Load() != 1 {
		t.Fatalf("bind must transmit exactly one session/start, sid=%q starts=%d", sid, st.starts.Load())
	}
	b, err := getMuseBinding("nlc")
	if err != nil || b.NativeID != sid || b.Workspace != wsRoot {
		t.Fatalf("binding must pin the native session and workspace: %+v err=%v", b, err)
	}
	if ls, _, _ := loadLogicalSession("nlc"); ls.NativeID != sid {
		t.Fatalf("logical record must learn the bound native session: %+v", ls)
	}
	assertNoShellBacking(t, "bind")

	// DISCOVER: bound plus socketed resolves to the native target
	// carrying its binding identities.
	tgt, err := resolveExecTarget("nlc")
	if err != nil {
		t.Fatalf("bound native session must resolve: %v", err)
	}
	if tgt.Kind != TargetMuseNative || tgt.NativeID != sid || tgt.Provider != "echo" {
		t.Fatalf("native target must carry binding identities, got %+v", tgt)
	}
	if err := sessionBindable("nlc"); err != nil {
		t.Fatalf("native session must be bindable: %v", err)
	}

	// USE: two turns on the same native session, observed purely
	// through Muse events — terminal values and answer text both
	// come from the event stream, never from shell output.
	obs1, err := runNativeObservedTurn(ctx, h1, "nlc", "first prompt", 10*time.Second)
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if obs1.Terminal != "completed" || !strings.Contains(obs1.Text, "first prompt") {
		t.Fatalf("turn 1 must complete with the echoed prompt, got %+v", obs1)
	}
	if obs1.Events == 0 {
		t.Fatal("turn 1 must consume events")
	}
	obs2, err := runNativeObservedTurn(ctx, h1, "nlc", "second prompt", 10*time.Second)
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if obs2.Terminal != "completed" || !strings.Contains(obs2.Text, "second prompt") {
		t.Fatalf("turn 2 must complete with the echoed prompt, got %+v", obs2)
	}
	if obs2.TurnID == obs1.TurnID {
		t.Fatal("continuation must mint a distinct turn id")
	}
	if obs2.NativeID != sid {
		t.Fatalf("both turns must run on the same native session: %q vs %q", obs2.NativeID, sid)
	}
	cur, err := getMuseBinding("nlc")
	if err != nil || cur.Cursor != obs2.Cursor || cur.Cursor == "" {
		t.Fatalf("turn-terminal wait must advance the durable cursor: %+v", cur)
	}

	// OBSERVE: point-in-time reads never move the cursor.
	o1, err := observeNativeSession(ctx, h1, "nlc")
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	o2, err := observeNativeSession(ctx, h1, "nlc")
	if err != nil {
		t.Fatalf("observe again: %v", err)
	}
	if o1.Status != "active" || o1.Cursor != cur.Cursor || o2.Cursor != cur.Cursor {
		t.Fatalf("reads must report status without advancing: %+v %+v stored=%q", o1, o2, cur.Cursor)
	}
	if o1.NativeID != sid {
		t.Fatalf("observation must name the native session: %+v", o1)
	}

	// CANCEL: a withheld turn ends cancelled through turn/cancel
	// plus its terminal event — the ack alone never confirms.
	st.autoComplete.Store(false)
	ack, err := museSubmitTurn(ctx, h1, sid, "third prompt (cancel me)", "")
	if err != nil {
		t.Fatalf("submit turn 3: %v", err)
	}
	disp, err := cancelNativeObservedTurn(ctx, h1, "nlc", ack.TurnID, 10*time.Second)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if disp != ExecutionCancelConfirmed {
		t.Fatalf("observed terminal must confirm the stop, got %s", disp)
	}
	st.autoComplete.Store(true)

	// RESTART/RESUME: the host dies, a fresh one resumes by stored
	// identity, and the session keeps working. One session/start
	// total across the whole lifecycle: recovery never duplicates.
	_ = h1.Close()
	h2 := newNativeStubHost(t, st)
	robs, err := resumeNativeObserved(ctx, h2, "nlc")
	if err != nil {
		t.Fatalf("resume after restart: %v", err)
	}
	if robs.Status != "active" || robs.NativeID != sid {
		t.Fatalf("resume must re-attach the same native session: %+v", robs)
	}
	obs4, err := runNativeObservedTurn(ctx, h2, "nlc", "fourth prompt", 10*time.Second)
	if err != nil {
		t.Fatalf("post-restart turn: %v", err)
	}
	if obs4.Terminal != "completed" || !strings.Contains(obs4.Text, "fourth prompt") {
		t.Fatalf("post-restart turn must complete, got %+v", obs4)
	}
	if st.starts.Load() != 1 {
		t.Fatalf("lifecycle must transmit exactly one session/start, saw %d", st.starts.Load())
	}
	if st.sessionCount() != 1 {
		t.Fatalf("lifecycle must own exactly one native session, saw %d", st.sessionCount())
	}
	assertNoShellBacking(t, "full lifecycle")
}

// Provisioning authorizes the root first: an unregistered workdir
// is refused with no intent row and no logical record. A native
// provision with no workdir parks under the run-scoped default like
// shell provisions do.
func TestNativeProvisionAuthorizesRoot(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	outside := t.TempDir()
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rn", "session": "nx", "objective": "do",
		"actor": "orch", "request_id": "nx-1", "backend": "muse-native",
		"workdir": outside, "provider": "echo",
	}))
	if err != nil {
		t.Fatalf("refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("native provision outside a registered root must refuse")
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMcpProvisionTable(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_provision WHERE request_id = 'nx-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("refused native provision must leave no intent row")
	}
	if _, found, _ := loadLogicalSession("nx"); found {
		t.Fatal("refused native provision must leave no logical record")
	}

	mustRegisterRoot(t, stateDir(), false)
	parked, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rn2", "session": "np", "objective": "do",
		"actor": "orch", "request_id": "np-1", "backend": "muse-native",
		"provider": "echo", "model": "m1",
	}))
	if err != nil || parked.IsError {
		t.Fatalf("parked native provision: %v %v", err, parked)
	}
	ls, found, err := loadLogicalSession("np")
	if err != nil || !found {
		t.Fatalf("parked provision must establish the logical record (found=%v err=%v)", found, err)
	}
	if ls.Model != "m1" || ls.Provider != "echo" {
		t.Fatalf("logical record must carry the scope: %+v", ls)
	}
	if fi, err := os.Stat(ls.Workspace); err != nil || !fi.IsDir() {
		t.Fatalf("provisioned native workspace must exist: %q %v", ls.Workspace, err)
	}
	assertNoShellBacking(t, "native authorize/park")
}

// A stopped logical session never dispatches: the canonical
// resolver refuses it as stopped without needing any shell state.
func TestResolveStoppedNativeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	ws, err := canonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := claimLogicalSession(logicalSession{
		Name: "nz", Backend: provisionBackendNative, Workspace: ws,
		Provider: "echo",
	})
	if err != nil || !claimed {
		t.Fatalf("seed logical session: claimed=%v err=%v", claimed, err)
	}
	if err := stopLogicalSession("nz"); err != nil {
		t.Fatalf("seed stop: %v", err)
	}
	if _, err := resolveExecTarget("nz"); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("stopped native session must be refused as stopped, got %v", err)
	}
	if err := sessionBindable("nz"); err == nil {
		t.Fatal("sessionBindable must refuse a stopped native session")
	}
	assertNoShellBacking(t, "stopped resolve")
}

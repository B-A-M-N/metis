package main

// muse_bind_test.go: binding identity semantics plus host-restart
// recovery against the echo provider. A rebind to a different native
// ID is refused (identity swaps must be explicit); the same ID
// refreshes the cursor. Recovery: bind, kill the host, resume from
// a fresh host with the stored cursor, and complete a turn —
// proving disconnect/reconnect neither duplicates nor loses the
// session.

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// startTestDaemon spawns an echo-provider session daemon on an
// isolated muse home and waits for its well-known socket. The daemon
// owns sessions across client exits; killing it ends durability for
// the test home only.
func startTestDaemon(t *testing.T, home string) func() {
	t.Helper()
	path, err := exec.LookPath("muse")
	if err != nil {
		t.Skipf("muse binary absent: %v", err)
	}
	cmd := exec.Command(path, "serve", "--provider", "echo", "--listen")
	cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+home)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	sock := filepath.Join(home, "muse", "session-server", "session-server.sock")
	deadline := time.Now().Add(20 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("daemon socket never appeared")
		}
		time.Sleep(100 * time.Millisecond)
	}
	return func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
}

func TestMuseBindingRefusesSilentRebind(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "s", "bind")
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := bindMuseSession("s", "native-1", "c1", "echo", "", req); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := bindMuseSession("s", "native-1", "c2", "echo", "", req); err != nil {
		t.Fatalf("same-id rebind must refresh: %v", err)
	}
	got, err := getMuseBinding("s")
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != "native-1" || got.Cursor != "c2" {
		t.Fatalf("rebind must refresh cursor, got %+v", got)
	}
	if err := bindMuseSession("s", "native-2", "c3", "echo", "", req); err == nil {
		t.Fatal("rebind to a different native id must be refused")
	}
	got, err = getMuseBinding("s")
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != "native-1" {
		t.Fatalf("refused rebind changed identity: %+v", got)
	}
	if _, err := getMuseBinding("missing"); err == nil {
		t.Fatal("unknown session must have no binding")
	}
	if err := updateMuseCursor("missing", "c", "turn-x"); err == nil {
		t.Fatal("cursor update on unknown session must fail")
	}
}

func TestMuseBindingRequiresRequest(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := bindMuseSession("s", "n", "c", "echo", "", OperationRequest{}); err == nil {
		t.Fatal("anonymous bind must be refused")
	}
	if err := bindMuseSession("", "n", "c", "echo", "", testReq("t")); err == nil {
		t.Fatal("empty session bind must be refused")
	}
}

// TestMuseNativeWorkerLoopEndToEnd is the P0 qualification loop:
// delegate (create+dispatch) -> native turn executes -> execution
// receipt + runner report filed, all against the echo provider with
// no model calls and no quota. The echo turn returns the prompt, so
// the report must contain the task's own objective text.
func TestMuseNativeWorkerLoopEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("METIS_MUSE_TURN_TIMEOUT", "2m")
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	installFakeTmux(t, "nativeE2E")
	seedOwnedSession(t, "w", "nativeE2E")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	nativeID, cursor, err := museStartSession(ctx, h, "metis-qual-e2e", loadRegistry()["w"].Workdir)
	h.Close()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := bindMuseSession("w", nativeID, cursor, "echo", "", testReq("orch")); err != nil {
		t.Fatalf("bind: %v", err)
	}

	tk, err := createTask("run-native-e2e", "agent", "say DONE", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	ran, err := runWorkerOnce("w", museNativeAdapter(), testReq("op"))
	if err != nil {
		t.Fatalf("native loop failed: %v", err)
	}
	if !ran {
		t.Fatal("native loop must report it executed the task")
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskReportReady {
		t.Fatalf("expected report_ready, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND reporter LIKE 'worker-loop/%' AND source = 'runner-observation' AND report LIKE '%terminal=completed%'`, tk.ID); n != 1 {
		t.Fatalf("expected one completed runner-observation, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_receipts WHERE task_id = ? AND kind = 'execution_completed' AND detail LIKE '%terminal=completed turn=%'`, tk.ID); n != 1 {
		t.Fatalf("expected one terminal-labeled execution receipt, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND report LIKE '%say DONE%'`, tk.ID); n != 1 {
		t.Fatal("report must carry the executed objective (echo returns the prompt)")
	}
}

// An unbound session must be refused before anything executes, with
// the claim refunded so the attempt is not consumed.
func TestMuseNativeWorkerLoopRefusesUnbound(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	installFakeTmux(t, "nativeUnbound")
	seedOwnedSession(t, "wu", "nativeUnbound")

	tk, err := createTask("run-native-unbound", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "wu"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if ran, err := runWorkerOnce("wu", museNativeAdapter(), testReq("op")); err == nil || ran {
		t.Fatalf("unbound native run must be refused, ran=%v err=%v", ran, err)
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskDispatched {
		t.Fatalf("refused claim must be refunded to dispatched, got %s", got.Status)
	}
}

func TestMuseResumeRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "resume")
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	h1, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host 1: %v", err)
	}
	sid, cursor, err := museStartSession(ctx, h1, "metis-qual-rebind", "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := bindMuseSession("w", sid, cursor, "echo", "", testReq("t")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := h1.Close(); err != nil {
		t.Logf("host 1 close: %v", err)
	}

	// Fresh host, same durable store: resume by stored identity and
	// complete a turn. The session survives the host, the cursor
	// replays, and nothing executes twice.
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	h2, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host 2: %v", err)
	}
	defer h2.Close()
	if _, _, err := museResumeSession(ctx, h2, b.NativeID, b.Cursor); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ack, err := museSubmitTurn(ctx, h2, b.NativeID, "recovered?", "")
	if err != nil {
		t.Fatalf("turn submit: %v", err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 60*time.Second)
	defer wcancel()
	terminal, nc, err := museWaitTurn(wctx, h2, b.NativeID, ack.TurnID, nil)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if terminal != "completed" {
		t.Fatalf("terminal = %q", terminal)
	}
	if err := updateMuseCursor("w", nc, "turn-resume"); err != nil {
		t.Fatal(err)
	}
	got, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != sid || got.Cursor == "" {
		t.Fatalf("binding lost identity or cursor: %+v", got)
	}
}

// Binding pins the session's workspace: the row stores the
// canonical root, rebinding to a different root is refused, and a
// session that moves after binding fails the execution scope
// check instead of silently executing elsewhere.
func TestMuseBindingPinsWorkspace(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "ws")
	req := testReq("orch")
	if err := bindMuseSession("w", "native-1", "c0", "echo", "", req); err != nil {
		t.Fatalf("bind: %v", err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	want, err := canonicalRoot(loadRegistry()["w"].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	if b.Workspace != want {
		t.Fatalf("binding must pin the session workspace %q, got %+v", want, b)
	}
	// Re-homing the session breaks the scope check.
	reg := loadRegistry()
	entry := reg["w"]
	entry.Workdir = t.TempDir()
	reg["w"] = entry
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := checkMuseExecutionScope("w", b); err == nil {
		t.Fatal("turn on a re-homed session must be refused")
	}
	// Rebinding the same native session from the new root is a
	// workspace switch, not a refresh.
	if err := bindMuseSession("w", "native-1", "c1", "echo", "", req); err == nil {
		t.Fatal("silent re-home on rebind must be refused")
	}
}

// Binding a session with no resolvable workspace is refused: a
// native turn with no authorized root has nowhere legal to run.
func TestMuseBindRefusesUnknownSession(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := bindMuseSession("ghost", "native-1", "c0", "echo", "", testReq("orch")); err == nil {
		t.Fatal("bind without a session workspace must be refused")
	}
}

// Retrying session creation with the same caller command id must
// not create a second native session: the first ack is replayed
// from the durable intent instead of retransmitting session/start.
func TestSessionStartDedupsOnCommandID(t *testing.T) {
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
				"session":    map[string]string{"sessionId": "native-dedup"},
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
	sid2, _, err := museStartSessionWithID(ctx, h, "t", "", museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if sid1 != sid2 || sid1 != "native-dedup" {
		t.Fatalf("retry must replay the created session, got %q %q", sid1, sid2)
	}
	if starts != 1 {
		t.Fatalf("retry must not retransmit session/start, saw %d calls", starts)
	}
}

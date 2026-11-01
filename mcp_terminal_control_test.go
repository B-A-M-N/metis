package main

// mcp_terminal_control_test.go: F-08 failing-first.
//
// metis_terminal is read-only; authorized terminal lifecycle arrives
// via metis_terminal_input, metis_terminal_resize, and
// metis_session_stop. These tools must delegate through the existing
// terminal/session domain services — the single-writer input-lease
// authority, generation fencing, and ownership-bound targets — with
// scoped capability grants, request idempotency, and typed outcomes.
// They must never expose unrestricted remote send-keys, and remote
// callers without the admin capability must fail closed (F-02).

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// seedLiveSelfPtyRow records a PTY session whose supervisor and
// worker are this test process: identity checks pass (pid + /proc
// starttime + fresh heartbeat) without spawning anything.
func seedLiveSelfPtyRow(t *testing.T, name string) {
	t.Helper()
	seedPtyRow(t, name, t.TempDir())
	self := os.Getpid()
	start, err := procStartTime(self)
	if err != nil {
		t.Fatalf("proc start time: %v", err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE pty_sessions SET status = 'running',
		supervisor_pid = ?, supervisor_start = ?,
		worker_pid = ?, worker_start = ?, heartbeat = ? WHERE name = ?`,
		self, start, self, start, nowUTC(), name); err != nil {
		t.Fatal(err)
	}
}

// openFifoReader holds the session FIFO's read end open so control
// writes succeed without a live supervisor process draining them.
func openFifoReader(t *testing.T, name string) {
	t.Helper()
	p := ptyFifoPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("fifo reader: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

func setPtyGeneration(t *testing.T, name string, gen int) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE pty_sessions SET generation = ? WHERE name = ?`, gen, name); err != nil {
		t.Fatal(err)
	}
}

func terminalInput(t *testing.T, session, text, actor, requestID string, extra map[string]any) mcpToolResult {
	t.Helper()
	args := map[string]any{"session": session, "text": text, "actor": actor, "request_id": requestID}
	for k, v := range extra {
		args[k] = v
	}
	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, args))
	if err != nil {
		t.Fatalf("terminal input RPC: %v", err)
	}
	return res
}

func terminalOut(t *testing.T, res mcpToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("terminal isError: %s", res.Content[0].Text)
	}
	return mcpTaskText(t, res)
}

func terminalErrText(t *testing.T, res mcpToolResult, err error) string {
	t.Helper()
	if err != nil {
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("expected refusal, got success: %s", res.Content[0].Text)
	}
	return res.Content[0].Text
}

// Rival writers arbitrate through the lease authority: a live rival
// lease refuses the write and names the holder, and no keystroke
// reaches the transport. After the rival releases, the same
// request_id resumes instead of replaying a failure.
func TestMCPTerminalInputRivalWriters(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "rival-owner")
	seedOwnedSession(t, "s", "rival-owner")
	if err := tmuxAcquireLease("s", "rival-writer"); err != nil {
		t.Fatalf("seed rival lease: %v", err)
	}

	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "s", "text": "sneak", "actor": "mcp-caller", "request_id": "riv-1",
	}))
	text := terminalErrText(t, res, err)
	if !strings.Contains(text, "rival-writer") {
		t.Fatalf("rival refusal must name the holder, got: %s", text)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "send-keys") {
		t.Fatalf("refused input reached send-keys:\n%s", logged)
	}

	// The rival releases; the identical retry resumes the claimed
	// key instead of replaying the refusal.
	db, dberr := openTasksDB()
	if dberr != nil {
		t.Fatal(dberr)
	}
	if _, dberr := db.Exec(`DELETE FROM input_leases WHERE channel = 'tmux:s'`); dberr != nil {
		t.Fatal(dberr)
	}
	_ = db.Close()
	out := terminalOut(t, terminalInput(t, "s", "sneak", "mcp-caller", "riv-1", nil))
	if out["disposition"] != DispositionConfirmed {
		t.Fatalf("resumed input must confirm, got %v", out)
	}
}

// The same fencing holds on the PTY channel: a rival lease holder
// vetoes the write before any frame is queued.
func TestMCPTerminalInputRivalWritersPty(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "p")
	if err := ptyAcquireLease("p", "rival-writer"); err != nil {
		t.Fatalf("seed rival lease: %v", err)
	}
	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "pty:p", "text": "sneak", "actor": "mcp-caller", "request_id": "riv-2",
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(text, "rival-writer") {
		t.Fatalf("pty rival refusal must name the holder, got: %s", text)
	}
}

// A stale generation is refused before any lease is touched; the
// matching generation proceeds to the transport.
func TestMCPTerminalInputStaleGeneration(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "p")
	openFifoReader(t, "p")

	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "pty:p", "text": "hi", "actor": "op", "request_id": "gen-1", "generation": 7,
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(text, "generation changed") {
		t.Fatalf("stale generation must be refused as such, got: %s", text)
	}

	out := terminalOut(t, terminalInput(t, "pty:p", "hi", "op", "gen-2", map[string]any{"generation": 0}))
	if out["disposition"] != DispositionQueued {
		t.Fatalf("matching generation must reach the transport, got %v", out)
	}
	if out["generation"] != float64(0) {
		t.Fatalf("result must report the fenced generation, got %v", out)
	}
}

// Input into a one-shot harness is blocked with a typed outcome:
// no send, never silent success.
func TestMCPTerminalInputBlocked(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "block-owner")
	seedOwnedSession(t, "b", "block-owner")
	reg := loadRegistry()
	e := reg["b"]
	e.Harness = "muse" // one-shot, no live input per adapter registry
	reg["b"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}

	out := terminalOut(t, terminalInput(t, "b", "hello?", "op", "blk-1", nil))
	if out["disposition"] != DispositionBlocked {
		t.Fatalf("one-shot input must be blocked, got %v", out)
	}
	if !strings.Contains(out["detail"].(string), "one-shot") {
		t.Fatalf("blocked detail must name the reason, got %v", out)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "send-keys") {
		t.Fatalf("blocked input reached send-keys:\n%s", logged)
	}
	// The blocked verdict is recorded: an identical retry converges.
	replay := terminalOut(t, terminalInput(t, "b", "hello?", "op", "blk-1", nil))
	if replay["replay"] != true || replay["disposition"] != DispositionBlocked {
		t.Fatalf("blocked replay must converge, got %v", replay)
	}
}

// Resize after a restart: the generation bumped by recovery fences
// the stale request, while the fresh generation resizes.
func TestMCPTerminalResizeAfterRestart(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "p")
	openFifoReader(t, "p")

	resize := func(requestID string, extra map[string]any) (mcpToolResult, error) {
		args := map[string]any{"session": "pty:p", "rows": 30, "cols": 100, "actor": "op", "request_id": requestID}
		for k, v := range extra {
			args[k] = v
		}
		return mcpCallTool("metis_terminal_resize", mustJSON(t, args))
	}
	r1, e1 := resize("rs-1", map[string]any{"generation": 0})
	out := terminalOut(t, mustTool(t, r1, e1))
	if out["state"] != "resized" {
		t.Fatalf("fresh resize must succeed, got %v", out)
	}

	// The session restarts: recovery bumps the generation.
	setPtyGeneration(t, "p", 1)

	res, err := resize("rs-2", map[string]any{"generation": 0})
	if text := terminalErrText(t, res, err); !strings.Contains(text, "generation changed") {
		t.Fatalf("post-restart stale resize must be refused, got: %s", text)
	}
	r3, e3 := resize("rs-3", map[string]any{"generation": 1})
	out = terminalOut(t, mustTool(t, r3, e3))
	if out["state"] != "resized" || out["generation"] != float64(1) {
		t.Fatalf("fresh-generation resize must succeed, got %v", out)
	}
}

func mustTool(t *testing.T, res mcpToolResult, err error) mcpToolResult {
	t.Helper()
	if err != nil {
		t.Fatalf("tool RPC: %v", err)
	}
	return res
}

// Terminating a replaced session refuses: a stop naming the old
// generation never signals the new owner's processes, while the
// fresh generation stops the row it actually fenced.
func TestMCPSessionStopReplacedSession(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedPtyRow(t, "p", t.TempDir()) // dead supervisor/worker, generation 0

	stop := func(requestID string, extra map[string]any) (mcpToolResult, error) {
		args := map[string]any{"session": "pty:p", "actor": "op", "request_id": requestID}
		for k, v := range extra {
			args[k] = v
		}
		return mcpCallTool("metis_session_stop", mustJSON(t, args))
	}

	res, err := stop("st-0", map[string]any{"generation": 9})
	if text := terminalErrText(t, res, err); !strings.Contains(text, "generation changed") {
		t.Fatalf("future generation must be refused, got: %s", text)
	}

	// The name is taken over: new identity at generation 1.
	setPtyGeneration(t, "p", 1)
	res, err = stop("st-1", map[string]any{"generation": 0})
	if text := terminalErrText(t, res, err); !strings.Contains(text, "generation changed") {
		t.Fatalf("stop of a replaced session must refuse, got: %s", text)
	}
	if m, _ := ptyGet("p"); m.Status == "gone" {
		t.Fatal("refused stop must not touch the replacement row")
	}

	sr, se := stop("st-2", map[string]any{"generation": 1})
	out := terminalOut(t, mustTool(t, sr, se))
	if out["state"] != "stopped" {
		t.Fatalf("fenced stop must terminate, got %v", out)
	}
	if m, _ := ptyGet("p"); m.Status != "gone" {
		t.Fatalf("stopped row must read gone, got %q", m.Status)
	}
}

// Replayed commands converge: the identical retry returns the
// recorded outcome with the same op id, while a reused key with
// different text is a conflict — and the transport ran exactly once.
func TestMCPTerminalInputReplay(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "replay-owner")
	seedOwnedSession(t, "s", "replay-owner")

	first := terminalOut(t, terminalInput(t, "s", "do it", "op", "rep-1", nil))
	if first["replay"] != false {
		t.Fatalf("fresh input must not be a replay: %v", first)
	}
	second := terminalOut(t, terminalInput(t, "s", "do it", "op", "rep-1", nil))
	if second["replay"] != true || second["op_id"] != first["op_id"] {
		t.Fatalf("identical replay must converge on the recorded op: %v vs %v", second, first)
	}
	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "s", "text": "do something else", "actor": "op", "request_id": "rep-1",
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(text, "conflict") {
		t.Fatalf("reused key with different text must conflict, got: %s", text)
	}
	logged, _ := os.ReadFile(logPath)
	if n := strings.Count(string(logged), "send-keys -t $99 -l"); n != 1 {
		t.Fatalf("transport must run exactly once, saw %d sends:\n%s", n, logged)
	}
}

// F-02 boundary: without the admin capability a remote caller fails
// closed on every lifecycle tool with no side effects; a wrong
// credential fails at authentication.
func TestMCPTerminalUnauthorizedRemote(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-terminal")
	t.Setenv("METIS_MCP_INGRESS", "remote")
	logPath := installFakeTmux(t, "u-owner")
	seedOwnedSession(t, "s", "u-owner")

	res, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "s", "text": "sneak", "actor": "mallory", "request_id": "u-1", "auth": "tok-terminal",
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(strings.ToLower(text), "admin") {
		t.Fatalf("unqualified remote input must fail on the admin capability, got: %s", text)
	}
	res, err = mcpCallTool("metis_session_stop", mustJSON(t, map[string]any{
		"session": "s", "actor": "mallory", "request_id": "u-2", "auth": "tok-terminal",
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(strings.ToLower(text), "admin") {
		t.Fatalf("unqualified remote stop must fail on the admin capability, got: %s", text)
	}
	res, err = mcpCallTool("metis_terminal_resize", mustJSON(t, map[string]any{
		"session": "s", "rows": 24, "cols": 80, "actor": "mallory", "request_id": "u-3", "auth": "tok-terminal",
	}))
	if text := terminalErrText(t, res, err); !strings.Contains(strings.ToLower(text), "admin") {
		t.Fatalf("unqualified remote resize must fail on the admin capability, got: %s", text)
	}
	db, dberr := openTasksDB()
	if dberr != nil {
		t.Fatal(dberr)
	}
	if err := ensureMcpTerminalTable(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_terminal_ops`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if n != 0 {
		t.Fatalf("refused calls must claim no request keys, got %d rows", n)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "send-keys") || strings.Contains(string(logged), "kill-session") {
		t.Fatalf("refused calls must not touch the transport:\n%s", logged)
	}

	// Wrong credential fails at authentication, before anything else.
	if _, err := mcpCallTool("metis_terminal_input", mustJSON(t, map[string]any{
		"session": "s", "text": "sneak", "actor": "mallory", "request_id": "u-4", "auth": "wrong",
	})); err == nil {
		t.Fatal("wrong auth must be refused")
	}
}

// A scoped admin grant authorizes the remote principal across all
// three lifecycle tools.
func TestMCPTerminalAuthorizedRemote(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-terminal")
	t.Setenv("METIS_MCP_INGRESS", "remote")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "admin")
	installFakeTmux(t, "g-owner")
	seedOwnedSession(t, "s", "g-owner")

	out := terminalOut(t, terminalInput(t, "s", "hello", "op", "g-1", map[string]any{"auth": "tok-terminal"}))
	if out["disposition"] != DispositionConfirmed {
		t.Fatalf("admin-granted remote input must proceed, got %v", out)
	}
	res, err := mcpCallTool("metis_session_stop", mustJSON(t, map[string]any{
		"session": "s", "actor": "op", "request_id": "g-2", "auth": "tok-terminal",
	}))
	if err != nil || res.IsError {
		t.Fatalf("admin-granted remote stop must proceed: %v %v", res, err)
	}
}

// tmux success paths run the real domain services: input addresses
// the bound session ID, stop kills it and clears its leases.
func TestMCPTerminalTmuxSuccess(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "ok-owner")
	seedOwnedSession(t, "s", "ok-owner")

	out := terminalOut(t, terminalInput(t, "s", "hello", "op", "ok-1", nil))
	if out["disposition"] != DispositionConfirmed || out["backend"] != TargetTMUX {
		t.Fatalf("tmux input must confirm, got %v", out)
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "send-keys -t $99 -l") {
		t.Fatalf("input must address the bound session id:\n%s", logged)
	}

	// The earlier input holds the channel lease; release it so the
	// rival lease below is the only holder stop must preempt.
	releaseDB, rerr := openTasksDB()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, rerr := releaseDB.Exec(`DELETE FROM input_leases WHERE channel = 'tmux:s'`); rerr != nil {
		t.Fatal(rerr)
	}
	_ = releaseDB.Close()
	if err := tmuxAcquireLease("s", "lingering-rival"); err != nil {
		t.Fatal(err)
	}
	res, err := mcpCallTool("metis_session_stop", mustJSON(t, map[string]any{
		"session": "s", "actor": "op", "request_id": "ok-2",
	}))
	if err != nil || res.IsError {
		t.Fatalf("tmux stop must proceed: %v %v", res, err)
	}
	if got := mcpTaskText(t, res); got["state"] != "stopped" {
		t.Fatalf("stop must report stopped, got %v", got)
	}
	logged, _ = os.ReadFile(logPath)
	if !strings.Contains(string(logged), "kill-session -t $99") {
		t.Fatalf("stop must kill the bound session id:\n%s", logged)
	}
	if got := loadRegistry()["s"]; !got.Stopped {
		t.Fatal("stopped session not marked stopped after confirmed kill")
	}
	db, dberr := openTasksDB()
	if dberr != nil {
		t.Fatal(dberr)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM input_leases WHERE channel IN ('tmux:s','pty:s')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stop must clear session leases, %d remain", n)
	}
	// Identical stop retry converges on the recorded outcome.
	replay, err := mcpCallTool("metis_session_stop", mustJSON(t, map[string]any{
		"session": "s", "actor": "op", "request_id": "ok-2",
	}))
	if err != nil || replay.IsError {
		t.Fatalf("stop replay must converge: %v %v", replay, err)
	}
	if got := mcpTaskText(t, replay); got["replay"] != true {
		t.Fatalf("stop replay must be marked, got %v", got)
	}
}

// PTY input through a live supervisor queues under the caller's
// lease and reports the queued disposition honestly.
func TestMCPTerminalPtyInputQueued(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "p")
	openFifoReader(t, "p")

	out := terminalOut(t, terminalInput(t, "pty:p", "echo hi", "op", "q-1", nil))
	if out["disposition"] != DispositionQueued || out["backend"] != TargetPTY {
		t.Fatalf("pty input must queue honestly, got %v", out)
	}
	if out["op_id"] == "" || out["op_id"] == nil {
		t.Fatalf("queued input must carry an op id: %v", out)
	}
	replay := terminalOut(t, terminalInput(t, "pty:p", "echo hi", "op", "q-1", nil))
	if replay["replay"] != true || replay["op_id"] != out["op_id"] {
		t.Fatalf("identical replay must converge: %v vs %v", replay, out)
	}
}

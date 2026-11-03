package main

// mcp_adapter_test.go: the thin adapter speaks newline-delimited
// JSON-RPC, advertises only implemented tools, and never duplicates
// work on retried mutations.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mcpRoundTrip feeds request lines through the serve loop and returns
// reply lines, mimicking one Ocliv stdio exchange per line.
func mcpRoundTrip(t *testing.T, lines ...string) []string {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	var in bytes.Buffer
	for _, l := range lines {
		in.WriteString(l + "\n")
	}
	var out bytes.Buffer
	mcpServeLoop(&in, &out)
	raw := strings.TrimSpace(out.String())
	if raw == "" {
		return nil
	}
	return strings.Split(raw, "\n")
}

func mcpDecode(t *testing.T, line string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(line), &v); err != nil {
		t.Fatalf("reply not JSON: %v", line)
	}
	return v
}

func mcpCall(t *testing.T, id int, tool string, args string) string {
	t.Helper()
	line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		id, tool, args)
	replies := mcpRoundTrip(t, line)
	if len(replies) != 1 {
		t.Fatalf("one exchange, one reply: %v", replies)
	}
	return replies[0]
}

func TestMCPInitializeAndList(t *testing.T) {
	replies := mcpRoundTrip(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
	)
	if len(replies) != 2 {
		t.Fatalf("notifications must not be answered: %v", replies)
	}
	init := mcpDecode(t, replies[0])
	if init["result"] == nil {
		t.Fatalf("initialize failed: %v", init)
	}
	list := mcpDecode(t, replies[1])
	res, _ := list["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"metis_status", "metis_agents", "metis_events", "metis_delegate", "metis_send", "metis_report", "metis_reconcile"} {
		if !names[want] {
			t.Fatalf("tool %s not advertised", want)
		}
	}
	// Graph tools ship with the graph module, not before.
	if names["metis_graph_walk"] || names["metis_graph_coverage"] {
		t.Fatal("graph tools advertised without a backend")
	}
}

func TestMCPUnknownToolAndBadParams(t *testing.T) {
	reply := mcpCall(t, 1, "metis_hack", `{}`)
	v := mcpDecode(t, reply)
	errObj, _ := v["error"].(map[string]any)
	if errObj == nil || int(errObj["code"].(float64)) != mcpMethodNotFound {
		t.Fatalf("unknown tool must be -32601: %v", v)
	}
	reply2 := mcpCall(t, 2, "metis_delegate", `{"run":"r"}`)
	v2 := mcpDecode(t, reply2)
	err2, _ := v2["error"].(map[string]any)
	if err2 == nil || int(err2["code"].(float64)) != mcpInvalidParams {
		t.Fatalf("missing args must be -32602: %v", v2)
	}
}

func TestMCPDelegateIdempotent(t *testing.T) {
	// The loop helper resets env per call, so drive the tool
	// handler directly here, sharing one state dir.
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "mcpw2")
	seedOwnedSession(t, "w", "mcpw2")
	args := `{"run":"run-mcp","session":"w","objective":"do thing","actor":"chatgpt","request_id":"req-1","key":"k-1"}`
	r1 := mcpDecode(t, mcpCallDirect(t, "metis_delegate", args))
	r2 := mcpDecode(t, mcpCallDirect(t, "metis_delegate", args))
	id1 := toolTextField(t, r1, "task_id")
	id2 := toolTextField(t, r2, "task_id")
	if id1 == "" || id1 != id2 {
		t.Fatalf("retry must return the same task id: %q %q", id1, id2)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Fatalf("duplicate dispatch created %d tasks", n)
	}
}

func TestMCPSendIdempotent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "mcpw3")
	seedOwnedSession(t, "w", "mcpw3")
	args := `{"session":"w","text":"look here","actor":"chatgpt","request_id":"send-1"}`
	r1 := mcpDecode(t, mcpCallDirect(t, "metis_send", args))
	r2 := mcpDecode(t, mcpCallDirect(t, "metis_send", args))
	id1 := toolTextField(t, r1, "guidance_id")
	id2 := toolTextField(t, r2, "guidance_id")
	if id1 == "" || id1 != id2 {
		t.Fatalf("retry must return the same guidance id: %q %q", id1, id2)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM guidance`); n != 1 {
		t.Fatalf("duplicate send created %d guidance rows", n)
	}
}

func TestMCPAgentsRedactOwnership(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "mcpw4")
	seedOwnedSession(t, "sec", "mcpw4")
	reply := mcpDecode(t, mcpCallDirect(t, "metis_agents", `{}`))
	text := toolText(reply)
	if strings.Contains(text, "owner") {
		t.Fatalf("agent view leaks ownership material: %s", text)
	}
	if !strings.Contains(text, "sec") {
		t.Fatalf("agent view missing session: %s", text)
	}
}

// mcpCallDirect invokes the tool handler in-process sharing the
// test's METIS_STATE (the loop helper resets env per call).
func mcpCallDirect(t *testing.T, tool, args string) string {
	t.Helper()
	res, err := mcpCallTool(tool, json.RawMessage(args))
	if err != nil {
		out, _ := json.Marshal(mcpResponse{JSONRPC: "2.0", ID: 1, Error: &mcpError{Code: mcpInternalError, Message: err.Error()}})
		return string(out)
	}
	out, _ := json.Marshal(mcpResponse{JSONRPC: "2.0", ID: 1, Result: res})
	return string(out)
}

func toolText(v map[string]any) string {
	res, _ := v["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	m, _ := content[0].(map[string]any)
	s, _ := m["text"].(string)
	return s
}

func toolTextField(t *testing.T, v map[string]any, field string) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(toolText(v)), &decoded); err != nil {
		t.Fatalf("tool text not JSON: %v", v)
	}
	s, _ := decoded[field].(string)
	return s
}

// TestStalledHostDoesNotBlockControlOps is the P0-F regression:
// one submit wedged on a hung provider binary must not hold the
// connection hostage. A live daemon socket lets the submit past
// the daemon gate, but the fake binary never answers the
// initialize handshake, so the call drains its own RPC budget
// and returns unknown (retry with the same request_id). The
// unrelated status check on the second session must answer
// first — its reply line precedes the stalled one — proving
// bounded dispatch, not a serial wedge.
func TestStalledHostDoesNotBlockControlOps(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	// A real daemon socket with nothing behind it.
	sockDir := filepath.Join(xdg, "muse", "session-server")
	if err := os.MkdirAll(sockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(sockDir, "session-server.sock"))
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	defer ln.Close()
	// A provider binary that never speaks: exec succeeds, the
	// handshake hangs until the submit budget fires.
	bindir := t.TempDir()
	fake := filepath.Join(bindir, "muse")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 45\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// Remote credentials go up only after local-operator setup
	// (provision, bind, stub completion) is done: with a token
	// configured every write op — setup included — demands it.

	bindEcho := func(name string) *museHost {
		work := t.TempDir()
		mustRegisterRoot(t, work, true)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
			"run": "rmsg", "session": name, "objective": "receive messages",
			"actor": "orch", "request_id": name + "-prov-1", "backend": "muse-native",
			"workdir": work, "provider": "echo",
		}))
		if err != nil {
			t.Fatalf("native provision %s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("native provision %s isError: %s", name, res.Content[0].Text)
		}
		st := newNativeStub()
		h := newNativeStubHost(t, st)
		wsRoot, err := canonicalRoot(work)
		if err != nil {
			t.Fatal(err)
		}
		model, err := resolveSessionModel(name)
		if err != nil {
			t.Fatal(err)
		}
		req := OperationRequest{Actor: "orch", RequestID: name + "-bind-1", Principal: "tester"}
		cmdID, err := claimBindIntent(req, name, "echo", "", "metis:"+name, wsRoot, model)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := bindMuseNativeSessionOnHost(ctx, h, name, "echo", "", "metis:"+name, wsRoot, model, cmdID, req); err != nil {
			t.Fatalf("bind %s: %v", name, err)
		}
		return h
	}
	bindEcho("stallA")
	hB := bindEcho("quickB")

	// quickB completes through the stub host, so its terminal is
	// durable before the loop test: the status poll answers from
	// the cache without touching any host.
	bB, err := getMuseBinding("quickB")
	if err != nil {
		t.Fatal(err)
	}
	bmsg := MuseMessageRequest{SessionID: "quickB", Text: "fast", RequestID: "quickB-m1", Actor: "orch"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Claim as the same principal the stdio loop will present for
	// the token-authed status poll below (mcpPrincipalRemoteToken):
	// one operator, one label. A different label would be a
	// different principal and is rightly refused.
	quickCmd, _, _, err := claimMuseMessageIntent(bmsg, bB, mcpPrincipalRemoteToken)
	if err != nil {
		t.Fatal(err)
	}
	done, err := sendMuseMessageOnHost(ctx, hB, bmsg, bB, quickCmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "completed" {
		t.Fatalf("stub submit must complete, got %+v", done)
	}

	t.Setenv("METIS_MCP_REMOTE_INGRESS", "1")
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-stall")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "stallA,quickB")

	call := func(id int, tool string, args map[string]any) string {
		args["auth"] = "tok-stall"
		return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			id, tool, string(mustJSON(t, args)))
	}
	var in bytes.Buffer
	in.WriteString(call(1, "metis_muse_message", map[string]any{
		"session": "stallA", "text": "wedged hello", "actor": "orch", "request_id": "stallA-m1"}) + "\n")
	in.WriteString(call(2, "metis_muse_message_status", map[string]any{
		"request_id": "quickB-m1", "actor": "orch"}) + "\n")
	var out bytes.Buffer
	start := time.Now()
	mcpServeLoop(&in, &out)
	elapsed := time.Since(start)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("two exchanges, two replies: %q", out.String())
	}
	first := mcpDecode(t, lines[0])
	second := mcpDecode(t, lines[1])
	// The fast control op answers first despite being second on
	// the wire: bounded dispatch, not a serial wedge.
	if id, _ := first["id"].(float64); id != 2 {
		t.Fatalf("quick status reply must precede the stalled one, got order %v then %v", first["id"], second["id"])
	}
	if got := toolTextField(t, first, "state"); got != "completed" {
		t.Fatalf("quick status must replay its durable terminal, got %q", toolText(first))
	}
	if got := toolTextField(t, second, "state"); got != "unknown" {
		t.Fatalf("stalled submit must be honest unknown, got %q", toolText(second))
	}
	if text := toolText(second); !strings.Contains(text, "retry") {
		t.Fatalf("stalled submit must stay recoverable, got %q", text)
	}
	if elapsed >= 30*time.Second {
		t.Fatalf("bounded RPC must stay under the exchange budget, took %v", elapsed)
	}
}

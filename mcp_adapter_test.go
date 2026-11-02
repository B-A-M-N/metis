package main

// mcp_adapter_test.go: the thin adapter speaks newline-delimited
// JSON-RPC, advertises only implemented tools, and never duplicates
// work on retried mutations.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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

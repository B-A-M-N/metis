package main

// mcp_hardening_test.go: Ocliv-bridge interop hardening for the thin
// Metis stdio adapter. Every test here exercises a real failure at
// the Ocliv `mcp expose` boundary: one request line in, exactly one
// reply line out, 30s budget, no notifications across the bridge.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// Numeric zero is a valid JSON-RPC id. Through Ocliv's bridge a
// missing reply is not a shrug — the exchange hangs to the 30s
// timeout and the adapter retires. An id of 0 must round-trip.
func TestMCPNumericZeroID(t *testing.T) {
	replies := mcpRoundTrip(t,
		`{"jsonrpc":"2.0","id":0,"method":"ping","params":{}}`,
		`{"jsonrpc":"2.0","id":0,"method":"tools/call","params":{"name":"metis_status","arguments":{}}}`,
	)
	if len(replies) != 2 {
		t.Fatalf("id 0 must get one reply per request, got %d: %v", len(replies), replies)
	}
	for i, line := range replies {
		v := mcpDecode(t, line)
		id, ok := v["id"].(float64)
		if !ok || id != 0 {
			t.Fatalf("reply %d must echo id 0, got %v", i, v["id"])
		}
	}
	if mcpDecode(t, replies[1])["result"] == nil {
		t.Fatalf("id-0 tools/call must succeed: %s", replies[1])
	}
}

// Agent enumeration must be deterministic across calls: sorted by
// name with a total and an explicit truncation flag, plus
// cursor pagination. Ocliv forwards one bounded reply; silent map
// order or silent truncation would show different fleets per call.
func TestMCPAgentsSortedAndPaged(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pagertest")
	seedOwnedSession(t, "zeta", "pagertest")
	seedOwnedSession(t, "alpha", "pagertest")
	seedOwnedSession(t, "mid", "pagertest")

	all := mcpDecode(t, mcpCallDirect(t, "metis_agents", `{}`))
	var body struct {
		Agents    []map[string]any `json:"agents"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(toolText(all)), &body); err != nil {
		t.Fatalf("agents text not JSON: %v", all)
	}
	if body.Total != 3 || len(body.Agents) != 3 || body.Truncated {
		t.Fatalf("expected 3/3 untruncated, got %+v", body)
	}
	for i, want := range []string{"alpha", "mid", "zeta"} {
		if body.Agents[i]["name"] != want {
			t.Fatalf("agent order not deterministic: %+v", body.Agents)
		}
	}

	page := mcpDecode(t, mcpCallDirect(t, "metis_agents", `{"limit":2}`))
	var p1 struct {
		Agents    []map[string]any `json:"agents"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(toolText(page)), &p1); err != nil {
		t.Fatalf("paged agents text not JSON: %v", page)
	}
	if p1.Total != 3 || len(p1.Agents) != 2 || !p1.Truncated {
		t.Fatalf("expected 2/3 truncated page, got %+v", p1)
	}

	next := mcpDecode(t, mcpCallDirect(t, "metis_agents", `{"limit":2,"after":"mid"}`))
	var p2 struct {
		Agents    []map[string]any `json:"agents"`
		Total     int              `json:"total"`
		Truncated bool             `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(toolText(next)), &p2); err != nil {
		t.Fatalf("cursor page text not JSON: %v", next)
	}
	if p2.Total != 3 || len(p2.Agents) != 1 || p2.Truncated {
		t.Fatalf("expected 1/3 final page, got %+v", p2)
	}
	if p2.Agents[0]["name"] != "zeta" {
		t.Fatalf("cursor page wrong agent: %+v", p2.Agents)
	}
}

// An over-budget tool result is an explicit bounded-result error,
// never a byte-cut document: a truncated JSON payload stops being
// parseable, and the caller cannot tell data from cut mark.
func TestMCPResultTooLargeIsExplicit(t *testing.T) {
	big := strings.Repeat("é", 200000)
	_, err := mcpTextResult(map[string]any{"kk": big})
	if err == nil {
		t.Fatal("over-budget result must be refused, not shipped partial")
	}
	var tle *mcpTooLargeError
	if !errors.As(err, &tle) {
		t.Fatalf("refusal must be a typed size error, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "narrow the query") {
		t.Fatalf("error must name the narrowing knobs, got: %v", err)
	}
	// Under-budget payloads still pass through, multi-byte intact.
	small, err := mcpTextResult(map[string]any{"kk": "é"})
	if err != nil {
		t.Fatalf("mcpTextResult: %v", err)
	}
	line, err := json.Marshal(mcpResponse{JSONRPC: "2.0", ID: 1, Result: small})
	if err != nil {
		t.Fatalf("marshal reply: %v", err)
	}
	if !json.Valid(line) || !utf8.Valid(line) {
		t.Fatal("under-budget reply must stay valid JSON/UTF-8")
	}
}

// Mutations bind to an operator-set write token when configured.
// Reads stay open. Unset token preserves local-stdio trust.
func TestMCPWriteTokenGatesMutations(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "s3cret")
	installFakeTmux(t, "toktest")
	seedOwnedSession(t, "w", "toktest")

	// Delegate without auth must be refused, not executed.
	bad := mcpDecode(t, mcpCallDirect(t, "metis_delegate",
		`{"run":"r","session":"w","objective":"x","actor":"mallory","request_id":"q1","key":"kq1"}`))
	if e, _ := bad["error"].(map[string]any); e == nil {
		t.Fatalf("unauthenticated delegate must be refused: %v", bad)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("refused delegate created %d tasks", n)
	}

	// Delegate with auth but no remote opt-in is refused: the
	// token proves transport, not authority.
	noOpt := mcpDecode(t, mcpCallDirect(t, "metis_delegate",
		`{"run":"r","session":"w","objective":"x","actor":"chatgpt","request_id":"q1","key":"kq1","auth":"s3cret"}`))
	if e, _ := noOpt["error"].(map[string]any); e == nil {
		t.Fatalf("remote delegate without opt-in must be refused: %v", noOpt)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("refused delegate created %d tasks", n)
	}
	// With the operator's explicit opt-in, the token call proceeds.
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	good := mcpDecode(t, mcpCallDirect(t, "metis_delegate",
		`{"run":"r","session":"w","objective":"x","actor":"chatgpt","request_id":"q2","key":"kq2","auth":"s3cret"}`))
	if good["error"] != nil {
		t.Fatalf("opted-in delegate must succeed: %v", good)
	}

	// Send without auth refused; status read without auth allowed.
	badSend := mcpDecode(t, mcpCallDirect(t, "metis_send",
		`{"session":"w","text":"hi","actor":"mallory","request_id":"s1"}`))
	if e, _ := badSend["error"].(map[string]any); e == nil {
		t.Fatalf("unauthenticated send must be refused: %v", badSend)
	}
	status := mcpDecode(t, mcpCallDirect(t, "metis_status", `{}`))
	if status["error"] != nil || status["result"] == nil {
		t.Fatalf("reads must stay open without auth: %v", status)
	}
}

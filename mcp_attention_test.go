package main

import (
	"strings"
	"testing"
)

// Listing surfaces open escalations with their revision guards: a
// resolver must quote the revision it saw, or the settle is refused.
func TestMCPAttentionListOpen(t *testing.T) {
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	id, err := openAttention(req, "run-att", "task-att", "sess", "signoff", "need signoff", `["ev1"]`)
	if err != nil {
		t.Fatalf("openAttention: %v", err)
	}
	res, err := mcpCallTool("metis_attention", mustJSON(t, map[string]any{"run": "run-att"}))
	if err != nil {
		t.Fatalf("metis_attention: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, id) {
		t.Fatalf("open escalation %q missing from listing: %s", id, res.Content[0].Text)
	}
}

// Settling with a stale revision is refused: two operators racing
// to resolve the same escalation cannot both win silently.
func TestMCPAttentionSettleGuardsRevision(t *testing.T) {
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	id, err := openAttention(req, "run-att2", "task-att2", "sess", "replan", "need replan", `[]`)
	if err != nil {
		t.Fatalf("openAttention: %v", err)
	}
	// Wrong revision first: must fail and leave the row open.
	raw := mustJSON(t, map[string]any{
		"id": id, "revision": 99, "action": "resolve",
		"outcome": "proceed", "actor": "op", "request_id": newRequestID(),
	})
	if _, err := mcpCallTool("metis_attention_settle", raw); err == nil {
		t.Fatal("settle with stale revision succeeded, want refusal")
	}
	attns, err := listAttention("run-att2", true)
	if err != nil || len(attns) != 1 || attns[0].Status != "open" {
		t.Fatalf("row changed by refused settle: %+v, err %v", attns, err)
	}
	// Correct revision settles exactly once; replay is refused.
	raw = mustJSON(t, map[string]any{
		"id": id, "revision": 0, "action": "resolve",
		"outcome": "proceed", "actor": "op", "request_id": newRequestID(),
	})
	if _, err := mcpCallTool("metis_attention_settle", raw); err != nil {
		t.Fatalf("settle rev 0: %v", err)
	}
	raw = mustJSON(t, map[string]any{
		"id": id, "revision": 1, "action": "dismiss",
		"outcome": "x", "actor": "op", "request_id": newRequestID(),
	})
	if _, err := mcpCallTool("metis_attention_settle", raw); err == nil {
		t.Fatal("second settle on resolved row succeeded, want refusal")
	}
}

// Unauthenticated settle calls are rejected when a write token is
// configured: attention resolution is operator authority.
func TestMCPAttentionSettleAuth(t *testing.T) {
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-att")
	raw := mustJSON(t, map[string]any{
		"id": "att-x", "revision": 0, "action": "resolve",
		"outcome": "x", "actor": "op", "request_id": newRequestID(),
	})
	if _, err := mcpCallTool("metis_attention_settle", raw); err == nil {
		t.Fatal("unauthenticated settle succeeded, want rejection")
	}
}

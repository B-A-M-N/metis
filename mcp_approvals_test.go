package main

// mcp_approvals_test.go: MCP verbs for native approvals. The
// host-level decision contract (offered-only choices, requirement
// guard, commandId shape, stale-guard surfacing) is proven in
// muse_approvals_test.go against a canned server; here the gates
// are proven at the MCP boundary: token first, binding second,
// daemon third — a refusal at any layer moves nothing.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func mcpApproveCall(t *testing.T, args map[string]any) (mcpToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return mcpCallTool("metis_approve", raw)
}

func TestMCPApproveGated(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-mcp-approve")
	base := map[string]any{
		"session": "w", "provider": "echo", "approval_id": "ap-x",
		"choice": "allow", "actor": "reviewer-01", "request_id": "r1",
		"auth": "wrong",
	}
	// Wrong token: refused before anything else. No daemon, no
	// binding, no row is touched.
	if _, err := mcpApproveCall(t, base); err == nil {
		t.Fatal("wrong auth must be refused")
	}
	// Correct token, unknown session: no binding, refused.
	// (Domain refusals arrive as isError results, not RPC errors.)
	ok := map[string]any{
		"session": "w", "provider": "echo", "approval_id": "ap-k",
		"choice": "allow", "actor": "reviewer-01", "request_id": "r1",
		"auth": "tok-mcp-approve",
	}
	if res, err := mcpApproveCall(t, ok); err != nil || !res.IsError {
		t.Fatalf("unbound session must be refused: %v %v", res, err)
	}
	// Bound session plus a seeded row: validation passes, then
	// the missing daemon fails closed — infra never outruns the
	// row gates, and nothing is sent. The musefi profile points
	// at an empty store, so no ambient daemon can satisfy the
	// gate: the refusal is hermetic, not ambient-dependent.
	// Local operator path from here on (no token): row and daemon
	// gates are local properties. Remote denial is proven in
	// TestMCPRemotePrincipalDenied.
	os.Unsetenv("METIS_MCP_WRITE_TOKEN")
	t.Setenv("MUSEFI_DATA_HOME", t.TempDir())
	seedOwnedSession(t, "w", "appr")
	if err := bindMuseSession("w", "native-9", "c9", "echo", "musefi", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := recordMuseApprovalRequested("w", "native-9", []byte(`{"approvalId":"ap-k",
		"availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)); err != nil {
		t.Fatal(err)
	}
	ok["profile"] = "musefi"
	res, err := mcpApproveCall(t, ok)
	if err == nil && !res.IsError {
		t.Fatal("approve without a daemon must fail closed")
	}
	if err == nil && !strings.Contains(res.Content[0].Text, "daemon") {
		t.Fatalf("fail-closed must name the daemon, got: %s", res.Content[0].Text)
	}
	seeded := map[string]any{
		"session": "w", "provider": "echo", "profile": "musefi", "approval_id": "ap-nope",
		"choice": "allow", "actor": "reviewer-01", "request_id": "r2",
		"auth": "tok-mcp-approve",
	}
	if res, err := mcpApproveCall(t, seeded); err != nil || !res.IsError {
		t.Fatalf("unknown approval must be refused: %v %v", res, err)
	}
	unoffered := map[string]any{
		"session": "w", "provider": "echo", "profile": "musefi", "approval_id": "ap-k",
		"choice": "rm-rf", "actor": "reviewer-01", "request_id": "r3",
		"auth": "tok-mcp-approve",
	}
	if res, err := mcpApproveCall(t, unoffered); err != nil || !res.IsError {
		t.Fatalf("unoffered choice must be refused: %v %v", res, err)
	}
	line := mcpHandle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	for _, name := range []string{"metis_approvals", "metis_approve"} {
		if !strings.Contains(string(line), name) {
			t.Fatalf("tools/list missing %s", name)
		}
	}
}

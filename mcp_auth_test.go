package main

// mcp_auth_test.go: the MCP transport boundary must not let a
// caller-supplied `actor` string establish authorization.
//
// A shared token only proves the transport; it confers no
// identity. Token-authenticated (remote) callers therefore cannot
// record pass verdicts, decide approvals, or — by default — mutate
// at all. Local operator behavior (no token configured) is
// unchanged.

import (
	"encoding/json"
	"strings"
	"testing"
)

func mcpAuthCall(t *testing.T, tool string, args map[string]any) (mcpToolResult, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return mcpCallTool(tool, raw)
}

func seedReportedTask(t *testing.T, run, agent, session string) string {
	t.Helper()
	tk, err := createTask(run, agent, "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, session); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "rep", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	return tk.ID
}

func TestMCPRemotePrincipalDenied(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-remote")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	// A remote caller forging an independent reviewer name must
	// not record a pass verdict.
	passID := seedReportedTask(t, "r", "worker-a", "s")
	res, err := mcpAuthCall(t, "metis_verify", map[string]any{
		"task_id": passID, "pass": true, "actor": "reviewer-x",
		"request_id": "r1", "auth": "tok-remote",
	})
	if err == nil && !res.IsError {
		t.Fatal("remote pass verdict must be denied")
	}
	if err == nil && !strings.Contains(res.Content[0].Text, "remote") {
		t.Fatalf("denial must name the remote principal, got: %s", res.Content[0].Text)
	}
	if got, _ := getTask(passID); got.Status != TaskReportReady {
		t.Fatalf("denied verdict must move nothing, task is %s", got.Status)
	}

	// A remote fail verdict (retraction) is NOT allowed on a bare
	// token: rejection needs the explicit "reject" grant (P0-C).
	// The full bare/grant/blanket matrix is pinned below; here,
	// the denied call must move nothing.
	seedOwnedSession(t, "s2", "o1")
	failID := seedReportedTask(t, "r", "worker-a", "s2")
	if res, err := mcpAuthCall(t, "metis_verify", map[string]any{
		"task_id": failID, "pass": false, "actor": "worker-a",
		"request_id": "r2", "auth": "tok-remote",
	}); err == nil && !res.IsError {
		t.Fatal("bare-token remote rejection must be denied")
	}
	if got, _ := getTask(failID); got.Status != TaskReportReady {
		t.Fatalf("denied rejection must move nothing, got %s", got.Status)
	}

	// Remote mutations are disabled by default, even with a valid
	// token: the token proves transport, not authority.
	if res, err := mcpAuthCall(t, "metis_delegate", map[string]any{
		"run": "r", "session": "s", "objective": "do x",
		"actor": "orch", "request_id": "r3", "auth": "tok-remote",
	}); err == nil && !res.IsError {
		t.Fatal("remote delegate must be denied by default")
	} else if err == nil && !strings.Contains(res.Content[0].Text, "remote") {
		t.Fatalf("denial must name the remote principal, got: %s", res.Content[0].Text)
	}

	// Explicit opt-in restores remote writes — still as the
	// remote-token principal, never as the asserted actor. A
	// third session keeps workspace occupancy out of the way.
	seedOwnedSession(t, "s3", "o1")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	if res, err := mcpAuthCall(t, "metis_delegate", map[string]any{
		"run": "r", "session": "s3", "objective": "do x",
		"actor": "orch", "request_id": "r3", "auth": "tok-remote",
	}); err != nil || res.IsError {
		t.Fatalf("opted-in remote delegate must proceed: %v %v", res, err)
	}

	// A bare token proves transport, not rejection authority: a
	// remote pass=false without the explicit "reject" grant must be
	// denied and move nothing (P0-C). With the grant, retraction
	// proceeds; the local operator path stays ungated. Start from
	// no grants: the delegate arm above enabled the blanket opt-in.
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "")
	seedOwnedSession(t, "s9a", "o1")
	bareID := seedReportedTask(t, "r", "worker-a", "s9a")
	res, err = mcpAuthCall(t, "metis_verify", map[string]any{
		"task_id": bareID, "pass": false, "actor": "worker-a",
		"request_id": "r9a", "auth": "tok-remote",
	})
	if err == nil && !res.IsError {
		t.Fatal("bare-token remote rejection must be denied")
	}
	if got, _ := getTask(bareID); got.Status != TaskReportReady {
		t.Fatalf("denied rejection must move nothing, task is %s", got.Status)
	}

	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "reject")
	seedOwnedSession(t, "s9b", "o1")
	grantedID := seedReportedTask(t, "r", "worker-a", "s9b")
	if res, err := mcpAuthCall(t, "metis_verify", map[string]any{
		"task_id": grantedID, "pass": false, "actor": "worker-a",
		"request_id": "r9b", "auth": "tok-remote",
	}); err != nil || res.IsError {
		t.Fatalf("granted remote rejection must proceed: %v %v", res, err)
	}
	if got, _ := getTask(grantedID); got.Status != TaskFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	// The legacy blanket write opt-in does not confer rejection
	// authority: only the explicit grant does.
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	seedOwnedSession(t, "s9c", "o1")
	blanketID := seedReportedTask(t, "r", "worker-a", "s9c")
	if res, err := mcpAuthCall(t, "metis_verify", map[string]any{
		"task_id": blanketID, "pass": false, "actor": "worker-a",
		"request_id": "r9c", "auth": "tok-remote",
	}); err == nil && !res.IsError {
		t.Fatal("blanket-opt-in remote rejection must be denied without the reject grant")
	}
	if got, _ := getTask(blanketID); got.Status != TaskReportReady {
		t.Fatalf("denied rejection must move nothing, task is %s", got.Status)
	}

	// Remote approval decisions are never available to the token
	// principal, opt-in or not.
	t.Setenv("MUSEFI_DATA_HOME", t.TempDir())
	seedOwnedSession(t, "w", "auth")
	if err := bindMuseSession("w", "native-9", "c9", "echo", "musefi", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := recordMuseApprovalRequested("w", "native-9", []byte(`{"approvalId":"ap-k",
		"availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err = mcpAuthCall(t, "metis_approve", map[string]any{
		"session": "w", "provider": "echo", "profile": "musefi",
		"approval_id": "ap-k", "choice": "allow", "actor": "reviewer-01",
		"request_id": "r4", "auth": "tok-remote",
	})
	if err == nil && !res.IsError {
		t.Fatal("remote approve must be denied")
	}
	if err == nil && !strings.Contains(res.Content[0].Text, "remote") {
		t.Fatalf("denial must name the remote principal, got: %s", res.Content[0].Text)
	}
}

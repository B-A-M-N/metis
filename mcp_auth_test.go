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
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
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
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("METIS_STATE", t.TempDir())
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

// TestUnauthorizedMetisWriteRefused pins the Ocliv-facing gate for
// metis_muse_message: a remote caller with a valid transport token
// but no grant cannot message any session, and the refused call
// persists no intent. The broader tasks grant does not authorize
// messaging; only the message-only scoped grant passes
// authorization and reaches the domain (which refuses the unbound
// session on its own terms — refused state, not an auth error),
// proving the tool is remotely reachable when granted.
func TestUnauthorizedMetisWriteRefused(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-msg")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "")
	call := func(extra map[string]any) (mcpToolResult, error) {
		args := map[string]any{
			"session": "qa", "text": "hello", "actor": "remote",
			"request_id": "msg-r1", "auth": "tok-msg",
		}
		for k, v := range extra {
			args[k] = v
		}
		return mcpAuthCall(t, "metis_muse_message", args)
	}
	if res, err := call(nil); err == nil && !res.IsError {
		t.Fatal("ungranted remote muse message must be refused")
	} else if err == nil && !strings.Contains(res.Content[0].Text, "remote") {
		t.Fatalf("denial must name the remote principal, got: %s", res.Content[0].Text)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM muse_message_intents`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refused remote call must persist no intent, got %d rows", n)
	}
	// The broader tasks grant confers no messaging authority: the
	// message-only capability is narrower by design.
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "tasks")
	if res, err := call(nil); err == nil && !res.IsError {
		t.Fatal("tasks grant must not authorize muse messaging")
	} else if err == nil && !strings.Contains(res.Content[0].Text, "message") {
		t.Fatalf("capability denial must name the message capability, got: %s", res.Content[0].Text)
	}
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	defer os.Unsetenv("METIS_MCP_REMOTE_CAPABILITIES")
	res, err := call(nil)
	if err != nil {
		t.Fatalf("granted remote call must reach the domain, got RPC error: %v", err)
	}
	if res.IsError {
		t.Fatalf("granted remote call must reach the domain, got tool error: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "refused") {
		t.Fatalf("unbound session must refuse at the domain, got: %s", res.Content[0].Text)
	}
}

// TestMuseMessageStatusToolAuth pins the status tool's transport
// gate: ungranted remote callers are refused before any read, the
// message grant reaches the domain, and an unknown request ID
// surfaces a domain error instead of an invented state.
func TestMuseMessageStatusToolAuth(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-msgst")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "")
	call := func(extra map[string]any) (mcpToolResult, error) {
		args := map[string]any{
			"request_id": "no-such-req", "auth": "tok-msgst",
		}
		for k, v := range extra {
			args[k] = v
		}
		return mcpAuthCall(t, "metis_muse_message_status", args)
	}
	if res, err := call(nil); err == nil && !res.IsError {
		t.Fatal("ungranted remote status poll must be refused")
	} else if err == nil && !strings.Contains(res.Content[0].Text, "remote") {
		t.Fatalf("status denial must name the remote principal, got: %s", res.Content[0].Text)
	}
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "tasks")
	if res, err := call(nil); err == nil && !res.IsError {
		t.Fatal("tasks grant must not authorize message status")
	}
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	defer os.Unsetenv("METIS_MCP_REMOTE_CAPABILITIES")
	res, err := call(nil)
	if err != nil {
		t.Fatalf("granted remote status must reach the domain, got RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("unknown request ID must surface a domain error, not success")
	}
	if !strings.Contains(res.Content[0].Text, "no such muse message request") {
		t.Fatalf("unknown request must name itself, got: %s", res.Content[0].Text)
	}
}

// TestRemoteMessageScopeIsolation is the P0-G regression: a remote
// caller holding ONLY the message capability reaches its named
// target and nothing else. The validated transport token becomes
// the fixed remote principal server-side; the caller-supplied
// actor string (even "metis-operator") confers no authority.
func TestRemoteMessageScopeIsolation(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // no daemon socket: deterministic domain refusal
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
	bindEcho("gA")
	hB := bindEcho("gB")

	// gB completes through the stub host: its terminal is durable.
	bB, err := getMuseBinding("gB")
	if err != nil {
		t.Fatal(err)
	}
	bmsg := MuseMessageRequest{SessionID: "gB", Text: "owned", RequestID: "gB-m1", Actor: "orch"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done, err := sendMuseMessageOnHost(ctx, hB, bmsg, bB, mustClaimMessage(t, bmsg, bB), "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if done.State != "completed" {
		t.Fatalf("stub submit must complete, got %+v", done)
	}

	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-scope")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "gA")
	remote := func(tool string, args map[string]any) (mcpToolResult, error) {
		args["auth"] = "tok-scope"
		args["actor"] = "metis-operator" // must confer nothing
		return mcpAuthCall(t, tool, args)
	}
	// Allowed target: passes auth AND the target gate, refuses
	// only at the daemon check — proof the chain reached the
	// domain. The actor masquerade bought nothing extra.
	if res, err := remote("metis_muse_message", map[string]any{
		"session": "gA", "text": "hi", "request_id": "gA-r1"}); err != nil {
		t.Fatalf("allowed remote message must reach the domain, got RPC error: %v", err)
	} else if res.IsError {
		t.Fatalf("allowed remote message must reach the domain, got tool error: %s", res.Content[0].Text)
	} else if text := res.Content[0].Text; !strings.Contains(text, "daemon") {
		t.Fatalf("allowed target must pass the target gate to the daemon check, got: %s", text)
	}
	// Anything else is refused: another session, its history, the
	// task plane, approvals, and worker stops.
	if res, err := remote("metis_muse_message", map[string]any{
		"session": "gB", "text": "hi", "request_id": "gB-r9"}); err != nil || !res.IsError && !strings.Contains(res.Content[0].Text, "not an authorized") {
		t.Fatalf("unlisted session must be refused at the target gate, got err=%v res=%+v", err, res)
	}
	if res, err := remote("metis_muse_message_status", map[string]any{
		"request_id": "gB-m1"}); err != nil {
		t.Fatalf("status call must answer, got RPC error: %v", err)
	} else if text := res.Content[0].Text; strings.Contains(text, `"completed"`) || strings.Contains(text, done.TurnID) {
		t.Fatalf("unauthorized status must not leak the cached terminal, got: %s", text)
	} else if !strings.Contains(text, "refused") {
		t.Fatalf("unauthorized status must be refused, got: %s", text)
	}
	if _, err := remote("metis_provision", map[string]any{
		"run": "rx", "session": "evil", "objective": "o", "request_id": "evil-1",
		"backend": "muse-native", "workdir": t.TempDir(), "provider": "echo"}); err == nil {
		t.Fatal("message-only remote caller must not provision agents")
	}
	if _, err := remote("metis_delegate", map[string]any{
		"run": "rx", "session": "gA", "objective": "o", "request_id": "evil-2"}); err == nil {
		t.Fatal("message-only remote caller must not touch the task plane")
	}
	if res, err := remote("metis_approve", map[string]any{
		"session": "gA", "provider": "echo", "approval_id": "ap1", "choice": "approve",
		"request_id": "evil-3"}); err != nil || !res.IsError {
		t.Fatalf("remote caller must not approve even as actor metis-operator, got err=%v res=%+v", err, res)
	} else if !strings.Contains(res.Content[0].Text, "local operator") {
		t.Fatalf("approve denial must name the local-operator boundary, got: %s", res.Content[0].Text)
	}
	if res, err := remote("metis_session_stop", map[string]any{
		"session": "gA", "request_id": "evil-4"}); err != nil || !res.IsError {
		t.Fatalf("message-only remote caller must not stop workers, got err=%v res=%+v", err, res)
	} else if !strings.Contains(res.Content[0].Text, "admin") {
		t.Fatalf("stop denial must name the admin capability, got: %s", res.Content[0].Text)
	}
	// No explicit targets: even the allowed session refuses for
	// remote callers. Local-operator default-allow is untouched —
	// covered by the local (tokenless) message suites.
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "")
	if res, err := remote("metis_muse_message", map[string]any{
		"session": "gA", "text": "hi", "request_id": "gA-r2"}); err != nil || !res.IsError && !strings.Contains(res.Content[0].Text, "explicit METIS_MUSE_MESSAGE_TARGETS") {
		t.Fatalf("remote default-allow must be refused, got err=%v res=%+v", err, res)
	}
}

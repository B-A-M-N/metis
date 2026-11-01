package main

// mcp_ingress_test.go: F-02 fail-closed ingress at the MCP boundary.
//
// Local-operator authority must never depend on an optional ingress
// flag: on a hosted transport (bridged stdio, e.g. Ocliv `mcp
// expose`), an undeclared mode or a missing credential fails closed
// instead of silently promoting a remote caller to local-operator.
// Explicit LocalTrusted vs RemoteAuthenticated modes, distinct
// scoped capabilities (tasks / approve / verify / admin), and the
// operator-only doctor diagnostic pin the contract here.

import (
	"os"
	"strings"
	"testing"
)

// unsetMCPEnv hermetically clears the ingress-related env for one
// test, restoring whatever the ambient process carried.
func unsetMCPEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		k := k
		if v, ok := os.LookupEnv(k); ok {
			t.Cleanup(func() { _ = os.Setenv(k, v) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(k) })
		}
		_ = os.Unsetenv(k)
	}
}

func mcpIngressEnv(t *testing.T) {
	t.Helper()
	unsetMCPEnv(t,
		"METIS_MCP_INGRESS",
		"METIS_MCP_REMOTE_INGRESS",
		"METIS_MCP_HOSTED",
		"METIS_MCP_WRITE_TOKEN",
		"METIS_MCP_ALLOW_REMOTE_WRITE",
		"METIS_MCP_REMOTE_CAPABILITIES",
	)
}

// F-02 core: hosted transport, no token, no ingress declaration must
// NOT yield local-operator authority. A delegated task must not be
// created; the refusal must name the ingress trust problem.
func TestMCPIngressHostedUndeclaredFailsClosed(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_HOSTED", "1")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	_, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "hosted sneak",
		"actor": "mallory", "request_id": "h1", "key": "hk1",
	}))
	if err == nil {
		t.Fatal("hosted transport without ingress declaration must refuse mutation, not inherit local-operator trust")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "hosted") || !strings.Contains(msg, "local-operator") {
		t.Fatalf("refusal must name the hosted ingress trust problem, got: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("refused delegate created %d tasks", n)
	}
}

// Explicit remote mode without a credential fails closed too.
func TestMCPIngressExplicitRemoteRequiresToken(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_INGRESS", "remote")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	_, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "no creds",
		"actor": "mallory", "request_id": "h2", "key": "hk2",
	}))
	if err == nil {
		t.Fatal("remote ingress without a token must refuse mutation")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("refused delegate created %d tasks", n)
	}
}

// A hosted launch that declares remote ingress but ships no
// credential still fails closed: declaration alone is not trust.
func TestMCPIngressHostedDeclaredModeStillNeedsCredential(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_HOSTED", "1")
	t.Setenv("METIS_MCP_INGRESS", "remote")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	_, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "declare only",
		"actor": "mallory", "request_id": "h3", "key": "hk3",
	}))
	if err == nil {
		t.Fatal("hosted remote ingress without a token must refuse mutation")
	}
}

// Wrong token is refused before anything is touched.
func TestMCPIngressWrongTokenRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	_, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "x",
		"actor": "mallory", "request_id": "w1", "key": "wk1", "auth": "wrong",
	}))
	if err == nil {
		t.Fatal("wrong auth must be refused")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("refused delegate created %d tasks", n)
	}
}

// Reads stay open: no token, no auth, status and agents answer.
func TestMCPIngressReadsStayOpen(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	if res, err := mcpCallTool("metis_status", mustJSON(t, map[string]any{})); err != nil || res.IsError {
		t.Fatalf("status must stay open without auth: %v %v", res, err)
	}
	if res, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{})); err != nil || res.IsError {
		t.Fatalf("agents must stay open without auth: %v %v", res, err)
	}
}

// An authorized writer (valid token + granted task capability)
// proceeds as the remote-token principal, never as the asserted
// actor. Both the legacy opt-in and the scoped capability grant
// the task-management capability.
func TestMCPIngressAuthorizedWriter(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	res, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "do x",
		"actor": "orch", "request_id": "w2", "key": "wk2", "auth": "tok-ingress",
	}))
	if err != nil || res.IsError {
		t.Fatalf("opted-in remote delegate must proceed: %v %v", res, err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Fatalf("authorized delegate must create exactly one task, got %d", n)
	}
}

// The scoped capability grant alone (no legacy blanket opt-in)
// authorizes task management for the remote principal.
func TestMCPIngressScopedTaskCapability(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "tasks")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	res, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "scoped",
		"actor": "orch", "request_id": "w3", "key": "wk3", "auth": "tok-ingress",
	}))
	if err != nil || res.IsError {
		t.Fatalf("scoped-capability remote delegate must proceed: %v %v", res, err)
	}
}

// A task capability is not an approval capability: even fully
// opted-in, a remote caller cannot decide a native approval.
func TestMCPIngressRemoteApproveDeniedDespiteCaps(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "tasks,admin")
	t.Setenv("MUSEFI_DATA_HOME", t.TempDir())
	seedOwnedSession(t, "w", "auth")
	if err := bindMuseSession("w", "native-9", "c9", "echo", "musefi", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := recordMuseApprovalRequested("w", "native-9", []byte(`{"approvalId":"ap-k",
		"availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)); err != nil {
		t.Fatal(err)
	}
	res, err := mcpCallTool("metis_approve", mustJSON(t, map[string]any{
		"session": "w", "provider": "echo", "profile": "musefi",
		"approval_id": "ap-k", "choice": "allow", "actor": "reviewer-01",
		"request_id": "r4", "auth": "tok-ingress",
	}))
	if err == nil && !res.IsError {
		t.Fatal("remote approve must be denied despite capability grants")
	}
	var text string
	if err != nil {
		text = err.Error()
	} else {
		text = res.Content[0].Text
	}
	if !strings.Contains(strings.ToLower(text), "local operator") {
		t.Fatalf("approve denial must name local-operator authority, got: %s", text)
	}
}

// An unauthorized verifier cannot record a pass verdict at the
// boundary; the task does not move. Retraction (pass=false)
// requires the explicit "reject" grant (P0-C): the blanket write
// opt-in does not confer rejection authority.
func TestMCPIngressUnauthorizedVerifier(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "reject")
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	passID := seedReportedTask(t, "r", "worker-a", "s")
	res, err := mcpCallTool("metis_verify", mustJSON(t, map[string]any{
		"task_id": passID, "pass": true, "actor": "reviewer-x",
		"request_id": "v1", "auth": "tok-ingress",
	}))
	if err == nil && !res.IsError {
		t.Fatal("remote pass verdict must be denied")
	}
	if got, _ := getTask(passID); got.Status != TaskReportReady {
		t.Fatalf("denied verdict moved task to %s", got.Status)
	}

	seedOwnedSession(t, "s2", "o1")
	failID := seedReportedTask(t, "r", "worker-a", "s2")
	if res, err := mcpCallTool("metis_verify", mustJSON(t, map[string]any{
		"task_id": failID, "pass": false, "actor": "worker-a",
		"request_id": "v2", "auth": "tok-ingress",
	})); err != nil || res.IsError {
		t.Fatalf("remote fail verdict must stay allowed: %v %v", res, err)
	}
	if got, _ := getTask(failID); got.Status != TaskFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
}

// Backward compatibility pin: unspecified mode on a non-hosted
// transport with no token keeps local-stdio trust.
func TestMCPIngressImplicitLocalTrustPreserved(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	installFakeTmux(t, "o1")
	seedOwnedSession(t, "s", "o1")

	res, err := mcpCallTool("metis_delegate", mustJSON(t, map[string]any{
		"run": "r", "session": "s", "objective": "local work",
		"actor": "op", "request_id": "l1", "key": "lk1",
	}))
	if err != nil || res.IsError {
		t.Fatalf("implicit local trust must still delegate: %v %v", res, err)
	}
}

// Startup security diagnostic: a hosted-but-undeclared posture is
// BLOCKED with an evidence source; a declared remote+credential
// posture is OK and names the resolved mode. Doctor output is the
// operator-only surface for the resolved ingress mode.
func TestDoctorMCPIngressFailClosed(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_HOSTED", "1")

	rep, err := doctorReport()
	if err != nil {
		t.Fatal(err)
	}
	var found *DoctorCondition
	for i, c := range rep.Conditions {
		if c.Name == "mcp-ingress" {
			found = &rep.Conditions[i]
		}
		if c.Source == "" {
			t.Fatalf("condition %q must name its evidence source", c.Name)
		}
	}
	if found == nil {
		t.Fatal("doctor must report an mcp-ingress condition")
	}
	if found.Status != ConditionBlocked {
		t.Fatalf("hosted-but-undeclared ingress must be BLOCKED, got %s: %s", found.Status, found.Detail)
	}
	if !strings.Contains(strings.ToLower(found.Detail), "hosted") {
		t.Fatalf("diagnostic must name the hosted transport, got: %s", found.Detail)
	}
}

func TestDoctorMCPIngressRemoteOK(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	mcpIngressEnv(t)
	t.Setenv("METIS_MCP_INGRESS", "remote")
	t.Setenv("METIS_MCP_HOSTED", "1")
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-ingress")

	rep, err := doctorReport()
	if err != nil {
		t.Fatal(err)
	}
	var found *DoctorCondition
	for i, c := range rep.Conditions {
		if c.Name == "mcp-ingress" {
			found = &rep.Conditions[i]
		}
	}
	if found == nil {
		t.Fatal("doctor must report an mcp-ingress condition")
	}
	if found.Status != ConditionOK {
		t.Fatalf("declared remote ingress with credential must be OK, got %s: %s", found.Status, found.Detail)
	}
	detail := strings.ToLower(found.Detail)
	if !strings.Contains(detail, "remote") || !strings.Contains(detail, "hosted") {
		t.Fatalf("diagnostic must name the resolved mode and transport, got: %s", found.Detail)
	}
	if strings.Contains(found.Detail, "tok-ingress") {
		t.Fatalf("diagnostic must never echo the credential, got: %s", found.Detail)
	}
}

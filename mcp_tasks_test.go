package main

// mcp_tasks_test.go: the Muse.ai-first read surface (tasks list,
// single-task status, projects) plus the approval-gated verify verb.
//
// Reads are side-effect free and deterministic; verify is a real
// state change behind the write token, with TD-094 independence
// enforced inside verifyTask — a caller cannot self-approve by
// naming two identities.

import (
	"encoding/json"
	"strings"
	"testing"
)

func mcpTaskCall(t *testing.T, name string, args map[string]any) mcpToolResult {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mcpCallTool(name, raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s returned isError: %s", name, res.Content[0].Text)
	}
	return res
}

func mcpTaskText(t *testing.T, res mcpToolResult) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("tool result is not JSON: %v", err)
	}
	return out
}

func TestMCPTasksList(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "mtasks")
	seedOwnedSession(t, "w", "mtasks")
	for _, obj := range []string{"alpha one", "alpha two"} {
		if _, err := createTask("run-a", "w", obj, "done"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createTask("run-b", "w", "beta one", "done"); err != nil {
		t.Fatal(err)
	}
	// Unfiltered: all three, oldest first (created order).
	out := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{}))
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(tasks))
	}
	if tasks[0].(map[string]any)["objective"] != "alpha one" {
		t.Fatalf("tasks not in created order: %v", tasks[0])
	}
	// Run filter + pagination: first page of one, then the rest.
	p1 := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"run": "run-a", "limit": 1}))
	if p1["total"] != float64(2) || p1["truncated"] != true {
		t.Fatalf("bad page one: %v", p1)
	}
	first := p1["tasks"].([]any)[0].(map[string]any)["id"].(string)
	p2 := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"run": "run-a", "after": first}))
	rest := p2["tasks"].([]any)
	if len(rest) != 1 || rest[0].(map[string]any)["id"] == first {
		t.Fatalf("cursor page must return exactly the remainder: %v", p2)
	}
	if _, ok := rest[0].(map[string]any)["status"]; !ok {
		t.Fatalf("task row must carry status: %v", rest[0])
	}
	// Unknown cursor: empty page, never a fabricated position.
	unk := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"after": "no-such-id"}))
	if len(unk["tasks"].([]any)) != 0 || unk["truncated"] != false {
		t.Fatalf("unknown cursor must yield an empty page: %v", unk)
	}
}

func TestMCPTaskStatus(t *testing.T) {
	run, ids := seedGraphRun(t)
	// Blocked task: names its gate, never invents readiness.
	out := mcpTaskText(t, mcpTaskCall(t, "metis_task_status", map[string]any{"task_id": ids["c"]}))
	if out["status"] != string(TaskPending) {
		t.Fatalf("c must be pending: %v", out)
	}
	waiting, _ := out["waiting_on"].([]any)
	if len(waiting) != 1 || !strings.Contains(waiting[0].(string), ids["b"]) {
		t.Fatalf("c must wait on b: %v", out)
	}
	if out["verified"].(bool) {
		t.Fatal("pending task must not read verified")
	}
	// Verified task: carries its criterion verdicts.
	a := mcpTaskText(t, mcpTaskCall(t, "metis_task_status", map[string]any{"task_id": ids["a"]}))
	if a["status"] != string(TaskVerified) {
		t.Fatalf("a must be verified after seed: %v", a)
	}
	_ = run
}

func TestMCPProjects(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "mproj")
	seedOwnedSession(t, "w", "mproj")
	other := t.TempDir()
	reg := loadRegistry()
	reg["o"] = Session{Name: "o", Tmux: "tui-o", Workdir: other, Command: "sleep 60", Created: "now"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := createTask("run-p", "w", "do work", "done"); err != nil {
		t.Fatal(err)
	}
	out := mcpTaskText(t, mcpTaskCall(t, "metis_projects", map[string]any{}))
	projs, _ := out["projects"].([]any)
	if len(projs) != 2 {
		t.Fatalf("two distinct workdirs expected: %v", out)
	}
	first := projs[0].(map[string]any)["workdir"].(string)
	second := projs[1].(map[string]any)["workdir"].(string)
	if first >= second {
		t.Fatalf("projects must sort by workdir: %v", out)
	}
	// Inspect: sessions plus the tasks filed through them.
	wdir := loadRegistry()["w"].Workdir
	insp := mcpTaskText(t, mcpTaskCall(t, "metis_projects", map[string]any{"workdir": wdir}))
	sess, _ := insp["sessions"].([]any)
	if len(sess) != 1 || sess[0] != "w" {
		t.Fatalf("inspect must name the session: %v", insp)
	}
	tasks, _ := insp["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("inspect must carry the session's tasks: %v", insp)
	}
}

// The verify verb is a real mutation: token-gated, explicit pass,
// TD-094 independence enforced — naming a reviewer never confers
// approval by itself.
func TestMCPVerifyGated(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-mcp-verify")
	// Rejection is an authoritative transition: the remote-token
	// principal needs the explicit "reject" grant (P0-C).
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "reject")
	installFakeTmux(t, "mverify")
	seedOwnedSession(t, "w", "mverify")
	id, p := setupReportedTask(t, "w", "build widget", "widget exists\nwidget passes checks")
	base := map[string]any{"task_id": id, "actor": "reviewer-01", "request_id": "r1", "auth": "tok-mcp-verify"}
	// Wrong token: refused before anything is read or written.
	badAuth := map[string]any{"task_id": id, "pass": false, "actor": "reviewer-01", "request_id": "r1", "auth": "wrong"}
	if _, err := mcpCallTool("metis_verify", mustJSON(t, badAuth)); err == nil {
		t.Fatal("wrong auth must be refused")
	}
	// pass omitted is not pass=false: rejection must be explicit.
	noPass := map[string]any{"task_id": id, "actor": "reviewer-01", "request_id": "r1", "auth": "tok-mcp-verify"}
	if _, err := mcpCallTool("metis_verify", mustJSON(t, noPass)); err == nil {
		t.Fatal("missing pass must be refused, never coerced to rejection")
	}
	if got, _ := getTask(id); got.Status != TaskReportReady {
		t.Fatalf("refused calls must not move the task, got %s", got.Status)
	}
	// Explicit rejection lands failed with a receipt.
	rej := map[string]any{"task_id": id, "pass": false, "actor": "reviewer-01", "request_id": "r2", "auth": "tok-mcp-verify"}
	out := mcpTaskText(t, mcpTaskCall(t, "metis_verify", rej))
	if out["status"] != string(TaskFailed) {
		t.Fatalf("explicit reject must fail the task: %v", out)
	}
	_ = base
	_ = p
}

func TestMCPVerifyPassNeedsVerdicts(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	// Local operator path (no token): this test proves verdict
	// logic, not transport auth. Remote pass verdicts are denied
	// outright; see TestMCPRemotePrincipalDenied.
	installFakeTmux(t, "mverify2")
	seedOwnedSession(t, "w2", "mverify2")
	id, p := setupReportedTask(t, "w2", "build widget", "widget exists\nwidget passes checks")
	call := func(pass any, approve, actor, reqID string) (mcpToolResult, error) {
		return mcpCallTool("metis_verify", mustJSON(t, map[string]any{
			"task_id": id, "pass": pass, "approve": approve,
			"actor": actor, "request_id": reqID, "auth": "tok-mcp-verify2",
		}))
	}
	// No verdicts: pass is refused, task stays report_ready.
	// (Domain refusals arrive as isError results, not RPC errors.)
	if res, err := call(true, "", "reviewer-01", "r1"); err != nil || !res.IsError {
		t.Fatalf("pass without criterion verdicts must be refused: %v %v", res, err)
	}
	if got, _ := getTask(id); got.Status != TaskReportReady {
		t.Fatalf("refused verify must not move the task, got %s", got.Status)
	}
	// Worker self-approval is refused even with verdicts filed.
	fileAllPassVerdicts(t, id, p, "reviewer-01")
	if res, err := call(true, "w2", "w2", "r2"); err != nil || !res.IsError {
		t.Fatalf("worker self-approval must be refused: %v %v", res, err)
	}
	// Independent reviewer with filed verdicts: verified.
	res, err := call(true, "", "reviewer-01", "r3")
	if err != nil {
		t.Fatalf("independent pass must verify: %v", err)
	}
	if mcpTaskText(t, res)["status"] != string(TaskVerified) {
		t.Fatalf("expected verified: %s", res.Content[0].Text)
	}
}

func TestMCPNewToolsAdvertised(t *testing.T) {
	line := mcpHandle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	for _, name := range []string{"metis_tasks", "metis_task_status", "metis_projects", "metis_verify"} {
		if !strings.Contains(string(line), name) {
			t.Fatalf("tools/list missing %s", name)
		}
	}
}

func mustJSON(t *testing.T, v map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

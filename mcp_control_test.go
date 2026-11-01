package main

// mcp_control_test.go: the three MCP control tools.
//
// metis_depend wraps addTaskDep (refuses self-deps and unknown IDs
// as isError, idempotent by request_id). metis_cancel wraps
// cancelTask (real path; refusing/terminal tasks are isError, never
// silent success). metis_correct wraps followupTask (lineage-linked
// correction, idempotent by request_id).

import (
	"testing"
)

func TestMCPDependRoundTrip(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	first, err := createTask("run-d", "w", "first thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createTask("run-d", "w", "second thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	depArgs := func(req string) map[string]any {
		return map[string]any{
			"task_id": second.ID, "dep_id": first.ID,
			"actor": "orch", "request_id": req,
		}
	}
	res, err := mcpCallTool("metis_depend", mustJSON(t, depArgs("d1")))
	if err != nil {
		t.Fatalf("depend: %v", err)
	}
	if res.IsError {
		t.Fatalf("depend isError: %s", res.Content[0].Text)
	}
	if n := countDepEdges(t, second.ID, first.ID); n != 1 {
		t.Fatalf("expected exactly one dep edge, got %d", n)
	}
	// Idempotent replay: same request_id returns the original
	// without duplicating the edge.
	res2, err := mcpCallTool("metis_depend", mustJSON(t, depArgs("d1")))
	if err != nil {
		t.Fatalf("depend replay: %v", err)
	}
	if res2.IsError {
		t.Fatalf("depend replay isError: %s", res2.Content[0].Text)
	}
	if n := countDepEdges(t, second.ID, first.ID); n != 1 {
		t.Fatalf("replay must not duplicate the edge, got %d rows", n)
	}
	// Self-dependency is refused as isError with no row written.
	self, err := mcpCallTool("metis_depend", mustJSON(t, map[string]any{
		"task_id": second.ID, "dep_id": second.ID,
		"actor": "orch", "request_id": "d-self",
	}))
	if err != nil {
		t.Fatalf("self-dep refusal must arrive as isError, not RPC error: %v", err)
	}
	if !self.IsError {
		t.Fatal("self-dependency must be refused")
	}
	if n := countDepEdges(t, second.ID, second.ID); n != 0 {
		t.Fatal("refused self-dep must write no row")
	}
	// Unknown task IDs are refusals, never silent success.
	unknown, err := mcpCallTool("metis_depend", mustJSON(t, map[string]any{
		"task_id": "no-such-task", "dep_id": first.ID,
		"actor": "orch", "request_id": "d-unknown",
	}))
	if err != nil {
		t.Fatalf("unknown-dep refusal must arrive as isError, not RPC error: %v", err)
	}
	if !unknown.IsError {
		t.Fatal("depend on unknown task must be refused")
	}
}

func countDepEdges(t *testing.T, taskID, depID string) int {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_deps WHERE task_id = ? AND depends_on = ?`, taskID, depID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMCPCancelRoundTrip(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	made, err := createTask("run-c", "w", "live work", "done")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mcpCallTool("metis_cancel", mustJSON(t, map[string]any{
		"task_id": made.ID, "actor": "orch", "request_id": "c1",
	}))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if res.IsError {
		t.Fatalf("cancel isError: %s", res.Content[0].Text)
	}
	cur, err := getTask(made.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != TaskCancelled {
		t.Fatalf("cancel must go through the real path (status=cancelled), got %q", cur.Status)
	}
	// Cancelling a terminal task is a refusal, never silent success.
	again, err := mcpCallTool("metis_cancel", mustJSON(t, map[string]any{
		"task_id": made.ID, "actor": "orch", "request_id": "c2",
	}))
	if err != nil {
		t.Fatalf("terminal-cancel refusal must arrive as isError, not RPC error: %v", err)
	}
	if !again.IsError {
		t.Fatal("cancel of a terminal task must be refused")
	}
	// Cancelling an unknown task is a refusal.
	unknown, err := mcpCallTool("metis_cancel", mustJSON(t, map[string]any{
		"task_id": "no-such-task", "actor": "orch", "request_id": "c3",
	}))
	if err != nil {
		t.Fatalf("unknown-cancel refusal must arrive as isError, not RPC error: %v", err)
	}
	if !unknown.IsError {
		t.Fatal("cancel of an unknown task must be refused")
	}
}

func TestMCPCorrectLineage(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	src, err := createTask("run-f", "w", "original work", "done")
	if err != nil {
		t.Fatal(err)
	}
	res, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix the report",
		"actor": "orch", "request_id": "f1",
	}))
	if err != nil {
		t.Fatalf("correct: %v", err)
	}
	if res.IsError {
		t.Fatalf("correct isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	fixedID, _ := out["task_id"].(string)
	if fixedID == "" || fixedID == src.ID {
		t.Fatalf("correct must create a new linked task, got %v", out)
	}
	fixed, err := getTask(fixedID)
	if err != nil {
		t.Fatal(err)
	}
	if fixed.Parent != src.ID {
		t.Fatalf("correction must link lineage to the source (parent=%q want %q)", fixed.Parent, src.ID)
	}
	if fixed.Run != src.Run {
		t.Fatalf("correction must stay in the source run (%q), got %q", src.Run, fixed.Run)
	}
	// Replay with the same request_id returns the same task.
	res2, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix the report",
		"actor": "orch", "request_id": "f1",
	}))
	if err != nil {
		t.Fatalf("correct replay: %v", err)
	}
	if res2.IsError {
		t.Fatalf("correct replay isError: %s", res2.Content[0].Text)
	}
	if out2 := mcpTaskText(t, res2); out2["task_id"] != fixedID {
		t.Fatalf("replay must return the same task %q, got %v", fixedID, out2)
	}
	if tasks, err := listTasks("run-f"); err != nil {
		t.Fatal(err)
	} else if len(tasks) != 2 {
		t.Fatalf("replay must not create a second correction, got %d tasks", len(tasks))
	}
	// Correcting an unknown task is a refusal.
	unknown, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": "no-such-task", "objective": "fix nothing",
		"actor": "orch", "request_id": "f2",
	}))
	if err != nil {
		t.Fatalf("unknown-correct refusal must arrive as isError, not RPC error: %v", err)
	}
	if !unknown.IsError {
		t.Fatal("correct of an unknown task must be refused")
	}
}

func TestMCPDependAuth(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	a, err := createTask("run-a", "w", "a work", "done")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-a", "w", "b work", "done")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-control")
	calls := []struct {
		name string
		args map[string]any
	}{
		{"metis_depend", map[string]any{"task_id": b.ID, "dep_id": a.ID, "actor": "orch", "request_id": "x1"}},
		{"metis_cancel", map[string]any{"task_id": a.ID, "actor": "orch", "request_id": "x2"}},
		{"metis_correct", map[string]any{"task_id": a.ID, "objective": "fix it", "actor": "orch", "request_id": "x3"}},
	}
	for _, c := range calls {
		if _, err := mcpCallTool(c.name, mustJSON(t, c.args)); err == nil {
			t.Fatalf("%s without auth must be rejected", c.name)
		}
		bad := map[string]any{"auth": "wrong"}
		for k, v := range c.args {
			bad[k] = v
		}
		if _, err := mcpCallTool(c.name, mustJSON(t, bad)); err == nil {
			t.Fatalf("%s with wrong auth must be rejected", c.name)
		}
	}
	// Authenticated calls succeed once the operator opts into
	// remote writes (repo convention: token callers are remote
	// principals gated by METIS_MCP_ALLOW_REMOTE_WRITE).
	t.Setenv("METIS_MCP_ALLOW_REMOTE_WRITE", "1")
	good := func(args map[string]any) map[string]any {
		out := map[string]any{"auth": "tok-control"}
		for k, v := range args {
			out[k] = v
		}
		return out
	}
	if res, err := mcpCallTool("metis_depend", mustJSON(t, good(calls[0].args))); err != nil || res.IsError {
		t.Fatalf("authenticated depend must succeed (err=%v isError=%v)", err, res.IsError)
	}
	if n := countDepEdges(t, b.ID, a.ID); n != 1 {
		t.Fatalf("rejected calls must have written nothing before the good one; edges=%d", n)
	}
}

package main

// mcp_read_split_test.go: characterization pins for the P2 read-tool
// split. These tests pin current read-tool outputs BEFORE the code
// moves out of mcp_read_tools.go into per-domain files, and must
// pass unchanged AFTER the move. No authority changes.

import (
	"encoding/json"
	"strings"
	"testing"
)

// Task row projection: identity, lifecycle, objective head line.
func TestMCPReadSplitTaskRowShape(t *testing.T) {
	tk := Task{ID: "t-1", Run: "run-1", Agent: "w", Status: TaskPending,
		Attempt: 2, Parent: "p-0", CorrectionNo: 1,
		Objective: "first line\nsecond line", Created: "c", Updated: "u"}
	row := mcpTaskRow(tk)
	for _, k := range []string{"id", "run", "agent", "status", "attempt",
		"parent", "correction_no", "objective", "created", "updated"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("task row missing key %q: %v", k, row)
		}
	}
	if row["objective"] != "first line" {
		t.Fatalf("row must carry the objective head line: %v", row["objective"])
	}
	if row["id"] != "t-1" || row["attempt"] != 2 {
		t.Fatalf("row must carry identity verbatim: %v", row)
	}
}

// Tasks listing: totals, truncation flag, positional cursor, unknown
// cursor yields an empty page.
func TestMCPReadSplitTasksListShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "rsplit")
	seedOwnedSession(t, "w", "rsplit")
	for _, obj := range []string{"one", "two", "three"} {
		if _, err := createTask("run-rs", "w", obj, "done"); err != nil {
			t.Fatal(err)
		}
	}
	out := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"run": "run-rs"}))
	if out["total"] != float64(3) || out["truncated"] != false {
		t.Fatalf("unfiltered list must carry all 3: %v", out)
	}
	p1 := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"run": "run-rs", "limit": 2}))
	if p1["truncated"] != true {
		t.Fatalf("capped page must flag truncated: %v", p1)
	}
	first := p1["tasks"].([]any)[0].(map[string]any)["id"].(string)
	p2 := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"run": "run-rs", "after": first}))
	if len(p2["tasks"].([]any)) != 2 {
		t.Fatalf("cursor page must return the remainder: %v", p2)
	}
	unk := mcpTaskText(t, mcpTaskCall(t, "metis_tasks", map[string]any{"after": "no-such-id"}))
	if len(unk["tasks"].([]any)) != 0 || unk["truncated"] != false {
		t.Fatalf("unknown cursor must yield an empty page: %v", unk)
	}
}

// Single-task status: full objective, gate, verdicts, counts.
func TestMCPReadSplitTaskStatusShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "rsplit2")
	seedOwnedSession(t, "w", "rsplit2")
	tk, err := createTask("run-rs2", "w", "full objective\nline two", "acc text")
	if err != nil {
		t.Fatal(err)
	}
	out := mcpTaskText(t, mcpTaskCall(t, "metis_task_status", map[string]any{"task_id": tk.ID}))
	for _, k := range []string{"objective", "acceptance", "waiting_on",
		"verdicts", "artifact_count", "has_report", "verified", "status"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("task_status missing key %q: %v", k, out)
		}
	}
	if out["objective"] != "full objective\nline two" {
		t.Fatalf("status must carry the full objective: %v", out["objective"])
	}
	if out["verified"] != false || out["has_report"] != false {
		t.Fatalf("fresh task must read unverified without report: %v", out)
	}
	if _, err := mcpCallTool("metis_task_status",
		mustJSON(t, map[string]any{})); err == nil {
		t.Fatal("missing task_id must be a param error")
	}
}

// Graph: unknown op is a param error; missing run is a param error.
func TestMCPReadSplitGraphParamShape(t *testing.T) {
	if _, err := mcpCallTool("metis_graph",
		mustJSON(t, map[string]any{"run": "r", "op": "bogus"})); err == nil {
		t.Fatal("unknown graph op must be a param error")
	} else if !strings.Contains(err.Error(), "unknown graph op") {
		t.Fatalf("unexpected graph error text: %v", err)
	}
	if _, err := mcpCallTool("metis_graph",
		mustJSON(t, map[string]any{"op": "walk"})); err == nil {
		t.Fatal("missing run must be a param error")
	}
}

// Events: page carries events plus trailing cursor.
func TestMCPReadSplitEventsShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "rsplit3")
	seedOwnedSession(t, "w", "rsplit3")
	out := mcpTaskText(t, mcpTaskCall(t, "metis_events", map[string]any{"run": "run-none"}))
	// Empty run pages a null events list (encoding/json renders an
	// empty Go slice as null): pin the current shape, not an ideal
	// one.
	if ev, ok := out["events"].([]any); ok && len(ev) != 0 {
		t.Fatalf("empty run must page no events: %v", out)
	}
	if _, ok := out["cursor"]; !ok {
		t.Fatalf("events page must carry a cursor: %v", out)
	}
}

// Sessions/status surface: keys and server identity.
func TestMCPReadSplitStatusShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	out := mcpTaskText(t, mcpTaskCall(t, "metis_status", map[string]any{}))
	for _, k := range []string{"muse_state", "open", "blocked", "latest_event_seq", "server"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("status missing key %q: %v", k, out)
		}
	}
	if out["server"] != mcpServerName {
		t.Fatalf("status must name this server: %v", out["server"])
	}
}

// Agents: empty fleet reads empty, never an error.
func TestMCPReadSplitAgentsEmptyShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	out := mcpTaskText(t, mcpTaskCall(t, "metis_agents", map[string]any{}))
	if out["total"] != float64(0) || out["truncated"] != false {
		t.Fatalf("empty fleet must read total=0 truncated=false: %v", out)
	}
	if _, ok := out["agents"]; !ok {
		t.Fatalf("agents must carry an agents list: %v", out)
	}
	raw, _ := json.Marshal(map[string]any{"limit": 1, "after": "zzz"})
	res, err := mcpCallTool("metis_agents", raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = res
}

// Projects: empty state reads zero projects, never an error.
func TestMCPReadSplitProjectsEmptyShape(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	out := mcpTaskText(t, mcpTaskCall(t, "metis_projects", map[string]any{}))
	if out["total"] != float64(0) {
		t.Fatalf("empty state must read total=0: %v", out)
	}
	if _, ok := out["projects"]; !ok {
		t.Fatalf("projects must carry a projects list: %v", out)
	}
}

// Approvals: missing binding args are a param error (no host hit).
func TestMCPReadSplitApprovalsParamShape(t *testing.T) {
	if _, err := mcpCallTool("metis_approvals",
		mustJSON(t, map[string]any{})); err == nil {
		t.Fatal("missing session/provider must be a param error")
	}
}

package main

// mcp_idempotency_test.go: exact-payload idempotency for the control
// tools, mirroring the P0-C provision contract.
//
// A reused request_id with different arguments is a conflict, never
// a silent convergence on the original outcome. Failing-first:
// both tools currently return the original without comparing.

import (
	"strings"
	"testing"
)

func TestMCPDependReplayConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-di", "w", "a thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-di", "w", "b thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	c, err := createTask("run-di", "w", "c thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"task_id": b.ID, "dep_id": a.ID, "actor": "orch", "request_id": "k1"}
	if res, err := mcpCallTool("metis_depend", mustJSON(t, base)); err != nil || res.IsError {
		t.Fatalf("depend: %v %v", err, res)
	}
	// Same key, different edge: conflict, not the original edge.
	diff := map[string]any{"task_id": b.ID, "dep_id": c.ID, "actor": "orch", "request_id": "k1"}
	res, err := mcpCallTool("metis_depend", mustJSON(t, diff))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused request ID with a different edge must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if n := countDepEdges(t, b.ID, c.ID); n != 0 {
		t.Fatal("conflicting replay must record no edge")
	}
	if n := countDepEdges(t, b.ID, a.ID); n != 1 {
		t.Fatalf("original edge must survive the conflict, got %d", n)
	}
}

func TestMCPCorrectReplayConflict(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	src, err := createTask("run-ci", "w", "source thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	first, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix A", "actor": "orch", "request_id": "kc1",
	}))
	if err != nil || first.IsError {
		t.Fatalf("correct: %v %v", err, first)
	}
	// Same key, different correction objective: conflict, not the
	// original correction.
	res, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix B", "actor": "orch", "request_id": "kc1",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused request ID with a different objective must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if n := countCorrections(t, src.ID); n != 1 {
		t.Fatalf("conflicting replay must fork no second correction, got %d", n)
	}
}

func countCorrections(t *testing.T, parentID string) int {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE parent = ?`, parentID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

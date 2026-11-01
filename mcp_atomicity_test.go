package main

// mcp_atomicity_test.go: P0 MCP idempotency atomicity.
//
// The key claim and the domain mutation must commit in ONE SQLite
// transaction: concurrent same-key conflicting requests must leave
// exactly one mutation, and an injected failure mid-transaction must
// leave none.

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestMCPDependConcurrentConflictAtomic(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	a, err := createTask("run-atomic-dep", "w", "a thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-atomic-dep", "w", "b thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	c, err := createTask("run-atomic-dep", "w", "c thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]mcpToolResult, n)
	callErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			dep := a.ID
			if i%2 == 1 {
				dep = c.ID
			}
			res, err := mcpCallTool("metis_depend", mustJSON(t, map[string]any{
				"task_id": b.ID, "dep_id": dep, "actor": "orch", "request_id": "katomic-dep",
			}))
			results[i] = res
			callErrs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if callErrs[i] != nil {
			t.Fatalf("call %d: conflict must arrive as isError, not RPC error: %v", i, callErrs[i])
		}
	}
	na := countDepEdges(t, b.ID, a.ID)
	nc := countDepEdges(t, b.ID, c.ID)
	if na+nc != 1 {
		t.Fatalf("concurrent conflicting depends must leave exactly one edge, got b->a=%d b->c=%d", na, nc)
	}
	winnerDep := a.ID
	if nc == 1 {
		winnerDep = c.ID
	}
	// The key row must name the winning edge.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	var keyTask, keyDep string
	if err := db.QueryRow(`SELECT task_id, dep_id FROM mcp_depend WHERE request_id = ?`, "katomic-dep").Scan(&keyTask, &keyDep); err != nil {
		db.Close()
		t.Fatalf("key row must survive: %v", err)
	}
	db.Close()
	if keyTask != b.ID || keyDep != winnerDep {
		t.Fatalf("key row names %s -> %s, winner is %s -> %s", keyTask, keyDep, b.ID, winnerDep)
	}
	sawSuccess := false
	for i, res := range results {
		if res.IsError {
			if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
				t.Fatalf("call %d error must be a conflict: %s", i, text)
			}
			continue
		}
		sawSuccess = true
		out := mcpTaskText(t, res)
		if out["task_id"] != b.ID || out["dep_id"] != winnerDep {
			t.Fatalf("call %d converged on the wrong edge: %v", i, out)
		}
	}
	if !sawSuccess {
		t.Fatal("at least the winner must succeed")
	}
	// Sequential conflict still refuses and records no second edge.
	loserDep := c.ID
	if winnerDep == c.ID {
		loserDep = a.ID
	}
	res, err := mcpCallTool("metis_depend", mustJSON(t, map[string]any{
		"task_id": b.ID, "dep_id": loserDep, "actor": "orch", "request_id": "katomic-dep",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused key with a different edge must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if n := countDepEdges(t, b.ID, loserDep); n != 0 {
		t.Fatal("conflicting replay must record no edge")
	}
}

func TestMCPCorrectConcurrentConflictAtomic(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	src, err := createTask("run-atomic-cor", "w", "source thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]mcpToolResult, n)
	callErrs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			obj := "fix A"
			if i%2 == 1 {
				obj = "fix B"
			}
			res, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
				"task_id": src.ID, "objective": obj, "actor": "orch", "request_id": "katomic-cor",
			}))
			results[i] = res
			callErrs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if callErrs[i] != nil {
			t.Fatalf("call %d: conflict must arrive as isError, not RPC error: %v", i, callErrs[i])
		}
	}
	if n := countCorrections(t, src.ID); n != 1 {
		t.Fatalf("concurrent conflicting corrections must fork exactly one correction, got %d", n)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	var winnerTask, winnerObjective string
	if err := db.QueryRow(`SELECT id, objective FROM tasks WHERE parent = ?`, src.ID).Scan(&winnerTask, &winnerObjective); err != nil {
		db.Close()
		t.Fatalf("winner correction: %v", err)
	}
	var keyTask string
	if err := db.QueryRow(`SELECT task_id FROM mcp_correct WHERE request_id = ?`, "katomic-cor").Scan(&keyTask); err != nil {
		db.Close()
		t.Fatalf("key row must survive: %v", err)
	}
	db.Close()
	if keyTask != winnerTask {
		t.Fatalf("key row names %s, winner is %s", keyTask, winnerTask)
	}
	sawSuccess := false
	for i, res := range results {
		if res.IsError {
			if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
				t.Fatalf("call %d error must be a conflict: %s", i, text)
			}
			continue
		}
		sawSuccess = true
		out := mcpTaskText(t, res)
		if out["task_id"] != winnerTask {
			t.Fatalf("call %d converged on the wrong correction: %v", i, out)
		}
	}
	if !sawSuccess {
		t.Fatal("at least the winner must succeed")
	}
	// Sequential conflict still refuses and forks no second
	// correction.
	loserObj := "fix B"
	if winnerObjective == "fix B" {
		loserObj = "fix A"
	}
	res, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": loserObj, "actor": "orch", "request_id": "katomic-cor",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("reused key with a different objective must conflict")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
	if n := countCorrections(t, src.ID); n != 1 {
		t.Fatalf("conflicting replay must fork no second correction, got %d", n)
	}
}

func TestMCPDependAtomicRollbackOnInjectedFailure(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	a, err := createTask("run-atomic-fdep", "w", "a thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-atomic-fdep", "w", "b thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	mcpIdemInjectTxFailure = errors.New("injected tx failure")
	res, err := mcpCallTool("metis_depend", mustJSON(t, map[string]any{
		"task_id": b.ID, "dep_id": a.ID, "actor": "orch", "request_id": "kfail-dep",
	}))
	mcpIdemInjectTxFailure = nil
	if err != nil {
		t.Fatalf("injected failure must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("injected tx failure must fail the call")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "injected tx failure") {
		t.Fatalf("failure must surface: %s", text)
	}
	// Rollback must undo both the edge and the key row.
	if n := countDepEdges(t, b.ID, a.ID); n != 0 {
		t.Fatal("rolled-back depend must leave no edge")
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_depend WHERE request_id = ?`, "kfail-dep").Scan(&n); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if n != 0 {
		t.Fatal("rolled-back depend must leave no key row")
	}
	// A retry after the failure succeeds exactly once.
	res, err = mcpCallTool("metis_depend", mustJSON(t, map[string]any{
		"task_id": b.ID, "dep_id": a.ID, "actor": "orch", "request_id": "kfail-dep",
	}))
	if err != nil || res.IsError {
		t.Fatalf("retry after rollback must succeed: %v %v", err, res)
	}
	if n := countDepEdges(t, b.ID, a.ID); n != 1 {
		t.Fatalf("retry must record exactly one edge, got %d", n)
	}
}

func TestMCPCorrectAtomicRollbackOnInjectedFailure(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	src, err := createTask("run-atomic-fcor", "w", "source thing", "done")
	if err != nil {
		t.Fatal(err)
	}
	mcpIdemInjectTxFailure = errors.New("injected tx failure")
	res, err := mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix A", "actor": "orch", "request_id": "kfail-cor",
	}))
	mcpIdemInjectTxFailure = nil
	if err != nil {
		t.Fatalf("injected failure must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("injected tx failure must fail the call")
	}
	if text := res.Content[0].Text; !strings.Contains(text, "injected tx failure") {
		t.Fatalf("failure must surface: %s", text)
	}
	// Rollback must undo both the correction and the key row.
	if n := countCorrections(t, src.ID); n != 0 {
		t.Fatal("rolled-back correct must leave no correction")
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM mcp_correct WHERE request_id = ?`, "kfail-cor").Scan(&n); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if n != 0 {
		t.Fatal("rolled-back correct must leave no key row")
	}
	// A retry after the failure forks exactly one correction.
	res, err = mcpCallTool("metis_correct", mustJSON(t, map[string]any{
		"task_id": src.ID, "objective": "fix A", "actor": "orch", "request_id": "kfail-cor",
	}))
	if err != nil || res.IsError {
		t.Fatalf("retry after rollback must succeed: %v %v", err, res)
	}
	if n := countCorrections(t, src.ID); n != 1 {
		t.Fatalf("retry must fork exactly one correction, got %d", n)
	}
}

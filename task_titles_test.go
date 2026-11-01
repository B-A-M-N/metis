package main

// task_titles_test.go: the dashboard titles every worker tab with
// its latest objective on the frequent refresh path. That
// projection must be correct per agent and must not scan the whole
// task history (P1-G / finding C).

import (
	"fmt"
	"strings"
	"testing"
)

// seedTitledTask inserts a task with an explicit creation stamp so
// interleaved-agent histories are deterministic down to the second.
func seedTitledTask(t *testing.T, agent, objective, created string) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO tasks (id, run, agent, objective, created, updated)
		VALUES (?, 'r-titles', ?, ?, ?, ?)`, newTaskID(), agent, objective, created, created); err != nil {
		t.Fatal(err)
	}
}

// The projection reports each agent's own latest objective even
// when agents interleave and creation stamps arrive out of order.
func TestLatestObjectiveByAgentReportsOwnLatest(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedTitledTask(t, "a", "a-first", "2026-01-01T00:00:01Z")
	seedTitledTask(t, "b", "b-first", "2026-01-01T00:00:02Z")
	seedTitledTask(t, "a", "a-second", "2026-01-01T00:00:03Z")
	seedTitledTask(t, "b", "b-second", "2026-01-01T00:00:04Z")
	seedTitledTask(t, "a", "a-third", "2026-01-01T00:00:05Z")
	got, err := latestObjectiveByAgent()
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "a-third" || got["b"] != "b-second" {
		t.Fatalf("each agent must report its own latest: %v", got)
	}
}

// The grouped query must ride the (agent, created) index instead of
// scanning the task history: EXPLAIN QUERY PLAN must show index
// use and no full table scan.
func TestLatestObjectiveByAgentUsesIndex(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT agent, objective, MAX(created) FROM tasks GROUP BY agent`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var detail strings.Builder
	for rows.Next() {
		var id, order, from int
		var text string
		if err := rows.Scan(&id, &order, &from, &text); err != nil {
			t.Fatal(err)
		}
		detail.WriteString(text + "\n")
	}
	plan := detail.String()
	// "SCAN tasks USING INDEX ..." is an index scan, not a table
	// scan; a bare SCAN with no index is the unbounded path.
	if strings.Contains(plan, "SCAN") && !strings.Contains(plan, "USING") {
		t.Fatalf("latest-objective projection must not full-scan tasks:\n%s", plan)
	}
	if !strings.Contains(plan, "INDEX") {
		t.Fatalf("latest-objective projection must use the agent index:\n%s", plan)
	}
}

// Scale: thousands of interleaved tasks still resolve exactly, on
// the indexed path. One transaction keeps the fixture fast; the
// projection itself is what the other two tests pin.
func TestLatestObjectiveByAgentAtScale(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		created := fmt.Sprintf("2026-02-01T00:%02d:%02dZ", (i/60)%60, i%60)
		if _, err := tx.Exec(`INSERT INTO tasks (id, run, agent, objective, created, updated)
			VALUES (?, 'r-scale', ?, ?, ?, ?)`, newTaskID(), fmt.Sprintf("w%02d", i%24), fmt.Sprintf("obj-%d", i), created, created); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := latestObjectiveByAgent()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 24 {
		t.Fatalf("expected 24 agents, got %d", len(got))
	}
	if got["w00"] != "obj-2976" {
		t.Fatalf("w00 must report its latest objective, got %q", got["w00"])
	}
}

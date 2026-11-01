package main

// graph_lineage.go: lineage construction — task_deps edges plus
// followup parent links projected into a deterministic run graph.
// This file creates no authority and grants no permission; it is a
// read-only observation over existing lineage. Cycles cannot appear
// (addDep refuses them); the walk reports an edge twice rather than
// looping if history ever does.

import (
	"database/sql"
	"sort"
)

// graphNode is one task vertex of the run lineage projection.
type graphNode struct {
	ID           string   `json:"id"`
	Status       string   `json:"status"`
	Attempt      int      `json:"attempt"`
	CorrectionNo int      `json:"correction_no"`
	Parent       string   `json:"parent,omitempty"`
	HasReport    bool     `json:"has_report"`
	Deps         []string `json:"deps"`
}

// graphEdge is one lineage edge: "depends-on" | "correction-of".
type graphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// runGraph is the deterministic snapshot of one run's lineage.
type runGraph struct {
	Run   string      `json:"run"`
	Nodes []graphNode `json:"nodes"`
	Edges []graphEdge `json:"edges"`
}

// depReader is the small read-repository seam behind the raw
// task_deps query, so the projection can be exercised without a
// database. The production implementation stays a plain SQLite
// read; behavior is unchanged.
type depReader interface {
	depIDs(taskID string) ([]string, error)
}

// sqliteDepReader is the production depReader over the tasks DB.
type sqliteDepReader struct{}

var _ depReader = sqliteDepReader{}

// dbDepReader is the handle-scoped production depReader: same query
// as sqliteDepReader, shared handle, no per-task open.
type dbDepReader struct{ db *sql.DB }

var _ depReader = dbDepReader{}

func (r dbDepReader) depIDs(taskID string) ([]string, error) {
	return depIDsOn(r.db, taskID)
}

func (sqliteDepReader) depIDs(taskID string) ([]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return depIDsOn(db, taskID)
}

// depIDsOn is the handle-scoped core of the task_deps query: same
// rows, shared handle. dbDepReader carries it into the
// buildRunGraphWithDeps seam so one render opens once.
func depIDsOn(db *sql.DB, taskID string) ([]string, error) {
	rows, err := db.Query(`SELECT depends_on FROM task_deps WHERE task_id = ? ORDER BY depends_on`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// taskDepIDs lists the direct dependencies of one task, oldest
// sort order (depends_on ASC). Kept as the package-level query
// entry point; buildRunGraph routes through the depReader seam.
func taskDepIDs(taskID string) ([]string, error) {
	return sqliteDepReader{}.depIDs(taskID)
}

// buildRunGraph snapshots one run's tasks plus both edge families,
// sorted for deterministic output. Opens once and threads a
// handle-scoped reader through the edge queries; the row listing
// inside buildRunGraphWithDeps still opens its own (one-shot CLI
// path, documented residual).
func buildRunGraph(run string) (runGraph, error) {
	db, err := openTasksDB()
	if err != nil {
		return runGraph{Run: run}, err
	}
	defer db.Close()
	return buildRunGraphWithDeps(run, dbDepReader{db: db})
}

// buildRunGraphWithDeps is the injectable core of buildRunGraph.
func buildRunGraphWithDeps(run string, deps depReader) (runGraph, error) {
	g := runGraph{Run: run}
	tasks, err := listTasks(run)
	if err != nil {
		return g, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	for _, t := range tasks {
		ds, err := deps.depIDs(t.ID)
		if err != nil {
			return g, err
		}
		if ds == nil {
			ds = []string{}
		}
		n := graphNode{
			ID: t.ID, Status: t.Status, Attempt: t.Attempt,
			CorrectionNo: t.CorrectionNo, Parent: t.Parent,
			HasReport: t.Report != "", Deps: ds,
		}
		g.Nodes = append(g.Nodes, n)
		for _, d := range ds {
			g.Edges = append(g.Edges, graphEdge{From: t.ID, To: d, Kind: "depends-on"})
		}
		if t.Parent != "" {
			g.Edges = append(g.Edges, graphEdge{From: t.ID, To: t.Parent, Kind: "correction-of"})
		}
	}
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Kind < g.Edges[j].Kind
	})
	return g, nil
}

package main

// graph_project_coverage.go: run/project/session coverage
// projections — per-run establishment counts plus the
// task/project/session coverage graph. Observation only: this
// stays separate from the authoritative coverage-obligation
// service (coverage_*), which tracks owed duty; here we project
// what a run has established. Read-only: no rows are written,
// and traversal grants no mutation authority over the projects
// observed.

import (
	"database/sql"
	"sort"
)

// runCoverage counts what a run has established.
type runCoverage struct {
	Run             string         `json:"run"`
	Total           int            `json:"total"`
	ByStatus        map[string]int `json:"by_status"`
	WithReports     int            `json:"with_reports"`
	Verified        int            `json:"verified"`
	MaxCorrectionNo int            `json:"max_correction_depth"`
	Corrections     int            `json:"corrections"`
	Blocked         int            `json:"blocked"`
	Ready           int            `json:"ready"`
}

// coverageNode is one typed vertex of the project-coverage
// projection: a task, a canonical execution root (project), or a
// native session bound through the turn-intent ledger. Kinds keep
// the namespaces disjoint; a task ID never collides with a root
// path or a session name in this projection.
type coverageNode struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"` // task | project | session
	Status  string `json:"status,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// coverageEdge is one coverage edge:
// depends-on | correction-of | executes-in | bound-to.
type coverageEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// coverageGraph is the snapshot of one run's task lineage plus
// the cross-cutting identities the ledger already binds.
type coverageGraph struct {
	Run   string         `json:"run"`
	Nodes []coverageNode `json:"nodes"`
	Edges []coverageEdge `json:"edges"`
}

// sessionReader is the small read-repository seam behind the raw
// turn-intent ledger query, so the projection can be exercised
// without a database. The production implementation stays a
// plain SQLite read; behavior is unchanged.
type sessionReader interface {
	sessionsByTask(run string) (map[string][]string, error)
}

// sqliteSessionReader is the production sessionReader over the
// tasks DB.
type sqliteSessionReader struct{}

var _ sessionReader = sqliteSessionReader{}

func (sqliteSessionReader) sessionsByTask(run string) (map[string][]string, error) {
	out := map[string][]string{}
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT task_id, session FROM muse_turn_intents
		WHERE task_id IN (SELECT id FROM tasks WHERE run = ?)
		ORDER BY task_id, rowid`, run)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]map[string]bool{}
	for rows.Next() {
		var taskID, sess string
		if err := rows.Scan(&taskID, &sess); err != nil {
			return nil, err
		}
		if sess == "" {
			continue
		}
		if seen[taskID] == nil {
			seen[taskID] = map[string]bool{}
		}
		if !seen[taskID][sess] {
			seen[taskID][sess] = true
			out[taskID] = append(out[taskID], sess)
		}
	}
	return out, rows.Err()
}

// coverageSessions maps task IDs of one run to the distinct native
// sessions bound through the turn-intent ledger, oldest first per
// task. Tasks without a native binding map to nothing.
func coverageSessions(run string) (map[string][]string, error) {
	return sqliteSessionReader{}.sessionsByTask(run)
}

// buildCoverageGraph snapshots one run's task lineage plus the
// cross-cutting identities the ledger already binds: which
// canonical project root each task executes in (executes-in) and
// which native session each dispatched attempt is bound to
// (bound-to).
func buildCoverageGraph(run string) (coverageGraph, error) {
	return buildCoverageGraphWith(run, sqliteDepReader{}, sqliteSessionReader{})
}

// buildCoverageGraphWith is the injectable core of
// buildCoverageGraph.
func buildCoverageGraphWith(run string, deps depReader, sess sessionReader) (coverageGraph, error) {
	g := coverageGraph{Run: run}
	tasks, err := listTasks(run)
	if err != nil {
		return g, err
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].ID < tasks[j].ID })
	seenProject := map[string]bool{}
	seenSession := map[string]bool{}
	for _, t := range tasks {
		ds, err := deps.depIDs(t.ID)
		if err != nil {
			return g, err
		}
		g.Nodes = append(g.Nodes, coverageNode{
			ID: t.ID, Kind: "task", Status: t.Status, Attempt: t.Attempt,
		})
		for _, d := range ds {
			g.Edges = append(g.Edges, coverageEdge{From: t.ID, To: d, Kind: "depends-on"})
		}
		if t.Parent != "" {
			g.Edges = append(g.Edges, coverageEdge{From: t.ID, To: t.Parent, Kind: "correction-of"})
		}
		if t.ExecutionRoot != "" {
			if !seenProject[t.ExecutionRoot] {
				seenProject[t.ExecutionRoot] = true
				g.Nodes = append(g.Nodes, coverageNode{ID: t.ExecutionRoot, Kind: "project"})
			}
			g.Edges = append(g.Edges, coverageEdge{From: t.ID, To: t.ExecutionRoot, Kind: "executes-in"})
		}
	}
	sessByTask, err := sess.sessionsByTask(run)
	if err != nil {
		return g, err
	}
	for _, t := range tasks {
		for _, s := range sessByTask[t.ID] {
			if !seenSession[s] {
				seenSession[s] = true
				g.Nodes = append(g.Nodes, coverageNode{ID: s, Kind: "session"})
			}
			g.Edges = append(g.Edges, coverageEdge{From: t.ID, To: s, Kind: "bound-to"})
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Kind != g.Nodes[j].Kind {
			return g.Nodes[i].Kind < g.Nodes[j].Kind
		}
		return g.Nodes[i].ID < g.Nodes[j].ID
	})
	sort.Slice(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		return g.Edges[i].Kind < g.Edges[j].Kind
	})
	if g.Nodes == nil {
		g.Nodes = []coverageNode{}
	}
	if g.Edges == nil {
		g.Edges = []coverageEdge{}
	}
	return g, nil
}

// graphCoverageOn is the handle-scoped core of graphCoverage:
// same counts, shared handle, no per-section opens.
func graphCoverageOn(db *sql.DB, run string) (runCoverage, error) {
	c := runCoverage{Run: run, ByStatus: map[string]int{}}
	tasks, err := listTasksOn(db, run)
	if err != nil {
		return c, err
	}
	c.Total = len(tasks)
	for _, t := range tasks {
		c.ByStatus[t.Status]++
		if t.Report != "" {
			c.WithReports++
		}
		if t.Status == TaskVerified {
			c.Verified++
		}
		if t.CorrectionNo > 0 {
			c.Corrections++
			if t.CorrectionNo > c.MaxCorrectionNo {
				c.MaxCorrectionNo = t.CorrectionNo
			}
		}
	}
	blockers, err := graphBlockersOn(db, run)
	if err != nil {
		return c, err
	}
	c.Blocked = len(blockers)
	ready, err := graphReadyOn(db, run)
	if err != nil {
		return c, err
	}
	c.Ready = len(ready)
	return c, nil
}

// graphCoverage counts what a run has established. Verified means
// the task row reached verified through the verdict path — never a
// model self-claim, never process exit alone. One-shot wrapper over
// graphCoverageOn.
func graphCoverage(run string) (runCoverage, error) {
	db, err := openTasksDB()
	if err != nil {
		return runCoverage{Run: run, ByStatus: map[string]int{}}, err
	}
	defer db.Close()
	return graphCoverageOn(db, run)
}

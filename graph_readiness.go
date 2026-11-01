package main

// graph_readiness.go: dependency-readiness projection — which
// non-terminal tasks still wait on unverified dependencies
// (blockers) and which pending tasks the graph no longer holds
// back (ready). Readiness here is a state projection only:
// dispatch still enforces occupancy, run limits, bindings, and
// attempt claims at the claim site, so graph-ready is necessary,
// never sufficient.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// graphBlockersOn is the handle-scoped core of graphBlockers: same
// projection, shared handle. graphMDLines threads one handle through
// ready/blocked/coverage so a render opens once.
func graphBlockersOn(db *sql.DB, run string) ([]graphBlocker, error) {
	tasks, err := listTasksOn(db, run)
	if err != nil {
		return nil, err
	}
	var out []graphBlocker
	for _, t := range tasks {
		if terminalTaskStatus(t.Status) {
			continue
		}
		blocking, err := unsatisfiedDepsOn(db, t.ID)
		if err != nil {
			return nil, err
		}
		if len(blocking) == 0 {
			continue
		}
		ids := make([]string, 0, len(blocking))
		for _, b := range blocking {
			ids = append(ids, b)
		}
		sort.Strings(ids)
		out = append(out, graphBlocker{Task: t.ID, Status: t.Status, WaitingOn: ids})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Task < out[j].Task })
	if out == nil {
		out = []graphBlocker{}
	}
	return out, nil
}

// graphReadyOn is the handle-scoped core of graphReady.
func graphReadyOn(db *sql.DB, run string) ([]string, error) {
	tasks, err := listTasksOn(db, run)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tasks {
		if t.Status != TaskPending {
			continue
		}
		blocking, err := unsatisfiedDepsOn(db, t.ID)
		if err != nil {
			return nil, err
		}
		if len(blocking) == 0 {
			out = append(out, t.ID)
		}
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// graphBlocker is one non-terminal task held back by unsatisfied
// dependencies.
type graphBlocker struct {
	Task      string   `json:"task"`
	Status    string   `json:"status"`
	WaitingOn []string `json:"waiting_on"`
}

// graphBlockers lists non-terminal tasks with unsatisfied
// dependencies, using the dispatcher's own unsatisfiedDeps gate so
// the projection can never disagree with admission. One-shot wrapper
// over graphBlockersOn.
func graphBlockers(run string) ([]graphBlocker, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return graphBlockersOn(db, run)
}

// graphReady lists pending tasks whose dependencies are all
// verified: the graph imposes no further wait. Occupancy, run
// limits, and bindings may still refuse the claim — graph-ready is
// necessary, never sufficient. One-shot wrapper over graphReadyOn.
func graphReady(run string) ([]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return graphReadyOn(db, run)
}

// readGraphBanner is the read-model entry point for the path banner:
// opens once and threads the handle through ready/blocked/coverage
// so one render costs one open, not one per task. Called by the
// panel (graph_panel.go), which owns layout and text shaping only
// and never touches persistence (boundary tests pin the panel side).
func readGraphBanner(run string, maxLines int) []string {
	db, err := openTasksDB()
	if err != nil {
		return []string{"(graph unavailable: " + sanitizeLine(err.Error()) + ")"}
	}
	defer db.Close()
	return graphMDLinesOn(db, run, maxLines)
}

// graphMDLinesOn is the handle-scoped banner assembly: same lines
// the panel used to build with per-task opens, shared handle.
func graphMDLinesOn(db *sql.DB, run string, maxLines int) []string {
	if maxLines <= 0 {
		maxLines = graphPanelCap
	}
	lines := []string{mdBold("# path: " + run)}
	add := func(s string) bool {
		if len(lines) >= maxLines {
			return false
		}
		lines = append(lines, s)
		return true
	}
	ready, err := graphReadyOn(db, run)
	if err != nil {
		return []string{"(graph unavailable: " + sanitizeLine(err.Error()) + ")"}
	}
	add(mdBold(fmt.Sprintf("## ready (%d)", len(ready))))
	for _, id := range ready {
		t, err := getTaskOn(db, id)
		if err != nil {
			continue
		}
		if !add("- " + shortID(id) + " " + sanitizeLine(t.Status) + " — " + sanitizeLine(firstLine(t.Objective))) {
			break
		}
	}
	blockers, err := graphBlockersOn(db, run)
	if err != nil {
		return []string{"(graph unavailable: " + sanitizeLine(err.Error()) + ")"}
	}
	add(mdBold(fmt.Sprintf("## blocked (%d)", len(blockers))))
	for _, b := range blockers {
		if !add("- " + shortID(b.Task) + " waits on " + sanitizeLine(strings.Join(b.WaitingOn, ", "))) {
			break
		}
		add("  path: " + sanitizeLine(depChainOn(db, b.Task, 4)))
	}
	cov, err := graphCoverageOn(db, run)
	if err != nil {
		return []string{"(graph unavailable: " + sanitizeLine(err.Error()) + ")"}
	}
	add(mdBold("## coverage"))
	add(fmt.Sprintf("- %d tasks · %d verified · %d with reports · depth %d",
		cov.Total, cov.Verified, cov.WithReports, cov.MaxCorrectionNo))
	return lines
}

// depChainOn renders one dependency path root-first, capped at
// depth: `a(verified) <- b(pending) <- c(pending)`. Follows the
// first dep (sorted) so the output is deterministic. Handle-scoped:
// the render threads one handle through every path walk.
func depChainOn(db *sql.DB, taskID string, depth int) string {
	var segs []string
	cur := taskID
	for i := 0; i < depth && cur != ""; i++ {
		t, err := getTaskOn(db, cur)
		if err != nil {
			segs = append(segs, shortID(cur)+"(?)")
			break
		}
		segs = append(segs, shortID(cur)+"("+t.Status+")")
		deps, err := depIDsOn(db, cur)
		if err != nil || len(deps) == 0 {
			break
		}
		cur = deps[0]
	}
	// Reverse to root-first.
	for i, j := 0, len(segs)-1; i < j; i, j = i+1, j-1 {
		segs[i], segs[j] = segs[j], segs[i]
	}
	return strings.Join(segs, " <- ")
}

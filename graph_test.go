package main

// graph_test.go: the run graph is a deterministic read-only
// projection — same lineage in, same nodes/edges out, with the
// dispatcher's own gate deciding blocked vs ready so the two can
// never disagree.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedGraphRun(t *testing.T) (run string, ids map[string]string) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "graphfix")
	seedOwnedSession(t, "w", "graphfix")
	run = "run-graph"
	ids = map[string]string{}
	mk := func(obj, acc string) string {
		tk, err := createTask(run, "w", obj, acc)
		if err != nil {
			t.Fatal(err)
		}
		ids[obj] = tk.ID
		return tk.ID
	}
	a := mk("a", "write f")
	mk("b", "write g")
	mk("c", "write h")
	mk("d-free", "write i")
	mk("e-free", "write j")
	if err := addTaskDep(ids["b"], a, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(ids["c"], ids["b"], testReq("orch")); err != nil {
		t.Fatal(err)
	}
	// Verify A through the real verdict path so the gate sees it.
	if err := dispatchTask(ids["a"], "w"); err != nil {
		t.Fatal(err)
	}
	root, err := sessionRoot("w")
	if err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(root, "f.txt")
	if err := os.WriteFile(ev, []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(a, ev); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(a, ReportFiling{Report: "a done", Source: SourceCLISubmission, Reporter: "t"}); err != nil {
		t.Fatal(err)
	}
	fileAllPassVerdicts(t, a, ev, "reviewer")
	if err := verifyTask(a, true, "", testReq("reviewer-01")); err != nil {
		t.Fatalf("verify A: %v", err)
	}
	// Correction edge: D corrects A.
	d, err := followupTask(a, "fix a", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	ids["d-corr"] = d.ID
	return run, ids
}

func TestGraphWalkDeterministic(t *testing.T) {
	run, ids := seedGraphRun(t)
	g, err := buildRunGraph(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 6 {
		t.Fatalf("expected 6 nodes, got %d", len(g.Nodes))
	}
	for i := 1; i < len(g.Nodes); i++ {
		if g.Nodes[i-1].ID >= g.Nodes[i].ID {
			t.Fatalf("nodes not sorted: %+v", g.Nodes)
		}
	}
	wantEdges := map[string]string{
		ids["b"] + "->" + ids["a"]:      "depends-on",
		ids["c"] + "->" + ids["b"]:      "depends-on",
		ids["d-corr"] + "->" + ids["a"]: "correction-of",
	}
	if len(g.Edges) != len(wantEdges) {
		t.Fatalf("expected %d edges, got %+v", len(wantEdges), g.Edges)
	}
	for _, e := range g.Edges {
		if wantEdges[e.From+"->"+e.To] != e.Kind {
			t.Fatalf("unexpected edge %+v", e)
		}
	}
	// Second build must be identical: projection, not sampling.
	g2, err := buildRunGraph(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Nodes) != 6 || len(g2.Edges) != 3 {
		t.Fatalf("rebuild diverged: %+v", g2)
	}
}

func TestGraphBlockersAndReady(t *testing.T) {
	run, ids := seedGraphRun(t)
	blockers, err := graphBlockers(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers) != 1 || blockers[0].Task != ids["c"] {
		t.Fatalf("only C blocked (by pending B), got %+v", blockers)
	}
	ready, err := graphReady(run)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{ids["b"]: true, ids["d-corr"]: true, ids["d-free"]: true, ids["e-free"]: true}
	if len(ready) != len(want) {
		t.Fatalf("ready = %v, want %v", ready, want)
	}
	for _, id := range ready {
		if !want[id] {
			t.Fatalf("ready = %v, want %v", ready, want)
		}
	}
	// Reported-but-unverified never unblocks: dispatch B, file a
	// report without verdicts, and confirm C stays blocked.
	if err := dispatchTask(ids["b"], "w"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(ids["b"], ReportFiling{Report: "b claims done", Source: SourceCLISubmission, Reporter: "t"}); err != nil {
		t.Fatal(err)
	}
	blockers2, err := graphBlockers(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(blockers2) != 1 {
		t.Fatalf("unverified report must not unblock: %+v", blockers2)
	}
}

func TestMCPGraphTool(t *testing.T) {
	run, ids := seedGraphRun(t)
	raw, _ := json.Marshal(map[string]any{"run": run, "op": "coverage"})
	res, err := mcpCallTool("metis_graph", raw)
	if err != nil {
		t.Fatalf("metis_graph coverage: %v", err)
	}
	text := res.Content[0].Text
	if !strings.Contains(text, `"total":6`) || !strings.Contains(text, `"verified":1`) {
		t.Fatalf("coverage text missing counts: %s", text)
	}
	raw, _ = json.Marshal(map[string]any{"run": run, "op": "ready"})
	res, err = mcpCallTool("metis_graph", raw)
	if err != nil {
		t.Fatalf("metis_graph ready: %v", err)
	}
	if !strings.Contains(res.Content[0].Text, ids["b"]) {
		t.Fatalf("ready must include B: %s", res.Content[0].Text)
	}
	raw, _ = json.Marshal(map[string]any{"run": run, "op": "bogus"})
	if _, err := mcpCallTool("metis_graph", raw); err == nil {
		t.Fatal("unknown graph op must be refused")
	}
	// tools/list must advertise the new read-only tool.
	line := mcpHandle([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if line == nil || !strings.Contains(string(line), "metis_graph") {
		t.Fatalf("tools/list missing metis_graph: %s", string(line))
	}
}

func TestGraphCoverage(t *testing.T) {
	run, _ := seedGraphRun(t)
	cov, err := graphCoverage(run)
	if err != nil {
		t.Fatal(err)
	}
	if cov.Total != 6 || cov.Verified != 1 || cov.Corrections != 1 || cov.MaxCorrectionNo != 1 {
		t.Fatalf("bad coverage: %+v", cov)
	}
	if cov.Blocked != 1 || cov.Ready != 4 {
		t.Fatalf("bad blocked/ready: %+v", cov)
	}
	if cov.WithReports != 1 {
		t.Fatalf("only verified A has a report so far: %+v", cov)
	}
	if cov.ByStatus[TaskVerified] != 1 || cov.ByStatus[TaskPending] != 5 {
		t.Fatalf("bad status counts: %+v", cov.ByStatus)
	}
}

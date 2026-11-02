package main

// coverage_graph_test.go: the project-coverage graph is a
// read-only projection over task lineage plus the identities the
// ledger already binds: canonical execution roots (projects),
// native session bindings, and dependency/correction edges.
// Traversal must never mutate and must never imply authority.

import (
	"os"
	"path/filepath"
	"testing"
)

func seedCoverageRun(t *testing.T) (run string, ids map[string]string, roots map[string]string) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "covfix")
	seedOwnedSession(t, "w1", "covfix")
	seedOwnedSession(t, "w2", "covfix")
	run = "run-coverage"
	ids = map[string]string{}
	roots = map[string]string{}
	mk := func(obj, acc string) string {
		tk, err := createTask(run, "w1", obj, acc)
		if err != nil {
			t.Fatal(err)
		}
		ids[obj] = tk.ID
		return tk.ID
	}
	a := mk("a", "write f")
	mk("b", "write g")
	if err := addTaskDep(ids["b"], a, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(ids["a"], "w1"); err != nil {
		t.Fatal(err)
	}
	// Verify A through the real verdict path so B's dependency is
	// satisfied and B may dispatch on the second project root.
	root, err := sessionRoot("w1")
	if err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(root, "f.txt")
	if err := os.WriteFile(ev, []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(ids["a"], ev); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(ids["a"], ReportFiling{Report: "a done", Source: SourceCLISubmission, Reporter: "t"}); err != nil {
		t.Fatal(err)
	}
	fileAllPassVerdicts(t, ids["a"], ev, "reviewer")
	if err := verifyTask(ids["a"], true, "", testReq("reviewer-01")); err != nil {
		t.Fatalf("verify A: %v", err)
	}
	if err := dispatchTask(ids["b"], "w2"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{ids["a"], ids["b"]} {
		tk, err := getTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if tk.ExecutionRoot == "" {
			t.Fatalf("task %s has no bound execution root after dispatch", id)
		}
		roots[id] = tk.ExecutionRoot
	}
	if roots[ids["a"]] == roots[ids["b"]] {
		t.Fatalf("expected distinct execution roots, got %q", roots[ids["a"]])
	}
	// Bind task A to a native Muse session via the real intent path.
	tkA, err := getTask(ids["a"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureTurnIntent(ids["a"], tkA.Attempt, "muse-sess-1", "native-1", "fi", "prov", roots[ids["a"]], "prompt a", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	return run, ids, roots
}

func TestCoverageGraphProjectsAndSessions(t *testing.T) {
	run, ids, roots := seedCoverageRun(t)
	g, err := buildCoverageGraph(run)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]coverageNode{}
	for _, n := range g.Nodes {
		byID[n.Kind+":"+n.ID] = n
	}
	// Two task nodes, two project nodes, one session node.
	if len(g.Nodes) != 5 {
		t.Fatalf("expected 5 nodes, got %+v", g.Nodes)
	}
	for _, id := range []string{ids["a"], ids["b"]} {
		n, ok := byID["task:"+id]
		if !ok {
			t.Fatalf("missing task node %s in %+v", id, g.Nodes)
		}
		if n.Status == "" {
			t.Fatalf("task node carries no status: %+v", n)
		}
	}
	for _, id := range []string{ids["a"], ids["b"]} {
		if _, ok := byID["project:"+roots[id]]; !ok {
			t.Fatalf("missing project node for %s in %+v", roots[id], g.Nodes)
		}
	}
	if _, ok := byID["session:muse-sess-1"]; !ok {
		t.Fatalf("missing session node in %+v", g.Nodes)
	}
	wantEdges := map[string]string{
		ids["b"] + "->" + ids["a"]:                "depends-on",
		ids["a"] + "->" + roots[ids["a"]]:         "executes-in",
		ids["b"] + "->" + roots[ids["b"]]:         "executes-in",
		ids["a"] + "->" + "muse-sess-1":           "bound-to",
	}
	if len(g.Edges) != len(wantEdges) {
		t.Fatalf("expected %d edges, got %+v", len(wantEdges), g.Edges)
	}
	for _, e := range g.Edges {
		if wantEdges[e.From+"->"+e.To] != e.Kind {
			t.Fatalf("unexpected edge %+v", e)
		}
	}
	// Rebuild must be identical: projection, not sampling.
	g2, err := buildCoverageGraph(run)
	if err != nil {
		t.Fatal(err)
	}
	if len(g2.Nodes) != 5 || len(g2.Edges) != 4 {
		t.Fatalf("rebuild diverged: %+v", g2)
	}
}

package main

// graph_split_test.go: the graph.go split moved CLI dispatch into
// cli_graph.go unchanged. These tests pin the DB-free error paths
// of the moved graphCmd so the split stays covered even without a
// seeded run.

import (
	"strings"
	"testing"
)

func TestGraphCmdRequiresRun(t *testing.T) {
	if err := graphCmd("walk", nil); err == nil ||
		!strings.Contains(err.Error(), "--run RUN") {
		t.Fatalf("walk without --run must fail, got %v", err)
	}
}

func TestGraphCmdRunFlagWithoutValue(t *testing.T) {
	if err := graphCmd("ready", []string{"--run"}); err == nil ||
		!strings.Contains(err.Error(), "--run RUN") {
		t.Fatalf("dangling --run must fail, got %v", err)
	}
}

func TestGraphCmdUnknownOp(t *testing.T) {
	if err := graphCmd("bogus", []string{"--run", "run-x"}); err == nil ||
		!strings.Contains(err.Error(), "unknown graph op") {
		t.Fatalf("unknown op must be refused, got %v", err)
	}
}

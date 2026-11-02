package main

// task_readmodel_test.go: characterization tests for the read-model
// seam (task_readmodel.go). They pin the projection behavior the MCP
// read tools and the graph panel consumed inline before the boundary
// migration: same rows, same empty states, same errors.

import (
	"testing"
)

func TestReadModelTaskStatusDetailUnknownTask(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if _, _, _, _, err := taskStatusDetail("no-such-task"); err == nil {
		t.Fatalf("taskStatusDetail(unknown) = nil error, want getTask failure")
	}
}

func TestReadModelTaskStatusDetailRoundTrip(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	created, err := createTask("run-1", "agent-1", "objective head\nbody", "accept!")
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	tk, waiting, verdicts, count, err := taskStatusDetail(created.ID)
	if err != nil {
		t.Fatalf("taskStatusDetail: %v", err)
	}
	if tk.ID != created.ID || tk.Objective != created.Objective || tk.Attempt != created.Attempt {
		t.Errorf("task mismatch: got %+v, want %+v", tk, created)
	}
	if len(waiting) != 0 {
		t.Errorf("waiting = %v, want empty", waiting)
	}
	if len(verdicts) != 0 {
		t.Errorf("verdicts = %v, want empty", verdicts)
	}
	if count != 0 {
		t.Errorf("artifactCount = %d, want 0", count)
	}
}

func TestReadModelLatestRunEmpty(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if got := latestRun(); got != "" {
		t.Errorf("latestRun() = %q, want empty", got)
	}
	if got := latestRunForAgent("nobody"); got != "" {
		t.Errorf("latestRunForAgent(nobody) = %q, want empty", got)
	}
}

func TestReadModelLatestRunRoundTrip(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if _, err := createTask("run-9", "agent-9", "obj", "acc"); err != nil {
		t.Fatalf("createTask: %v", err)
	}
	if got := latestRun(); got != "run-9" {
		t.Errorf("latestRun() = %q, want run-9", got)
	}
	if got := latestRunForAgent("agent-9"); got != "run-9" {
		t.Errorf("latestRunForAgent(agent-9) = %q, want run-9", got)
	}
	if got := latestRunForAgent("someone-else"); got != "" {
		t.Errorf("latestRunForAgent(someone-else) = %q, want empty", got)
	}
}

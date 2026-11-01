package main

// report_projection.go: read-only views over filed reports.
//
// Retrieval (getFullReport) and run-level comparison
// (compareReports) judge nothing and mutate nothing: admission and
// verification live in report_service.go and verification.go. Tasks
// without reports are omitted from comparison — comparison is over
// evidence, not over claims that were never filed.

import (
	"fmt"
	"strings"
)

// fullReport bundles a task with its artifacts and receipt trail.
type fullReport struct {
	Task      Task       `json:"task"`
	Artifacts []Artifact `json:"artifacts"`
	Receipts  []Receipt  `json:"receipts"`
}

// getFullReport returns the task plus attached evidence and receipts.
func getFullReport(taskID string) (fullReport, error) {
	t, err := getTask(taskID)
	if err != nil {
		return fullReport{}, err
	}
	arts, err := listArtifacts(taskID)
	if err != nil {
		return fullReport{}, err
	}
	recs, err := listReceipts(taskID)
	if err != nil {
		return fullReport{}, err
	}
	return fullReport{Task: t, Artifacts: arts, Receipts: recs}, nil
}

// compareReports returns every task in a run that has a report,
// newest first, each still attributed to its own agent and verdict.
// Tasks without reports are omitted: comparison is over evidence,
// not over claims that were never filed.
func compareReports(run string) ([]Task, error) {
	if strings.TrimSpace(run) == "" {
		return nil, fmt.Errorf("compare requires a run")
	}
	tasks, err := listTasks(run)
	if err != nil {
		return nil, err
	}
	var out []Task
	for i := len(tasks) - 1; i >= 0; i-- {
		if tasks[i].Report != "" {
			out = append(out, tasks[i])
		}
	}
	return out, nil
}

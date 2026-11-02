package main

import (
	"os"
	"testing"
)

// TestMCPControlSmokeLoop is the smallest end-to-end control loop
// a Muse.ai-class client can drive: delegate over MCP, observe over
// MCP, worker reports, independent verdicts through the
// intentionally local-only filing path, then verify over MCP to a
// terminal verified state. Every controller transition in the loop
// goes through mcpCallTool except verdict filing, which LEDGER
// deliberately keeps local-only (criterion-verdict filing over MCP
// was explicitly rejected); the test names that boundary instead
// of hiding it.
func TestMCPControlSmokeLoop(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "msmoke")
	seedOwnedSession(t, "w", "msmoke")

	// 1. Delegate: a model client admits work with one call.
	delegated := mcpTaskText(t, mcpTaskCall(t, "metis_delegate", map[string]any{
		"run": "run-smoke", "session": "w", "objective": "smoke objective",
		"acceptance": "smoke holds\nsmoke observed",
		"actor":      "model", "request_id": "smoke-1",
	}))
	id, _ := delegated["task_id"].(string)
	if id == "" {
		t.Fatalf("delegate returned no task_id: %v", delegated)
	}

	// 2. Observe before acting: status, events, and project surface.
	mcpTaskCall(t, "metis_task_status", map[string]any{"task_id": id})
	mcpTaskCall(t, "metis_events", map[string]any{"run": "run-smoke"})
	mcpTaskCall(t, "metis_graph", map[string]any{"run": "run-smoke", "op": "ready"})

	// 3. Worker side: the bound session picks up and reports.
	committed, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTaskWorking(id, committed.Attempt, testReq("worker-loop/w")); err != nil {
		t.Fatal(err)
	}
	workdir := loadRegistry()["w"].Workdir
	evidence := workdir + "/result.txt"
	if err := os.WriteFile(evidence, []byte("smoke bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(id, evidence); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(id, ReportFiling{Report: "smoke done", Source: SourceWorkerClaim, Reporter: "w"}); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(id)
	if got.Status != TaskReportReady {
		t.Fatalf("report did not land report_ready: %s", got.Status)
	}

	// 4. Observe the report over MCP.
	rep := mcpTaskText(t, mcpTaskCall(t, "metis_report", map[string]any{"task_id": id}))
	taskObj, _ := rep["task"].(map[string]any)
	if taskObj["id"] != id {
		t.Fatalf("report names wrong task: %v", rep)
	}

	// 5. Independent verdicts: intentionally local-only filing.
	// A remote model client cannot file these over MCP by design.
	fileAllPassVerdicts(t, id, evidence, "reviewer-01")

	// 6. Verify over MCP: explicit pass by an independent reviewer.
	ver := mcpTaskText(t, mcpTaskCall(t, "metis_verify", map[string]any{
		"task_id": id, "pass": true, "actor": "reviewer-01", "request_id": "smoke-verify",
	}))
	if ver["status"] != string(TaskVerified) {
		t.Fatalf("verify did not verify: %v", ver)
	}
	if final, _ := getTask(id); final.Status != TaskVerified {
		t.Fatalf("task not verified: %s", final.Status)
	}
}

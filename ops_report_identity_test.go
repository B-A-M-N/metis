package main

// ops_report_identity_test.go: report-phase observation carries
// real request/task/attempt/session identity — nothing invented.

import (
	"testing"
)

func reportIdentitySetup(t *testing.T) Task {
	t.Helper()
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("TUI_DASH_OPS_DIAG", "")
	opsReset()
	installFakeTmux(t, "op-ops")
	seedOwnedSession(t, "w", "op-ops")
	tk, err := createTask("run-ops-id", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := markTaskWorking(tk.ID, 1, testReq("test")); err != nil {
		t.Fatalf("working: %v", err)
	}
	return tk
}

func lastOpsEvent(t *testing.T) opsEvent {
	t.Helper()
	evs := opsSnapshot()
	if len(evs) == 0 {
		t.Fatal("want an observed report event, got none")
	}
	return evs[len(evs)-1]
}

func TestOpsReportIdentityWithRequest(t *testing.T) {
	tk := reportIdentitySetup(t)
	req := testReq("reporter")
	if err := submitReportWithRequest(tk.ID,
		ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}, req); err != nil {
		t.Fatalf("report: %v", err)
	}
	ev := lastOpsEvent(t)
	if ev.Phase != opsPhaseReport {
		t.Fatalf("want report phase, got %q", ev.Phase)
	}
	if ev.RequestID != req.RequestID {
		t.Fatalf("request ID must pass through, want %q got %q", req.RequestID, ev.RequestID)
	}
	if ev.TaskID != tk.ID {
		t.Fatalf("task ID must pass through, want %q got %q", tk.ID, ev.TaskID)
	}
	if ev.Attempt != 1 {
		t.Fatalf("attempt must be the authoritative revision, want 1 got %d", ev.Attempt)
	}
	if ev.Disposition != opsDispositionOK {
		t.Fatalf("want ok disposition: %+v", ev)
	}
}

func TestOpsReportLegacyAdapterNoInventedRequest(t *testing.T) {
	tk := reportIdentitySetup(t)
	if err := submitReport(tk.ID,
		ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	ev := lastOpsEvent(t)
	if ev.RequestID != "" {
		t.Fatalf("legacy filing carries no request; must stay empty, got %q", ev.RequestID)
	}
	if ev.TaskID != tk.ID || ev.Attempt != 1 {
		t.Fatalf("attempt/task must still resolve from the ledger: %+v", ev)
	}
	if ev.Session != "" {
		t.Fatalf("CLI execution binds no native session; must stay empty, got %q", ev.Session)
	}
}

func TestOpsReportSessionFromTurnIntent(t *testing.T) {
	tk := reportIdentitySetup(t)
	req := testReq("orch")
	if _, err := ensureTurnIntent(tk.ID, 1, "sess-w", "native-123",
		"profile", "provider", "ws", "prompt text", req); err != nil {
		t.Fatalf("intent: %v", err)
	}
	if err := submitReportWithRequest(tk.ID,
		ReportFiling{Report: "done", Source: SourceWorkerClaim, Reporter: "w"}, req); err != nil {
		t.Fatalf("report: %v", err)
	}
	ev := lastOpsEvent(t)
	if ev.Session != "native-123" {
		t.Fatalf("session must be the bound native identity, got %q", ev.Session)
	}
	if ev.Attempt != 1 || ev.RequestID != req.RequestID {
		t.Fatalf("request/attempt must ride along: %+v", ev)
	}
}

func TestOpsReportErrorKeepsIdentity(t *testing.T) {
	tk := reportIdentitySetup(t)
	req := testReq("reporter")
	first := ReportFiling{Report: "r1", Source: SourceCLISubmission, Reporter: "test"}
	if err := submitReportWithRequest(tk.ID, first, req); err != nil {
		t.Fatalf("first report: %v", err)
	}
	opsReset()
	if err := submitReportWithRequest(tk.ID, first, req); err == nil {
		t.Fatal("duplicate report must be refused")
	}
	ev := lastOpsEvent(t)
	if ev.Disposition != opsDispositionError {
		t.Fatalf("want error disposition: %+v", ev)
	}
	if ev.TaskID != tk.ID || ev.Attempt != 1 || ev.RequestID != req.RequestID {
		t.Fatalf("refusals must stay correlated: %+v", ev)
	}
	if ev.ErrorClass == "" {
		t.Fatalf("refusal must classify: %+v", ev)
	}
}

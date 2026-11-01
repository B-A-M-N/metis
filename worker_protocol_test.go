package main

// Slice 1 (remediation direction §6.1–2): the worker loop consumes a
// versioned envelope, and report ingestion must never let an outside
// submission impersonate the worker. These tests pin the envelope
// contract and the provenance/atomicity guarantees before the loop
// itself lands.

import (
	"strings"
	"sync"
	"testing"
)

// The envelope carries everything needed to bind an execution to its
// ledger attempt; validation rejects anything ambiguous.
func TestTaskEnvelopeValidation(t *testing.T) {
	valid := TaskEnvelope{
		SchemaVersion: WorkerProtocolVersion,
		TaskID:        "task-1",
		Attempt:       1,
		RunID:         "run-1",
		SessionID:     "w",
		ExecutionRoot: "/work",
		Objective:     "do it",
		Acceptance:    []Criterion{{ID: "C1", Requirement: "done means done"}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid envelope refused: %v", err)
	}
	bad := valid
	bad.SchemaVersion = 999
	if err := bad.Validate(); err == nil {
		t.Fatal("unknown schema version must be refused")
	}
	bad = valid
	bad.TaskID = ""
	if err := bad.Validate(); err == nil {
		t.Fatal("missing task id must be refused")
	}
	bad = valid
	bad.Acceptance = []Criterion{{ID: "", Requirement: "x"}}
	if err := bad.Validate(); err == nil {
		t.Fatal("criterion without id must be refused")
	}
	raw, err := valid.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseTaskEnvelope(raw)
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if parsed.TaskID != "task-1" || parsed.Attempt != 1 || len(parsed.Acceptance) != 1 {
		t.Fatalf("round trip lost data: %+v", parsed)
	}
	if _, err := ParseTaskEnvelope([]byte("{nope")); err == nil {
		t.Fatal("malformed JSON must be refused")
	}
}

// A filing must say what it is and who filed it; empty or
// unlabelled filings are refused before touching the ledger.
func TestReportFilingValidation(t *testing.T) {
	ok := ReportFiling{Report: "r", Source: SourceWorkerClaim, Reporter: "worker-loop/w"}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid filing refused: %v", err)
	}
	for _, bad := range []ReportFiling{
		{Report: "", Source: SourceWorkerClaim, Reporter: "x"},
		{Report: "r", Source: "mystery", Reporter: "x"},
		{Report: "r", Source: SourceWorkerClaim, Reporter: ""},
		{Report: "r", Source: SourceWorkerClaim, Reporter: "  "},
	} {
		if err := bad.Validate(); err == nil {
			t.Fatalf("invalid filing accepted: %+v", bad)
		}
	}
}

// P0 elevation: a CLI submission on a worker's task must be recorded
// as an operator filing — worker identity stays on the attempt it
// owns (TD-092), but the reporter must name the actual submitter so
// no outside text acquires worker attribution.
func TestSubmitReportProvenance(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW")
	seedOwnedSession(t, "w", "pW")
	tk, err := createTask("run-pW", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	filing := ReportFiling{Report: "operator notes", Source: SourceCLISubmission, Reporter: "local-cli"}
	if err := submitReport(tk.ID, filing); err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var worker, reporter, source string
	if err := db.QueryRow(`SELECT actor, reporter, source FROM task_reports WHERE task_id = ?`, tk.ID).Scan(&worker, &reporter, &source); err != nil {
		t.Fatal(err)
	}
	if worker != "w" {
		t.Fatalf("history must stay bound to the worker attempt, got %q", worker)
	}
	if reporter != "local-cli" || source != string(SourceCLISubmission) {
		t.Fatalf("history must name the actual submitter, got %q %q", reporter, source)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "report_received" && strings.Contains(r.Detail, "local-cli") && strings.Contains(r.Detail, "cli-submission") {
			found = true
		}
	}
	if !found {
		t.Fatalf("receipt must name submitter and source: %+v", listReceiptsOrFail(t, tk.ID))
	}
}

// Concurrent filings converge on one history row: the losing
// submitter is refused, never partially applied.
func TestSubmitReportSingleHistoryRow(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pW2")
	seedOwnedSession(t, "w", "pW2")
	tk, err := createTask("run-pW2", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = submitReport(tk.ID, ReportFiling{Report: "r", Source: SourceCLISubmission, Reporter: "test"})
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("exactly one filing must win, got %d (%v)", wins, errs)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("exactly one history row, got %d", n)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("winner must leave task report_ready, got %s", got.Status)
	}
}

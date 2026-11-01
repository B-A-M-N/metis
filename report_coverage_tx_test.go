package main

// report_coverage_tx_test.go: P0 report/coverage transaction defect.
//
// submitReport() must never mutate coverage state for a report it
// refuses. A duplicate report at the same attempt is refused (TD-087)
// AND the obligation ledger must be untouched: no supersession of
// r1's duties to the rejected r2 digest, no new rows at r2.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A refused duplicate report must leave coverage bindings exactly as
// the accepted report left them: duties still review r1's digest,
// nothing is stale, and nothing points at the rejected r2 digest.
func TestDuplicateReportLeavesCoverageUnchanged(t *testing.T) {
	run, work := coverageSetup(t, "duptx")
	tk, err := createTask(run, "w", "touch auth", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	p := filepath.Join(work, "auth.go")
	if err := os.WriteFile(p, []byte("package auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "r1-content", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report r1: %v", err)
	}
	rev1 := reportDigestOf("r1-content")
	rev2 := reportDigestOf("r2-different-content")
	before, err := listObligations(run, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].TargetRev != rev1 || before[0].Status != ObligationRequired {
		t.Fatalf("setup: want one required duty at r1, got %+v", before)
	}

	if err := submitReport(tk.ID, ReportFiling{Report: "r2-different-content", Source: SourceCLISubmission, Reporter: "test"}); err == nil {
		t.Fatal("second report at the same attempt must be refused")
	} else if !strings.Contains(err.Error(), "already") {
		t.Fatalf("refusal must say a report was already filed (TD-087): %v", err)
	}

	if got, _ := getTask(tk.ID); got.Report != "r1-content" {
		t.Fatalf("filed claim must not be overwritten, got %q", got.Report)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("report history must stay append-only with 1 row, got %d", n)
	}
	after, err := listObligations(run, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused report must not add coverage rows: before %d, after %d (%+v)", len(before), len(after), after)
	}
	for _, o := range after {
		if o.TargetRev == rev2 {
			t.Fatalf("refused report superseded coverage to the rejected digest: %+v", after)
		}
		if o.TargetRev != rev1 {
			t.Fatalf("coverage binding moved off r1's digest: %+v", after)
		}
		if o.Status != ObligationRequired {
			t.Fatalf("r1 duty must stay %q, got %q (%+v)", ObligationRequired, o.Status, after)
		}
	}
}

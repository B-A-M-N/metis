package main

// report_tx_boundary_test.go: Package A — report transaction boundary.
//
// Two defects, both line-grounded in submitReport (report_service.go):
//  1. Stale snapshot: the task was loaded via getTask BEFORE the tx
//     and the ledger UPDATE fenced on status only, so a concurrent
//     retry/lifecycle change between the read and the commit could
//     file under a stale attempt. The tx now re-reads the row
//     (status + attempt) after BEGIN, fences the UPDATE on the
//     expected attempt, and feeds that authoritative revision to
//     coverage generation on the same tx.
//  2. Receipt-on-open-tx: recordReceipt (its own connection) ran
//     after a coverage error while the tx was still open, wedging
//     the best-effort diagnostic on the caller's own lock under
//     SQLite write contention. The tx now rolls back BEFORE the
//     diagnostic is recorded.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// reportBoundaryTask creates, dispatches, and evidences one task on
// run via sess, returning its id. The attached artifact matches the
// coverageSetup policy (*auth*), so a filed report generates duties.
// Each caller uses its own session: a filed-but-unverified task keeps
// its workspace occupied (TD-051), so sharing one session across
// tasks would refuse the second dispatch.
func reportBoundaryTask(t *testing.T, run, sess, name string) string {
	t.Helper()
	work := loadRegistry()[sess].Workdir
	tk, err := createTask(run, sess, "touch auth", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, sess); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	p := filepath.Join(work, name)
	if err := os.WriteFile(p, []byte("package auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatalf("attach: %v", err)
	}
	return tk.ID
}

// bumpAttemptForTest simulates the ledger effect of a concurrent
// retry/lifecycle change that wins between a reporter's read and
// commit: the attempt advances while the task stays dispatchable.
func bumpAttemptForTest(t *testing.T, id string) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE tasks SET attempt = attempt + 1, status = ?, updated = ? WHERE id = ?`,
		TaskDispatched, nowUTC(), id)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("bump must move exactly one row for %s", id)
	}
}

type reportHistoryRow struct {
	attempt int
	report  string
	digest  string
}

func reportHistoryForTest(t *testing.T, id string) []reportHistoryRow {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT attempt_no, report, digest FROM task_reports WHERE task_id = ? ORDER BY attempt_no`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []reportHistoryRow
	for rows.Next() {
		var r reportHistoryRow
		if err := rows.Scan(&r.attempt, &r.report, &r.digest); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func isBusyErrorForTest(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "busy") || strings.Contains(s, "locked")
}

// A retry/lifecycle change landing between the authoritative re-read
// and the commit must defeat the filing: the attempt-fenced UPDATE
// matches zero rows and the report is refused as stale — never filed
// under the old attempt. reportTxProbe lands the bump on the
// victim's own tx (a separate connection would only serialize on the
// victim's lock); the rollback must unwind even the probe's write.
func TestStaleReportCannotFileAgainstNewerAttempt(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	run, _ := coverageSetup(t, "staler")
	id := reportBoundaryTask(t, run, "w", "auth-stale.go")
	base, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	reportTxProbe = func(tx *sql.Tx) {
		if _, err := tx.Exec(`UPDATE tasks SET attempt = attempt + 1 WHERE id = ?`, id); err != nil {
			t.Errorf("probe bump: %v", err)
		}
	}
	defer func() { reportTxProbe = nil }()

	err = submitReport(id, ReportFiling{Report: "stale-race-content", Source: SourceCLISubmission, Reporter: "test"})
	if err == nil {
		t.Fatal("stale report must be refused, it filed")
	}
	if !strings.Contains(err.Error(), "stale") || !strings.Contains(err.Error(), "attempt") {
		t.Fatalf("refusal must name the stale attempt, got: %v", err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, id); n != 0 {
		t.Fatalf("stale report must file nothing, got %d history rows", n)
	}
	got, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != base.Attempt {
		t.Fatalf("rollback must unwind the concurrent bump too: attempt %d, want %d", got.Attempt, base.Attempt)
	}
	if got.Status != TaskDispatched {
		t.Fatalf("task must stay dispatchable, got %s", got.Status)
	}
	if got.Report != "" {
		t.Fatalf("stale report must not touch the filed claim, got %q", got.Report)
	}
	obs, err := listObligations(run, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Fatalf("refused report must leave coverage empty, got %+v", obs)
	}
}

// Reports track the authoritative attempt across a real retry: file,
// reconcile to a newer attempt, and the next report must land on the
// newer attempt with history append-only — never under the stale one.
func TestReportVsRetryKeepsAuthoritativeAttempt(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	run, _ := coverageSetup(t, "authattempt")
	seedHarnessSession(t, "w-seq", "authattempt", "demo")
	id := reportBoundaryTask(t, run, "w-seq", "auth-seq.go")
	const r1 = "seq-r1-content"
	if err := submitReport(id, ReportFiling{Report: r1, Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report r1: %v", err)
	}
	a1, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	// A concurrent retry wins after r1: attempt advances, dispatchable.
	bumpAttemptForTest(t, id)
	a2, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Attempt != a1.Attempt+1 {
		t.Fatalf("bump must advance the attempt: %d -> %d", a1.Attempt, a2.Attempt)
	}
	const r2 = "seq-r2-content"
	if err := submitReport(id, ReportFiling{Report: r2, Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report r2 at newer attempt: %v", err)
	}
	rows := reportHistoryForTest(t, id)
	if len(rows) != 2 || rows[0].attempt != a1.Attempt || rows[1].attempt != a2.Attempt {
		t.Fatalf("history must be append-only at attempts %d,%d, got %+v", a1.Attempt, a2.Attempt, rows)
	}
	if rows[0].report != r1 || rows[0].digest != reportDigestOf(r1) {
		t.Fatalf("first claim must be untouched, got %+v", rows[0])
	}
	if rows[1].report != r2 || rows[1].digest != reportDigestOf(r2) {
		t.Fatalf("second report must file at the newer attempt, got %+v", rows[1])
	}
	got, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempt != a2.Attempt || got.Status != TaskReportReady || got.Report != r2 {
		t.Fatalf("ledger must show r2 filed at attempt %d, got %+v", a2.Attempt, got)
	}
	obs, err := listObligations(run, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) == 0 {
		t.Fatal("r2 must generate duties at its digest")
	}
	for _, o := range obs {
		if o.Status == ObligationRequired && o.TargetRev != reportDigestOf(r2) {
			t.Fatalf("required duties must review the newest digest, got %+v", obs)
		}
		if o.TargetRev == reportDigestOf(r1) && o.Status == ObligationRequired {
			t.Fatalf("r1 duties must be superseded, got %+v", obs)
		}
	}
}

// A genuine report-vs-retry race on the production paths: the filed
// claim always lands on the attempt that was authoritative in its
// own transaction — exactly one row, at the task's final attempt,
// with the ledger report_ready. SQLITE_BUSY is a contention
// artifact, not an outcome: the reporter retries it to quiescence.
func TestConcurrentReportVsRetryFilesAuthoritativeAttempt(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	run, _ := coverageSetup(t, "raceretry")
	for round := 0; round < 3; round++ {
		sess := fmt.Sprintf("w-race-%d", round)
		seedHarnessSession(t, sess, "raceretry", "demo")
		id := reportBoundaryTask(t, run, sess, fmt.Sprintf("auth-race-%d.go", round))
		body := fmt.Sprintf("race-content-%d", round)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			db, err := openTasksDB()
			if err != nil {
				return
			}
			defer db.Close()
			// Either outcome is legitimate: the retry wins and the
			// report files newer, or the report wins and there is
			// nothing uncertain left to reconcile.
			_, _ = reconcileForRetry(db, id, testReq("retry-race"))
		}()
		var repErr error
		go func() {
			defer wg.Done()
			<-start
			// BUSY is a contention artifact, not an outcome: retry
			// to quiescence on a deadline, not a fixed attempt
			// count. A fixed burst calibrates to attempt latency —
			// faster opens (epoch-gated DDL skip) tighten the burst
			// until it fits inside the reconciler's lock window and
			// all retries collide. The deadline matches the
			// busy_timeout horizon; assertions below are unchanged.
			deadline := time.Now().Add(5 * time.Second)
			for {
				repErr = submitReport(id, ReportFiling{Report: body, Source: SourceCLISubmission, Reporter: "test"})
				if repErr == nil || !isBusyErrorForTest(repErr) {
					break
				}
				if time.Now().After(deadline) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
		}()
		close(start)
		wg.Wait()
		if repErr != nil {
			t.Fatalf("round %d: report must file at its authoritative attempt, got %v", round, repErr)
		}
		final, err := getTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != TaskReportReady || final.Report != body {
			t.Fatalf("round %d: ledger must show the filed claim, got %+v", round, final)
		}
		rows := reportHistoryForTest(t, id)
		if len(rows) != 1 || rows[0].attempt != final.Attempt || rows[0].report != body {
			t.Fatalf("round %d: exactly one row at the final attempt %d, got %+v", round, final.Attempt, rows)
		}
	}
}

// An injected coverage failure must refuse promptly: the error path
// rolls back BEFORE the best-effort coverage_gap diagnostic (which
// opens its own connection), so under write contention it cannot
// wedge on the caller's own lock. Coverage and history are unchanged.
func TestCoverageFailureRefusesPromptlyWithoutLockWait(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	run, _ := coverageSetup(t, "covfault")
	seedHarnessSession(t, "w-fault", "covfault", "demo")
	id := reportBoundaryTask(t, run, "w-fault", "auth-fault.go")
	const r1 = "fault-r1-content"
	if err := submitReport(id, ReportFiling{Report: r1, Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report r1: %v", err)
	}
	// Advance the attempt so the failing report is not a TD-087
	// duplicate: the fault must fire inside the coverage checkpoint.
	bumpAttemptForTest(t, id)
	before, err := listObligations(run, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].TargetRev != reportDigestOf(r1) || before[0].Status != ObligationRequired {
		t.Fatalf("setup: want one required duty at r1, got %+v", before)
	}
	generateCoverageTxFault = func(taskID string) error {
		if taskID == id {
			return fmt.Errorf("injected coverage failure for test")
		}
		return nil
	}
	defer func() { generateCoverageTxFault = nil }()

	start := time.Now()
	err = submitReport(id, ReportFiling{Report: "fault-r2-content", Source: SourceCLISubmission, Reporter: "test"})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "injected coverage failure") {
		t.Fatalf("coverage failure must refuse with the checkpoint error, got %v", err)
	}
	// openTasksDB sets busy_timeout=5000: the old receipt-on-open-tx
	// order wedged ~5s on its own lock. The fixed order (rollback
	// first) refuses in milliseconds; 4s discriminates with margin.
	if elapsed >= 4*time.Second {
		t.Fatalf("coverage-failure refusal wedged on its own lock: took %v", elapsed)
	}
	after, err := listObligations(run, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused report must not add coverage rows: before %d, after %d (%+v)", len(before), len(after), after)
	}
	for i := range after {
		if after[i].ID != before[i].ID || after[i].TargetRev != before[i].TargetRev || after[i].Status != before[i].Status {
			t.Fatalf("coverage must be unchanged: before %+v, after %+v", before, after)
		}
		if after[i].TargetRev == reportDigestOf("fault-r2-content") {
			t.Fatalf("refused digest must never reach the ledger: %+v", after)
		}
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, id); n != 1 {
		t.Fatalf("history must stay at the one r1 row, got %d", n)
	}
	got, err := getTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched || got.Report != r1 {
		t.Fatalf("ledger must be untouched (dispatched, r1 claim), got %+v", got)
	}
	found := false
	for _, r := range listReceiptsOrFail(t, id) {
		if r.Kind == "coverage_gap" && strings.Contains(r.Detail, "injected coverage failure") {
			found = true
		}
	}
	if !found {
		t.Fatalf("coverage_gap diagnostic must still be recorded (after rollback): %+v", listReceiptsOrFail(t, id))
	}
}

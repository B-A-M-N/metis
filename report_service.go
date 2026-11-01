package main

// report_service.go: report admission and the submission transaction.
//
// A report is a worker's claim; filing one does not imply
// verification (verdicts live in verification.go). Evidence backing
// the claim lives in artifact_store.go; the acknowledgment trail
// lives in task_receipts.go; retrieval and comparison live in
// report_projection.go.

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
)

// reportTxProbe is a test-only hook invoked inside the submission
// transaction immediately after the authoritative task re-read. It
// lets a test land a concurrent retry/lifecycle change between the
// read and the commit — the exact window the attempt fence must
// defeat. Production code never sets it.
var reportTxProbe func(tx *sql.Tx)

// submitReport files a report with its provenance attached; it does
// not imply verification. Only dispatched/working tasks can report.
// Each report is filed under its attempt and kept forever (TD-087):
// one report per attempt, so a second report at the same attempt is
// refused instead of overwriting the first claim.
//
// Provenance (remediation §1 P0): the history row keeps actor as
// the worker session owning the attempt (TD-092) but records
// reporter/source for who actually filed the text and what it is.
// A CLI submission is labelled as such and can never acquire worker
// attribution — the receipt names the reporter explicitly.
//
// Atomicity: the authoritative task revision (status + attempt),
// the duplicate check, the coverage checkpoint, the ledger
// transition, and the history insert run in ONE transaction on ONE
// connection. The task row is re-read INSIDE the transaction after
// BEGIN: a retry or lifecycle change landing between any earlier
// read and this commit must not file under a stale attempt. The
// duplicate check runs first on that authoritative revision: a
// refused report rolls back before any duty is superseded or
// required, so a rejected digest never becomes the coverage
// ledger's review target (P0 report/coverage transaction defect).
// The ledger UPDATE is fenced on the expected attempt as well as
// the dispatchable statuses, so a concurrent retry that bumps the
// attempt defeats the commit instead of being filed under. A failed
// insert can no longer leave the task report_ready with no history
// row, and a refused duplicate can no longer leave r1 filed while
// the ledger points at r2. The coverage-gap diagnostic is recorded
// only AFTER the rollback: it opens its own connection, and
// recording it while this transaction still holds its lock wedges
// the diagnostic on our own write lock under contention.
// submitReport files a report with its provenance attached. It is
// the legacy adapter: no request context travels with the bare
// filing, so the observed request ID stays empty rather than
// inventing one. Attempt and session still resolve below from the
// authoritative re-read and the bound turn intent.
func submitReport(id string, filing ReportFiling) (opErr error) {
	return submitReportWithRequest(id, filing, OperationRequest{})
}

// submitReportWithRequest files a report carrying the caller's real
// request identity into the report-phase observation. The task ID
// comes from the path, the attempt from the authoritative
// in-transaction re-read, and the session from the turn intent
// bound to that attempt — nothing here is synthesized. Call sites
// with a request (worker loop, MCP, future CLI wiring) use this;
// bare filings use submitReport.
func submitReportWithRequest(id string, filing ReportFiling, req OperationRequest) (opErr error) {
	var attempt int
	var session string
	defer func() { observeOps(opsPhaseReport, req.RequestID, id, attempt, session, opErr) }()
	if err := filing.Validate(); err != nil {
		return err
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(filing.Report)))
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureLifecycleTables(db); err != nil {
		return err
	}
	if err := ensureObligationTable(db); err != nil {
		return err
	}
	if err := ensureReceiptsTable(db); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Authoritative re-read INSIDE the transaction: status, attempt,
	// and the run/agent scope the coverage checkpoint needs. Any
	// earlier snapshot is stale by definition once a concurrent
	// retry or lifecycle change commits.
	var curStatus, curRun, curAgent string
	var curAttempt int
	if err := tx.QueryRow(`SELECT status, attempt, run, agent FROM tasks WHERE id = ?`, id).Scan(
		&curStatus, &curAttempt, &curRun, &curAgent); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("no such task: %s", id)
		}
		return err
	}
	// Authoritative identity for the report-phase observation: the
	// attempt just re-read inside the transaction, and the native
	// session bound to it (CLI/subprocess execution leaves no
	// intent, so the session honestly stays empty there). The
	// intent read rides this same transaction: a second connection
	// here would wedge (its ensure-table write blocks on our read
	// lock), while our own SELECT cannot contend with itself. A
	// missing intent table or row leaves the session empty.
	attempt = curAttempt
	_ = tx.QueryRow(`SELECT native_id FROM muse_turn_intents WHERE task_id = ? AND attempt = ?`,
		id, curAttempt).Scan(&session)
	if reportTxProbe != nil {
		reportTxProbe(tx)
	}
	// Duplicate check FIRST, on the same tx and the authoritative
	// revision: nothing below may mutate coverage or the ledger
	// for a report that is refused.
	var dup int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM task_reports WHERE task_id = ? AND attempt_no = ?`,
		id, curAttempt).Scan(&dup); err != nil {
		return err
	}
	if dup > 0 {
		return fmt.Errorf("task %s already has a report for attempt %d; retry or follow up instead of re-reporting (TD-087)", id, curAttempt)
	}
	authTask := Task{ID: id, Run: curRun, Agent: curAgent, Status: curStatus, Attempt: curAttempt}
	// Coverage generation runs INSIDE the report transaction at the
	// authoritative revision: a new revision supersedes outstanding
	// duties pointing at older digests, and matching policy rules
	// require fresh duties at this digest. On generation failure
	// the report is refused — a filed-but-uncovered report would
	// slip past verification — and the rollback unwrites every duty
	// touched above, so the refusal leaves coverage exactly as it
	// found it. The transaction rolls back BEFORE the best-effort
	// diagnostic is recorded: the diagnostic opens its own
	// connection and would wedge on this transaction's own lock
	// under write contention.
	if gerr := generateCoverageObligationsTx(tx, authTask, digest, OperationRequest{
		Actor:     strings.TrimSpace(filing.Reporter),
		RequestID: newRequestID(),
	}); gerr != nil {
		_ = tx.Rollback()
		recordReceipt(id, "coverage_gap", fmt.Sprintf("report refused: coverage generation failed: %v", gerr))
		return gerr
	}
	res, err := tx.Exec(`UPDATE tasks SET status = ?, updated = ?, report = ? WHERE id = ? AND status IN ('dispatched','working') AND attempt = ?`,
		TaskReportReady, nowUTC(), filing.Report, id, curAttempt)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		var cur string
		var curA int
		if qerr := tx.QueryRow(`SELECT status, attempt FROM tasks WHERE id = ?`, id).Scan(&cur, &curA); qerr != nil {
			return fmt.Errorf("no such task: %s", id)
		} else if curA != curAttempt {
			return fmt.Errorf("stale report for task %s: task is at attempt %d, report names attempt %d", id, curA, curAttempt)
		} else if terminalTaskStatus(cur) {
			return fmt.Errorf("task %s is terminal (%s); use followup for more work", id, cur)
		}
		return fmt.Errorf("task %s is %s; only a dispatched or working task can report", id, cur)
	}
	if _, err := tx.Exec(`INSERT INTO task_reports (task_id, attempt_no, report, digest, actor, reporter, source, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, curAttempt, filing.Report, digest, curAgent, strings.TrimSpace(filing.Reporter), string(filing.Source), nowUTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	recordReceipt(id, "report_received", fmt.Sprintf("%s reporter=%s source=%s", digest, strings.TrimSpace(filing.Reporter), string(filing.Source)))
	// Worker reply for the Muse feed: payload carries the digest
	// reference; the full report text lives in task_reports.
	// MuseSession joins the report to the native session that
	// executed this attempt (turn intent, resolved above for the
	// observation), so the feed can trace a report — and any later
	// child turn — to its native identity. A task with no native
	// turn (CLI/subprocess execution) leaves the column empty
	// rather than fabricating one.
	if _, err := recordOrchEvent(OrchEvent{
		RunID: curRun, OriginPrincipal: strings.TrimSpace(filing.Reporter),
		MuseSession: session,
		TargetAgent: curAgent, TaskID: id, Attempt: curAttempt,
		EventType:   OrchWorkerReported,
		Payload:     fmt.Sprintf("digest=%s source=%s", digest, string(filing.Source)),
		Disposition: "filed",
	}); err != nil {
		recordReceipt(id, "orch_gap", fmt.Sprintf("worker.reported feed write failed: %v", err))
	}
	return nil
}

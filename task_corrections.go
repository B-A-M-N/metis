package main

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
)

// task_corrections.go: follow-up creation and correction budgets.
//
// Linked correction tasks preserving history; the parent chain is
// never rewritten.

// followupTask creates a new pending corrective task linked to a prior
// one. The correction carries the parent id, its depth in the chain,
// and a digest snapshot of the exact parent report it must address
// (TD-082); creation beyond the correction budget is refused (TD-036).
// The caller's request is required so the intervention is attributable
// (TD-006, TD-084).
func followupTask(id, objective string, req OperationRequest) (Task, error) {
	if err := requireRequest(req); err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(objective) == "" {
		return Task{}, fmt.Errorf("followup requires an objective")
	}
	prev, err := getTask(id)
	if err != nil {
		return Task{}, err
	}
	depth := prev.CorrectionNo + 1
	if depth > maxCorrectionDepth {
		return Task{}, fmt.Errorf("correction budget exhausted: task %s is already at depth %d (max %d); start a new run or override explicitly (TD-036)", id, prev.CorrectionNo, maxCorrectionDepth)
	}
	digest := ""
	if prev.Report != "" {
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(prev.Report)))
	}
	t := Task{
		ID: newTaskID(), Run: prev.Run, Agent: prev.Agent,
		Objective: objective, Acceptance: prev.Acceptance,
		Status: TaskPending, Parent: prev.ID, Created: nowUTC(), Updated: nowUTC(),
		CorrectionNo: depth, ParentReport: digest,
	}
	db, err := openTasksDB()
	if err != nil {
		return Task{}, err
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO tasks (id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?, '', ?, ?, '', 0)`,
		t.ID, t.Run, t.Agent, t.Objective, t.Acceptance, t.Status, t.Parent, t.Created, t.Updated, t.CorrectionNo, t.ParentReport)
	if err != nil {
		return Task{}, err
	}
	recordReceipt(t.ID, "correction_created", fmt.Sprintf("actor=%s req=%s parent=%s depth=%d parent_report=%s", req.Actor, req.RequestID, prev.ID, depth, digest))
	// The correction names the exact prior evidence it must address
	// (CB-14): every parent artifact with its filed digest, or an
	// explicit statement that the parent filed none.
	basis := fmt.Sprintf("actor=%s req=%s parent=%s parent_report=%s artifacts=none", req.Actor, req.RequestID, prev.ID, digest)
	if parts, berr := correctionBasis(prev.ID); berr == nil && len(parts) > 0 {
		basis = fmt.Sprintf("actor=%s req=%s parent=%s parent_report=%s artifacts=[%s]", req.Actor, req.RequestID, prev.ID, digest, strings.Join(parts, ", "))
	}
	recordReceipt(t.ID, "correction_basis", basis)
	// Correction instruction for the Muse feed: exact objective text
	// plus the parent binding it must address.
	if _, err := recordOrchEvent(OrchEvent{
		RunID: t.Run, OriginPrincipal: req.Actor, TargetAgent: t.Agent,
		TaskID: t.ID, Attempt: 0, EventType: OrchCorrectionRequested,
		RequestID: req.RequestID,
		Payload: fmt.Sprintf("parent=%s depth=%d parent_report=%s objective=%s",
			prev.ID, depth, digest, objective),
		Disposition: "requested",
	}); err != nil {
		recordReceipt(t.ID, "orch_gap", fmt.Sprintf("correction.requested feed write failed: %v", err))
	}
	return t, nil
}

// followupTaskIdempotent creates the lineage-linked correction task
// AND owns the caller's idempotency key in ONE transaction on ONE
// connection.
//
// BEGIN IMMEDIATE takes the write lock up front; the key row is
// inserted first (carrying the pre-generated correction ID), then
// the source is validated and the correction inserted, then both
// commit together. A concurrent same-key request either blocks on
// the lock and then reads the winner (converge on an identical
// (source, objective), *correctKeyConflict on a different one) or
// loses the key race outright — either way it never forks a second
// correction. Any failure before COMMIT rolls back both rows.
//
// Advisory receipts and the orchestration feed write land after the
// commit, exactly as in followupTask: they are best-effort and never
// fail the operation they record.
func followupTaskIdempotent(id, objective string, req OperationRequest, requestKey string) (correctIdemOutcome, error) {
	if err := requireRequest(req); err != nil {
		return correctIdemOutcome{}, err
	}
	if strings.TrimSpace(objective) == "" {
		return correctIdemOutcome{}, fmt.Errorf("followup requires an objective")
	}
	db, err := openTasksDB()
	if err != nil {
		return correctIdemOutcome{}, err
	}
	defer db.Close()
	if err := ensureMcpCorrectTable(db); err != nil {
		return correctIdemOutcome{}, err
	}
	// BEGIN IMMEDIATE takes the write lock up front: two
	// concurrent same-key corrections serialize here instead of
	// both forking a task before one loses the key race. The
	// handle is single-connection, so every statement below rides
	// the same transaction; db.Close rolls back an uncommitted one.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return correctIdemOutcome{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	// Authoritative source read inside the transaction: a row
	// observed before the lock is advisory, not authoritative.
	var prevRun, prevAgent, prevAcceptance, prevReport string
	var prevCorrectionNo int
	if err := db.QueryRow(`SELECT run, agent, acceptance, report, correction_no FROM tasks WHERE id = ?`,
		id).Scan(&prevRun, &prevAgent, &prevAcceptance, &prevReport, &prevCorrectionNo); err == sql.ErrNoRows {
		return correctIdemOutcome{}, fmt.Errorf("no such task: %s", id)
	} else if err != nil {
		return correctIdemOutcome{}, err
	}
	depth := prevCorrectionNo + 1
	if depth > maxCorrectionDepth {
		return correctIdemOutcome{}, fmt.Errorf("correction budget exhausted: task %s is already at depth %d (max %d); start a new run or override explicitly (TD-036)", id, prevCorrectionNo, maxCorrectionDepth)
	}
	digest := ""
	if prevReport != "" {
		digest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(prevReport)))
	}
	t := Task{
		ID: newTaskID(), Run: prevRun, Agent: prevAgent,
		Objective: objective, Acceptance: prevAcceptance,
		Status: TaskPending, Parent: id, Created: nowUTC(), Updated: nowUTC(),
		CorrectionNo: depth, ParentReport: digest,
	}
	// Claim the key first, carrying the pre-generated correction
	// ID. A lost key race reads the winner below and converges or
	// conflicts WITHOUT forking a second correction.
	if _, err := db.Exec(`INSERT INTO mcp_correct (request_id, task_id, created) VALUES (?, ?, ?)`,
		requestKey, t.ID, nowUTC()); err != nil {
		var winner string
		if qerr := db.QueryRow(`SELECT task_id FROM mcp_correct WHERE request_id = ?`, requestKey).Scan(&winner); qerr != nil {
			return correctIdemOutcome{}, err
		}
		var wParent, wObjective, wRun string
		if qerr := db.QueryRow(`SELECT parent, objective, run FROM tasks WHERE id = ?`, winner).Scan(&wParent, &wObjective, &wRun); qerr == sql.ErrNoRows {
			// The winner's task row is gone; only the key
			// survives, so report the key alone.
			return correctIdemOutcome{TaskID: winner, Replay: true, Bare: true}, nil
		} else if qerr != nil {
			return correctIdemOutcome{}, qerr
		}
		if wParent != id || wObjective != objective {
			return correctIdemOutcome{}, &correctKeyConflict{RequestKey: requestKey, WinnerTask: winner, WinnerParent: wParent}
		}
		// Identical replay: converge on the winner. The deferred
		// ROLLBACK ends the transaction (the losing INSERT staged
		// nothing); the winner's commit already holds the task.
		return correctIdemOutcome{TaskID: winner, Parent: wParent, Run: wRun, Replay: true}, nil
	}
	if _, err := db.Exec(`INSERT INTO tasks (id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?, '', ?, ?, '', 0)`,
		t.ID, t.Run, t.Agent, t.Objective, t.Acceptance, t.Status, t.Parent, t.Created, t.Updated, t.CorrectionNo, t.ParentReport); err != nil {
		return correctIdemOutcome{}, err
	}
	if mcpIdemInjectTxFailure != nil {
		return correctIdemOutcome{}, mcpIdemInjectTxFailure
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return correctIdemOutcome{}, err
	}
	committed = true
	out := correctIdemOutcome{TaskID: t.ID, Parent: t.Parent, Run: t.Run, Replay: false}
	recordReceipt(t.ID, "correction_created", fmt.Sprintf("actor=%s req=%s parent=%s depth=%d parent_report=%s", req.Actor, req.RequestID, t.Parent, depth, digest))
	// The correction names the exact prior evidence it must address
	// (CB-14): every parent artifact with its filed digest, or an
	// explicit statement that the parent filed none.
	basis := fmt.Sprintf("actor=%s req=%s parent=%s parent_report=%s artifacts=none", req.Actor, req.RequestID, t.Parent, digest)
	if parts, berr := correctionBasis(t.Parent); berr == nil && len(parts) > 0 {
		basis = fmt.Sprintf("actor=%s req=%s parent=%s parent_report=%s artifacts=[%s]", req.Actor, req.RequestID, t.Parent, digest, strings.Join(parts, ", "))
	}
	recordReceipt(t.ID, "correction_basis", basis)
	// Correction instruction for the Muse feed: exact objective text
	// plus the parent binding it must address.
	if _, err := recordOrchEvent(OrchEvent{
		RunID: t.Run, OriginPrincipal: req.Actor, TargetAgent: t.Agent,
		TaskID: t.ID, Attempt: 0, EventType: OrchCorrectionRequested,
		RequestID: req.RequestID,
		Payload: fmt.Sprintf("parent=%s depth=%d parent_report=%s objective=%s",
			t.Parent, depth, digest, objective),
		Disposition: "requested",
	}); err != nil {
		recordReceipt(t.ID, "orch_gap", fmt.Sprintf("correction.requested feed write failed: %v", err))
	}
	return out, nil
}

// correctionBasis lists the parent task's filed evidence as
// path=digest pairs across all attempts, so the correction addresses
// the exact snapshots the parent produced.
func correctionBasis(parentID string) ([]string, error) {
	arts, err := listArtifacts(parentID)
	if err != nil {
		return nil, err
	}
	var parts []string
	for _, a := range arts {
		parts = append(parts, fmt.Sprintf("attempt-%d:%s=%s", a.AttemptNo, a.Path, a.Digest))
	}
	return parts, nil
}

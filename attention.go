package main

// attention.go: durable attention requests for cross-session
// orchestration decisions Metis itself cannot take.
//
// Native Muse tool approvals belong to Muse (muse_approvals.go).
// Attention is different: uncertain turn outcomes, conflicting
// findings, repeated failure, missing signoff, strategic replan.
// Muse (or the operator) reasons; Metis records the request, the
// evidence refs, and the resolution — open, resolved, dismissed —
// with a revision guard so two resolvers cannot close the same
// request on different understandings.
//
// Metis never resolves attention by itself and never treats an
// open request as permission to advance the run.
//
// Approval-scoped escalation (raiseApprovalAttention,
// resolveApprovalAttention) lives in attention_approval.go; this
// file owns only generic persistence and revision-fenced
// transitions.

import (
	"database/sql"
	"fmt"
)

// attentionKinds are the only admissible escalation kinds. A
// free-form kind would let callers smuggle new decision types
// past review.
var attentionKinds = map[string]bool{
	"uncertain_outcome":    true,
	"repeated_failure":     true,
	"conflicting_findings": true,
	"missing_approval":     true,
	"replan":               true,
	"signoff":              true,
}

// attention is one durable escalation request.
type attention struct {
	ID         string `json:"id"`
	RunID      string `json:"run_id"`
	TaskID     string `json:"task_id"`
	Session    string `json:"session"`
	Kind       string `json:"kind"`
	Status     string `json:"status"` // open | resolved | dismissed
	Revision   int    `json:"revision"`
	Reason     string `json:"reason"`
	Evidence   string `json:"evidence_refs"`
	OpenedBy   string `json:"opened_by"`
	OpenedAt   string `json:"opened_at"`
	ResolvedBy string `json:"resolved_by"`
	ResolvedAt string `json:"resolved_at"`
	Outcome    string `json:"outcome"`
	RequestID  string `json:"request_id"`
}

func ensureAttentionTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS metis_attention (
		id TEXT NOT NULL PRIMARY KEY,
		run_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		session TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'open',
		revision INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		evidence_refs TEXT NOT NULL DEFAULT '[]',
		opened_by TEXT NOT NULL DEFAULT '',
		opened_at TEXT NOT NULL DEFAULT '',
		resolved_by TEXT NOT NULL DEFAULT '',
		resolved_at TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		request_id TEXT NOT NULL DEFAULT ''
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS metis_attention_req
		ON metis_attention (request_id)`)
	return err
}

// openAttention files an escalation. The caller's request ID is
// the idempotency key: the same ID with the same payload returns
// the existing row; the same ID with different payload is a
// conflict, never an overwrite.
func openAttention(req OperationRequest, runID, taskID, session, kind, reason, evidence string) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if !attentionKinds[kind] {
		return "", fmt.Errorf("unknown attention kind %q", kind)
	}
	if reason == "" {
		return "", fmt.Errorf("attention requires a reason")
	}
	if evidence == "" {
		evidence = "[]"
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureAttentionTable(db); err != nil {
		return "", err
	}
	var id, status, k, r, erun, etask, esess, eev string
	qerr := db.QueryRow(`SELECT id, status, kind, reason, run_id, task_id, session, evidence_refs
		FROM metis_attention WHERE request_id = ?`, req.RequestID).Scan(
		&id, &status, &k, &r, &erun, &etask, &esess, &eev)
	if qerr == nil {
		if k != kind || r != reason || erun != runID || etask != taskID || esess != session || eev != evidence {
			return "", fmt.Errorf("request id %q already opened attention %s with different payload; refusing overwrite",
				req.RequestID, id)
		}
		return id, nil
	}
	if qerr != sql.ErrNoRows {
		return "", qerr
	}
	id, err = newMuseCommandID()
	if err != nil {
		return "", err
	}
	if _, err := db.Exec(`INSERT INTO metis_attention
		(id, run_id, task_id, session, kind, status, revision, reason,
		 evidence_refs, opened_by, opened_at, request_id)
		VALUES (?, ?, ?, ?, ?, 'open', 0, ?, ?, ?, ?, ?)`,
		id, runID, taskID, session, kind, reason, evidence,
		req.Actor, nowUTC(), req.RequestID); err != nil {
		return "", err
	}
	recordReceipt(runID, "attention_opened",
		fmt.Sprintf("actor=%s req=%s attention=%s kind=%s task=%s", req.Actor, req.RequestID, id, kind, taskID))
	return id, nil
}

// raiseAttention files (or reuses) one bounded escalation for a
// genuinely actionable condition, called from authoritative
// transitions — never from polls or projections. The request ID is
// derived from the condition identity (kind, task, attempt,
// session), so repeated observations collapse to the existing row
// instead of opening a new one per poll. Reason and evidence must
// be stable for the identity (no error text, timestamps, or
// attempt-scoped noise): varying detail belongs in receipts, and a
// changed payload under the same identity is a conflict, never an
// overwrite. Resolving records a decision; it never launches work
// — dispatch stays on the task admission boundary.
func raiseAttention(runID, taskID string, attempt int, session, kind, reason, evidence string) (string, error) {
	if evidence == "" {
		evidence = "[]"
	}
	req := OperationRequest{
		Actor:     "metis-control-plane",
		RequestID: fmt.Sprintf("attention:%s:%s:%d:%s", kind, taskID, attempt, session),
	}
	return openAttention(req, runID, taskID, session, kind, reason, evidence)
}

// getAttention reads one request by id.
func getAttention(id string) (attention, error) {
	var a attention
	db, err := openTasksDB()
	if err != nil {
		return a, err
	}
	defer db.Close()
	if err := ensureAttentionTable(db); err != nil {
		return a, err
	}
	err = db.QueryRow(`SELECT id, run_id, task_id, session, kind, status,
		revision, reason, evidence_refs, opened_by, opened_at,
		resolved_by, resolved_at, outcome, request_id
		FROM metis_attention WHERE id = ?`, id).Scan(
		&a.ID, &a.RunID, &a.TaskID, &a.Session, &a.Kind, &a.Status,
		&a.Revision, &a.Reason, &a.Evidence, &a.OpenedBy, &a.OpenedAt,
		&a.ResolvedBy, &a.ResolvedAt, &a.Outcome, &a.RequestID)
	return a, err
}

// closeAttention is the shared resolve/dismiss path: only an
// open request may transition, and only at the expected
// revision. The winner is the single row updated from open.
func closeAttention(id string, expectedRevision int, by, outcome, to string) error {
	if outcome == "" {
		return fmt.Errorf("attention %s: closing requires an outcome note", id)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureAttentionTable(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE metis_attention
		SET status = ?, revision = revision + 1, resolved_by = ?,
			resolved_at = ?, outcome = ?
		WHERE id = ? AND status = 'open' AND revision = ?`,
		to, by, nowUTC(), outcome, id, expectedRevision)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		cur, gerr := getAttention(id)
		if gerr != nil {
			return fmt.Errorf("attention %s cannot close: %v", id, gerr)
		}
		if cur.Status != "open" {
			return fmt.Errorf("attention %s is %s, not open", id, cur.Status)
		}
		return fmt.Errorf("attention %s at revision %d, not %d; re-read before closing",
			id, cur.Revision, expectedRevision)
	}
	recordReceipt(curRunOf(id, db), "attention_"+to,
		fmt.Sprintf("by=%s attention=%s outcome=%s", by, id, outcome))
	return nil
}

// resolveAttention marks an open request resolved with the
// corrective decision recorded in outcome.
func resolveAttention(id string, expectedRevision int, by, outcome string) error {
	return closeAttention(id, expectedRevision, by, outcome, "resolved")
}

// dismissAttention marks an open request dismissed as moot.
func dismissAttention(id string, expectedRevision int, by, outcome string) error {
	return closeAttention(id, expectedRevision, by, outcome, "dismissed")
}

// listAttention reads requests for a run; openOnly drops
// resolved and dismissed rows from the projection.
func listAttention(runID string, openOnly bool) ([]attention, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureAttentionTable(db); err != nil {
		return nil, err
	}
	q := `SELECT id, run_id, task_id, session, kind, status,
		revision, reason, evidence_refs, opened_by, opened_at,
		resolved_by, resolved_at, outcome, request_id
		FROM metis_attention WHERE run_id = ?`
	if openOnly {
		q += ` AND status = 'open'`
	}
	// rowid tiebreak: attention ids carry a random suffix, so
	// same-second rows must keep insertion order, not id order.
	q += ` ORDER BY opened_at, rowid`
	rows, err := db.Query(q, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []attention
	for rows.Next() {
		var a attention
		if err := rows.Scan(&a.ID, &a.RunID, &a.TaskID, &a.Session,
			&a.Kind, &a.Status, &a.Revision, &a.Reason, &a.Evidence,
			&a.OpenedBy, &a.OpenedAt, &a.ResolvedBy, &a.ResolvedAt,
			&a.Outcome, &a.RequestID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// curRunOf re-reads the run for receipting after close.
func curRunOf(id string, db *sql.DB) string {
	var run string
	_ = db.QueryRow(`SELECT run_id FROM metis_attention WHERE id = ?`, id).Scan(&run)
	return run
}

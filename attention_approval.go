package main

import (
	"fmt"
)

// attention_approval.go: approval-scoped attention escalation.
//
// A pending native approval is a must-answer tool request; without
// this producer it would sit silently in the approval rows. These
// helpers file one bounded missing_approval escalation per approval
// and close it once the approval is decided. They call the generic
// persistence and revision-fenced transitions in attention.go —
// openAttention, resolveAttention — and own no state themselves.
// Called from the authoritative record path, never from polls or
// projections.

// raiseApprovalAttention files one bounded escalation per pending
// native approval so a must-answer tool request cannot sit silently
// in the approval rows. Identity is approval-scoped: the request ID
// names the approval (plus the task where one is dispatched), and
// the reason/evidence carry only stable identity. Choices and fold
// payloads change across observations and must not be part of the
// payload, or re-observed folds would conflict instead of reusing
// the row. Called from the authoritative record path, never from
// polls or projections.
func raiseApprovalAttention(session, approvalID string) error {
	reason := fmt.Sprintf("approval %s for session %s needs a decision", approvalID, session)
	evidence := fmt.Sprintf(`["approval:%s"]`, approvalID)
	ids, err := dispatchedTasksForSession(session)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		req := OperationRequest{Actor: "metis-control-plane",
			RequestID: "attention:missing_approval:" + approvalID + ":" + session}
		_, err := openAttention(req, "", "", session, "missing_approval", reason, evidence)
		return err
	}
	for _, tid := range ids {
		t, err := getTask(tid)
		if err != nil {
			return err
		}
		req := OperationRequest{Actor: "metis-control-plane",
			RequestID: fmt.Sprintf("attention:missing_approval:%s:%s", approvalID, tid)}
		if _, err := openAttention(req, t.Run, tid, session, "missing_approval", reason, evidence); err != nil {
			return err
		}
	}
	return nil
}

// resolveApprovalAttention closes open escalations filed for one
// approval once the approval is decided or resolved. Best-effort:
// the decision stands on its own row, so a stale open escalation
// is operator noise, never a reason to fail the decision itself.
func resolveApprovalAttention(approvalID, by, outcome string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	if err := ensureAttentionTable(db); err != nil {
		return
	}
	prefix := "attention:missing_approval:" + approvalID + ":"
	rows, err := db.Query(`SELECT id, revision, request_id FROM metis_attention
		WHERE kind = 'missing_approval' AND status = 'open'`)
	if err != nil {
		return
	}
	defer rows.Close()
	type pending struct {
		id  string
		rev int
		req string
	}
	var open []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.rev, &p.req); err != nil {
			return
		}
		open = append(open, p)
	}
	if err := rows.Err(); err != nil {
		return
	}
	for _, p := range open {
		if len(p.req) < len(prefix) || p.req[:len(prefix)] != prefix {
			continue
		}
		_ = resolveAttention(p.id, p.rev, by, outcome)
	}
}

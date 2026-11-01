package main

// approval_decision.go: approval decision service.
//
// Validates a proposed decision against the durable row before any
// infrastructure is touched: known approval, still pending, choice
// among those offered. No mutation, no wire. The command id is bound
// to the exact decision payload (choice + feedback): an identical
// retry reuses the id for server-side dedup, while a conflicting
// retry mints a fresh one — reusing an id bound to a different
// decision would either be rejected or, worse, decide something
// unintended.
//
// Authority: approval decisions require the local operator. A
// remote principal may assert any actor name, so it is never
// authority. This layer validates; the MSP transport adapter
// (approval_transport.go) only carries the validated decision.

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// museLoadPendingApproval validates a decision against the durable
// row before any infrastructure is touched: known approval, still
// pending, choice among those offered. No mutation, no wire.
func museLoadPendingApproval(session, approvalID, choiceID string) (museApproval, error) {
	var zero museApproval
	db, err := openTasksDB()
	if err != nil {
		return zero, err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return zero, err
	}
	rows, err := db.Query(`SELECT session, native_id, approval_id, item_id, requirement_id, choices,
		decide_command_id, status, decision, decided_by, requested_at, resolved_at
		FROM muse_approvals WHERE session = ? AND approval_id = ?`, session, approvalID)
	if err != nil {
		return zero, err
	}
	defer rows.Close()
	if !rows.Next() {
		return zero, fmt.Errorf("no pending approval %q for session %s", approvalID, session)
	}
	var a museApproval
	var choicesRaw, reqRaw string
	if err := rows.Scan(&a.Session, &a.NativeID, &a.ApprovalID, &a.ItemID,
		&reqRaw, &choicesRaw, &a.DecideCommandID, &a.Status, &a.Decision, &a.DecidedBy,
		&a.RequestedAt, &a.ResolvedAt); err != nil {
		return zero, err
	}
	if err := rows.Err(); err != nil {
		return zero, err
	}
	if a.Status != "pending" {
		return zero, fmt.Errorf("approval %q is %s, not pending", approvalID, a.Status)
	}
	a.RequirementID = reqRaw
	if err := json.Unmarshal([]byte(choicesRaw), &a.Choices); err != nil {
		return zero, fmt.Errorf("muse approval choices: %w", err)
	}
	for _, c := range a.Choices {
		if c.ChoiceID == choiceID {
			return a, nil
		}
	}
	return zero, fmt.Errorf("choice %q was not offered for approval %q", choiceID, approvalID)
}

// bindDecideCommand returns the command id bound to this exact
// decision payload, persisting a fresh UUIDv7 before transmit when
// no id exists or the stored id is bound to a different payload. A
// crash between send and record then retries value-identical
// instead of double-deciding.
func bindDecideCommand(db *sql.DB, session, approvalID, payload, storedCmd, storedPayload string) (string, error) {
	cmdID := storedCmd
	if cmdID == "" || storedPayload != payload {
		var err error
		cmdID, err = newMuseCommandID()
		if err != nil {
			return "", err
		}
		if _, err := db.Exec(`UPDATE muse_approvals SET decide_command_id = ?, decide_payload = ?
			WHERE session = ? AND approval_id = ? AND status = 'pending'`,
			cmdID, payload, session, approvalID); err != nil {
			return "", err
		}
	}
	return cmdID, nil
}

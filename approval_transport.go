package main

// approval_transport.go: MSP transport adapter for approval decisions.
//
// Carries one validated decision to the provider over MSP and
// records the outcome. It performs no validation of its own
// beyond the shared request gate: the decision service
// (approval_decision.go) validates actor authority, pending
// identity, offered choices, and idempotency before any byte
// moves. The stale requirement guard (requirementId) rides along
// as the race guard; a -32053/precondition rejection surfaces
// with the row left pending so the next fold heals the guard.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// musePendingFold is the lease-free server read: pending approvals
// plus open user-input prompts for one native session.
type musePendingFold struct {
	Approvals  []map[string]any `json:"approvals"`
	UserInputs []map[string]any `json:"userInputs"`
}

// museListPendingApprovals asks the host for the current pending
// fold. Point-in-time by construction — decide with the
// requirementId guard regardless of how the state was learned.
func museListPendingApprovals(ctx context.Context, h *museHost, nativeID string) (musePendingFold, error) {
	var out musePendingFold
	raw, err := h.call(ctx, "approval/listPending", map[string]any{"sessionId": nativeID})
	if err != nil {
		return out, err
	}
	var res struct {
		Approvals  []map[string]any `json:"approvals"`
		UserInputs []map[string]any `json:"userInputs"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return out, fmt.Errorf("approval/listPending: %w", err)
	}
	out.Approvals = res.Approvals
	out.UserInputs = res.UserInputs
	if out.Approvals == nil {
		out.Approvals = []map[string]any{}
	}
	if out.UserInputs == nil {
		out.UserInputs = []map[string]any{}
	}
	return out, nil
}

// museDecideApprovalOnHost sends one offered choice for a pending
// approval the row already knows. Local gates run before any byte
// moves: unknown approval, non-pending row, and unoffered choice
// are refused without touching the wire.
func museDecideApprovalOnHost(ctx context.Context, h *museHost, session, nativeID, approvalID, choiceID, feedback string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return err
	}
	var status, reqRaw, choicesRaw, storedCmd, storedPayload string
	err = db.QueryRow(`SELECT status, requirement_id, choices, decide_command_id, decide_payload
		FROM muse_approvals WHERE session = ? AND approval_id = ?`,
		session, approvalID).Scan(&status, &reqRaw, &choicesRaw, &storedCmd, &storedPayload)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no pending approval %q for session %s", approvalID, session)
	}
	if err != nil {
		return err
	}
	if status != "pending" {
		return fmt.Errorf("approval %q is %s, not pending", approvalID, status)
	}
	var choices []museFoldChoice
	if err := json.Unmarshal([]byte(choicesRaw), &choices); err != nil {
		return fmt.Errorf("muse approval choices: %w", err)
	}
	offered := false
	for _, c := range choices {
		if c.ChoiceID == choiceID {
			offered = true
			break
		}
	}
	if choiceID == "" || !offered {
		return fmt.Errorf("choice %q was not offered for approval %q", choiceID, approvalID)
	}
	payload := choiceID + "\n" + feedback
	cmdID, err := bindDecideCommand(db, session, approvalID, payload, storedCmd, storedPayload)
	if err != nil {
		return err
	}
	params := map[string]any{
		"approvalId": approvalID, "choiceId": choiceID, "commandId": cmdID,
	}
	if feedback != "" {
		params["feedback"] = feedback
	}
	if strings.TrimSpace(reqRaw) != "" {
		var guard any
		if err := json.Unmarshal([]byte(reqRaw), &guard); err == nil {
			params["requirementId"] = guard
		}
	}
	if _, err := h.Call(ctx, "approval/decide", params); err != nil {
		if merr, ok := err.(*museError); ok && merr.Code == -32053 {
			// Stale guard: the payload must change (fresh fold),
			// so the stored commandId must not be reused.
			_, _ = db.Exec(`UPDATE muse_approvals SET decide_command_id = '', decide_payload = ''
				WHERE session = ? AND approval_id = ?`, session, approvalID)
		}
		return fmt.Errorf("approval/decide: %w", err)
	}
	res, err := db.Exec(`UPDATE muse_approvals SET decision = ?, decided_by = ?
		WHERE session = ? AND approval_id = ? AND status = 'pending'`,
		choiceID, cmdID, session, approvalID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("approval %q left pending while deciding (resolved concurrently?)", approvalID)
	}
	recordReceipt(session, "muse_approval_decided",
		fmt.Sprintf("actor=%s req=%s approval=%s choice=%s cmd=%s", req.Actor, req.RequestID, approvalID, choiceID, cmdID))
	resolveApprovalAttention(approvalID, req.Actor, "decided:"+choiceID)
	return nil
}

// museApproveViaBinding is the shared decide path for CLI and MCP:
// binding scope check, durable-row validation, commander daemon
// gate, joined host, then the validated single-choice decision.
func museApproveViaBinding(session, provider, profile, approvalID, choiceID, feedback string, req OperationRequest) error {
	// Approval decisions are never available to remote
	// principals: the asserted actor name is not authority, and
	// no opt-in restores this path.
	if req.Remote {
		return fmt.Errorf("remote principal %q cannot decide approval %q; approval authority requires the local operator",
			req.Principal, approvalID)
	}
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return err
	}
	b, err := getMuseBinding(session)
	if err != nil {
		return err
	}
	if err := checkMuseBindingScope(b, provider, profile); err != nil {
		return err
	}
	if _, err := museLoadPendingApproval(session, approvalID, choiceID); err != nil {
		return err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return err
	}
	defer h.Close()
	return museDecideApprovalOnHost(ctx, h, session, b.NativeID, approvalID, choiceID, feedback, req)
}

package main

// approval_observer.go: provider-notification observer.
//
// Records server-initiated approval traffic into pending projections:
// the must-answer `approval/request` and the `approval/requested`
// fold both feed the same pending row keyed by approvalId, and
// `approval/resolved` closes the row with the server's terminal
// decision. `approval/listPending` is the lease-free server fold
// read here only as a projection source — deciding is slice B.
//
// Nothing here answers a must-answer request or decides anything:
// an unanswered request stays pending server-side and is re-issued
// on the next subscribe. Surfacing is not answering, and a receipt
// means durably recorded and operator-visible — never a decision
// and never a claim a human eyeball saw. A receipt that fails to
// send leaves the row recorded; the server re-issues on the next
// subscribe, which the stored row dedups by approvalId.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// museFoldChoice mirrors the wire ApprovalChoice tightly enough to
// preserve decide inputs: choiceId is what approval/decide takes,
// and only offered choices may be sent.
type museFoldChoice struct {
	ChoiceID string `json:"choiceId"`
	Label    string `json:"label"`
}

type museFoldParams struct {
	ApprovalID           string           `json:"approvalId"`
	ItemID               string           `json:"itemId"`
	CurrentRequirementID json.RawMessage  `json:"currentRequirementId"`
	AvailableChoices     []museFoldChoice `json:"availableChoices"`
}

// recordMuseApprovalRequested upserts the pending row for a fold
// (`approval/request` or `approval/requested` params). Re-requests
// refresh the requirement and choices; a resolved row is never
// reopened by a stale fold.
func recordMuseApprovalRequested(session, nativeID string, raw json.RawMessage) error {
	var p museFoldParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("muse approval fold: %w", err)
	}
	if p.ApprovalID == "" {
		return fmt.Errorf("muse approval fold without approvalId")
	}
	choices, err := json.Marshal(p.AvailableChoices)
	if err != nil {
		return fmt.Errorf("muse approval fold choices: %w", err)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return err
	}
	var status string
	err = db.QueryRow(`SELECT status FROM muse_approvals WHERE session = ? AND approval_id = ?`,
		session, p.ApprovalID).Scan(&status)
	switch {
	case err == sql.ErrNoRows:
		_, err = db.Exec(`INSERT INTO muse_approvals
			(session, native_id, approval_id, item_id, requirement_id, choices, status, requested_at)
			VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
			session, nativeID, p.ApprovalID, p.ItemID, string(p.CurrentRequirementID),
			string(choices), nowUTC())
		if err != nil {
			return err
		}
	case err != nil:
		return err
	case status == "resolved":
		return nil // stale fold; the terminal decision stands
	default:
		_, err = db.Exec(`UPDATE muse_approvals SET native_id = ?, item_id = ?,
			requirement_id = ?, choices = ?, requested_at = ?
			WHERE session = ? AND approval_id = ? AND status = 'pending'`,
			nativeID, p.ItemID, string(p.CurrentRequirementID),
			string(choices), nowUTC(), session, p.ApprovalID)
		if err != nil {
			return err
		}
	}
	// A pending native approval is a must-answer tool request: file
	// one bounded escalation so it cannot sit silently in the rows.
	// Re-observed folds carry the same approval identity and reuse
	// the row; the observer fails closed (no receipt) when the
	// escalation cannot be filed, so the server re-issues the fold.
	return raiseApprovalAttention(session, p.ApprovalID)
}

type museResolvedParams struct {
	ApprovalID         string `json:"approvalId"`
	Decision           any    `json:"decision"`
	DecidedByCommandID string `json:"decidedByCommandId"`
	DecidedAt          string `json:"decidedAt"`
}

// recordMuseApprovalResolved closes the row with the server's first
// durable terminal. Unknown approvals are refused — a resolution
// must match a request Metis actually observed.
func recordMuseApprovalResolved(session, approvalID string, raw json.RawMessage) error {
	if approvalID == "" {
		var p museResolvedParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("muse approval resolved: %w", err)
		}
		approvalID = p.ApprovalID
		if approvalID == "" {
			return fmt.Errorf("muse approval resolved without approvalId")
		}
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return err
	}
	var p museResolvedParams
	decision := ""
	decidedBy := ""
	decidedAt := ""
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err == nil {
			switch d := p.Decision.(type) {
			case string:
				decision = d
			case map[string]any:
				if s, _ := d["decision"].(string); s != "" {
					decision = s
				} else {
					decision = "decided"
				}
			default:
				decision = "decided"
			}
			decidedBy = p.DecidedByCommandID
			decidedAt = p.DecidedAt
		}
	}
	if decidedAt == "" {
		decidedAt = nowUTC()
	}
	res, err := db.Exec(`UPDATE muse_approvals SET status = 'resolved', decision = ?,
		decided_by = ?, resolved_at = ? WHERE session = ? AND approval_id = ? AND status = 'pending'`,
		decision, decidedBy, decidedAt, session, approvalID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("no pending approval %q for session %s", approvalID, session)
	}
	resolveApprovalAttention(approvalID, "metis-control-plane", "resolved:"+decision)
	return nil
}

// museObserveApproval routes one server-initiated approval message
// into the durable table. Unknown methods are not its business.
func museObserveApproval(n museNotification, session, nativeID string) error {
	switch n.Method {
	case "approval/request", "approval/requested", "approval/updated", "approval/reviewing":
		return recordMuseApprovalRequested(session, nativeID, n.Params)
	case "approval/resolved":
		var p museResolvedParams
		id := ""
		if err := json.Unmarshal(n.Params, &p); err == nil {
			id = p.ApprovalID
		}
		return recordMuseApprovalResolved(session, id, n.Params)
	default:
		return nil
	}
}

// museApprovalObserver returns a museWaitTurn observe func that
// durably records approval traffic for one session and answers
// must-answer requests once recorded. The receipt means: recorded
// durably and operator-visible (muse approvals / durable rows) —
// presentation to Metis's durable surface, never a decision and
// never a claim a human eyeball saw. A receipt that fails to
// send leaves the row recorded; the server re-issues on the next
// subscribe, which the stored row dedups by approvalId.
func museApprovalObserver(ctx context.Context, h *museHost, session, nativeID string) func(museNotification) {
	return func(n museNotification) {
		switch n.Method {
		case "approval/request", "approval/requested", "approval/updated", "approval/reviewing", "approval/resolved":
			if err := museObserveApproval(n, session, nativeID); err != nil {
				return // unrecorded: do not receipt what we did not keep
			}
			if n.RequestID != nil {
				_ = h.sendResult(*n.RequestID, map[string]any{})
			}
		}
	}
}

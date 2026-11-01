package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// guidance_store.go: guidance acceptance, settlement, recovery.
//
// Durable store for orchestrator guidance directed at sessions.

func scanGuidance(rows *sql.Rows) (Guidance, error) {
	var g Guidance
	err := rows.Scan(&g.ID, &g.Session, &g.Mode, &g.State, &g.Text, &g.Actor, &g.RequestID, &g.Detail, &g.Created, &g.Updated)
	return g, err
}

// findGuidanceByRequest returns the accepted-or-settled row for a
// request ID, if any. Retried submissions reuse the request ID and
// must observe the original row, never create a duplicate message.
func findGuidanceByRequest(requestID string) (id, state, text string, ok bool) {
	if strings.TrimSpace(requestID) == "" {
		return "", "", "", false
	}
	db, err := openTasksDB()
	if err != nil {
		return "", "", "", false
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT id, state, text FROM guidance WHERE request_id = ?`, requestID).Scan(&id, &state, &text); err != nil {
		return "", "", "", false
	}
	return id, state, text, true
}

// recordGuidanceAccepted durably accepts an intervention request
// before any delivery is attempted, so a crash between accept and
// settle leaves a recoverable row, never a silent loss (TD-083).
// The request ID is the idempotency key: a retried accept of the
// same operation returns the original row.
func recordGuidanceAccepted(session, text string, direct bool, req OperationRequest) (string, error) {
	if err := requireRequest(req); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("guidance text must not be empty")
	}
	if id, _, _, ok := findGuidanceByRequest(req.RequestID); ok {
		return id, nil
	}
	mode := "file"
	if direct {
		mode = "direct"
	}
	id := fmt.Sprintf("gd-%s-%d", time.Now().UTC().Format("20060102T150405"), time.Now().UnixNano()%10000)
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO guidance (id, session, mode, state, text, actor, request_id, detail, created, updated)
		VALUES (?, ?, ?, 'accepted', ?, ?, ?, '', ?, ?)`,
		id, session, mode, text, req.Actor, req.RequestID, nowUTC(), nowUTC()); err != nil {
		// Lost the insert race: the winner's row is the answer.
		if eid, _, _, ok := findGuidanceByRequest(req.RequestID); ok {
			return eid, nil
		}
		return "", err
	}
	return id, nil
}

// settleGuidance records the terminal disposition of an accepted
// request. Only accepted rows settle; terminal rows are immutable,
// so a disposition, once established, cannot be rewritten.
func settleGuidance(id, state, detail string) error {
	switch state {
	case GuidanceFiled, GuidanceDelivered, GuidanceRefused, GuidanceFailed:
	default:
		return fmt.Errorf("guidance %s cannot settle to non-terminal state %q", id, state)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE guidance SET state = ?, detail = ?, updated = ? WHERE id = ? AND state = 'accepted'`, state, detail, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("guidance %s is not outstanding (already settled or unknown)", id)
	}
	return nil
}

// submitGuidance accepts a durable intervention request, attempts
// delivery through the existing transport, and settles the row to
// the observed outcome. The error (if any) reaches the caller AND
// the audit trail (TD-084); file mode files the text durably,
// direct mode delivers or records the refusal.
func submitGuidance(session, text string, direct bool, req OperationRequest) (string, error) {
	// Replay of a settled operation: same request ID, same result,
	// no second delivery. A row still accepted means a crash between
	// accept and settle; fall through and settle it now.
	if id, state, _, ok := findGuidanceByRequest(req.RequestID); ok && state != GuidanceAccepted {
		return id, nil
	}
	id, err := recordGuidanceAccepted(session, text, direct, req)
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("actor=%s req=%s", req.Actor, req.RequestID)
	// One feed event per intervention with its ACTUAL terminal
	// disposition — filed, delivered, refused, or failed. The feed
	// never upgrades "sent to transport" into worker acknowledgment.
	emit := func(state, outcome string) {
		if _, err := recordOrchEvent(OrchEvent{
			OriginPrincipal: req.Actor, TargetAgent: session,
			EventType: OrchMessageSent, RequestID: req.RequestID,
			Payload: text, Disposition: state,
		}); err != nil {
			recordReceipt("", "orch_gap", fmt.Sprintf("message.sent feed write failed: %v", err))
		}
	}
	// Harness-specific dispatch: a muse-native session is steered
	// through an MSP turn on its bound native session, never
	// through terminal keystrokes or a guidance file its worker
	// loop may never consume.
	if sessionIsNative(session) {
		outcome, derr := deliverNativeGuidance(id, session, text, req)
		state := GuidanceDelivered
		if derr != nil {
			state = GuidanceFailed
			if strings.Contains(derr.Error(), "refused") {
				state = GuidanceRefused
			}
			outcome = derr.Error()
		}
		_ = settleGuidance(id, state, detail+" "+outcome)
		emit(state, outcome)
		if derr != nil {
			return "", fmt.Errorf("guidance %s %s: %w", id, state, derr)
		}
		return id, nil
	}
	if derr := injectGuidance(session, text, direct); derr != nil {
		state := GuidanceFailed
		if strings.Contains(derr.Error(), "refused") {
			state = GuidanceRefused
		}
		_ = settleGuidance(id, state, derr.Error())
		emit(state, derr.Error())
		return "", fmt.Errorf("guidance %s %s: %w", id, state, derr)
	}
	state := GuidanceFiled
	outcome := "filed to guidance.md"
	if direct {
		state = GuidanceDelivered
		outcome = "delivered to live session"
	}
	_ = settleGuidance(id, state, detail+" "+outcome)
	emit(state, outcome)
	return id, nil
}

// guidanceStatus returns one guidance record by id.
func guidanceStatus(id string) (Guidance, error) {
	db, err := openTasksDB()
	if err != nil {
		return Guidance{}, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, session, mode, state, text, actor, request_id, detail, created, updated FROM guidance WHERE id = ?`, id)
	if err != nil {
		return Guidance{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Guidance{}, fmt.Errorf("no such guidance: %s", id)
	}
	g, err := scanGuidance(rows)
	if err != nil {
		return Guidance{}, err
	}
	return g, rows.Err()
}

// listGuidance returns a session's intervention history, newest last.
func listGuidance(session string) ([]Guidance, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, session, mode, state, text, actor, request_id, detail, created, updated FROM guidance WHERE session = ? ORDER BY created, rowid`, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Guidance
	for rows.Next() {
		g, err := scanGuidance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// outstandingGuidance returns accepted-but-unsettled requests: the
// recovery scan after a controller restart (TD-083).
func outstandingGuidance() ([]Guidance, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, session, mode, state, text, actor, request_id, detail, created, updated FROM guidance WHERE state = 'accepted' ORDER BY created, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Guidance
	for rows.Next() {
		g, err := scanGuidance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

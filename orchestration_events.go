package main

// orchestration_events.go: durable, append-only, cursor-addressable
// communication history.
//
// This is the authoritative feed for the pinned Muse tab and the
// model-facing MCP adapter — not a reconstruction from task titles
// or terminal output. Every row records an exact communication
// Metis handled, with its disposition. Requested, committed, sent,
// acknowledged, and applied are distinct; an unacknowledged message
// is never labeled received.
//
// Partial visibility is explicit: message.acknowledged is a valid
// type, but no producer emits it until a harness exposes a real
// worker-consumption signal (only the demo worker writes acks, and
// nothing reads them). Consumers must render that as "not
// observable", never as received.
//
// Append is idempotent on (request_id, event_type) for non-empty
// request IDs: a retried mutation returns the original sequence
// instead of duplicating the event. Empty request IDs always append.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// parseOrchTime parses event timestamps written by nowUTC.
func parseOrchTime(s string) (int64, error) {
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return ts.UnixNano(), nil
}

// Orchestration event types.
const (
	OrchDelegationRequested   = "delegation.requested"
	OrchDelegationDispatched  = "delegation.dispatched"
	OrchMessageSent           = "message.sent"
	OrchMessageDelivered      = "message.delivered"
	OrchMessageAcknowledged   = "message.acknowledged"
	OrchWorkerReported        = "worker.reported"
	OrchVerificationRecorded  = "verification.recorded"
	OrchCorrectionRequested   = "correction.requested"
	OrchApprovalRequired      = "approval.required"
	OrchTransportDisconnected = "transport.disconnected"
	OrchProvisionRequested    = "provision.requested"
	OrchTerminalInput         = "terminal.input"
	OrchSessionStop           = "session.stop"
)

var orchEventTypes = map[string]bool{
	OrchDelegationRequested: true, OrchDelegationDispatched: true,
	OrchMessageSent: true, OrchMessageDelivered: true,
	OrchMessageAcknowledged: true, OrchWorkerReported: true,
	OrchVerificationRecorded: true, OrchCorrectionRequested: true,
	OrchApprovalRequired: true, OrchTransportDisconnected: true,
	OrchProvisionRequested: true,
	OrchTerminalInput: true, OrchSessionStop: true,
}

// maxOrchPayload bounds one event payload for bounded MCP responses.
// Excess is truncated with an explicit marker, never silently cut.
const maxOrchPayload = 64 << 10

// OrchEvent is one durable communication record.
type OrchEvent struct {
	Seq             int64  `json:"seq"`
	RunID           string `json:"run_id"`
	OriginPrincipal string `json:"origin_principal"`
	OriginHarness   string `json:"origin_harness"`
	MuseSession     string `json:"muse_session"`
	TargetAgent     string `json:"target_agent"`
	HostID          string `json:"host_id"`
	TaskID          string `json:"task_id"`
	Attempt         int    `json:"attempt"`
	EventType       string `json:"event_type"`
	RequestID       string `json:"request_id"`
	Payload         string `json:"payload"`
	Disposition     string `json:"disposition"`
	Created         string `json:"created"`
}

func ensureOrchEventsTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS orchestration_events (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		run_id TEXT NOT NULL DEFAULT '',
		origin_principal TEXT NOT NULL DEFAULT '',
		origin_harness TEXT NOT NULL DEFAULT '',
		muse_session TEXT NOT NULL DEFAULT '',
		target_agent TEXT NOT NULL DEFAULT '',
		host_id TEXT NOT NULL DEFAULT '',
		task_id TEXT NOT NULL DEFAULT '',
		attempt_no INTEGER NOT NULL DEFAULT 0,
		event_type TEXT NOT NULL,
		request_id TEXT NOT NULL DEFAULT '',
		payload TEXT NOT NULL DEFAULT '',
		disposition TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`); err != nil {
		return err
	}
	_, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS orch_events_req_type
		ON orchestration_events(request_id, event_type) WHERE request_id <> ''`)
	return err
}

// truncateOrchPayload caps payloads with an explicit marker so a
// bounded reader never mistakes a cut message for a whole one.
func truncateOrchPayload(p string) string {
	if len(p) <= maxOrchPayload {
		return p
	}
	return p[:maxOrchPayload] + fmt.Sprintf("…[truncated %d bytes]", len(p)-maxOrchPayload)
}

func scanOrchEvent(row *sql.Row) (OrchEvent, error) {
	var e OrchEvent
	err := row.Scan(&e.Seq, &e.RunID, &e.OriginPrincipal, &e.OriginHarness,
		&e.MuseSession, &e.TargetAgent, &e.HostID, &e.TaskID, &e.Attempt,
		&e.EventType, &e.RequestID, &e.Payload, &e.Disposition, &e.Created)
	return e, err
}

// insertOrchEventTx appends inside the caller's transaction so the
// event commits atomically with the state change it describes
// (e.g. delegation.dispatched lands in the dispatch commit).
func insertOrchEventTx(tx *sql.Tx, e OrchEvent) (int64, error) {
	if !orchEventTypes[e.EventType] {
		return 0, fmt.Errorf("unknown orchestration event type %q", e.EventType)
	}
	e.Payload = truncateOrchPayload(e.Payload)
	if e.RequestID != "" {
		var seq int64
		err := tx.QueryRow(`SELECT seq FROM orchestration_events WHERE request_id = ? AND event_type = ?`,
			e.RequestID, e.EventType).Scan(&seq)
		if err == nil {
			return seq, nil
		}
		if err != sql.ErrNoRows {
			return 0, err
		}
	}
	res, err := tx.Exec(`INSERT INTO orchestration_events
		(run_id, origin_principal, origin_harness, muse_session, target_agent, host_id,
		 task_id, attempt_no, event_type, request_id, payload, disposition, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.RunID, e.OriginPrincipal, e.OriginHarness, e.MuseSession, e.TargetAgent, e.HostID,
		e.TaskID, e.Attempt, e.EventType, e.RequestID, e.Payload, e.Disposition, nowUTC())
	if err != nil {
		if e.RequestID != "" && (strings.Contains(err.Error(), "constraint failed") || strings.Contains(err.Error(), "UNIQUE")) {
			var seq int64
			if qerr := tx.QueryRow(`SELECT seq FROM orchestration_events WHERE request_id = ? AND event_type = ?`,
				e.RequestID, e.EventType).Scan(&seq); qerr == nil {
				return seq, nil
			}
		}
		return 0, err
	}
	return res.LastInsertId()
}

// recordOrchEvent appends outside a caller transaction (report,
// verify, guidance outcomes). Callers record the gap via
// recordReceipt if this fails; the feed never silently drops.
func recordOrchEvent(e OrchEvent) (int64, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	seq, err := insertOrchEventTx(tx, e)
	if err != nil {
		return 0, err
	}
	return seq, tx.Commit()
}

// maxOrchPage bounds one feed read for bounded MCP responses.
const maxOrchPage = 500

// listLatestOrchEvents returns the newest up-to-limit events in
// chronological order for the pinned Muse feed. Bounded by
// construction; the TUI never pages the whole history.
func listLatestOrchEvents(limit int) ([]OrchEvent, error) {
	if limit <= 0 || limit > maxOrchPage {
		limit = 30
	}
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT seq, run_id, origin_principal, origin_harness, muse_session, target_agent, host_id,
		task_id, attempt_no, event_type, request_id, payload, disposition, created
		FROM orchestration_events ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []OrchEvent
	for rows.Next() {
		var e OrchEvent
		if err := rows.Scan(&e.Seq, &e.RunID, &e.OriginPrincipal, &e.OriginHarness,
			&e.MuseSession, &e.TargetAgent, &e.HostID, &e.TaskID, &e.Attempt,
			&e.EventType, &e.RequestID, &e.Payload, &e.Disposition, &e.Created); err != nil {
			return nil, err
		}
		rev = append(rev, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]OrchEvent, len(rev))
	for i, e := range rev {
		out[len(rev)-1-i] = e
	}
	return out, nil
}

// Muse activity states. Derived from the typed presence facts in
// muse_presence.go — never from the existence of a Muse process,
// and never from raw traffic age: bare Active means a live lease
// plus committed open work, while traffic alone reads as the
// labeled estimate MuseActiveEstimated.
const (
	MuseDisconnected    = "Disconnected"
	MuseIdle            = "Idle"
	MuseConnected       = "Connected"
	MuseActive          = "Active"
	MuseActiveEstimated = "Active (est)"
	MuseDelegated       = "Delegated"
	MuseAttention       = "Attention"
	MuseError           = "Error"
)

// museActiveWindow bounds recency: an event newer than this counts
// as fresh traffic. Fresh traffic is a labeled estimate of
// liveness (MuseActiveEstimated), never verified execution — see
// collectMuseFacts. The window also bounds how long an explicit
// transport.disconnect signal reads as a live outage.
const museActiveWindow = 120 * int64(time.Second)

// museActivity projects the indicator state plus outstanding counts
// from the typed presence facts (deriveMuseState). Disconnected
// means the state database itself is unreadable.
func museActivity() (state string, open, blocked int) {
	f := collectMuseFacts()
	return deriveMuseState(f), f.Open, f.Blocked
}

// listOrchEvents returns events after seq (exclusive), oldest first.
// The cursor for the next page is the last returned seq; an empty
// page means caught up. RunID scopes optionally; empty means all.
func listOrchEvents(run string, afterSeq int64, limit int) ([]OrchEvent, error) {
	if limit <= 0 || limit > maxOrchPage {
		limit = 100
	}
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		return nil, err
	}
	q := `SELECT seq, run_id, origin_principal, origin_harness, muse_session, target_agent, host_id,
		task_id, attempt_no, event_type, request_id, payload, disposition, created
		FROM orchestration_events WHERE seq > ?`
	args := []any{afterSeq}
	if run != "" {
		q += ` AND run_id = ?`
		args = append(args, run)
	}
	q += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, limit)
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrchEvent
	for rows.Next() {
		var e OrchEvent
		if err := rows.Scan(&e.Seq, &e.RunID, &e.OriginPrincipal, &e.OriginHarness,
			&e.MuseSession, &e.TargetAgent, &e.HostID, &e.TaskID, &e.Attempt,
			&e.EventType, &e.RequestID, &e.Payload, &e.Disposition, &e.Created); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

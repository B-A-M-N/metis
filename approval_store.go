package main

// approval_store.go: durable approval store.
//
// One row per (session, approvalId): the pending provider request plus,
// once observed, the server's terminal decision. Rows are keyed by the
// stable approvalId the schema mandates — never by the JSON-RPC id,
// which belongs to one connection. A resolved row is never reopened
// by a stale fold.
//
// This layer owns persistence only. It records what the provider
// announced and what was decided; it never answers, never validates
// authority, and never transports. Provider approval is provider
// approval — it is not task acceptance, and nothing here merges the
// two.

import (
	"database/sql"
	"encoding/json"
)

// museApproval is one durable approval record: the pending request
// plus, once observed, the server's terminal decision.
type museApproval struct {
	Session       string               `json:"session"`
	NativeID      string               `json:"native_id"`
	ApprovalID    string               `json:"approval_id"`
	ItemID        string               `json:"item_id"`
	RequirementID string               `json:"requirement_id"`
	Choices       []museApprovalChoice `json:"choices"`
	// DecideCommandID is the UUIDv7 sent (or to send) with the
	// decision: persisted before transmit so a crash between send
	// and record retries value-identical instead of double-deciding.
	DecideCommandID string `json:"decide_command_id"`
	Status          string `json:"status"` // pending | resolved
	Decision        string `json:"decision"`
	DecidedBy       string `json:"decided_by_command_id"`
	RequestedAt     string `json:"requested_at"`
	ResolvedAt      string `json:"resolved_at"`
}

type museApprovalChoice struct {
	ChoiceID string `json:"choiceId"`
	Label    string `json:"label"`
}

func ensureMuseApprovalTables(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_approvals (
		session TEXT NOT NULL,
		native_id TEXT NOT NULL DEFAULT '',
		approval_id TEXT NOT NULL,
		item_id TEXT NOT NULL DEFAULT '',
		requirement_id TEXT NOT NULL DEFAULT '',
		choices TEXT NOT NULL DEFAULT '[]',
		decide_command_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		decision TEXT NOT NULL DEFAULT '',
		decided_by TEXT NOT NULL DEFAULT '',
		requested_at TEXT NOT NULL,
		resolved_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (session, approval_id)
	)`); err != nil {
		return err
	}
	if !museApprovalHasColumn(db, "decide_command_id") {
		// Rows recorded before decide existed carry no command id.
		if _, err := db.Exec(`ALTER TABLE muse_approvals ADD COLUMN decide_command_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if museApprovalHasColumn(db, "decide_payload") {
		return nil
	}
	// The payload the stored command id was minted for
	// (choice + feedback). A conflicting retry must mint a fresh
	// id, never reuse one bound to a different decision.
	_, err := db.Exec(`ALTER TABLE muse_approvals ADD COLUMN decide_payload TEXT NOT NULL DEFAULT ''`)
	return err
}

func museApprovalHasColumn(db *sql.DB, want string) bool {
	rows, err := db.Query(`PRAGMA table_info(muse_approvals)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	var cid int
	var name, ctype string
	var notnull, pk int
	var dflt any
	for rows.Next() {
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == want {
			return true
		}
	}
	return false
}

// countPendingMuseApprovals reports how many approval rows still
// await a decision across all sessions. The activity indicator
// derives Attention from this live count, not from historical
// approval.required events.
func countPendingMuseApprovals() (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM muse_approvals WHERE status = 'pending'`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func listMuseApprovals(session string, pendingOnly bool) ([]museApproval, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		return nil, err
	}
	q := `SELECT session, native_id, approval_id, item_id, requirement_id, choices,
		decide_command_id, status, decision, decided_by, requested_at, resolved_at
		FROM muse_approvals WHERE session = ?`
	if pendingOnly {
		q += ` AND status = 'pending'`
	}
	q += ` ORDER BY requested_at DESC`
	rows, err := db.Query(q, session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []museApproval
	for rows.Next() {
		var a museApproval
		var choicesRaw, reqRaw string
		if err := rows.Scan(&a.Session, &a.NativeID, &a.ApprovalID, &a.ItemID,
			&reqRaw, &choicesRaw, &a.DecideCommandID, &a.Status, &a.Decision, &a.DecidedBy,
			&a.RequestedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		a.RequirementID = reqRaw
		_ = json.Unmarshal([]byte(choicesRaw), &a.Choices)
		if out == nil {
			out = []museApproval{}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

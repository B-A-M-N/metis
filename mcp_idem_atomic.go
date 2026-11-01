package main

import (
	"database/sql"
)

// mcp_idem_atomic.go: atomic idempotency-key ownership for the MCP
// control tools.
//
// The key row and the domain mutation commit in ONE SQLite
// transaction on ONE connection (BEGIN IMMEDIATE; key row first;
// rollback undoes both). Concurrent same-key conflicting requests
// therefore serialize on the key: exactly one mutates, the losers
// read the winner and converge or conflict without mutating.

// mcpIdemInjectTxFailure, when non-nil, is returned inside the
// idempotent depend/correct transaction after the key claim and the
// domain mutation are staged but before COMMIT, forcing a rollback
// that must undo both. Tests set it to prove atomicity; production
// leaves it nil.
var mcpIdemInjectTxFailure error

// ensureMcpDependTable records request_id -> edge so retried
// depends return the original instead of duplicating.
func ensureMcpDependTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS mcp_depend (
		request_id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		dep_id TEXT NOT NULL,
		created TEXT NOT NULL
	)`)
	return err
}

// ensureMcpCorrectTable records request_id -> correction task so
// retried corrections return the original instead of forking a
// second correction.
func ensureMcpCorrectTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS mcp_correct (
		request_id TEXT PRIMARY KEY,
		task_id TEXT NOT NULL,
		created TEXT NOT NULL
	)`)
	return err
}

// dependIdemOutcome is the converged result of an idempotent depend:
// the recorded edge plus whether this call replayed a prior claim.
type dependIdemOutcome struct {
	TaskID string
	DepID  string
	Replay bool
}

// dependKeyConflict reports a request-key reuse with a different
// edge payload. It carries the winner so the caller can name the
// recorded edge without another lookup.
type dependKeyConflict struct {
	RequestKey string
	WinnerTask string
	WinnerDep  string
}

func (e *dependKeyConflict) Error() string {
	return "depend conflict: request key already recorded a different edge"
}

// correctIdemOutcome is the converged result of an idempotent
// correct: the correction task plus whether this call replayed a
// prior claim. Bare covers a winner whose task row is gone: only
// the key survives, so the caller reports task_id alone.
type correctIdemOutcome struct {
	TaskID string
	Parent string
	Run    string
	Replay bool
	Bare   bool
}

// correctKeyConflict reports a request-key reuse with a different
// correction payload. It carries the winner so the caller can name
// the forked correction without another lookup.
type correctKeyConflict struct {
	RequestKey   string
	WinnerTask   string
	WinnerParent string
}

func (e *correctKeyConflict) Error() string {
	return "correct conflict: request key already forked a different correction"
}

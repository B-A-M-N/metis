package main

// CommandContext carries the authority under which a mutating command runs.
//
// PrincipalID MUST come from authenticated or trusted transport context
// (CLI operator identity, MCP session principal, worker-loop identity).
// It MUST NEVER be taken from a caller-supplied actor/display string.
// Actor is attribution only and grants no authority.
type CommandContext struct {
	PrincipalID string
	Actor       string
	RequestID   string
	Origin      OriginKind
	Scope       ResourceScope
}

// OriginKind identifies where a mutating command entered the system.
type OriginKind string

const (
	OriginCLI        OriginKind = "cli"
	OriginMCP        OriginKind = "mcp"
	OriginTUI        OriginKind = "tui"
	OriginWorkerLoop OriginKind = "worker-loop"
	OriginFleet      OriginKind = "fleet"
)

// ResourceScope narrows a command to the resources it may affect.
type ResourceScope struct {
	RunID     string
	TaskID    string
	AttemptID string
	SessionID string
}

// CmdDisposition is the authoritative outcome of a mutating command.
// It reports what the system did, never what the caller hoped.
type CmdDisposition string

const (
	CmdOK           CmdDisposition = "ok"
	CmdRejected     CmdDisposition = "rejected"
	CmdConflict     CmdDisposition = "conflict"
	CmdNotFound     CmdDisposition = "not_found"
	CmdUnauthorized CmdDisposition = "unauthorized"
	CmdBlocked      CmdDisposition = "blocked"
	// CmdUnknownState means the external side effect is uncertain
	// (crash between launch and acknowledgement, lost notification, etc.).
	// Callers MUST reconcile, never retry blindly or assume success/failure.
	CmdUnknownState CmdDisposition = "unknown_state"
)

// CommandError is the typed failure for authoritative operations.
type CommandError struct {
	Code    CmdDisposition
	Message string
}

func (e *CommandError) Error() string { return string(e.Code) + ": " + e.Message }

package main

// controller.go: durable task dispatch and lifecycle tracking.
//
// A task is one delegated unit of work. Its lifecycle is independent
// of the worker session's lifetime: a session can outlive its task,
// and a task can be cancelled without killing its session.
//
//	statuses: pending -> dispatched -> working -> report_ready
//	          report_ready -> verified | failed
//	          any non-terminal -> cancelled
//
//	followup creates a new task linked to its parent; history is never
//	rewritten. All state lives in SQLite (stateDir/tasks.db) so
//	concurrent CLI invocations serialize through real transactions.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Task statuses.
//
// cancelling and termination_unknown are non-terminal by design:
// a task being asked to stop, or one whose execution may still be
// alive, continues to occupy its workspace and block run
// finalization until it reaches a proven disposition.
const (
	TaskPending            = "pending"
	TaskDispatched         = "dispatched"
	TaskWorking            = "working"
	TaskReportReady        = "report_ready"
	TaskVerified           = "verified"
	TaskFailed             = "failed"
	TaskCancelled          = "cancelled"
	TaskCancelling         = "cancelling"
	TaskTerminationUnknown = "termination_unknown"
)

// Task is one delegated unit of work.
type Task struct {
	ID         string `json:"id"`
	Run        string `json:"run"`
	Agent      string `json:"agent"`
	Objective  string `json:"objective"`
	Acceptance string `json:"acceptance"`
	Status     string `json:"status"`
	Attempt    int    `json:"attempt"`
	Parent     string `json:"parent"`
	Report     string `json:"report"`
	Created    string `json:"created"`
	Updated    string `json:"updated"`
	// CorrectionNo is the depth in a correction chain: 0 for an
	// original task, parent depth + 1 for a followup (TD-036).
	CorrectionNo int `json:"correction_no"`
	// ParentReport snapshots the parent's report digest at the moment
	// a correction is created, binding the correction to the exact
	// preceding result it must address (TD-082). Empty when the
	// parent had submitted no report.
	ParentReport string `json:"parent_report"`
	// ExecutionRoot is the canonical workspace the task is bound to.
	// Empty until first dispatch binds it; immutable afterwards
	// (TD-050). A pre-authorized root is enforced at dispatch.
	ExecutionRoot string `json:"execution_root"`
	// SharedWorkspace records explicit authorization for this task
	// to execute in a workspace that already hosts another live
	// task. 0 = isolated (default), 1 = shared authorized (TD-051).
	SharedWorkspace int `json:"shared_workspace"`
	// Key is the client-supplied idempotency key. Empty means no
	// deduplication; non-empty keys are unique per run.
	Key string `json:"key"`
}

// migratedDBs records state paths whose schema this process has
// already migrated, keyed by path with a dbIdentity value (see
// schema_epoch.go). openTasksDB runs full DDL (CREATE TABLE,
// ALTER, indexes) — write locks on every open. Readers that
// re-migrate on each poll collide with concurrent writers and
// surface SQLITE_BUSY both ways. Migrating once per path per
// process keeps opens to PRAGMAs + the caller's own statements.
// WAL still allows cross-process readers alongside one writer.
// The value is a file identity, not struct{}: a bare path key is
// blind to the file being replaced at the same path, so a cache
// hit additionally requires the stat identity to match; a
// replacement falls through to full validation and migration.
var migratedDBs sync.Map // string -> dbIdentity

// migrationDDLruns counts full migration-path DDL executions in this
// process (observe-only metric, never control flow): steady-state
// opens must hit the epoch-gated fast path and leave this unchanged.
// A rising count alongside flat schema versions means the admission
// cache is self-invalidating and every open re-runs DDL.
var migrationDDLruns atomic.Int64

// dbOpenCount counts successful openTasksDB admissions in this
// process (observe-only metric, never control flow). Read paths that
// repeat per-item queries (graph renders, readiness scans) must hoist
// one handle and share it; the count pins N+1 regressions — a render
// over N tasks must not scale its opens with N.
var dbOpenCount atomic.Int64

// openMu serializes open+migrate per database path inside this
// process. The migratedDBs check-then-act races when two goroutines
// first-open the same fresh path: both run DDL, and the loser's
// ALTER TABLE fails (duplicate column) or its journal upgrade
// collides. The mutex makes the second opener a cache hit; the
// duplicate-column tolerance below covers cross-process races.
var openMu sync.Mutex

// cancelTask ends a task without touching any worker session.
// (cancelTask lives with the slice-2 delegation workflow below:
// attributed, cascading cancellation. See TD-037.)

// Bounded correction and retry budgets (TD-035, TD-036). These are
// explicit stopping conditions, not tuning knobs: a correction chain
// or dispatch-attempt sequence that exceeds its budget is refused
// with a named error so the orchestrator must make a deliberate
// decision (new run, new approach, operator override) instead of
// looping silently.
const (
	// maxCorrectionDepth caps followupTask chains: a correction of a
	// correction counts toward the same budget.
	maxCorrectionDepth = 5
	// maxDispatchAttempts caps dispatch + retryDispatch attempts on
	// one task. Reconciliation history is preserved; only further
	// replacement is refused.
	maxDispatchAttempts = 5
)

// ---------------------------------------------------------------------------
// Phase-1 delivery/lifecycle correctness (TD-006, TD-015, TD-026..TD-029,
// TD-116..TD-118).
//
// Every state-changing operation carries an identifiable, authorized
// request (TD-006). Retries create distinct task_attempts rows with stable
// IDs; per-attempt delivery rows (task_deliveries) track the observable
// states queued -> submitted -> transport_acknowledged -> agent_acknowledged
// -> working, plus delivery_failed and the non-success reconciled states
// unknown / uncertain / duplicate. Uncertain is never coerced into success
// or failure (TD-026, TD-116). The pending attempt is claimed atomically
// (TD-027) BEFORE any externally visible delivery side effect, and a retry
// after an uncertain outcome must reconcile the original attempt before a
// replacement starts (TD-029, TD-117, TD-118).
// ---------------------------------------------------------------------------

// Delivery states for task_deliveries rows.
const (
	DeliveryQueued          = "queued"
	DeliverySubmitted       = "submitted"
	DeliveryTransportAcked  = "transport_acknowledged"
	DeliveryAgentAcked      = "agent_acknowledged"
	DeliveryWorking         = "working"
	DeliveryFailed          = "delivery_failed"
	DeliveryBlocked         = "blocked"
	DeliveryUnknown         = "unknown"
	DeliveryUncertain       = "uncertain"
	DeliveryDuplicate       = "duplicate"
	DeliveryGuidanceFiled   = "guidance_submitted"
	DeliveryGuidanceBlocked = "guidance_blocked"
)

// Attempt states for task_attempts rows.
const (
	AttemptClaimed    = "claimed"
	AttemptSubmitted  = "submitted"
	AttemptWorking    = "working"
	AttemptFailed     = "delivery_failed"
	AttemptUncertain  = "uncertain"
	AttemptUnknown    = "unknown"
	AttemptReconciled = "reconciled-duplicate"
	AttemptSuperseded = "superseded"
	AttemptDelivered  = "transport_acknowledged"
)

// OperationRequest identifies the authorized request behind every
// state-changing operation (TD-006). Actor names the orchestrator or
// operator; RequestID is a unique per-operation identity used for
// attribution and duplicate suppression.
//
// Principal is the transport-authenticated identity and Remote says
// whether it arrived over a token-authenticated transport (MCP/Ocliv).
// A caller-supplied actor string never establishes authority: a
// remote caller may assert any actor name, so independence and
// approval gates must consult Principal/Remote, never Actor alone.
// Local CLI construction leaves both zero: local-operator, trusted
// by OS process ownership.
type OperationRequest struct {
	Actor     string
	RequestID string
	Principal string
	Remote    bool
	// FleetObserver is set only by the HTTP transport when the
	// verified JWT carries the explicit metis:fleet scope:
	// fleet-wide read observation. Absent otherwise — remote
	// reads are principal-scoped.
	FleetObserver bool
}

var requestCounter atomic.Uint64

// uniqueRandomSuffix returns 16 hex chars (64-bit) from crypto/rand.
// On PRNG failure it falls back to a timestamp+pid+counter mix so ID
// generation never fails; callers never branch on randomness.
func uniqueRandomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("fallback%x%d%d", time.Now().UnixNano(), os.Getpid(), requestCounter.Add(1))
}

// newUniqueID mints a collision-resistant identity with a stable
// operator-diagnostic prefix. A prior generation combined a
// second-resolution timestamp with a process-local counter, so every
// freshly started process minted byte-identical IDs within a second
// (P0-A). The timestamp segment is retained for rough ordering and
// log grep; uniqueness comes from 64-bit cryptographic randomness.
// The id PRIMARY KEY constraints remain the final defensive
// boundary; existing persisted IDs are untouched.
func newUniqueID(prefix string) string {
	return prefix + "-" + time.Now().UTC().Format("20060102T150405") + "-" + uniqueRandomSuffix()
}

// idemConflictError reports an idempotency-key reuse with a different
// payload (TD-028). It is never silently folded into the original task.
type idemConflictError struct {
	Run      string
	Key      string
	Existing string
}

func (e *idemConflictError) Error() string {
	return fmt.Sprintf("idempotency conflict: key %q in run %q was already used by task %s with a different payload", e.Key, e.Run, e.Existing)
}

// TaskAttempt is one execution try of a task. Retries add rows; no retry
// ever overwrites a prior attempt's history (TD-015).
type TaskAttempt struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	AttemptNo int    `json:"attempt_no"`
	State     string `json:"state"`
	RequestID string `json:"request_id"`
	Actor     string `json:"actor"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
}

// TaskDelivery is one observable delivery step for an attempt.
type TaskDelivery struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
	RequestID string `json:"request_id"`
	Actor     string `json:"actor"`
	Detail    string `json:"detail"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
}

// Guidance states: accepted is the durable handoff, the rest are
// terminal dispositions.
const (
	GuidanceAccepted  = "accepted"
	GuidanceFiled     = "filed"
	GuidanceDelivered = "delivered"
	GuidanceRefused   = "refused"
	GuidanceFailed    = "failed"
)

// Guidance is one durable intervention request and its disposition.
type Guidance struct {
	ID        string `json:"id"`
	Session   string `json:"session"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	Text      string `json:"text"`
	Actor     string `json:"actor"`
	RequestID string `json:"request_id"`
	Detail    string `json:"detail"`
	Created   string `json:"created"`
	Updated   string `json:"updated"`
}

// RunSummary counts tasks by terminal disposition at finalization.
type RunSummary struct {
	Run       string `json:"run"`
	Total     int    `json:"total"`
	Verified  int    `json:"verified"`
	Failed    int    `json:"failed"`
	Cancelled int    `json:"cancelled"`
}

// Execution-cancel dispositions. Requested means the signal was
// sent to the verified process; confirmed means the process is
// observably gone; uncertain means identity, liveness, or outcome
// could not be established — reconcile before retrying.
const (
	ExecutionCancelConfirmed = "confirmed"
	ExecutionCancelUncertain = "uncertain"
	ExecutionCancelNoBinding = "no_binding"
)

// insertAttempt records a distinct execution attempt. attemptNo comes
// from the tasks.attempt counter bumped by the atomic claim, so every
// attempt row maps to exactly one winning transition.
// sqlExecer covers *sql.DB and *sql.Tx so multi-statement commits
// (dispatch claim + attempt + deliveries) run atomically in one tx
// while single-statement callers keep passing *sql.DB unchanged.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

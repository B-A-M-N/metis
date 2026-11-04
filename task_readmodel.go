package main

// task_readmodel.go: read-model services behind the presentation /
// transport boundary. MCP read tools and TUI panels consume these
// typed projections; they never open the task database or run SQL
// themselves. Every persistence internal (openTasksDB, verdict
// queries, latest-run scans) lives here, on the domain side of the
// seam, so the boundary test can ban opener calls from transport /
// UI files without starving the adapters.
//
// Owned by Package B: presentation/transport boundary.

import (
	"database/sql"
	"fmt"
	"time"
)

// taskStatusSnapshot is a revision-pinned bundle behind
// metis_task_status: the task row plus everything the row carries
// (dependency edges with their statuses, criterion verdicts for the
// pinned attempt, current-attempt artifacts). It is produced by ONE
// SQLite read transaction (snapshotTaskStatus) and projected into
// MCP fields OUTSIDE that transaction (projectTaskStatus), so a
// concurrent retry/transition landing between the reads — or during
// the projection — cannot mix revisions in one response.
type taskStatusSnapshot struct {
	task      Task
	depEdges  []string
	depStatus map[string]string
	verdicts  map[string]CriterionVerdict
	artifacts []Artifact
}

// provisionOwnerForSession is the read-model seam behind the
// principal-scoped controller policy: transports ask who owns a
// session without opening the database themselves. Unknown
// sessions report found=false; the caller decides the refusal.
func provisionOwnerForSession(session string) (owner string, found bool, err error) {
	db, err := openTasksDB()
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	return lookupProvisionOwner(db, session)
}

// taskStatusDetailProbe is a test-only hook invoked once per
// taskStatusDetail call after the snapshot transaction commits and
// before the projection returns. Tests arm it with a concurrent
// transition driver to prove the returned bundle stays pinned to
// one revision. Nil in production; the projection never blocks on
// it.
var taskStatusDetailProbe func(pinned Task)

// taskStatusDetail is the read-model projection behind
// metis_task_status: the task row plus everything the row carries
// (unsatisfied gate deps, criterion verdicts for the current
// attempt, current-attempt artifact count). One entry point, so the
// MCP adapter cannot accumulate direct DB authority piece by piece.
//
// Revision pinning: every row comes from snapshotTaskStatus, a
// single read transaction, and the MCP fields are derived from
// that bundle after the transaction commits. A retry or transition
// racing this call moves the NEXT revision only.
func taskStatusDetail(taskID string) (tk Task, waiting []string, verdicts map[string]CriterionVerdict, artifactCount int, err error) {
	snap, err := snapshotTaskStatus(taskID)
	if err != nil {
		return Task{}, nil, nil, 0, err
	}
	// The snapshot transaction has committed: a concurrent
	// retry/transition landing here races the projection, not the
	// reads, so it cannot pollute the pinned rows.
	if taskStatusDetailProbe != nil {
		taskStatusDetailProbe(snap.task)
	}
	tk, waiting, verdicts, artifactCount = projectTaskStatus(snap)
	return tk, waiting, verdicts, artifactCount, nil
}

// projectTaskStatus derives the MCP response fields from a pinned
// snapshot. Pure projection: no DB access, so it runs outside the
// repository transaction that produced the bundle.
func projectTaskStatus(snap taskStatusSnapshot) (Task, []string, map[string]CriterionVerdict, int) {
	var waiting []string
	for _, depID := range snap.depEdges {
		if st := snap.depStatus[depID]; st != TaskVerified {
			waiting = append(waiting, fmt.Sprintf("%s (%s)", depID, st))
		}
	}
	return snap.task, waiting, snap.verdicts, len(snap.artifacts)
}

// snapshotTaskStatus reads the task row, its dependency edges plus
// their statuses, its current-attempt verdicts, and its
// current-attempt artifacts inside ONE SQLite read transaction on
// ONE handle. The deferred BEGIN takes a shared lock at the first
// SELECT, so the four reads see a single committed revision while a
// concurrent writer waits (busy_timeout) for the commit instead of
// interleaving between the reads.
func snapshotTaskStatus(taskID string) (taskStatusSnapshot, error) {
	var snap taskStatusSnapshot
	db, err := openTasksDB()
	if err != nil {
		return snap, err
	}
	defer db.Close()
	// Ensure BEFORE Begin: this handle allows one connection, so
	// DDL after Begin would wait on the transaction's own
	// connection. The artifacts table is ensured lazily (not in
	// the open-path schema), and the verdicts ensure is a
	// no-op when the schema already created it.
	if err := ensureArtifactsTable(db); err != nil {
		return snap, err
	}
	if err := ensureVerdictsTable(db); err != nil {
		return snap, err
	}
	if err := ensureJudgmentsTable(db); err != nil {
		return snap, err
	}
	tx, err := db.Begin()
	if err != nil {
		return snap, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	tk, err := scanTask(tx.QueryRow(`SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks WHERE id = ?`, taskID))
	if err == sql.ErrNoRows {
		return snap, fmt.Errorf("no such task: %s", taskID)
	}
	if err != nil {
		return snap, err
	}
	rows, err := tx.Query(`SELECT depends_on FROM task_deps WHERE task_id = ? ORDER BY depends_on`, taskID)
	if err != nil {
		return snap, err
	}
	var edges []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			rows.Close()
			return snap, err
		}
		edges = append(edges, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return snap, err
	}
	depStatus := make(map[string]string, len(edges))
	for _, depID := range edges {
		var st string
		if err := tx.QueryRow(`SELECT status FROM tasks WHERE id = ?`, depID).Scan(&st); err == sql.ErrNoRows {
			return snap, fmt.Errorf("no such task: %s", depID)
		} else if err != nil {
			return snap, err
		}
		depStatus[depID] = st
	}
	// Judgments are append-only across two stores (legacy
	// task_verdicts rows plus criterion_judgments); the pinned
	// read projects them inside this same transaction via the
	// shared union reader, so the display bundle and the legacy
	// map entry point render identical verdicts.
	judgments, err := judgmentsForAttemptTx(tx, taskID, tk.Attempt)
	if err != nil {
		return snap, err
	}
	verdicts := latestVerdictMap(judgments)
	arows, err := tx.Query(`SELECT task_id, path, digest, attempt_no, created FROM artifacts
		WHERE task_id = ? AND attempt_no = ? ORDER BY path`, taskID, tk.Attempt)
	if err != nil {
		return snap, err
	}
	var arts []Artifact
	for arows.Next() {
		var a Artifact
		if err := arows.Scan(&a.TaskID, &a.Path, &a.Digest, &a.AttemptNo, &a.Created); err != nil {
			arows.Close()
			return snap, err
		}
		arts = append(arts, a)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return snap, err
	}
	if err := tx.Commit(); err != nil {
		return snap, err
	}
	committed = true
	snap.task = tk
	snap.depEdges = edges
	snap.depStatus = depStatus
	snap.verdicts = verdicts
	snap.artifacts = arts
	return snap, nil
}

// verdictsForCurrentAttempt reads the criterion verdicts filed at
// the task's current attempt. The connection stays inside the read
// model: callers get rows, never a handle.
func verdictsForCurrentAttempt(tk Task) (map[string]CriterionVerdict, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return verdictsForAttempt(db, tk.ID, tk.Attempt)
}

// latestRunForAgent returns the newest task run for a worker session,
// or "" when the session has no tasks yet. Moved from graph_panel.go
// so the panel consumes a projection, not SQL.
func latestRunForAgent(agent string) string {
	run, err := queryLatestRun(`SELECT run FROM tasks WHERE agent = ? ORDER BY created DESC LIMIT 1`, agent)
	if err != nil || run == "" {
		return ""
	}
	return run
}

// latestRun returns the newest run overall, or "" when empty. Moved
// from graph_panel.go; same projection-seam rationale.
func latestRun() string {
	run, err := queryLatestRun(`SELECT run FROM tasks ORDER BY created DESC LIMIT 1`)
	if err != nil {
		return ""
	}
	return run
}

// queryLatestRun scans one run tag. No rows is an empty panel, not
// an error, matching the previous inline behavior.
func queryLatestRun(query string, args ...any) (string, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var run string
	if err := db.QueryRow(query, args...).Scan(&run); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return run, nil
}

// TaskAssurance is the lifecycle disposition + assurance level
// projection behind metis_task_status / metis_report consumers:
// what the ledger knows about this task's verification, and how
// strongly it knows it. Technical assurance requires unanimous
// current-revision technical PASS judgments; operator acceptance
// is exposed as operator-accepted, never silently promoted.
// Disputed criteria stay visible with both sides attached.
type TaskAssurance struct {
	TaskID      string                         `json:"task_id"`
	Status      string                         `json:"status"`
	Attempt     int                            `json:"attempt"`
	EvidenceRev string                         `json:"evidence_rev"`
	Disposition string                         `json:"disposition"`
	Assurance   string                         `json:"assurance"`
	Criteria    map[string]CriterionAssessment `json:"criteria"`
}

// Task-level dispositions. Terminal tasks report the recorded
// outcome (verified / rejected / override); open tasks report
// the aggregate over their declared criteria.
const (
	TaskDispositionVerified   = "verified"
	TaskDispositionRejected   = "rejected"
	TaskDispositionOverride   = "override"
	TaskDispositionSatisfied  = "satisfied"
	TaskDispositionFailed     = "failed"
	TaskDispositionDisputed   = "disputed"
	TaskDispositionPartial    = "partial"
	TaskDispositionUnjudged   = "unjudged"
	TaskDispositionInconcl    = "inconclusive"
	TaskDispositionOperatorOK = "operator-accepted"
)

// taskAssurance projects one task's verification posture: the
// current evidence revision, the per-criterion assessment
// aggregate (including unjudged declared criteria), and the
// overall lifecycle disposition with its assurance level. An
// evidence-free task completed via operator override reports
// override / operator-accepted — honest approval, never
// technical correctness.
func taskAssurance(taskID string) (TaskAssurance, error) {
	t, err := getTask(taskID)
	if err != nil {
		return TaskAssurance{}, err
	}
	rev, err := evidenceRevisionFor(taskID, t.Attempt)
	if err != nil {
		return TaskAssurance{}, err
	}
	db, err := openTasksDB()
	if err != nil {
		return TaskAssurance{}, err
	}
	defer db.Close()
	assessed, err := assessmentsForAttempt(db, taskID, t.Attempt, rev)
	if err != nil {
		return TaskAssurance{}, err
	}
	out := TaskAssurance{
		TaskID:      taskID,
		Status:      t.Status,
		Attempt:     t.Attempt,
		EvidenceRev: rev,
		Criteria:    map[string]CriterionAssessment{},
	}
	for _, c := range criteriaFromAcceptance(t.Acceptance) {
		if a, ok := assessed[c.ID]; ok {
			out.Criteria[c.ID] = a
			continue
		}
		out.Criteria[c.ID] = CriterionAssessment{
			CriterionID: c.ID,
			Attempt:     t.Attempt,
			EvidenceRev: rev,
			Disposition: AssessmentUnjudged,
			Assurance:   AssuranceNone,
		}
	}
	switch t.Status {
	case TaskVerified:
		if taskHasOverrideReceipt(taskID) {
			out.Disposition = TaskDispositionOverride
			out.Assurance = AssuranceOperatorAccepted
		} else {
			out.Disposition = TaskDispositionVerified
			out.Assurance = AssuranceTechnical
		}
		return out, nil
	case TaskFailed:
		out.Disposition = TaskDispositionRejected
		out.Assurance = AssuranceNone
		return out, nil
	}
	out.Disposition = aggregateDisposition(out.Criteria)
	out.Assurance = AssuranceNone
	return out, nil
}

// taskHasOverrideReceipt reports whether the task completed via
// the evidence-free operator-approval path. The override is a
// lifecycle fact recorded in receipts, not a judgment, so the
// read model derives it there rather than inventing state.
func taskHasOverrideReceipt(taskID string) bool {
	recs, err := listReceipts(taskID)
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.Kind == "operator_approved" {
			return true
		}
	}
	return false
}

// aggregateDisposition folds per-criterion dispositions into the
// task-level posture. Precedence is fixed: a dispute anywhere
// dominates a failure anywhere, a failure dominates any
// non-result, and satisfaction requires every declared
// criterion to pass. Two passes so a later criterion's dispute
// is never shadowed by an earlier failure.
func aggregateDisposition(criteria map[string]CriterionAssessment) string {
	if len(criteria) == 0 {
		return TaskDispositionUnjudged
	}
	if hasDisposition(criteria, AssessmentDisputed) {
		return TaskDispositionDisputed
	}
	if hasDisposition(criteria, AssessmentFail) {
		return TaskDispositionFailed
	}
	if hasDisposition(criteria, AssessmentInconclusive) {
		return TaskDispositionInconcl
	}
	if hasDisposition(criteria, AssessmentUnjudged) || hasDisposition(criteria, AssessmentStale) {
		return TaskDispositionPartial
	}
	if hasDisposition(criteria, AssessmentOperatorAccepted) {
		return TaskDispositionOperatorOK
	}
	return TaskDispositionSatisfied
}

// hasDisposition reports whether any criterion carries a given
// assessment disposition.
func hasDisposition(criteria map[string]CriterionAssessment, disposition string) bool {
	for _, a := range criteria {
		if a.Disposition == disposition {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// F-07: dispatch consumer-readiness projection (additive only).
//
// A dispatched task is an assignment, not a running worker. This
// projection surfaces which side of the handoff the assignment sits
// on, distinctly, so operators (and MCP readers) never mistake a
// parked assignment for a healthy handoff:
//
//   - dispatchable: pending, admittable work — no consumer needed yet.
//   - pickup_pending: dispatched with no live consumer behind the
//     session. Carries the blocked reason and the pickup deadline.
//   - consumer_attached: dispatched with a live loop heartbeat
//     behind the session. Carries the consumer generation.
//   - execution_claimed: a consumer claimed the attempt (working and
//     everything downstream). The claim receipt names the
//     generation; lifecycle disposition stays with taskAssurance.
//
// Pure projection over the task row plus the consumer table: no
// writes, no transitions, never authoritative for execution.

// Dispatch consumer-readiness states. Compare against the constants,
// not literals.
const (
	DispatchStateDispatchable     = "dispatchable"
	DispatchStateConsumerAttached = "consumer_attached"
	DispatchStatePickupPending    = "pickup_pending"
	DispatchStateExecutionClaimed = "execution_claimed"
)

// TaskDispatchState is one task's consumer-readiness posture.
type TaskDispatchState struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	Attempt  int    `json:"attempt"`
	State    string `json:"state"`
	Attached bool   `json:"consumer_attached"`
	// Generation is the consumer generation behind a
	// consumer_attached assignment, else 0.
	Generation int `json:"consumer_generation"`
	// PickupDeadline bounds the wait for a consumer pickup on a
	// pickup_pending assignment, else "".
	PickupDeadline string `json:"pickup_deadline"`
	// BlockedReason names why no consumer can pick the assignment
	// up; empty unless pickup_pending.
	BlockedReason string `json:"blocked_reason"`
	// Overdue is true when a pickup_pending assignment waited past
	// its pickup deadline.
	Overdue bool `json:"overdue"`
}

// taskDispatchState projects one task's consumer-readiness posture.
func taskDispatchState(taskID string) (TaskDispatchState, error) {
	t, err := getTask(taskID)
	if err != nil {
		return TaskDispatchState{}, err
	}
	return projectTaskDispatchState(t), nil
}

// projectTaskDispatchState derives the readiness state from a task
// row plus the live consumer table. Pure projection: no DB writes,
// so it runs anywhere a status display needs it.
func projectTaskDispatchState(t Task) TaskDispatchState {
	st := TaskDispatchState{
		TaskID:  t.ID,
		Agent:   t.Agent,
		Status:  t.Status,
		Attempt: t.Attempt,
	}
	switch t.Status {
	case TaskPending:
		st.State = DispatchStateDispatchable
		return st
	case TaskDispatched:
		if c, ok := attachedWorkerConsumer(t.Agent); ok {
			st.State = DispatchStateConsumerAttached
			st.Attached = true
			st.Generation = c.Generation
			return st
		}
		st.State = DispatchStatePickupPending
		st.PickupDeadline = dispatchPickupDeadlineFor(t)
		st.BlockedReason = dispatchBlockedReasonForSession(t.Agent)
		if deadline, err := time.Parse(time.RFC3339, st.PickupDeadline); err == nil {
			st.Overdue = time.Now().UTC().After(deadline)
		}
		return st
	default:
		st.State = DispatchStateExecutionClaimed
		return st
	}
}

// latestMuseMessageRow returns the newest direct-message intent for
// one session. Read-only and bounded to a single row; it never
// contacts Muse. found=false with err==nil means no row was ever
// recorded; err!=nil means storage itself failed. UI projections
// must render those two cases differently: absent history is
// "none recorded", a store that cannot answer is "unavailable".
// The sql.ErrNoRows distinction lives here so UI files never
// import the database driver (boundary: read/UI adapters render
// projections, never query persistence).
func latestMuseMessageRow(name string) (req, turn, terminal string, found bool, err error) {
	db, err := openReadOnlyDB()
	if err != nil {
		return "", "", "", false, err
	}
	defer db.Close()
	err = db.QueryRow(`SELECT request_id, turn_id, terminal FROM muse_message_intents
		WHERE session = ? ORDER BY updated DESC LIMIT 1`, name).Scan(&req, &turn, &terminal)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", "", false, nil
		}
		return "", "", "", false, err
	}
	return req, turn, terminal, true, nil
}

// sessionBusyAll is the read-model seam behind task occupancy:
// agent spellings with an open dispatched/working
// task. One query per call; callers preload once per listing.
// known=false means the store could not answer (unreadable/missing
// tasks table) — the projection must fall back to running, never
// ready.
func sessionBusyAll() (map[string]bool, bool) {
	db, err := openTasksDB()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	rows, err := db.Query(`SELECT DISTINCT agent FROM tasks WHERE status IN ('dispatched','working')`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var agent string
		if err := rows.Scan(&agent); err != nil {
			continue
		}
		if agent != "" {
			out[agent] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return out, true
}

// taskStatusCounts projects committed task-row counts by
// lifecycle status for the CONTROLLER execution progression.
// Read-only; a read failure yields an empty map (the pane then
// omits the line rather than fabricating zeros).
func taskStatusCounts() map[string]int {
	db, err := openTasksDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	rows, err := db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err == nil {
			out[st] = n
		}
	}
	return out
}

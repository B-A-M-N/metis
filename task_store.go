package main

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// task_store.go: durable task-ledger reads and writes.
//
// Schema creation and migration live in task_schema.go. This file
// owns HOW task facts persist; lifecycle transitions live in
// task_lifecycle.go, scheduling in task_dispatch.go.

func scanTask(row *sql.Row) (Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Run, &t.Agent, &t.Objective, &t.Acceptance, &t.Status, &t.Attempt, &t.Parent, &t.Report, &t.Created, &t.Updated, &t.Key, &t.CorrectionNo, &t.ParentReport, &t.ExecutionRoot, &t.SharedWorkspace)
	return t, err
}

// createTask records a new pending task.
func createTask(run, agent, objective, acceptance string) (Task, error) {
	return createTaskIdem(run, agent, objective, acceptance, "")
}

// createTaskIdem records a new pending task, or returns the existing task
// when a non-empty idempotency key was already used in this run with an
// identical payload. Concurrent callers with the same key converge on one
// row via the unique index. A reused key with a DIFFERENT payload is an
// explicit conflict (TD-028): it is never silently folded into the
// original task.
func createTaskIdem(run, agent, objective, acceptance, key string) (Task, error) {
	if strings.TrimSpace(run) == "" || strings.TrimSpace(agent) == "" || strings.TrimSpace(objective) == "" {
		return Task{}, fmt.Errorf("task requires run, agent, and objective")
	}
	if key != "" {
		if existing, err := findTaskByKey(run, key); err == nil {
			if !sameTaskPayload(existing, agent, objective, acceptance) {
				return Task{}, &idemConflictError{Run: run, Key: key, Existing: existing.ID}
			}
			return existing, nil
		}
	}
	t := Task{
		ID: newTaskID(), Run: run, Agent: agent,
		Objective: objective, Acceptance: acceptance,
		Status: TaskPending, Created: nowUTC(), Updated: nowUTC(), Key: key,
	}
	// Idempotent replay first: an existing row for this key is
	// returned even after close — replay creates no new work.
	if key != "" {
		if existing, ferr := findTaskByKey(run, key); ferr == nil {
			if !sameTaskPayload(existing, agent, objective, acceptance) {
				return Task{}, &idemConflictError{Run: run, Key: key, Existing: existing.ID}
			}
			return existing, nil
		}
	}
	// A finalized run admits no new work. This advisory check is a
	// fast path only; the authoritative check runs under the write
	// lock below, since a finalization may commit between here and
	// the insert (P0-3).
	if closed, cerr := runClosed(run); cerr != nil {
		return Task{}, cerr
	} else if closed {
		return Task{}, fmt.Errorf("run %q is closed (finalized); refusing new task", run)
	}
	db, err := openTasksDB()
	if err != nil {
		return Task{}, err
	}
	defer db.Close()
	if err := ensureRunStatesTable(db); err != nil {
		return Task{}, err
	}
	// Serialize against finalization (and concurrent admissions):
	// the closed re-check and the insert commit atomically, so a
	// close landing after the advisory check above still refuses.
	if _, err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return Task{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = db.Exec(`ROLLBACK`)
		}
	}()
	if closed, cerr := runClosedTx(db, run); cerr != nil {
		return Task{}, cerr
	} else if closed {
		return Task{}, fmt.Errorf("run %q is closed (finalized); refusing new task", run)
	}
	_, err = db.Exec(`INSERT INTO tasks (id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace)
		VALUES (?, ?, ?, ?, ?, ?, 0, '', '', ?, ?, ?, 0, '', '', 0)`,
		t.ID, t.Run, t.Agent, t.Objective, t.Acceptance, t.Status, t.Created, t.Updated, t.Key)
	if err != nil {
		// Release the write lock before the fallback read, which
		// opens its own handle: holding it would serialize (or
		// deadlock under a short busy_timeout) against itself.
		_, _ = db.Exec(`ROLLBACK`)
		committed = true
		// Lost a concurrent create with the same key: return the winner,
		// unless the winner carries a different payload (conflict).
		if key != "" {
			if existing, ferr := findTaskByKey(run, key); ferr == nil {
				if !sameTaskPayload(existing, agent, objective, acceptance) {
					return Task{}, &idemConflictError{Run: run, Key: key, Existing: existing.ID}
				}
				return existing, nil
			}
		}
		return Task{}, err
	}
	if _, err := db.Exec(`COMMIT`); err != nil {
		return Task{}, err
	}
	committed = true
	return t, nil
}

// findTaskByKey returns the task created earlier under (run, key).
func findTaskByKey(run, key string) (Task, error) {
	db, err := openTasksDB()
	if err != nil {
		return Task{}, err
	}
	defer db.Close()
	t, err := scanTask(db.QueryRow(`SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks WHERE run = ? AND key = ? AND key <> ''`, run, key))
	if err == sql.ErrNoRows {
		return Task{}, fmt.Errorf("no task with key %q in run %q", key, run)
	}
	return t, err
}

// getTaskOn is the handle-scoped core of getTask: read paths that
// already hold an open handle (graph renders, readiness scans) share
// it instead of opening per row. The public wrapper keeps one-shot
// semantics for single-row callers.
func getTaskOn(db *sql.DB, id string) (Task, error) {
	t, err := scanTask(db.QueryRow(`SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return Task{}, fmt.Errorf("no such task: %s", id)
	}
	return t, err
}

func getTask(id string) (Task, error) {
	db, err := openTasksDB()
	if err != nil {
		return Task{}, err
	}
	defer db.Close()
	return getTaskOn(db, id)
}

// listTasksOn is the handle-scoped core of listTasks: same rows,
// shared handle, no per-call open.
func listTasksOn(db *sql.DB, run string) ([]Task, error) {
	q := `SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks ORDER BY created`
	var rows *sql.Rows
	var err error
	if run != "" {
		q = `SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks WHERE run = ? ORDER BY created`
		rows, err = db.Query(q, run)
	} else {
		rows, err = db.Query(q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Run, &t.Agent, &t.Objective, &t.Acceptance, &t.Status, &t.Attempt, &t.Parent, &t.Report, &t.Created, &t.Updated, &t.Key, &t.CorrectionNo, &t.ParentReport, &t.ExecutionRoot, &t.SharedWorkspace); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// listTasks is the one-shot wrapper over listTasksOn: single-call
// sites keep open/close semantics; shared-handle paths call the core.
func listTasks(run string) ([]Task, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return listTasksOn(db, run)
}

func sameTaskPayload(t Task, agent, objective, acceptance string) bool {
	return t.Agent == agent && t.Objective == objective && t.Acceptance == acceptance
}

// listTasksByAgent returns one session's tasks, oldest first, over
// the (agent, created) index — the cancel cascade and other
// per-session readers never load the whole task history.
func listTasksByAgent(agent string) ([]Task, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, run, agent, objective, acceptance, status, attempt, parent, report, created, updated, key, correction_no, parent_report, execution_root, shared_workspace FROM tasks WHERE agent = ? ORDER BY created`, agent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.Run, &t.Agent, &t.Objective, &t.Acceptance, &t.Status, &t.Attempt, &t.Parent, &t.Report, &t.Created, &t.Updated, &t.Key, &t.CorrectionNo, &t.ParentReport, &t.ExecutionRoot, &t.SharedWorkspace); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// latestObjectiveByAgent returns the newest task objective per
// worker agent as one grouped query over the (agent, created)
// index — the dashboard refresh path titles every tab without
// loading the whole task history. SQLite resolves the bare
// `objective` column from the row holding each group's
// MAX(created), so interleaved agents each report their own
// latest objective.
func latestObjectiveByAgent() (map[string]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT agent, objective, MAX(created) FROM tasks GROUP BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var agent, objective, created string
		if err := rows.Scan(&agent, &objective, &created); err != nil {
			return nil, err
		}
		out[agent] = objective
	}
	return out, rows.Err()
}

// requireRequest rejects anonymous or unattributed state changes.
func requireRequest(r OperationRequest) error {
	if strings.TrimSpace(r.Actor) == "" {
		return fmt.Errorf("operation requires an authorized actor (TD-006)")
	}
	if strings.TrimSpace(r.RequestID) == "" {
		return fmt.Errorf("operation requires an identifiable request ID (TD-006)")
	}
	return nil
}

// latestAttempt returns the highest attempt_no row for a task.
func latestAttempt(db *sql.DB, taskID string) (TaskAttempt, error) {
	var a TaskAttempt
	err := db.QueryRow(`SELECT id, task_id, attempt_no, state, request_id, actor, created, updated
		FROM task_attempts WHERE task_id = ? ORDER BY attempt_no DESC LIMIT 1`, taskID).Scan(
		&a.ID, &a.TaskID, &a.AttemptNo, &a.State, &a.RequestID, &a.Actor, &a.Created, &a.Updated)
	if err == sql.ErrNoRows {
		return a, fmt.Errorf("no attempts for task %s", taskID)
	}
	return a, err
}

// latestDelivery returns the most recent delivery row for an attempt.
func latestDelivery(db *sql.DB, attemptID string) (TaskDelivery, error) {
	var d TaskDelivery
	err := db.QueryRow(`SELECT id, attempt_id, task_id, state, request_id, actor, detail, created, updated
		FROM task_deliveries WHERE attempt_id = ? ORDER BY rowid DESC LIMIT 1`, attemptID).Scan(
		&d.ID, &d.AttemptID, &d.TaskID, &d.State, &d.RequestID, &d.Actor, &d.Detail, &d.Created, &d.Updated)
	if err == sql.ErrNoRows {
		return d, fmt.Errorf("no deliveries for attempt %s", attemptID)
	}
	return d, err
}

// advanceDeliveriesForTask moves open delivery rows to a new state.
// Best-effort bulk move used after the task transition commits; rows
// belonging to superseded attempts are left untouched by the state
// predicate at each call site.
func advanceDeliveriesForTask(id string, from []string, to string) error {
	return advanceTaskRows("task_deliveries", id, from, to)
}

// advanceAttemptsForTask moves open attempt rows to a new state.
func advanceAttemptsForTask(id string, from []string, to string) error {
	return advanceTaskRows("task_attempts", id, from, to)
}

func advanceTaskRows(table, id string, from []string, to string) error {
	if len(from) == 0 {
		return fmt.Errorf("advance requires explicit from-states")
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	q := `UPDATE ` + table + ` SET state = ?, updated = ? WHERE task_id = ? AND state IN (` +
		strings.Repeat("?,", len(from)-1) + `?)`
	args := []any{to, nowUTC(), id}
	for _, s := range from {
		args = append(args, s)
	}
	_, err = db.Exec(q, args...)
	return err
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func newTaskID() string {
	// Collision-resistant across processes (P0-A): rapid bursts in
	// one second must never collide, or concurrent dispatch tests
	// (and real bursts) fail at setup with UNIQUE violations
	// instead of testing races.
	return newUniqueID("task")
}

func newRequestID() string {
	return newUniqueID("req")
}

func newAttemptID() string {
	return newUniqueID("atm")
}

func newDeliveryID() string {
	return newUniqueID("dlv")
}

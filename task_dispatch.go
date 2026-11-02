package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// task_dispatch.go: admission, workspace binding, dispatch, delivery.
//
// Validate -> reserve attempt -> prepare instruction/sidecar files ->
// single-transaction commit (claim + attempt + deliveries) -> worker
// may claim. Filesystem prepare precedes the commit; the ledger stays
// authoritative and workers refuse file/ledger divergence.

// sessionBindable refuses dispatch to sessions that cannot run work:
// unknown names, stopped registry entries, dead tmux sessions, and PTY
// sessions without a live supervisor and worker.
func sessionBindable(name string) error {
	_, err := resolveExecTarget(name)
	return err
}

// sessionWorkdir resolves the workspace a session executes in.
func sessionWorkdir(session string) (string, error) {
	if isPtySession(session) {
		m, err := ptyGet(ptyBare(session))
		if err != nil {
			return "", err
		}
		if m.Workdir == "" {
			return "", fmt.Errorf("no such session: %s", session)
		}
		return m.Workdir, nil
	}
	reg := loadRegistry()
	e, ok := reg[session]
	if !ok {
		return "", fmt.Errorf("no such session: %s", session)
	}
	return e.Workdir, nil
}

// canonicalRoot normalizes a workspace path so the same directory
// reached via different spellings (symlinks, relative segments,
// trailing slashes) compares equal. Root identity is the
// canonical string; without this, /tmp vs /private/tmp aliases
// would defeat occupancy and binding checks (TD-050).
func canonicalRoot(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("workspace root must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// sessionRoot resolves the canonical workspace a session executes in.
func sessionRoot(session string) (string, error) {
	workdir, err := sessionWorkdir(session)
	if err != nil {
		return "", err
	}
	return canonicalRoot(workdir)
}

// liveOccupants returns non-terminal tasks bound to root, excluding
// one task id. A root is occupied while any occupant is dispatched,
// working, or awaiting a verdict; terminal tasks release it (TD-051).
func liveOccupants(root, exclude string) ([]string, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, status FROM tasks WHERE execution_root = ? AND id <> ? AND status IN ('dispatched','working','report_ready','cancelling','termination_unknown') ORDER BY id`, root, exclude)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s (%s)", id, status))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// authorizeTaskRoot pins a pending, unbound task to an authorized
// workspace before dispatch. Dispatch to any other root is refused
// (TD-050). The authorization is attributed and receipted.
func authorizeTaskRoot(id, root string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	canon, err := canonicalRoot(root)
	if err != nil {
		return err
	}
	if st, serr := os.Stat(canon); serr != nil || !st.IsDir() {
		return fmt.Errorf("workspace root %q is not an accessible directory", root)
	}
	t, err := getTask(id)
	if err != nil {
		return err
	}
	if t.Status != TaskPending {
		return fmt.Errorf("task %s is %s; roots authorize only pending tasks", id, t.Status)
	}
	if t.ExecutionRoot != "" && t.ExecutionRoot != canon {
		return fmt.Errorf("task %s already bound to %s; refusing rebind to %s", id, t.ExecutionRoot, canon)
	}
	if t.ExecutionRoot == canon {
		return nil
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET execution_root = ?, updated = ? WHERE id = ? AND status = 'pending' AND execution_root = ''`, canon, nowUTC(), id); err != nil {
		return err
	}
	recordReceipt(id, "root_authorized", fmt.Sprintf("actor=%s req=%s root=%s", req.Actor, req.RequestID, canon))
	return nil
}

// authorizeSharedWorkspace records explicit, attributed permission
// for a task to execute in a workspace that already hosts another
// live task. Silence is never permission (TD-051).
func authorizeSharedWorkspace(id string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	t, err := getTask(id)
	if err != nil {
		return err
	}
	if t.Status != TaskPending {
		return fmt.Errorf("task %s is %s; sharing authorizes only pending tasks", id, t.Status)
	}
	if t.SharedWorkspace == 1 {
		return nil
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET shared_workspace = 1, updated = ? WHERE id = ? AND status = 'pending'`, nowUTC(), id); err != nil {
		return err
	}
	recordReceipt(id, "shared_authorized", fmt.Sprintf("actor=%s req=%s", req.Actor, req.RequestID))
	return nil
}

// deliverTask writes the task instruction into the worker's workspace
// before the dispatch transition commits. The content derives solely
// from the task row, so concurrent dispatch racers write identical
// bytes and only the transition winner proceeds. The workspace
// resolves decoupled (live shell backing, else the logical record)
// so native-only sessions receive instructions; an unknown name
// still fails with the shell store's error, never an empty root.
func deliverTask(t Task) (string, error) {
	root, err := resolveSessionWorkspace(t.Agent)
	if err != nil {
		return "", err
	}
	body := fmt.Sprintf("# Task %s\n\nrun: %s\nagent: %s\ncreated: %s\n\n## Objective\n\n%s\n\n## Acceptance\n\n%s\n",
		t.ID, t.Run, t.Agent, t.Created, t.Objective, t.Acceptance)
	path := filepath.Join(root, "task-"+t.ID+".md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return "", fmt.Errorf("instruction delivery failed: %w", err)
	}
	return path, nil
}

// dispatchTask assigns a pending task to a worker session and delivers
// the instruction into the session's workspace. It carries a synthetic
// identifiable request (TD-006) so the legacy CLI path stays attributable;
// request-scoped callers should use dispatchTaskWithRequest directly.
func dispatchTask(id, session string) error {
	return dispatchTaskWithRequest(id, session, OperationRequest{
		Actor:     "local-cli",
		RequestID: newRequestID(),
	})
}

// dispatchTaskWithRequest claims the pending task atomically through
// the DispatchService pipeline (admit -> prepare -> commit). The
// conditional claim transaction is the one winner-maker; a lost race
// returns the conflict and reconciles files left without a commit.
//
// AdmitDispatch (dispatch_plan.go) is the single shared admission
// path — every producer (CLI, MCP, worker loop, fleet adapter)
// funnels through it, and no caller commits an attempt without it
// (P0-B).
func dispatchTaskWithRequest(id, session string, req OperationRequest) (opErr error) {
	defer func() { observeOps(opsPhaseDispatch, req.RequestID, id, 0, session, opErr) }()
	outcome, err := DispatchService{}.Dispatch(id, session, req)
	if err != nil {
		return err
	}
	// F-07: the commit is authoritative, but it guarantees no
	// consumer. Record which side of the handoff this assignment
	// landed on — attached, or pending pickup with a deadline and
	// a blocked reason — so a parked session with no loop never
	// looks like a healthy handoff.
	if outcome.Dispatched {
		noteDispatchConsumerState(id, session, req)
		// P3 pickup metrics (additive only): stamp the dispatch
		// time. Best-effort — the error is dropped so metrics can
		// never alter admission or claim semantics.
		_ = recordPickupDispatched(outcome.TaskID, outcome.Attempt, nowPickupTime())
	}
	return nil
}

// ---------------------------------------------------------------------------
// F-07: dispatch admission with no consumer.
//
// The dispatch commit stays the execution authority, but a commit to
// a session with no live worker loop is an assignment with nobody to
// pick it up. These helpers give that assignment a pickup deadline
// and a blocked reason, receipted at dispatch time and projected by
// taskDispatchState (task_readmodel.go). They never refuse a
// dispatch: the ledger commit is the authority, and a later pickup
// still executes exactly once.

// dispatchPickupDeadline bounds how long a dispatched assignment may
// wait for a consumer pickup before it counts as overdue. A var (not
// a const) so tests can shrink the window without sleeping through
// the production value.
var dispatchPickupDeadline = 10 * time.Minute

// dispatchPickupDeadlineFor returns the pickup deadline for one
// task, measured from its last update (the dispatch commit).
func dispatchPickupDeadlineFor(t Task) string {
	base, err := time.Parse(time.RFC3339, t.Updated)
	if err != nil {
		base = time.Now().UTC()
	}
	return base.Add(dispatchPickupDeadline).UTC().Format(time.RFC3339)
}

// dispatchBlockedReasonForSession names why assignments to session
// have no consumer to pick them up. Empty means attached.
func dispatchBlockedReasonForSession(session string) string {
	return workerConsumerBlockedReason(session)
}

// dispatchAssignmentOverdue reports whether a dispatched task waited
// past its pickup deadline for a consumer that never attached.
func dispatchAssignmentOverdue(t Task) bool {
	if t.Status != TaskDispatched {
		return false
	}
	deadline, err := time.Parse(time.RFC3339, dispatchPickupDeadlineFor(t))
	if err != nil {
		return false
	}
	return time.Now().UTC().After(deadline)
}

// noteDispatchConsumerState receipts the consumer side of a freshly
// committed dispatch: consumer_attached when a live loop heartbeat
// backs the session, pickup_pending (with deadline and blocked
// reason) when nobody is there to pick it up. Best-effort: the
// commit already won, and tracking must never overturn it.
func noteDispatchConsumerState(taskID, session string, req OperationRequest) {
	t, err := getTask(taskID)
	if err != nil {
		return
	}
	if c, ok := attachedWorkerConsumer(session); ok {
		recordReceipt(taskID, "consumer_attached",
			fmt.Sprintf("actor=%s req=%s attempt=%d session=%s generation=%d capabilities=%q",
				req.Actor, req.RequestID, t.Attempt, session, c.Generation, c.Capabilities))
		return
	}
	recordReceipt(taskID, "pickup_pending",
		fmt.Sprintf("actor=%s req=%s attempt=%d session=%s pickup_deadline=%s blocked=%q",
			req.Actor, req.RequestID, t.Attempt, session, dispatchPickupDeadlineFor(t), dispatchBlockedReasonForSession(session)))
}

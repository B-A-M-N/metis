package main

import (
	"database/sql"
	"fmt"
	"io"
	"time"
)

// worker_subprocess.go: subprocess worker execution with bounded output
// capture. runWorkerOnce is a short coordinator over the typed phases
// in worker_phases.go: resolve, claim, execute, persist, settle. It
// never schedules and never infers success from launch or output.

const workerStreamCap = 256 << 10

// cappedWriter stores the first limit bytes on w and drains the
// rest: the producer never blocks on a full bound, and Discarded
// reports exactly what was not stored. Callers receipt the loss.
type cappedWriter struct {
	w         io.Writer
	limit     int
	stored    int
	discarded int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	kept := 0
	if c.stored < c.limit {
		kept = c.limit - c.stored
		if kept > len(p) {
			kept = len(p)
		}
		if _, err := c.w.Write(p[:kept]); err != nil {
			return 0, err
		}
		c.stored += kept
	}
	c.discarded += len(p) - kept
	// Full consumption: excess is drained, never stored, and the
	// producer never blocks on a full bound.
	return len(p), nil
}

// waitForCommittedWork blocks until the ledger holds at least one
// task committed to session, the deadline passes, or stop closes.
// It never executes anything: it only reports whether the loop has
// work to claim. A nil stop channel waits until the deadline.
func waitForCommittedWork(session string, poll, maxWait time.Duration, stop <-chan struct{}) bool {
	// One handle for the whole wait: opening and migrating a
	// handle per poll starves concurrent writers (per-open DDL
	// plus WAL upgrade churn holds locks a dispatch commit
	// cannot ride out). A single read handle never blocks a writer.
	db, err := openTasksDB()
	if err != nil {
		return false
	}
	defer db.Close()
	deadline := time.Now().Add(maxWait)
	for {
		if ids, qerr := dispatchedTasksForSessionDB(db, session); qerr == nil && len(ids) > 0 {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		sleep := poll
		if sleep > remaining {
			sleep = remaining
		}
		if stop == nil {
			time.Sleep(sleep)
			continue
		}
		select {
		case <-stop:
			return false
		case <-time.After(sleep):
		}
	}
}

// runWorkerOnce executes at most one ledger-committed task bound to
// session with adapter a. It returns true when it executed a task,
// false with nil error when idle. Every refusal names what stopped
// it and executes nothing. A fleet settlement failure after an
// established outcome returns (true, settlementErr): the attempt's
// report stands as filed and only the fleet binding needs repair.
func runWorkerOnce(session string, a Adapter, req OperationRequest) (done bool, err error) {
	if err := requireRequest(req); err != nil {
		return false, err
	}
	if a.ManagedArgv == nil && a.ManagedTurn == nil {
		return false, fmt.Errorf("harness %q has no managed invocation; the worker loop cannot launch it", a.Harness)
	}
	// F-07: a running loop is an attached consumer, idle or not.
	// Best-effort: a tracking failure must never fail an execution.
	_, _ = heartbeatWorkerConsumer(session, a)
	// Phase 1: resolve the assigned task and validate its attempt.
	asn, idle, err := workerResolveAssignment(session, a, req)
	if err != nil {
		return false, err
	}
	if idle {
		return false, nil
	}
	// Phase 2: claim the execution and fleet ownership for the
	// exact attempt. Phase 5 (fleet settlement) runs on every exit
	// below via defer, naming the same attempt identity the claim
	// bound. A settlement failure surfaces as the returned error
	// without touching the task ledger: when the attempt already
	// established its outcome (done=true), the report stands and
	// only the fleet binding still needs reconcileFleetSettlement.
	cl, err := workerClaimAttempt(session, asn)
	if err != nil {
		return false, err
	}
	// F-07: the attempt settled on every exit below (report filed,
	// superseded, or failed), so the consumer goes idle again. The
	// next claim re-binds; a loop restart clears it outright.
	// Best-effort: settlement results above decide the return.
	defer releaseWorkerAssignment(session)
	defer func() {
		if sres := workerSettleFleetOwnership(cl, err); sres.Err != nil {
			if err == nil {
				err = sres.Err
				return
			}
			err = fmt.Errorf("%w; fleet settlement also failed: %v (task outcome preserved; reconcile fleet settlement)", err, sres.Err)
		}
	}()
	// Phase 3: execute the native turn or the subprocess. The
	// native branch files its own downstream records.
	out, err := workerExecuteAttempt(session, a, cl)
	if err != nil {
		return false, err
	}
	if out.native {
		return out.done, nil
	}
	// Phase 4: persist the waited outcome and its evidence.
	return workerPersistOutcome(session, a, cl, out.exec)
}

// ensureReceiptsTableRef keeps execution receipts queryable even
// when only the worker path has run. Tables are ensured by
// ensureLifecycleTables; this is a compile-time anchor.

// ---------------------------------------------------------------------------
// F-07: durable worker-consumer identity.
//
// Dispatch admission used to commit a dispatched task with no guarantee
// a worker consumer existed to pick it up: the assignment sat in
// 'dispatched' with no heartbeat behind it, no deadline, and no
// blocked reason, so a parked session with no loop looked identical
// to a healthy handoff. worker_consumers is the durable half of
// consumer identity: one row per session carrying its heartbeat, its
// capabilities, its current assignment, and its generation.
//
//   - heartbeat: a running loop refreshes heartbeat_at (and its
//     capabilities) on every iteration, whether idle or executing.
//     Freshness within workerConsumerStaleAfter means attached.
//   - capabilities: the adapter's execution posture, recorded so an
//     operator can see WHAT is attached, not just that something is.
//   - current assignment: the task id the consumer claimed via
//     claimWorkerAssignment, cleared only by an explicit release or
//     a newer claim. It never authorizes anything — the ledger does.
//   - generation: bumped by startWorkerConsumer every time the loop
//     (re)starts. A claim receipt names its generation, so a pickup
//     from a killed loop stays distinguishable from the restart's.
//
// The table is ensured lazily by every function below (the open-path
// schema in task_schema.go is deliberately untouched): each opener
// calls ensureWorkerConsumersTable before touching the table.

// workerConsumerStaleAfter bounds how old a heartbeat may be before
// the consumer counts as detached. A var (not a const) so tests can
// shrink the window without sleeping through the production value.
var workerConsumerStaleAfter = 90 * time.Second

// workerConsumer is one session's durable consumer identity.
type workerConsumer struct {
	Session           string
	Capabilities      string
	HeartbeatAt       string
	CurrentAssignment string
	Generation        int
	Updated           string
}

// ensureWorkerConsumersTable creates the consumer identity table when
// missing. Called after open, before any consumer statement.
func ensureWorkerConsumersTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS worker_consumers (
		session TEXT PRIMARY KEY,
		capabilities TEXT NOT NULL DEFAULT '',
		heartbeat_at TEXT NOT NULL DEFAULT '',
		current_assignment TEXT NOT NULL DEFAULT '',
		generation INTEGER NOT NULL DEFAULT 1,
		updated TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// adapterCapabilities renders an adapter's execution posture as a
// stable, human-readable string for the consumer row.
func adapterCapabilities(a Adapter) string {
	return fmt.Sprintf("harness=%s oneshot=%v interactive=%v live_input=%v native=%v",
		a.Harness, a.OneShot, a.Interactive, a.AcceptsLiveInput, a.ManagedTurn != nil)
}

// startWorkerConsumer (re)starts the consumer identity for session:
// it bumps the generation, clears any stale assignment left by the
// killed loop, records the adapter's capabilities, and heartbeats.
// Every loop start — first or restart — funnels through here, so a
// restart is always observable as a new generation. It returns the
// new generation.
func startWorkerConsumer(session string, a Adapter) (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureWorkerConsumersTable(db); err != nil {
		return 0, err
	}
	now := nowUTC()
	var gen int
	if err := db.QueryRow(`SELECT generation FROM worker_consumers WHERE session = ?`, session).Scan(&gen); err == sql.ErrNoRows {
		gen = 1
		if _, err := db.Exec(`INSERT INTO worker_consumers (session, capabilities, heartbeat_at, current_assignment, generation, updated)
			VALUES (?, ?, ?, '', ?, ?)`, session, adapterCapabilities(a), now, gen, now); err != nil {
			return 0, err
		}
		return gen, nil
	} else if err != nil {
		return 0, err
	}
	gen++
	if _, err := db.Exec(`UPDATE worker_consumers SET generation = ?, capabilities = ?, heartbeat_at = ?, current_assignment = '', updated = ? WHERE session = ?`,
		gen, adapterCapabilities(a), now, now, session); err != nil {
		return 0, err
	}
	return gen, nil
}

// heartbeatWorkerConsumer refreshes a running loop's heartbeat and
// capabilities without touching its generation or current
// assignment. Best-effort liveness: runWorkerOnce calls it every
// iteration and ignores the error, so a tracking failure can never
// fail an execution. It returns the current generation.
func heartbeatWorkerConsumer(session string, a Adapter) (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureWorkerConsumersTable(db); err != nil {
		return 0, err
	}
	now := nowUTC()
	var gen int
	if err := db.QueryRow(`SELECT generation FROM worker_consumers WHERE session = ?`, session).Scan(&gen); err == sql.ErrNoRows {
		gen = 1
		if _, err := db.Exec(`INSERT INTO worker_consumers (session, capabilities, heartbeat_at, current_assignment, generation, updated)
			VALUES (?, ?, ?, '', ?, ?)`, session, adapterCapabilities(a), now, gen, now); err != nil {
			return 0, err
		}
		return gen, nil
	} else if err != nil {
		return 0, err
	}
	if _, err := db.Exec(`UPDATE worker_consumers SET heartbeat_at = ?, capabilities = ?, updated = ? WHERE session = ?`,
		now, adapterCapabilities(a), now, session); err != nil {
		return 0, err
	}
	return gen, nil
}

// claimWorkerAssignment binds the consumer's current assignment to
// the exact task the ledger claim just won, and refreshes the
// heartbeat in the same statement. It creates the row (generation 1)
// when the loop never registered — the ledger claim stays the sole
// execution authority either way. It returns the generation the
// claim executed under, for the execution_claimed receipt.
func claimWorkerAssignment(session, taskID string) (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	if err := ensureWorkerConsumersTable(db); err != nil {
		return 0, err
	}
	now := nowUTC()
	var gen int
	if err := db.QueryRow(`SELECT generation FROM worker_consumers WHERE session = ?`, session).Scan(&gen); err == sql.ErrNoRows {
		gen = 1
		if _, err := db.Exec(`INSERT INTO worker_consumers (session, capabilities, heartbeat_at, current_assignment, generation, updated)
			VALUES (?, '', ?, ?, ?, ?)`, session, now, taskID, gen, now); err != nil {
			return 0, err
		}
		return gen, nil
	} else if err != nil {
		return 0, err
	}
	if _, err := db.Exec(`UPDATE worker_consumers SET heartbeat_at = ?, current_assignment = ?, updated = ? WHERE session = ?`,
		now, taskID, now, session); err != nil {
		return 0, err
	}
	return gen, nil
}

// releaseWorkerAssignment clears the consumer's current assignment
// after the attempt settles. It never deletes identity: the
// heartbeat and generation stay so the next projection still sees
// an attached consumer with nothing to do.
func releaseWorkerAssignment(session string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureWorkerConsumersTable(db); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE worker_consumers SET current_assignment = '', updated = ? WHERE session = ?`, nowUTC(), session)
	return err
}

// getWorkerConsumer reads one session's consumer identity. The
// second return is false when the session never registered.
func getWorkerConsumer(session string) (workerConsumer, bool, error) {
	var c workerConsumer
	db, err := openTasksDB()
	if err != nil {
		return c, false, err
	}
	defer db.Close()
	if err := ensureWorkerConsumersTable(db); err != nil {
		return c, false, err
	}
	err = db.QueryRow(`SELECT session, capabilities, heartbeat_at, current_assignment, generation, updated
		FROM worker_consumers WHERE session = ?`, session).
		Scan(&c.Session, &c.Capabilities, &c.HeartbeatAt, &c.CurrentAssignment, &c.Generation, &c.Updated)
	if err == sql.ErrNoRows {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	return c, true, nil
}

// consumerFresh reports whether a heartbeat timestamp falls inside
// the attached window. Unparseable or empty heartbeats are stale:
// a consumer that cannot prove recency is detached.
func consumerFresh(heartbeatAt string) bool {
	if heartbeatAt == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, heartbeatAt)
	if err != nil {
		return false
	}
	return time.Since(ts) <= workerConsumerStaleAfter
}

// attachedWorkerConsumer returns the consumer identity when session
// has a fresh heartbeat, or false when no consumer is attached (no
// row, or a stale heartbeat from a killed loop).
func attachedWorkerConsumer(session string) (workerConsumer, bool) {
	c, ok, err := getWorkerConsumer(session)
	if err != nil || !ok {
		return workerConsumer{}, false
	}
	if !consumerFresh(c.HeartbeatAt) {
		return workerConsumer{}, false
	}
	return c, true
}

// workerConsumerAttached reports whether session currently has a
// live worker consumer behind it.
func workerConsumerAttached(session string) bool {
	_, ok := attachedWorkerConsumer(session)
	return ok
}

// workerConsumerBlockedReason names why an assignment to session
// cannot be picked up right now. Empty means a consumer is
// attached; anything else is the blocked reason surfaces in the
// pickup_pending projection and the dispatch receipt trail.
func workerConsumerBlockedReason(session string) string {
	if _, ok := attachedWorkerConsumer(session); ok {
		return ""
	}
	if _, registered, _ := getWorkerConsumer(session); registered {
		return fmt.Sprintf("dispatch blocked: worker consumer for session %q went stale (no worker-loop heartbeat within %s); start the loop to attach", session, workerConsumerStaleAfter)
	}
	return fmt.Sprintf("dispatch blocked: no worker consumer registered for session %q (no worker loop ever attached); start the loop to attach", session)
}

// ensureManagedWorkerLoop is the supervisor path for managed
// sessions: it resolves the session's bound harness, verifies the
// managed execution path, names any execution the previous loop
// left uncertain (observation only), and (re)starts the durable
// consumer identity, returning the adapter and the new generation.
// The CLI worker-loop command keeps its flag-driven flow (it allows
// --harness overrides the session binding does not carry) and calls
// startWorkerConsumer directly; programmatic supervisors (fleet
// adapters, tests) enter here.
func ensureManagedWorkerLoop(session string, req OperationRequest) (Adapter, int, error) {
	if err := requireRequest(req); err != nil {
		return Adapter{}, 0, err
	}
	harness, err := sessionHarness(session)
	if err != nil {
		return Adapter{}, 0, err
	}
	if harness == "" {
		return Adapter{}, 0, fmt.Errorf("worker-loop: session %s has no bound harness; bind one before starting the loop", session)
	}
	a, err := lookupAdapter(harness)
	if err != nil {
		return Adapter{}, 0, err
	}
	if a.ManagedArgv == nil && a.ManagedTurn == nil {
		return Adapter{}, 0, fmt.Errorf("harness %q has no managed execution path; refusing to supervise", harness)
	}
	if _, err := reconcileLoopRestart(session, req); err != nil {
		return Adapter{}, 0, err
	}
	gen, err := startWorkerConsumer(session, a)
	if err != nil {
		return Adapter{}, 0, err
	}
	return a, gen, nil
}

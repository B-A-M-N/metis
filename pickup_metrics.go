package main

// pickup_metrics.go: P3 pickup-latency + queue-depth metrics (Metis
// audit remediation P3).
//
// Disjoint ownership: this file owns the pickup_observations table
// (dispatched_at, claimed_at per task/attempt), the queue-depth
// gauge query (dispatched-but-unclaimed count), and the pure
// projection from timestamps to pickup latency + queue age. No
// other file reads or writes this table.
//
// Hooks into task_dispatch.go are ADDITIVE ONLY: after a committed
// dispatch, recordPickupDispatched stores the dispatch timestamp.
// The call is best-effort (its error is dropped by the caller) so
// metrics can never alter admission or claim semantics. The claim
// side is observed through observePickupClaim, the seam the claim
// path wires; syncPickupClaimFromLedger backfills a missed claim
// observation from the durable ledger without touching it.
//
// The queue-depth gauge reads the ledger (tasks in dispatched
// status), not this table, so it can never drift from the
// authority. The latency/age projection (projectPickup) is pure:
// no I/O, deterministic in its inputs.

import (
	"database/sql"
	"time"
)

// nowPickupTime is the metrics clock. A var (not a direct call) so
// tests can pin dispatch/claim stamps without sleeping.
var nowPickupTime = func() time.Time { return time.Now().UTC() }

// pickupObservation is one task/attempt's durable pickup stamps.
// HasDispatched/HasClaimed distinguish a recorded zero time from
// a stamp that was never recorded.
type pickupObservation struct {
	TaskID        string
	Attempt       int
	DispatchedAt  time.Time
	HasDispatched bool
	ClaimedAt     time.Time
	HasClaimed    bool
}

// pickupProjection is the pure read model over one observation:
// Latency is claimed_at - dispatched_at for a claimed attempt
// (clamped at zero against clock skew); QueueAge is now -
// dispatched_at for a pending attempt. Unrecorded inputs project
// zero, never a guess.
type pickupProjection struct {
	TaskID       string
	Attempt      int
	Claimed      bool
	Latency      time.Duration
	QueueAge     time.Duration
	DispatchedAt time.Time
	ClaimedAt    time.Time
}

// ensurePickupMetricsTable creates the table this file owns. It is
// called lazily by every writer/reader here, so no schema file
// needs to change and concurrent writers converge on IF NOT EXISTS.
func ensurePickupMetricsTable(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS pickup_observations (
		task_id TEXT NOT NULL,
		attempt INTEGER NOT NULL,
		dispatched_at TEXT NOT NULL DEFAULT '',
		claimed_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (task_id, attempt)
	)`); err != nil {
		return err
	}
	return nil
}

// recordPickupDispatched stores the dispatch timestamp for one
// task/attempt. First dispatch wins (INSERT OR IGNORE): a retry or
// a duplicate hook call never rewrites history. Best-effort by
// contract — callers drop the error so metrics stay additive.
func recordPickupDispatched(taskID string, attempt int, at time.Time) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensurePickupMetricsTable(db); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR IGNORE INTO pickup_observations (task_id, attempt, dispatched_at, claimed_at)
		VALUES (?, ?, ?, '')`,
		taskID, attempt, at.UTC().Format(time.RFC3339Nano))
	return err
}

// observePickupClaim stores the claim timestamp for one
// task/attempt. It never overwrites a recorded claim: the first
// pickup is the latency event, duplicate claims stay refused
// elsewhere. A row with no dispatch stamp (pre-metrics dispatch,
// backfilled ledger) is inserted with an empty dispatched_at so
// the claim is still durable.
func observePickupClaim(taskID string, attempt int, at time.Time) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensurePickupMetricsTable(db); err != nil {
		return err
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO pickup_observations (task_id, attempt, dispatched_at, claimed_at)
		VALUES (?, ?, '', ?)
		ON CONFLICT (task_id, attempt) DO NOTHING`,
		taskID, attempt, stamp); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE pickup_observations SET claimed_at = ?
		WHERE task_id = ? AND attempt = ? AND claimed_at = ''`,
		stamp, taskID, attempt)
	return err
}

// loadPickupObservation reads one task/attempt's stamps. A missing
// row is a zero observation, not an error: tasks dispatched before
// this table existed simply have no stamps.
func loadPickupObservation(taskID string, attempt int) (pickupObservation, error) {
	obs := pickupObservation{TaskID: taskID, Attempt: attempt}
	db, err := openTasksDB()
	if err != nil {
		return obs, err
	}
	defer db.Close()
	if err := ensurePickupMetricsTable(db); err != nil {
		return obs, err
	}
	var dispatched, claimed string
	if err := db.QueryRow(`SELECT dispatched_at, claimed_at FROM pickup_observations
		WHERE task_id = ? AND attempt = ?`, taskID, attempt).Scan(&dispatched, &claimed); err != nil {
		if err == sql.ErrNoRows {
			return obs, nil
		}
		return obs, err
	}
	if dispatched != "" {
		if t, perr := time.Parse(time.RFC3339Nano, dispatched); perr == nil {
			obs.DispatchedAt, obs.HasDispatched = t, true
		} else if t, perr := time.Parse(time.RFC3339, dispatched); perr == nil {
			obs.DispatchedAt, obs.HasDispatched = t, true
		}
	}
	if claimed != "" {
		if t, perr := time.Parse(time.RFC3339Nano, claimed); perr == nil {
			obs.ClaimedAt, obs.HasClaimed = t, true
		} else if t, perr := time.Parse(time.RFC3339, claimed); perr == nil {
			obs.ClaimedAt, obs.HasClaimed = t, true
		}
	}
	return obs, nil
}

// syncPickupClaimFromLedger backfills a missed claim observation
// from the durable ledger: when the task has moved past dispatched
// for this attempt (or a later attempt exists), the pickup happened
// even if observePickupClaim never ran. The backfill stamp is the
// task's own updated time — an approximation, documented as such —
// and it never overwrites a recorded claim. It reports whether a
// stamp was filled. Read-only on the ledger; writes only this
// file's table.
func syncPickupClaimFromLedger(taskID string, attempt int) (bool, error) {
	obs, err := loadPickupObservation(taskID, attempt)
	if err != nil {
		return false, err
	}
	if obs.HasClaimed {
		return false, nil
	}
	t, err := getTask(taskID)
	if err != nil {
		return false, err
	}
	pickedUp := t.Attempt > attempt || (t.Attempt == attempt && t.Status != TaskPending && t.Status != TaskDispatched)
	if !pickedUp {
		return false, nil
	}
	stamp := time.Now().UTC()
	if parsed, perr := time.Parse(time.RFC3339, t.Updated); perr == nil {
		stamp = parsed
	}
	if err := observePickupClaim(taskID, attempt, stamp); err != nil {
		return false, err
	}
	return true, nil
}

// pickupQueueDepth is the queue-depth gauge: tasks the ledger holds
// in dispatched status — assigned, but no worker has picked them
// up. It reads the authority directly, so a missed or duplicated
// metrics write cannot move it.
func pickupQueueDepth() (int, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status = ?`, TaskDispatched).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// projectPickup maps one observation to latency + queue age. Pure:
// no I/O, deterministic. Claimed observations project a fixed
// latency (now-independent); pending ones project an age that grows
// with now. Negative skew clamps to zero; missing stamps project
// zero with Claimed false.
func projectPickup(obs pickupObservation, now time.Time) pickupProjection {
	p := pickupProjection{TaskID: obs.TaskID, Attempt: obs.Attempt}
	if !obs.HasDispatched {
		return p
	}
	p.DispatchedAt = obs.DispatchedAt
	if obs.HasClaimed {
		p.Claimed = true
		p.ClaimedAt = obs.ClaimedAt
		if d := obs.ClaimedAt.Sub(obs.DispatchedAt); d > 0 {
			p.Latency = d
		}
		return p
	}
	if d := now.Sub(obs.DispatchedAt); d > 0 {
		p.QueueAge = d
	}
	return p
}

// summarizePickup loads one task/attempt's stamps and projects
// them at now. Convenience over loadPickupObservation +
// projectPickup; still read-only.
func summarizePickup(taskID string, attempt int, now time.Time) (pickupProjection, error) {
	obs, err := loadPickupObservation(taskID, attempt)
	if err != nil {
		return pickupProjection{}, err
	}
	return projectPickup(obs, now), nil
}

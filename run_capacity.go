package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// run_capacity.go: run concurrency budgets.
//
// This file owns the live-task budget knob: setting and reading the
// explicit per-run limit, counting live (dispatched/working) tasks,
// and the advisory admission calculation. Capacity admission never
// finalizes a run and never mutates the dependency graph; the claim
// transaction re-enforces the bound atomically at dispatch time, so
// racers that both pass the advisory check still converge on one
// winner at commit.

// setRunLimit records an explicit bound on simultaneously live
// (dispatched/working) tasks for one run (TD-032). The limit is a
// scheduler knob only: it constrains dispatch, not UI panes,
// provider quotas, or host capacity, which are represented
// separately (TD-034). Retries are exempt — a retry replaces an
// already-live attempt 1:1 under its own attempt budget instead of
// consuming a new slot.
func setRunLimit(run string, maxLive int, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	if strings.TrimSpace(run) == "" {
		return fmt.Errorf("limit requires a run")
	}
	if maxLive < 1 {
		return fmt.Errorf("run %q limit must be at least 1, got %d", run, maxLive)
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(`INSERT INTO run_limits (run, max_live, actor, request_id, updated)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(run) DO UPDATE SET max_live = excluded.max_live, actor = excluded.actor, request_id = excluded.request_id, updated = excluded.updated`,
		run, maxLive, req.Actor, req.RequestID, nowUTC())
	return err
}

// getRunLimit returns a run's concurrency bound and whether one is
// set. No row means unbounded delegation for that run.
func getRunLimit(run string) (int, bool, error) {
	db, err := openTasksDB()
	if err != nil {
		return 0, false, err
	}
	defer db.Close()
	var maxLive int
	err = db.QueryRow(`SELECT max_live FROM run_limits WHERE run = ?`, run).Scan(&maxLive)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return maxLive, true, nil
}

// admitRunLimit is the run-budget calculation slice of
// AdmitDispatch: the explicit live limit plus an advisory live
// count, refused fast and side-effect free. The claim transaction
// re-enforces the bound atomically, so racers that both pass here
// still converge on one winner at commit. No row means unbounded
// delegation for that run.
func admitRunLimit(run, taskID string) (int, bool, error) {
	lim, set, lerr := getRunLimit(run)
	if lerr != nil {
		return 0, false, lerr
	}
	if !set {
		return 0, false, nil
	}
	live, lerr := liveTaskCount(run)
	if lerr != nil {
		return 0, false, lerr
	}
	if live >= lim {
		return 0, false, fmt.Errorf("run %q at concurrency limit %d (%d live); resolve a task before dispatching %s (TD-032)", run, lim, live, taskID)
	}
	return lim, true, nil
}

// liveTaskCount counts a run's tasks currently holding a worker
// slot (dispatched or working). Report-ready tasks released their
// worker; terminal tasks released everything.
func liveTaskCount(run string) (int, error) {
	tasks, err := listTasks(run)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if t.Status == TaskDispatched || t.Status == TaskWorking {
			n++
		}
	}
	return n, nil
}

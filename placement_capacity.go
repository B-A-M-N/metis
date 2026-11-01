package main

import (
	"database/sql"
	"fmt"
	"strings"
)

// placement_capacity.go: worker-slot accounting + admission against
// a supplied IMMUTABLE policy snapshot.
//
// Two phases, deliberately split:
//
//  1. Preflight (admitTeamPolicy): resolves the run's pinned
//     revision, reads the live worker count, and runs the pure
//     checkTeamSlot judgment. Fail-fast and side-effect free —
//     refusal here leaves no attempt, no delivery, no reservation.
//
//  2. Claim (teamCapacityClaimPredicate): the claim transaction
//     re-enforces the same budget atomically as a conditional-UPDATE
//     predicate evaluated under the write lock. Preflight alone is
//     INSUFFICIENT under concurrency: two dispatchers that both pass
//     the precheck must still converge on one winner at commit.
//     dispatch_commit.go (AttemptCommitter.Commit) embeds this exact
//     predicate in its claiming UPDATE — verified by reading that
//     file; this file owns the predicate text so the two cannot
//     drift without the drift being visible here.
//
// The snapshot rule: every function here takes the pinned teamPolicy
// by value. A policy edit landing between preflight and claim never
// changes what the claim enforces — the claim enforces the snapshot
// the run was admitted under.

// sessionProfile resolves the commander profile a session executes
// under: the muse binding's profile, or ambient ("") when the
// session carries no native binding. Unknown sessions are an error;
// unbound sessions are ambient, never an invented profile.
func sessionProfile(session string) (string, error) {
	if isPtySession(session) {
		m, err := ptyGet(ptyBare(session))
		if err != nil {
			return "", err
		}
		b, berr := getMuseBinding(m.Name)
		if berr != nil {
			return "", nil
		}
		return b.Profile, nil
	}
	reg := loadRegistry()
	if _, ok := reg[session]; !ok {
		return "", fmt.Errorf("no such session: %s", session)
	}
	b, berr := getMuseBinding(session)
	if berr != nil {
		return "", nil
	}
	return b.Profile, nil
}

// teamActiveCount returns live (dispatched/working) tasks across
// every run pinned to the same project revision: the Team budget
// is project-wide, not per-run, so two runs under one policy
// share the same concurrency ceiling.
func teamActiveCount(db *sql.DB, project string, rev int) (int, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM tasks t JOIN run_teams r ON r.run_id = t.run
		WHERE r.project = ? AND r.revision = ? AND t.status IN ('dispatched','working')`,
		project, rev).Scan(&n)
	return n, err
}

// teamCapacityClaimPredicate renders the atomic claim-time capacity
// predicate for one pinned snapshot: the placeholder binds the
// snapshot's ceiling, the subquery counts live workers under the
// same (project, revision). The claim UPDATE conjoins this clause
// so a dispatcher that passed preflight but lost the race matches
// zero rows and is refused instead of oversubscribing.
//
// Clause and args must be appended to the same UPDATE (in order):
//
//	claimWhere += clause
//	claimArgs = append(claimArgs, args...)
func teamCapacityClaimPredicate(pol teamPolicy) (clause string, args []any) {
	return ` AND (? > (SELECT COUNT(*) FROM tasks t JOIN run_teams r ON r.run_id = t.run WHERE r.project = ? AND r.revision = ? AND t.status IN ('dispatched','working')))`,
		[]any{pol.Limits.MaxActiveWorkers, pol.Project, pol.Revision}
}

// admitTeamPolicy resolves the pinned contract for a run and runs
// the fail-fast eligibility and capacity precheck. It is the policy
// calculation slice of AdmitDispatch: static eligibility against
// the immutable pinned revision plus an advisory live count. The
// claim transaction re-enforces capacity atomically (see
// teamCapacityClaimPredicate), so racers that both pass here still
// converge on one winner at commit.
//
// A run with no pinned policy is ungoverned: this returns a nil
// policy and version 0, and the caller skips the gate (documented,
// not silently fenced).
func admitTeamPolicy(run, harness, profile string) (pol *teamPolicy, pre, version int, err error) {
	pinned, perr := pinnedTeamPolicy(run)
	if perr != nil {
		if strings.Contains(perr.Error(), "has no pinned policy") {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, perr
	}
	db, derr := openTasksDB()
	if derr != nil {
		return nil, 0, 0, derr
	}
	defer db.Close()
	active, aerr := teamActiveCount(db, pinned.Project, pinned.Revision)
	if aerr != nil {
		return nil, 0, 0, aerr
	}
	if gerr := checkTeamSlot(pinned, harness, profile, active); gerr != nil {
		return nil, 0, 0, gerr
	}
	polCopy := pinned
	return &polCopy, active, pinned.Revision, nil
}

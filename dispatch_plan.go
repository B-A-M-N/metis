package main

// dispatch_plan.go: admission calculation for dispatch (P0-A step 1).
//
// AdmitDispatch validates every authoritative admission requirement
// BEFORE any externally visible side effect and snapshots the result
// as an immutable DispatchPlan: the identity, placement, and policy
// bounds one dispatch carries into its commit. The claim transaction
// (AttemptCommitter) re-enforces every mutable-resource bound
// atomically, so racers that both pass this fail-fast check converge
// on one winner at commit time.
//
// Transaction order is unchanged: admission performs read-only
// checks in the same sequence as before; no statement here writes.

import (
	"fmt"
	"strings"
)

// DispatchPlan is the immutable outcome of admission: identity,
// placement, and policy version for one dispatch attempt. Constructed
// once by AdmitDispatch and never mutated afterwards — the committer
// treats it as the snapshot to re-validate, not to edit.
type DispatchPlan struct {
	TaskID        string
	Attempt       int
	SessionID     string
	ExecutionRoot string
	PolicyVersion int
	RequestID     string
}

// DispatchAdmission is the validated pre-claim state one dispatch
// carries into its commit: the immutable plan plus the policy inputs
// the claim transaction re-enforces atomically. P0-B: this is the
// single shared admission path — every producer (CLI, MCP, worker
// loop, fleet adapter) funnels through AdmitDispatch, and no caller
// commits an attempt without it.
type DispatchAdmission struct {
	Plan DispatchPlan
	Task Task

	RunLimit    int
	RunLimitSet bool

	Harness string
	Profile string

	Team         *teamPolicy
	TeamPreCount int
}

// dispatchSessionScope resolves the execution scope for one dispatch
// target: canonical root, harness, commander profile. Shell-backed
// sessions resolve through the live stores (today's behavior,
// errors included); a name with no shell backing resolves through
// its active logical record, which is what lets a bound native-only
// session receive dispatched work. Stopped records and unknown names
// are refused, never treated as ambient shell sessions. Scoped to
// dispatch so retry/terminate/input guidance keep their own
// shell-backing semantics.
func dispatchSessionScope(session string) (root, harness, profile string, err error) {
	if isPtySession(session) {
		if _, perr := ptyGet(ptyBare(session)); perr == nil {
			return dispatchShellScope(session)
		}
	} else if _, ok := loadRegistry()[session]; ok {
		return dispatchShellScope(session)
	}
	ls, found, lerr := loadLogicalSession(session)
	if lerr != nil || !found {
		return "", "", "", fmt.Errorf("no such session: %s", session)
	}
	if ls.State == logicalStopped {
		return "", "", "", fmt.Errorf("dispatch refused: logical session %q is stopped", session)
	}
	if ls.Backend != TargetMuseNative {
		return "", "", "", fmt.Errorf("no such session: %s", session)
	}
	root, rerr := canonicalRoot(ls.Workspace)
	if rerr != nil {
		return "", "", "", rerr
	}
	profile = ls.Profile
	if b, berr := getMuseBinding(session); berr == nil {
		profile = b.Profile
	}
	return root, ls.Harness, profile, nil
}

// dispatchShellScope is the established shell-backed resolution:
// live registry/PTY stores, with their exact errors preserved.
func dispatchShellScope(session string) (root, harness, profile string, err error) {
	root, err = sessionRoot(session)
	if err != nil {
		return "", "", "", err
	}
	harness, err = sessionHarness(session)
	if err != nil {
		return "", "", "", err
	}
	profile, err = sessionProfile(session)
	if err != nil {
		return "", "", "", err
	}
	return root, harness, profile, nil
}

// AdmitDispatch validates every authoritative admission requirement
// BEFORE any externally visible side effect: request identity,
// session bindability, dependencies, execution root, advisory
// workspace occupancy, run concurrency, and — when the run carries
// a pinned Team policy — worker eligibility and Team capacity.
// Eligibility is static against the immutable pinned revision, so
// the pre-claim check is decisive; capacity is re-enforced by a
// predicate inside the claim transaction, so racers that both pass
// this fail-fast check cannot both commit. Runs without a pinned
// policy dispatch ungoverned by Team rules (documented, not
// silently fenced).
func AdmitDispatch(id, session string, req OperationRequest) (DispatchAdmission, error) {
	var adm DispatchAdmission
	if err := requireRequest(req); err != nil {
		return adm, err
	}
	if err := sessionBindable(session); err != nil {
		return adm, err
	}
	// Dependency enforcement (TD-030): a task with unsatisfied
	// mandatory dependencies must not start. Checked before the
	// claim so a refused dispatch leaves no attempt or delivery.
	if blocking, derr := unsatisfiedDeps(id); derr != nil {
		return adm, derr
	} else if len(blocking) > 0 {
		return adm, fmt.Errorf("task %s cannot start: unsatisfied dependencies: %s", id, strings.Join(blocking, ", "))
	}
	pending, err := getTask(id)
	if err != nil {
		return adm, err
	}
	adm.Task = pending
	// Admission is the first gate, so it must refuse what the
	// claim transaction would refuse: only a pending task can be
	// dispatched. Without this, a dispatcher that admits after a
	// rival's commit prepares files (including the attempt
	// sidecar) for an attempt the ledger will never bind, and the
	// same-root keep rule then preserves a sidecar that
	// disagrees with the committed attempt.
	if pending.Status != TaskPending {
		return adm, fmt.Errorf("task %s is %s, expected pending (concurrent dispatch won)", id, pending.Status)
	}
	// Run concurrency bound (TD-032): a run at its explicit live
	// limit refuses new delegation until a slot frees. The pre-claim
	// check refuses fast and side-effect free; the claim itself
	// re-enforces the bound atomically (see AttemptCommitter), so
	// racers that both pass this check cannot both claim.
	if lim, set, lerr := admitRunLimit(pending.Run, id); lerr != nil {
		return adm, lerr
	} else if set {
		adm.RunLimit, adm.RunLimitSet = lim, true
	}
	// Execution-root correctness (TD-050): the task executes in the
	// session's canonical workspace. A pre-authorized root is
	// enforced; otherwise the first dispatch binds it immutably.
	// Scope resolves dispatch-wide (shell stores, else the active
	// logical record) so bound native-only sessions are
	// dispatchable; harness/profile come along from the same scope.
	root, harness, profile, rerr := dispatchSessionScope(session)
	if rerr != nil {
		return adm, rerr
	}
	if pending.ExecutionRoot != "" && pending.ExecutionRoot != root {
		return adm, fmt.Errorf("task %s authorized for %s; session %s executes in %s (TD-050)", id, pending.ExecutionRoot, session, root)
	}
	// Worktree isolation (TD-051): a live occupant blocks a second
	// task in the same root unless sharing was explicitly
	// authorized. Checked before the claim so refusal is side-effect
	// free (no file, no attempt, no delivery).
	if pending.SharedWorkspace == 0 {
		if occupants, oerr := liveOccupants(root, id); oerr != nil {
			return adm, oerr
		} else if len(occupants) > 0 {
			return adm, fmt.Errorf("workspace %s occupied by live task(s) %s; authorize sharing before dispatching %s (TD-051)", root, strings.Join(occupants, ", "), id)
		}
	}
	// Team policy (P0-B): when the run pinned a Team revision at
	// admission, the (harness, profile) pair must be an eligible
	// worker and live usage must sit under the project-wide budget.
	// Runs with no pinned policy skip this gate.
	adm.Harness, adm.Profile = harness, profile
	pol, pre, ver, perr := admitTeamPolicy(pending.Run, harness, profile)
	if perr != nil {
		return adm, perr
	}
	if pol != nil {
		adm.Team, adm.TeamPreCount = pol, pre
	}
	adm.Plan = DispatchPlan{
		TaskID:        id,
		Attempt:       pending.Attempt + 1,
		SessionID:     session,
		ExecutionRoot: root,
		PolicyVersion: ver,
		RequestID:     req.RequestID,
	}
	return adm, nil
}

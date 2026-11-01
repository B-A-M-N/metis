package main

// coverage_gate.go: READ-ONLY assessment of unresolved obligations
// for an exact (task, attempt, evidence revision).
//
// assessCoverageGate reads the duties reviewing exactly digest and
// returns a typed blocking result; it writes nothing and decides
// nothing. The verification authority (verification.go) turns that
// result into a refusal, and the run-finalize checkpoint reads the
// run-wide blocking set the same way. Satisfied and excepted rows
// never block; excepted is the operator's explicit waiver.
//
// Role boundary (F-03/F-04): this gate keeps its required-reviews
// role only. It blocks completion while owed review duties are
// outstanding; it never judges criterion correctness, never reads
// the judgment ledger, and no duty it tracks can substitute for a
// criterion judgment. Conversely, filing or satisfying a
// criterion judgment never satisfies a duty here — only the
// obligation's own transitions do. Correctness is judged by the
// assessment projection (verdict.go) under the verification
// authority (verification.go); coverage and correctness stay
// separate refusals with separate remedies.
//
// coverageGate below is the completion checkpoint for one verify
// call, kept here (not in the service) because refusing completion
// is a verification outcome, not generation: it catches up duties
// for evidence attached after the report through the service
// (require only — verification files no new evidence, so it never
// supersedes), then delegates the decision to the read-only
// assessment.

import (
	"fmt"
	"sort"
	"strings"
)

// reviewDutyKinds is the closed set of duties the completion gate
// may block on: review work owed against filed evidence. The gate
// knows only these kinds — criterion judgments live in a
// separate ledger (verdict.go) and can neither satisfy a duty
// nor be satisfied by one. Sorting keeps refusal-adjacent output
// deterministic.
func reviewDutyKinds() []string {
	out := make([]string, 0, len(obligationKinds))
	for k := range obligationKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// gateBlockingIsReviewOnly reports whether every duty in a gate
// decision is a recognized review kind. The gate refuses
// completion while reviews are owed; it never judges verdicts,
// so a blocking set carrying anything else would be the gate
// overstepping its required-reviews role.
func gateBlockingIsReviewOnly(d coverageGateDecision) bool {
	allowed := map[string]bool{}
	for _, k := range reviewDutyKinds() {
		allowed[k] = true
	}
	for _, o := range d.Blocking {
		if !allowed[o.Kind] {
			return false
		}
	}
	return true
}

// coverageGateDecision is the typed blocking result for one verify
// call: the exact revision under review and the duties still owed
// against it. An empty Blocking set means the gate is open.
type coverageGateDecision struct {
	TaskID   string
	Attempt  int
	Digest   string
	Blocking []coverageObligation
}

// Blocked reports whether any duty reviewing the exact revision is
// still owed.
func (d coverageGateDecision) Blocked() bool {
	return len(d.Blocking) > 0
}

// asError renders a blocked decision as the refusal the operator
// sees: it names the blocking duties so they know what to review
// or except. An open gate renders as nil.
func (d coverageGateDecision) asError() error {
	if !d.Blocked() {
		return nil
	}
	kinds := make([]string, 0, len(d.Blocking))
	for _, o := range d.Blocking {
		kinds = append(kinds, fmt.Sprintf("%s(%s)", o.Kind, o.Status))
	}
	return fmt.Errorf("task %s attempt %d has unresolved coverage obligations: %s; satisfy them or except with reason before completing",
		d.TaskID, d.Attempt, strings.Join(kinds, ", "))
}

// assessCoverageGate is the READ-ONLY core of the checkpoint: the
// still-owed duties reviewing exactly digest. It performs no
// generation, no supersede, and no state change of any kind.
func assessCoverageGate(taskID string, attempt int, digest string) (coverageGateDecision, error) {
	blocking, err := blockingTaskObligations(taskID, digest)
	if err != nil {
		return coverageGateDecision{}, err
	}
	return coverageGateDecision{TaskID: taskID, Attempt: attempt, Digest: digest, Blocking: blocking}, nil
}

// blockingTaskObligations returns the task's still-owed duties
// reviewing exactly digest: required, running, and stale rows.
// Scoping to the filed revision is what separates an applicable
// duty from an unrelated one — verifying a task never
// auto-satisfies any duty, but a duty reviewing other evidence
// does not judge this attempt either. Satisfied and excepted rows
// never block; excepted is the operator's explicit waiver.
func blockingTaskObligations(taskID, digest string) ([]coverageObligation, error) {
	t, err := getTask(taskID)
	if err != nil {
		return nil, err
	}
	all, err := listObligations(t.Run, taskID)
	if err != nil {
		return nil, err
	}
	out := []coverageObligation{}
	for _, o := range all {
		if o.TargetRev != digest {
			continue
		}
		switch o.Status {
		case ObligationRequired, ObligationRunning, ObligationStale:
			out = append(out, o)
		}
	}
	return out, nil
}

// coverageGate is the completion checkpoint for one verify call:
// catch up duties for evidence attached after the report (require
// only — verification files no new evidence, so it never
// supersedes), then refuse while any duty reviewing this exact
// filed revision is still owed. The error names the blocking
// duties so the operator knows what to review or except.
func coverageGate(taskID string, attempt int, req OperationRequest) error {
	digest, err := reportDigestFor(taskID, attempt)
	if err != nil {
		return err
	}
	if digest != "" {
		t, terr := getTask(taskID)
		if terr != nil {
			return terr
		}
		pol, perr := pinnedTeamPolicy(t.Run)
		if perr != nil {
			if !strings.Contains(perr.Error(), "has no pinned policy") {
				return perr
			}
		} else if len(pol.Coverage) > 0 {
			if gerr := requireCoverageObligations(t, pol, digest, req); gerr != nil {
				return gerr
			}
		}
	}
	decision, err := assessCoverageGate(taskID, attempt, digest)
	if err != nil {
		return err
	}
	return decision.asError()
}

// obligationSummary names blocking duties for refusal messages:
// kind(status) on task, comma-separated.
func obligationSummary(blocking []coverageObligation) string {
	kinds := make([]string, 0, len(blocking))
	for _, o := range blocking {
		kinds = append(kinds, fmt.Sprintf("%s(%s) on %s", o.Kind, o.Status, o.TaskID))
	}
	return strings.Join(kinds, ", ")
}

// obligationsBlocking returns the run's still-owed coverage:
// required, running, and stale rows block; satisfied and
// excepted rows do not. A verified task never leaves this set
// by itself — only the obligation's own transitions do.
func obligationsBlocking(run string) ([]coverageObligation, error) {
	all, err := listObligations(run, "")
	if err != nil {
		return nil, err
	}
	out := []coverageObligation{}
	for _, o := range all {
		switch o.Status {
		case ObligationRequired, ObligationRunning, ObligationStale:
			out = append(out, o)
		}
	}
	return out, nil
}

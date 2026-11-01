package main

// dispatch_service.go: dispatch coordination (P0-A step 4).
//
// DispatchService coordinates one dispatch as admit -> prepare ->
// commit, with exactly one typed outcome. Admission decides,
// preparation stages files, and the claim transaction alone makes
// the dispatch observable. A failed commit reconciles the prepared
// files, which never gained a ledger binding.

import (
	"fmt"
	"strings"
)

// DispatchOutcome is the one typed result of a dispatch: either the
// task was claimed for the committed attempt, or it was yielded
// back to pending when a concurrent occupant held the root. Any
// other failure returns a nil outcome with the refusal cause.
type DispatchOutcome struct {
	Dispatched    bool
	Yielded       bool
	TaskID        string
	SessionID     string
	ExecutionRoot string
	Attempt       int
	RequestID     string
}

// DispatchService coordinates one dispatch operation across its
// three failure domains: admission (read-only checks), preparation
// (filesystem), and commit (ledger authority).
type DispatchService struct {
	Prepare DeliveryPreparer
	Commit  AttemptCommitter
}

// Dispatch assigns a pending task to a worker session and delivers
// the instruction into the session's workspace. It carries the
// same observable contract as the legacy path: one winner per
// task, one attempt, one delivery chain — plus reconciliation of
// files left without a commit when the claim loses.
func (s DispatchService) Dispatch(id, session string, req OperationRequest) (DispatchOutcome, error) {
	adm, err := AdmitDispatch(id, session, req)
	if err != nil {
		return DispatchOutcome{}, err
	}
	prep, err := s.Prepare.Prepare(adm, req)
	if err != nil {
		return DispatchOutcome{}, err
	}
	harness := adm.Harness
	detail := fmt.Sprintf("actor=%s req=%s session=%s harness=%s root=%s path=%s", req.Actor, req.RequestID, session, harness, prep.ExecutionRoot, prep.InstructionPath)
	if cerr := s.Commit.Commit(adm, prep, req, detail); cerr != nil {
		// The prepare has no ledger binding without the commit:
		// settle the files so a cross-session loser leaves no
		// orphan behind.
		reconcilePreparedDelivery(prep, req)
		return DispatchOutcome{}, cerr
	}
	claimed, err := getTask(id)
	if err != nil {
		return DispatchOutcome{}, err
	}
	// Post-claim re-verification closes the check-then-claim race:
	// a concurrent dispatcher may have occupied the root between
	// the pre-check and the claim. The loser yields deterministically
	// (back to pending, receipted) instead of silently sharing.
	if claimed.SharedWorkspace == 0 {
		if occupants, oerr := liveOccupants(adm.Plan.ExecutionRoot, id); oerr != nil {
			return DispatchOutcome{}, oerr
		} else if len(occupants) > 0 {
			if yerr := transitionTask(id, []string{TaskDispatched}, TaskPending, ""); yerr != nil {
				return DispatchOutcome{}, fmt.Errorf("workspace race on %s: yield failed: %v", adm.Plan.ExecutionRoot, yerr)
			}
			recordReceipt(id, "dispatch_yielded", fmt.Sprintf("actor=%s req=%s root=%s occupants=%s", req.Actor, req.RequestID, adm.Plan.ExecutionRoot, strings.Join(occupants, ", ")))
			return DispatchOutcome{TaskID: id, SessionID: session, ExecutionRoot: adm.Plan.ExecutionRoot, Attempt: adm.Plan.Attempt, RequestID: req.RequestID, Yielded: true},
				fmt.Errorf("workspace %s occupied by live task(s) %s; %s yielded (TD-051)", adm.Plan.ExecutionRoot, strings.Join(occupants, ", "), id)
		}
	}
	recordReceipt(id, "submitted", detail)
	return DispatchOutcome{
		Dispatched:    true,
		TaskID:        id,
		SessionID:     session,
		ExecutionRoot: prep.ExecutionRoot,
		Attempt:       adm.Plan.Attempt,
		RequestID:     req.RequestID,
	}, nil
}

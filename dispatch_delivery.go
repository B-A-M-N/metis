package main

// dispatch_delivery.go: filesystem delivery preparation (P0-A step 3).
//
// DeliveryPreparer is the filesystem failure domain, separate from
// the ledger failure domain owned by AttemptCommitter. Prepare
// writes the instruction file and the attempt sidecar and records
// the prepared identity; it never touches the ledger. When the
// commit that follows fails, the prepared files have no ledger
// binding — reconcilePreparedDelivery removes them when they are
// provably orphaned, so reconciliation (not the worker) settles
// files-without-commit.

import (
	"fmt"
	"os"
	"path/filepath"
)

// PreparedDelivery is the recorded identity of one filesystem
// prepare: which attempt's bytes live at which paths. The ledger
// stays authoritative; a sidecar that disagrees with the ledger is
// refused at pickup, and a missing sidecar defers to the ledger.
type PreparedDelivery struct {
	TaskID          string
	SessionID       string
	Attempt         int
	ExecutionRoot   string
	InstructionPath string
	SidecarPath     string
}

// DeliveryPreparer writes the worker-visible files for one admitted
// dispatch. Filesystem prepare happens BEFORE the commit: the
// instruction file and the attempt sidecar exist before the ledger
// names this attempt. Content derives solely from the admitted task
// row, so concurrent dispatch racers write identical bytes and only
// the transition winner proceeds.
type DeliveryPreparer struct{}

// Prepare delivers the instruction into the session workspace and
// records the attempt sidecar beside it, in that order. Failures
// are receipted and leave no ledger trace: a refused prepare never
// becomes a phantom dispatch.
func (DeliveryPreparer) Prepare(adm DispatchAdmission, req OperationRequest) (PreparedDelivery, error) {
	pending := adm.Task
	pending.Agent = adm.Plan.SessionID
	path, derr := deliverTask(pending)
	if derr != nil {
		recordReceipt(pending.ID, "delivery_failed", fmt.Sprintf("actor=%s req=%s %v", req.Actor, req.RequestID, derr))
		return PreparedDelivery{}, derr
	}
	nextAttempt := adm.Plan.Attempt
	prepared := pending
	prepared.Attempt = nextAttempt
	if prepared.ExecutionRoot == "" {
		prepared.ExecutionRoot = adm.Plan.ExecutionRoot
	}
	if serr := writeAttemptSidecar(adm.Plan.ExecutionRoot, prepared); serr != nil {
		recordReceipt(pending.ID, "sidecar_failed", fmt.Sprintf("actor=%s req=%s %v", req.Actor, req.RequestID, serr))
		return PreparedDelivery{}, serr
	}
	return PreparedDelivery{
		TaskID:          pending.ID,
		SessionID:       adm.Plan.SessionID,
		Attempt:         nextAttempt,
		ExecutionRoot:   prepared.ExecutionRoot,
		InstructionPath: path,
		SidecarPath:     filepath.Join(adm.Plan.ExecutionRoot, "task-"+pending.ID+".attempt.json"),
	}, nil
}

// reconcilePreparedDelivery settles files left without a commit:
// the prepare succeeded but the claim that would have bound them
// lost or failed. Files are removed only when provably orphaned —
// the ledger moved past our attempt AND binds the task to a
// different root, so no worker will ever execute from our paths. In
// every other state the files stay: same-root racers write
// identical bytes the winner owns, and a still-pending task may
// have a retry in flight whose prepare must not be pulled away.
// Removal is best-effort and receipted; the ledger stays
// authoritative either way.
func reconcilePreparedDelivery(prep PreparedDelivery, req OperationRequest) {
	cur, err := getTask(prep.TaskID)
	if err != nil {
		return
	}
	if cur.ExecutionRoot == "" || cur.ExecutionRoot == prep.ExecutionRoot {
		return
	}
	if cur.Attempt < prep.Attempt {
		return
	}
	for _, path := range []string{prep.InstructionPath, prep.SidecarPath} {
		if path == "" {
			continue
		}
		if rmerr := os.Remove(path); rmerr != nil && !os.IsNotExist(rmerr) {
			return
		}
	}
	recordReceipt(prep.TaskID, "delivery_reconciled", fmt.Sprintf("actor=%s req=%s attempt=%d root=%s", req.Actor, req.RequestID, prep.Attempt, prep.ExecutionRoot))
}

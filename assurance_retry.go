package main

// assurance_retry.go: retry policy and replacement dispatch.
//
// A retry reconciles the uncertain prior attempt first
// (assurance_reconcile.go), then delivers again as a new attempt.
// It never silently treats the original execution as legitimate
// (CB-12).
//
// Policy gates, in order: the dispatch-attempt budget (TD-035,
// enforced by reconciliation), the bound workspace root (TD-050 —
// a replacement in a different workspace is refused before any
// reconciliation), and the filesystem binding for the NEW attempt
// (the pickup gate cross-checks the sidecar against the ledger, so
// delivering with the pre-reconcile row would leave a stale sidecar
// that refuses the replacement forever; a crash here stays
// fail-safe, never phantom).

import (
	"fmt"
)

// checkRetryRoot enforces workspace continuity (TD-050): a
// replacement attempt in a different workspace is refused before
// any reconciliation runs.
func checkRetryRoot(t Task, session string) (string, error) {
	root, err := sessionRoot(session)
	if err != nil {
		return "", err
	}
	if t.ExecutionRoot != "" && t.ExecutionRoot != root {
		return "", fmt.Errorf("task %s bound to %s; retry into %s refused (TD-050)", t.ID, t.ExecutionRoot, root)
	}
	return root, nil
}

// retryDispatch reconciles an uncertain prior attempt, then delivers
// again as a new attempt. It never silently treats the original
// execution as legitimate (CB-12).
func retryDispatch(id, session string, req OperationRequest) error {
	if err := requireRequest(req); err != nil {
		return err
	}
	if err := sessionBindable(session); err != nil {
		return err
	}
	pending, err := getTask(id)
	if err != nil {
		return err
	}
	// Retries keep the bound root (TD-050): a replacement attempt in
	// a different workspace is refused before any reconciliation.
	root, rerr := checkRetryRoot(pending, session)
	if rerr != nil {
		return rerr
	}
	pending.Agent = session
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := reconcileForRetry(db, id, req); err != nil {
		return err
	}
	fresh, err := getTask(id)
	if err != nil {
		return err
	}
	fresh.Agent = session
	// Filesystem binding for the NEW attempt: the pickup gate
	// cross-checks the sidecar against the ledger, so delivering
	// with the pre-reconcile row would leave a stale sidecar that
	// refuses the replacement forever. A missing sidecar defers to
	// the ledger, so a crash here stays fail-safe (never phantom).
	if serr := writeAttemptSidecar(root, fresh); serr != nil {
		recordReceipt(id, "sidecar_failed", fmt.Sprintf("actor=%s req=%s %v", req.Actor, req.RequestID, serr))
		return serr
	}
	path, derr := deliverTask(fresh)
	harness, _ := sessionHarness(session)
	detail := fmt.Sprintf("actor=%s req=%s session=%s harness=%s path=%s", req.Actor, req.RequestID, session, harness, path)
	// The replacement attempt is the current latest after reconcile.
	cur, err := latestAttempt(db, id)
	if err != nil {
		return err
	}
	if derr != nil {
		_, _ = insertDelivery(db, id, cur.ID, DeliveryFailed, req, detail+fmt.Sprintf(" err=%v", derr))
		if err := setAttemptState(db, cur.ID, AttemptUncertain); err != nil {
			return err
		}
		recordReceipt(id, "delivery_failed", fmt.Sprintf("actor=%s req=%s %v", req.Actor, req.RequestID, derr))
		return derr
	}
	if _, err := insertDelivery(db, id, cur.ID, DeliveryQueued, req, detail); err != nil {
		return err
	}
	if _, err := insertDelivery(db, id, cur.ID, DeliverySubmitted, req, detail); err != nil {
		return err
	}
	recordReceipt(id, "submitted", detail)
	return nil
}

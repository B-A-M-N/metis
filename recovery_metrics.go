package main

// recovery_metrics.go: P3 unknown-state age + recovery-duration
// metrics (Metis audit remediation P3).
//
// Unknown-state age tracks how long a task/attempt identity has
// been in a needs-reconciliation (uncertain/unknown) state without
// a recorded resolution. Recovery duration measures the
// uncertain-to-resolved span per identity once it resolves.
//
// What this is NOT:
//   - Not a second source of truth. Metrics OBSERVE the durable
//     ledger transitions via additive hooks in reconcile.go and
//     assurance_reconcile.go; they never gate control flow and
//     nothing branches on them. Hooks record timestamps only —
//     they never alter reconcile decisions or retry behavior.
//   - No additional persistence. Entries live in a bounded
//     in-memory map (mutex-guarded, oldest overwritten). No per-event
//     DB writes happen outside the receipts the underlying
//     operations already file.
//   - No wall-clock reads inside the projection. projectRecoveryMetrics
//     is a pure function of its inputs (observations + now); only
//     the thin observe/record wrappers read the clock.
//
// Identity is per task+attempt: a retry supersede-and-bump resolves
// the prior attempt's identity and any fresh uncertainty starts a
// new identity under the new attempt number.

import (
	"fmt"
	"sync"
	"time"
)

// recoveryIdentity names one measured attempt: a task at one
// attempt number. A replacement attempt is a new identity.
type recoveryIdentity struct {
	TaskID  string
	Attempt int
}

func (id recoveryIdentity) key() string {
	return fmt.Sprintf("%s#%d", id.TaskID, id.Attempt)
}

// recoveryEntry is the mutable record for one identity: when it
// entered the uncertain state and, once resolved, when it left.
// The identity rides along so snapshots never parse map keys.
type recoveryEntry struct {
	TaskID         string
	Attempt        int
	UncertainSince time.Time
	ResolvedAt     time.Time
	Resolved       bool
}

// recoveryRingCap bounds the in-memory map. When full, the oldest
// uncertain-since entry is evicted; memory use stays constant.
const recoveryRingCap = 256

var (
	recoveryMu      sync.Mutex
	recoveryEntries = map[string]recoveryEntry{}
	recoveryOrder   []string
)

// resetRecoveryMetrics clears all entries. Tests only.
func resetRecoveryMetrics() {
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	recoveryEntries = map[string]recoveryEntry{}
	recoveryOrder = nil
}

// recordUncertain stamps when an identity entered the uncertain
// state. First write wins: repeated WAIT/INVESTIGATE probes must
// not reset the age. A resolved identity keeps its resolution —
// re-uncertainty after resolution is a new episode only when the
// attempt number advances (a new identity).
func recordUncertain(taskID string, attempt int, at time.Time) {
	id := recoveryIdentity{TaskID: taskID, Attempt: attempt}.key()
	at = at.UTC()
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	if _, ok := recoveryEntries[id]; ok {
		return
	}
	if len(recoveryOrder) >= recoveryRingCap {
		oldest := ""
		var oldestAt time.Time
		for k, e := range recoveryEntries {
			if oldest == "" || e.UncertainSince.Before(oldestAt) {
				oldest, oldestAt = k, e.UncertainSince
			}
		}
		delete(recoveryEntries, oldest)
		for i, k := range recoveryOrder {
			if k == oldest {
				recoveryOrder = append(recoveryOrder[:i], recoveryOrder[i+1:]...)
				break
			}
		}
	}
	recoveryEntries[id] = recoveryEntry{TaskID: taskID, Attempt: attempt, UncertainSince: at}
	recoveryOrder = append(recoveryOrder, id)
}

// recordResolved stamps when an uncertain identity resolved. It is
// a no-op for identities never observed uncertain (nothing to
// measure) and keeps the first resolution (supersede-and-bump is
// the resolution, not a later DONE re-probe).
func recordResolved(taskID string, attempt int, at time.Time) {
	id := recoveryIdentity{TaskID: taskID, Attempt: attempt}.key()
	at = at.UTC()
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	e, ok := recoveryEntries[id]
	if !ok || e.Resolved {
		return
	}
	if at.Before(e.UncertainSince) {
		at = e.UncertainSince
	}
	e.ResolvedAt = at
	e.Resolved = true
	recoveryEntries[id] = e
}

// unknownStateAge reports how long an identity has been uncertain
// without resolution. ok is false when the identity was never
// observed uncertain or has since resolved.
func unknownStateAge(taskID string, attempt int, now time.Time) (age time.Duration, ok bool) {
	id := recoveryIdentity{TaskID: taskID, Attempt: attempt}.key()
	now = now.UTC()
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	e, found := recoveryEntries[id]
	if !found || e.Resolved {
		return 0, false
	}
	if now.Before(e.UncertainSince) {
		return 0, true
	}
	return now.Sub(e.UncertainSince), true
}

// recoveryDuration reports the uncertain-to-resolved span for an
// identity. ok is false when the identity never resolved.
func recoveryDuration(taskID string, attempt int) (d time.Duration, ok bool) {
	id := recoveryIdentity{TaskID: taskID, Attempt: attempt}.key()
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	e, found := recoveryEntries[id]
	if !found || !e.Resolved {
		return 0, false
	}
	return e.ResolvedAt.Sub(e.UncertainSince), true
}

// RecoveryObservation is one identity's measured timestamps: the
// pure-projection input. ResolvedAt is nil while still uncertain.
type RecoveryObservation struct {
	TaskID         string
	Attempt        int
	UncertainSince time.Time
	ResolvedAt     *time.Time
}

// RecoveryMetricView is the pure-projection output for one
// identity: its current state plus either a live age (uncertain)
// or a final duration (resolved).
type RecoveryMetricView struct {
	TaskID    string
	Attempt   int
	State     string // "uncertain" or "resolved"
	Age       time.Duration
	Duration  time.Duration
	HasAge    bool
	HasResult bool
}

// projectRecoveryMetrics renders observations as metric views at
// one instant. Pure: no globals, no clock reads, no I/O — the same
// inputs and now always yield the same views.
func projectRecoveryMetrics(obs []RecoveryObservation, now time.Time) []RecoveryMetricView {
	now = now.UTC()
	out := make([]RecoveryMetricView, 0, len(obs))
	for _, o := range obs {
		v := RecoveryMetricView{TaskID: o.TaskID, Attempt: o.Attempt}
		if o.ResolvedAt != nil {
			since := o.UncertainSince.UTC()
			at := o.ResolvedAt.UTC()
			if at.Before(since) {
				at = since
			}
			v.State = "resolved"
			v.Duration = at.Sub(since)
			v.HasResult = true
		} else {
			v.State = "uncertain"
			since := o.UncertainSince.UTC()
			if !now.Before(since) {
				v.Age = now.Sub(since)
			}
			v.HasAge = true
		}
		out = append(out, v)
	}
	return out
}

// recoverySnapshot observes current entries and renders them via
// the pure projection. now is explicit so tests stay deterministic.
func recoverySnapshot(now time.Time) []RecoveryMetricView {
	recoveryMu.Lock()
	obs := make([]RecoveryObservation, 0, len(recoveryEntries))
	for _, e := range recoveryEntries {
		obs = append(obs, RecoveryObservation{
			TaskID:         e.TaskID,
			Attempt:        e.Attempt,
			UncertainSince: e.UncertainSince,
			ResolvedAt:     resolvedPtr(e),
		})
	}
	recoveryMu.Unlock()
	return projectRecoveryMetrics(obs, now)
}

func resolvedPtr(e recoveryEntry) *time.Time {
	if !e.Resolved {
		return nil
	}
	t := e.ResolvedAt
	return &t
}

// isUncertainReconciliation reports whether a reconciliation result
// leaves the orchestrator without a usable outcome: an uncertain
// or unknown attempt/delivery state, or a WAIT/INVESTIGATE
// recommendation. Terminal DONE and verdict-gating recommendations
// count as resolved, never uncertain.
func isUncertainReconciliation(r TaskReconciliation) bool {
	switch r.AttemptState {
	case AttemptUncertain, AttemptUnknown:
		return true
	}
	switch r.DeliveryState {
	case DeliveryUncertain, DeliveryUnknown:
		return true
	}
	switch r.Recommendation {
	case "WAIT", "INVESTIGATE":
		return true
	}
	return false
}

// isResolvedReconciliation reports whether a reconciliation result
// hands the orchestrator a usable outcome: verdicts to file or
// review, or a terminal DONE.
func isResolvedReconciliation(r TaskReconciliation) bool {
	switch r.Recommendation {
	case "FILE_VERDICTS", "REVIEW_VERDICTS", "DONE":
		return true
	}
	return false
}

// observeReconciliation is the additive reconcile.go hook: it
// records timestamps only and never alters the recommendation,
// the reason, or any error. Uncertain results open (or keep) the
// identity's age; resolved results close it.
func observeReconciliation(taskID string, attempt int, r TaskReconciliation) {
	now := time.Now().UTC()
	switch {
	case isResolvedReconciliation(r):
		recordResolved(taskID, attempt, now)
	case isUncertainReconciliation(r):
		recordUncertain(taskID, attempt, now)
	}
}

// observeRetryReconciled is the additive assurance_reconcile.go
// success hook: a supersede-and-bump resolves the prior attempt's
// identity via replacement. Timestamps only; retry behavior is
// untouched.
func observeRetryReconciled(taskID string, prevAttempt int) {
	recordResolved(taskID, prevAttempt, time.Now().UTC())
}

// observeRetryRefused is the additive assurance_reconcile.go
// refusal hook: a live-attempt refusal keeps the prior attempt in
// the uncertain state, so its age keeps accruing. Timestamps only.
func observeRetryRefused(taskID string, prevAttempt int) {
	recordUncertain(taskID, prevAttempt, time.Now().UTC())
}

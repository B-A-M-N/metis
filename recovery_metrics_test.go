package main

// P3 unknown-state-age + recovery-duration metrics (failing-first:
// these fail without recovery_metrics.go and its reconcile hooks).

import (
	"testing"
	"time"
)

// An uncertain identity ages visibly: the reported age grows with
// the gap between entry and observation, and repeated uncertain
// probes do not reset it.
func TestRecoveryUncertainAgesVisibly(t *testing.T) {
	resetRecoveryMetrics()
	base := time.Now().UTC()
	recordUncertain("task-age", 1, base.Add(-2*time.Hour))
	// A second probe is additive noise, never a reset.
	recordUncertain("task-age", 1, base)
	age, ok := unknownStateAge("task-age", 1, base)
	if !ok {
		t.Fatal("uncertain identity must report an age")
	}
	if age < 2*time.Hour {
		t.Fatalf("uncertain age must span the full 2h since entry, got %v", age)
	}
	// A different attempt is a different identity: no age.
	if _, ok := unknownStateAge("task-age", 2, base); ok {
		t.Fatal("unobserved attempt identity must report no age")
	}
}

// A resolved identity reports its uncertain-to-resolved duration
// and stops aging.
func TestRecoveryResolvedReportsDuration(t *testing.T) {
	resetRecoveryMetrics()
	base := time.Now().UTC()
	recordUncertain("task-dur", 3, base)
	recordResolved("task-dur", 3, base.Add(90*time.Minute))
	d, ok := recoveryDuration("task-dur", 3)
	if !ok {
		t.Fatal("resolved identity must report a recovery duration")
	}
	if d != 90*time.Minute {
		t.Fatalf("recovery duration must be the 90m uncertain span, got %v", d)
	}
	if _, ok := unknownStateAge("task-dur", 3, base.Add(3*time.Hour)); ok {
		t.Fatal("resolved identity must stop aging")
	}
	// Resolving an identity never observed uncertain measures nothing.
	if _, ok := recoveryDuration("task-never", 1); ok {
		t.Fatal("never-uncertain identity must report no duration")
	}
}

// The projection is pure: same inputs and instant, same views —
// one uncertain identity with a live age, one resolved identity
// with a final duration.
func TestRecoveryProjectionIsPure(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-45 * time.Minute)
	resolvedAt := since.Add(30 * time.Minute)
	obs := []RecoveryObservation{
		{TaskID: "t-uncertain", Attempt: 1, UncertainSince: since},
		{TaskID: "t-resolved", Attempt: 2, UncertainSince: since, ResolvedAt: &resolvedAt},
	}
	for i := 0; i < 2; i++ {
		views := projectRecoveryMetrics(obs, now)
		if len(views) != 2 {
			t.Fatalf("projection must render every observation, got %d", len(views))
		}
		byID := map[string]RecoveryMetricView{}
		for _, v := range views {
			byID[v.TaskID] = v
		}
		u := byID["t-uncertain"]
		if u.State != "uncertain" || !u.HasAge || u.HasResult {
			t.Fatalf("uncertain view must carry a live age only: %+v", u)
		}
		if u.Age < 45*time.Minute {
			t.Fatalf("uncertain age must be visibly 45m, got %v", u.Age)
		}
		r := byID["t-resolved"]
		if r.State != "resolved" || !r.HasResult || r.HasAge {
			t.Fatalf("resolved view must carry a final duration only: %+v", r)
		}
		if r.Duration != 30*time.Minute {
			t.Fatalf("resolved duration must be the 30m span, got %v", r.Duration)
		}
	}
}

// End to end through the additive hooks only: an open execution
// reconciles WAIT (uncertain, hook records the entry) and the
// terminal reconcile closes it (DONE, hook records the duration).
// No reconcile decision or retry behavior may change.
func TestReconcileHookTracksUncertainToResolved(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	resetRecoveryMetrics()
	installFakeTmux(t, "rcm")
	seedOwnedSession(t, "w", "rcm")
	tk, err := createTask("run-rcm", "w", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("worker-loop/w")); err != nil {
		t.Fatal(err)
	}
	if err := recordExecutionStart(tk.ID, got.Attempt, "w", "test-fake", `["sh"]`, 4242, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	r := reconcileOrFail(t, tk.ID)
	if r.Recommendation != "WAIT" {
		t.Fatalf("open execution must stay WAIT, got %q", r.Recommendation)
	}
	age, ok := unknownStateAge(tk.ID, got.Attempt, time.Now().UTC())
	if !ok {
		t.Fatal("WAIT reconcile must open an unknown-state age via the hook")
	}
	if age < 0 {
		t.Fatalf("age must be non-negative, got %v", age)
	}
	if err := recordExecutionResult(tk.ID, got.Attempt, 0, "sha256:done"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceWorkerClaim, Reporter: "w"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	r = reconcileOrFail(t, tk.ID)
	if r.Recommendation != "DONE" {
		t.Fatalf("terminal task must reconcile DONE, got %q", r.Recommendation)
	}
	d, ok := recoveryDuration(tk.ID, got.Attempt)
	if !ok {
		t.Fatal("DONE reconcile must close the recovery duration via the hook")
	}
	if d < 0 {
		t.Fatalf("duration must be non-negative, got %v", d)
	}
	if _, ok := unknownStateAge(tk.ID, got.Attempt, time.Now().UTC()); ok {
		t.Fatal("resolved identity must stop aging")
	}
}

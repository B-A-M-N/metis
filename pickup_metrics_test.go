package main

// pickup_metrics_test.go: P3 pickup-latency + queue-depth metrics
// (Metis audit remediation P3), failing-first.
//
// A dispatched task must carry a durable dispatched_at timestamp;
// once a worker claims it, the (dispatched_at, claimed_at) pair
// must project the correct pickup latency. A dispatched-but-never-
// claimed task must keep the queue-depth gauge at 1 and its queue
// age must grow with time (pure projection, deterministic).

import (
	"testing"
	"time"
)

// Dispatch -> claim must expose the dispatch timestamp, the claim
// timestamp, and the correct latency between them.
func TestPickupDispatchedThenClaimedExposesLatency(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "p3lat")
	seedOwnedSession(t, "w1", "p3lat")
	tk, err := createTask("run-p3-lat", "agent", "latency objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if err := dispatchTaskWithRequest(tk.ID, "w1", testReq("orch")); err != nil {
		t.Fatalf("dispatch must commit: %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched {
		t.Fatalf("expected dispatched, got %s", got.Status)
	}
	// The dispatch hook alone must have durably recorded dispatched_at.
	obs, err := loadPickupObservation(tk.ID, got.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if !obs.HasDispatched {
		t.Fatal("dispatch hook must record dispatched_at without altering dispatch")
	}
	if obs.DispatchedAt.Before(before.Add(-time.Minute)) || obs.DispatchedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Fatalf("dispatched_at %v is not near dispatch time %v", obs.DispatchedAt, before)
	}
	// Claim through the production path, then observe the claim at
	// the claim-path seam (the hook the claim owner wires).
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("loop")); err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	claimedAt := time.Now().UTC()
	if err := observePickupClaim(tk.ID, got.Attempt, claimedAt); err != nil {
		t.Fatal(err)
	}
	proj, err := summarizePickup(tk.ID, got.Attempt, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !proj.Claimed {
		t.Fatal("claimed task must project Claimed")
	}
	want := claimedAt.Sub(obs.DispatchedAt)
	if proj.Latency != want {
		t.Fatalf("latency = %v, want %v", proj.Latency, want)
	}
	if proj.Latency < 0 || proj.Latency > 10*time.Minute {
		t.Fatalf("latency %v is implausible for an immediate claim", proj.Latency)
	}
}

// A dispatched-but-unclaimed task must count in the queue-depth
// gauge, while a claimed task must not.
func TestPickupUnclaimedTaskAgesQueueGauge(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "p3q")
	seedOwnedSession(t, "w1", "p3q")
	seedOwnedSession(t, "w2", "p3q")
	claimed, err := createTask("run-p3-q", "agent", "claimed objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	queued, err := createTask("run-p3-q", "agent", "queued objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(claimed.ID, "w1", testReq("orch")); err != nil {
		t.Fatalf("dispatch claimed task: %v", err)
	}
	got, err := getTask(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := markTaskWorking(claimed.ID, got.Attempt, testReq("loop")); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := observePickupClaim(claimed.ID, got.Attempt, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(queued.ID, "w2", testReq("orch")); err != nil {
		t.Fatalf("dispatch queued task: %v", err)
	}
	gotQ, err := getTask(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	depth, err := pickupQueueDepth()
	if err != nil {
		t.Fatal(err)
	}
	if depth != 1 {
		t.Fatalf("queue depth = %d, want 1 (only the unclaimed task)", depth)
	}
	proj, err := summarizePickup(queued.ID, gotQ.Attempt, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if proj.Claimed {
		t.Fatal("unclaimed task must not project Claimed")
	}
	if proj.QueueAge < 0 || proj.QueueAge > 10*time.Minute {
		t.Fatalf("queue age %v is implausible for a just-dispatched task", proj.QueueAge)
	}
}

// The latency/age projection must be pure: fixed inputs project
// fixed outputs, unclaimed observations age with `now`, and clock
// skew (claim before dispatch) clamps to zero instead of going
// negative.
func TestPickupProjectionIsPure(t *testing.T) {
	dispatched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	claimedAt := dispatched.Add(90 * time.Second)
	obs := pickupObservation{
		TaskID: "t", Attempt: 1,
		DispatchedAt: dispatched, HasDispatched: true,
		ClaimedAt: claimedAt, HasClaimed: true,
	}
	p1 := projectPickup(obs, claimedAt)
	p2 := projectPickup(obs, claimedAt.Add(time.Hour))
	if p1.Latency != 90*time.Second || p2.Latency != 90*time.Second {
		t.Fatalf("claimed latency must be fixed at 90s, got %v and %v", p1.Latency, p2.Latency)
	}
	pending := pickupObservation{
		TaskID: "q", Attempt: 1,
		DispatchedAt: dispatched, HasDispatched: true,
	}
	early := projectPickup(pending, dispatched.Add(time.Minute))
	late := projectPickup(pending, dispatched.Add(11*time.Minute))
	if early.QueueAge != time.Minute || late.QueueAge != 11*time.Minute {
		t.Fatalf("unclaimed queue age must grow with now, got %v and %v", early.QueueAge, late.QueueAge)
	}
	if early.Claimed || early.Latency != 0 {
		t.Fatalf("unclaimed projection must not claim latency, got %+v", early)
	}
	skewed := pickupObservation{
		TaskID: "s", Attempt: 1,
		DispatchedAt: dispatched, HasDispatched: true,
		ClaimedAt: dispatched.Add(-time.Second), HasClaimed: true,
	}
	if got := projectPickup(skewed, dispatched); got.Latency != 0 {
		t.Fatalf("negative skew must clamp to 0, got %v", got.Latency)
	}
	empty := projectPickup(pickupObservation{}, dispatched)
	if empty.Claimed || empty.Latency != 0 || empty.QueueAge != 0 {
		t.Fatalf("empty observation must project zero, got %+v", empty)
	}
}

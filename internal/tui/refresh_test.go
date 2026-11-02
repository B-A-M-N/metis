package tui

import "testing"

// Pure coordinator tests: no tmux, registry, DB, or event loop.
// The shell adapter (main package) preserves the F-18 single-flight
// behavior through these transitions; TestRefreshSkipsWhileOneIsInFlight
// pins the integrated path.

func TestRefreshTickSingleFlight(t *testing.T) {
	var r Refresh
	var out TickOutcome
	// Tick 1 (odd, idle): dispatch gen 1 with the git probe.
	r, out = r.Tick()
	if !out.Dispatch || out.Gen != 1 || !out.EveryGit {
		t.Fatalf("tick 1 must dispatch gen 1 with git, got %+v", out)
	}
	// Tick 2 (even): reschedule only.
	r, out = r.Tick()
	if out.Dispatch {
		t.Fatalf("tick 2 must not dispatch, got %+v", out)
	}
	// Tick 3 (odd, outstanding): coalesce, no second round.
	r, out = r.Tick()
	if out.Dispatch {
		t.Fatalf("tick 3 must coalesce onto the in-flight round, got %+v", out)
	}
	// Delivery of the outstanding round re-arms; tick 4 is even
	// so tick 5 dispatches gen 2 without the git probe (5%4 != 1).
	r = r.OnDelivered(1)
	r, out = r.Tick()
	if out.Dispatch {
		t.Fatalf("tick 4 must not dispatch, got %+v", out)
	}
	r, out = r.Tick()
	if !out.Dispatch || out.Gen != 2 || !out.EveryGit {
		t.Fatalf("tick 5 must dispatch gen 2 with git, got %+v", out)
	}
	// Tick 7 dispatches gen 3 without the git probe (7%4 != 1).
	r = r.OnDelivered(2)
	r, _ = r.Tick()
	r, out = r.Tick()
	if !out.Dispatch || out.Gen != 3 || out.EveryGit {
		t.Fatalf("tick 7 must dispatch gen 3 without git, got %+v", out)
	}
}

func TestRefreshReselectCoalescesBehindPending(t *testing.T) {
	var r Refresh
	r, out := r.Tick()
	if !out.Dispatch {
		t.Fatal("tick 1 must dispatch")
	}
	// A selection change invalidates the old projection without
	// launching a second acquisition.
	r, gen := r.Reselect()
	if gen != 2 {
		t.Fatalf("reselect must bump to gen 2, got %d", gen)
	}
	if !r.Pending {
		t.Fatal("reselect must not clear in-flight accounting")
	}
	// The original command still owns the permit until it completes.
	r = r.OnDelivered(1)
	if r.Pending {
		t.Fatal("originating delivery must release its exact permit")
	}
	r, out = r.DispatchLatest()
	if !out.Dispatch || out.Gen != gen {
		t.Fatalf("latest requested generation must acquire after completion: %+v", out)
	}
}

func TestRefreshStartupBumpsWithoutPending(t *testing.T) {
	var r Refresh
	r, gen := r.Startup()
	if gen != 1 {
		t.Fatalf("startup must bump to gen 1, got %d", gen)
	}
	if r.Pending {
		t.Fatal("synchronous startup paint must not mark in-flight")
	}
	r, out := r.Tick()
	if !out.Dispatch || out.Gen != 2 {
		t.Fatalf("first tick after startup must dispatch gen 2, got %+v", out)
	}
}

func TestRefreshDeliveryCorrelatesExactly(t *testing.T) {
	var r Refresh
	// Delivery with nothing outstanding is a no-op, never an error
	// path: the shell reports every refreshMsg with its gen.
	r = r.OnDelivered(7)
	if r.Pending || r.Gen != 0 || r.Ticks != 0 {
		t.Fatalf("spurious delivery must change nothing, got %+v", r)
	}
	r, out := r.Tick() // gen 1 dispatched, outstanding
	if !out.Dispatch {
		t.Fatal("tick 1 must dispatch")
	}
	// A foreign gen (never dispatched) must not re-arm dispatch.
	r = r.OnDelivered(999)
	if !r.Pending {
		t.Fatal("foreign delivery must not clear in-flight accounting")
	}
	// The outstanding round's delivery re-arms.
	r = r.OnDelivered(1)
	if r.Pending {
		t.Fatal("current delivery must clear in-flight accounting")
	}
	// A duplicate of that delivery is a no-op, never a second arm.
	r = r.OnDelivered(1)
	if r.Pending || r.Gen != 1 {
		t.Fatalf("duplicate delivery must change nothing, got %+v", r)
	}
}

func TestRefreshStaleCompletionCannotReleaseNewPermit(t *testing.T) {
	var r Refresh
	r, first := r.Tick()
	r, latestGen := r.Reselect()
	if !first.Dispatch || latestGen == first.Gen {
		t.Fatal("reselection must invalidate the active projection")
	}
	r = r.OnDelivered(first.Gen)
	r, next := r.DispatchLatest()
	if !next.Dispatch || next.Gen != latestGen {
		t.Fatalf("latest request was not dispatched after the first completed: %+v", next)
	}
	r = r.OnDelivered(first.Gen) // duplicate late completion
	if !r.Pending || r.outstanding != latestGen {
		t.Fatalf("stale completion released the newer permit: %+v", r)
	}
}

// Rapid navigation fires a storm of superseding rounds while an
// older collection is still running. Only the latest round's
// delivery may re-arm dispatch; stale and superseded deliveries
// must leave accounting untouched so they can never resurrect an
// abandoned round or starve the current one.
func TestRefreshReselectStormKeepsPendingUntilLatest(t *testing.T) {
	var r Refresh
	r, out := r.Tick() // gen 1 dispatched, outstanding
	if !out.Dispatch {
		t.Fatal("tick 1 must dispatch")
	}
	r, g2 := r.Reselect()
	r, g3 := r.Reselect()
	if g2 != 2 || g3 != 3 {
		t.Fatalf("reselects must bump to gens 2,3, got %d,%d", g2, g3)
	}
	// Ticks during the storm still single-flight: no stacking.
	r, out = r.Tick()
	if out.Dispatch {
		t.Fatalf("tick during reselect storm must coalesce, got %+v", out)
	}
	r = r.OnDelivered(1)
	if r.Pending {
		t.Fatal("the only active collector's completion must release capacity")
	}
	r, out = r.DispatchLatest()
	if !out.Dispatch || out.Gen != g3 {
		t.Fatalf("only latest selection should be dispatched: %+v", out)
	}
}

// A reselect with nothing outstanding still dispatches a command,
// so it must mark the round outstanding: the next odd tick has to
// coalesce onto it instead of dispatching a second concurrent
// collection.
func TestRefreshIdleReselectMarksOutstanding(t *testing.T) {
	var r Refresh
	r, gen := r.Reselect()
	if gen != 1 {
		t.Fatalf("reselect from zero must dispatch gen 1, got %d", gen)
	}
	r, out := r.Tick() // tick 1, odd
	if out.Dispatch {
		t.Fatalf("tick after idle reselect must coalesce, got %+v", out)
	}
	r = r.OnDelivered(gen)
	if r.Pending {
		t.Fatal("current delivery must clear in-flight accounting")
	}
}

// A timeout or long wait cannot release a permit while the source is
// still executing. Recovery requires the originating completion.
func TestRefreshNeverAbandonsExecutingAcquisition(t *testing.T) {
	var r Refresh
	r, out := r.Tick() // gen 1 dispatched, outstanding
	if !out.Dispatch {
		t.Fatal("tick 1 must dispatch")
	}
	for i := 0; i < 100; i++ {
		r, out = r.Tick()
		if out.Dispatch {
			t.Fatalf("tick %d must retain the permit, got %+v", i+2, out)
		}
	}
	if !r.Pending {
		t.Fatal("hung round must still be outstanding at the deadline")
	}
	r = r.OnDelivered(1)
	if r.Pending {
		t.Fatal("originating completion must release permit")
	}
}

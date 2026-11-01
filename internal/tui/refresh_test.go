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
	// Delivery re-arms; tick 4 is even so tick 5 dispatches gen 2
	// without the git probe (5%4 != 1).
	r = r.OnDelivered()
	r, out = r.Tick()
	if out.Dispatch {
		t.Fatalf("tick 4 must not dispatch, got %+v", out)
	}
	r, out = r.Tick()
	if !out.Dispatch || out.Gen != 2 || !out.EveryGit {
		t.Fatalf("tick 5 must dispatch gen 2 with git, got %+v", out)
	}
	// Tick 7 dispatches gen 3 without the git probe (7%4 != 1).
	r = r.OnDelivered()
	r, _ = r.Tick()
	r, out = r.Tick()
	if !out.Dispatch || out.Gen != 3 || out.EveryGit {
		t.Fatalf("tick 7 must dispatch gen 3 without git, got %+v", out)
	}
}

func TestRefreshReselectBypassesPending(t *testing.T) {
	var r Refresh
	r, out := r.Tick()
	if !out.Dispatch {
		t.Fatal("tick 1 must dispatch")
	}
	// A selection change prioritizes fresh data even while a round
	// is outstanding; the older generation is refused downstream by
	// the gen gate, not here.
	r, gen := r.Reselect()
	if gen != 2 {
		t.Fatalf("reselect must bump to gen 2, got %d", gen)
	}
	if !r.Pending {
		t.Fatal("reselect must not clear in-flight accounting")
	}
	// The stale round's delivery still re-arms dispatch; the next
	// odd tick collects the current generation.
	r = r.OnDelivered()
	if r.Pending {
		t.Fatal("delivery must clear in-flight accounting")
	}
	r, out = r.Tick() // tick 2: even
	if out.Dispatch {
		t.Fatalf("tick 2 must not dispatch, got %+v", out)
	}
	r, out = r.Tick() // tick 3: odd, idle
	if !out.Dispatch || out.Gen != 3 {
		t.Fatalf("tick 3 must dispatch gen 3, got %+v", out)
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

func TestRefreshOnDeliveredIsUnconditional(t *testing.T) {
	var r Refresh
	// Delivery with nothing outstanding is a no-op, never an error
	// path: the shell clears accounting on every refreshMsg.
	r = r.OnDelivered()
	if r.Pending || r.Gen != 0 || r.Ticks != 0 {
		t.Fatalf("spurious delivery must change nothing, got %+v", r)
	}
}

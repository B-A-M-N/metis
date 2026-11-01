package tui

// stallAfterTicks bounds how long one outstanding collection can
// suppress dispatch. At the 500ms tick cadence this is ~5s: past
// the deadline the next tick abandons the wedged round and
// dispatches the current generation instead of stalling forever.
// Abandonment cannot stop a stuck command, but the abandoned
// round's late delivery is refused by both gen gates (accounting
// here, install in main's applyRefresh), so outstanding work stays
// bounded at the wedged round plus its replacement.
const stallAfterTicks = 10

// Refresh is the refresh coordinator's private state: tick count,
// projection generation, and in-flight accounting. Only the
// coordinator transitions mutate it. The shell builds commands from
// the returned decisions and reports each delivery with its
// generation; applyRefresh compares generations read-only
// downstream. Value semantics: the Bubble Tea model holds one by
// value and replaces it with each transition result, exactly like
// Nav.
type Refresh struct {
	Ticks   int
	Gen     int
	Pending bool
	// outstanding is the generation of the latest dispatched round
	// still awaiting delivery; valid only while Pending. Only a
	// delivery carrying exactly this generation clears accounting:
	// stale, superseded, duplicate, and foreign deliveries leave it
	// untouched, so they can neither resurrect an abandoned round
	// nor starve the current one.
	outstanding int
	// dispatchedAt is the tick count when the outstanding round was
	// dispatched; valid only while Pending. It drives the stall
	// deadline.
	dispatchedAt int
}

// TickOutcome is what one tick decides: whether to dispatch a
// collection, at which generation, and whether this collection
// carries the periodic git probe.
type TickOutcome struct {
	Dispatch bool
	Gen      int
	EveryGit bool
}

// Tick advances one 500ms tick. Odd ticks dispatch when no
// collection is outstanding (single-flight: ticks coalesce onto an
// in-flight round instead of stacking concurrent ones); an
// outstanding round older than the stall deadline is abandoned and
// re-dispatched instead of stalling refresh forever. The
// generation bumps with the dispatch so a delivery is always fenced
// to the round that produced it. Every fourth tick carries the git
// probe. Pure: no I/O, no commands.
func (r Refresh) Tick() (Refresh, TickOutcome) {
	r.Ticks++
	if r.Ticks%2 == 1 && (!r.Pending || r.Ticks-r.dispatchedAt >= stallAfterTicks) {
		return r.dispatch()
	}
	return r, TickOutcome{Gen: r.Gen}
}

// dispatch bumps the generation and records the new round as the
// outstanding one. Both tick dispatch and reselect funnel through
// it so every in-flight command is always accounted.
func (r Refresh) dispatch() (Refresh, TickOutcome) {
	r.Gen++
	r.Pending = true
	r.outstanding = r.Gen
	r.dispatchedAt = r.Ticks
	return r, TickOutcome{Dispatch: true, Gen: r.Gen, EveryGit: r.Ticks%4 == 1}
}

// Reselect bumps the generation for a selection-driven repaint.
// It always dispatches (the caller builds the command): a selection
// change prioritizes fresh data over an in-flight round, which the
// new outstanding generation supersedes — the older round's
// delivery is refused downstream by the gen gates, not here. The
// dispatched round is recorded as outstanding so ticks coalesce
// onto it instead of stacking a second concurrent collection.
func (r Refresh) Reselect() (Refresh, int) {
	r.Gen++
	r.Pending = true
	r.outstanding = r.Gen
	r.dispatchedAt = r.Ticks
	return r, r.Gen
}

// Startup bumps the generation for the synchronous initial paint,
// which collects inline before the event loop starts and never
// touches in-flight accounting.
func (r Refresh) Startup() (Refresh, int) {
	r.Gen++
	return r, r.Gen
}

// OnDelivered clears in-flight accounting exactly when the delivery
// carries the outstanding round's generation. Stale, superseded,
// duplicate, and foreign deliveries change nothing: the current
// round stays outstanding until its own delivery lands (or the
// stall deadline abandons it), and a wedged round's late arrival
// can never re-arm dispatch.
func (r Refresh) OnDelivered(gen int) Refresh {
	if r.Pending && gen == r.outstanding {
		r.Pending = false
	}
	return r
}

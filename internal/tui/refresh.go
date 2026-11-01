package tui

// Refresh is the refresh coordinator's private state: tick count,
// projection generation, and in-flight accounting. Only the
// coordinator transitions mutate it. The shell builds commands from
// the returned decisions; applyRefresh compares generations
// read-only downstream. Value semantics: the Bubble Tea model holds
// one by value and replaces it with each transition result, exactly
// like Nav.
type Refresh struct {
	Ticks   int
	Gen     int
	Pending bool
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
// in-flight round instead of stacking concurrent ones); the
// generation bumps with the dispatch so a delivery is always fenced
// to the round that produced it. Every fourth tick carries the git
// probe. Pure: no I/O, no commands.
func (r Refresh) Tick() (Refresh, TickOutcome) {
	r.Ticks++
	if r.Ticks%2 == 1 && !r.Pending {
		r.Gen++
		r.Pending = true
		return r, TickOutcome{Dispatch: true, Gen: r.Gen, EveryGit: r.Ticks%4 == 1}
	}
	return r, TickOutcome{Gen: r.Gen}
}

// Reselect bumps the generation for a selection-driven repaint.
// It always dispatches (the caller builds the command): a selection
// change prioritizes fresh data over an in-flight round, whose
// older generation is refused downstream by the gen gate.
func (r Refresh) Reselect() (Refresh, int) {
	r.Gen++
	return r, r.Gen
}

// Startup bumps the generation for the synchronous initial paint,
// which collects inline before the event loop starts and never
// touches in-flight accounting.
func (r Refresh) Startup() (Refresh, int) {
	r.Gen++
	return r, r.Gen
}

// OnDelivered clears in-flight accounting when a collection lands.
// Unconditional, like the shell it replaces: a stale delivery's
// generation is refused downstream by the gen gate, and clearing
// re-arms dispatch so the current generation gets collected. The
// two gates jointly converge instead of stacking rounds.
func (r Refresh) OnDelivered() Refresh {
	r.Pending = false
	return r
}

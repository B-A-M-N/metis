package tui

// MaxAcquisitions is the hard acquisition budget. The coordinator
// never abandons a still-running command to start a replacement:
// a timeout cannot return a permit while its source is executing.
const MaxAcquisitions = 1

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
	// delivery carrying exactly this generation clears accounting;
	// duplicate and foreign deliveries cannot release another request.
	outstanding int
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
// collection is outstanding. A running acquisition is never
// abandoned merely to release capacity; ticks coalesce until its
// correlated completion. Every fourth tick carries the git probe.
// Pure: no I/O, no commands.
func (r Refresh) Tick() (Refresh, TickOutcome) {
	r.Ticks++
	if r.Ticks%2 == 1 && !r.Pending {
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
	return r, TickOutcome{Dispatch: true, Gen: r.Gen, EveryGit: r.Ticks%4 == 1}
}

// Reselect invalidates any projection collected for the old
// selection. If a command is already active, collection of the
// latest selection waits until that exact command completes.
func (r Refresh) Reselect() (Refresh, int) {
	r.Gen++
	if !r.Pending {
		r.Pending = true
		r.outstanding = r.Gen
	}
	return r, r.Gen
}

// DispatchLatest starts the current generation after a previous
// command released the acquisition permit.
func (r Refresh) DispatchLatest() (Refresh, TickOutcome) {
	if r.Pending {
		return r, TickOutcome{Gen: r.Gen}
	}
	r.Pending = true
	r.outstanding = r.Gen
	return r, TickOutcome{Dispatch: true, Gen: r.Gen, EveryGit: true}
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

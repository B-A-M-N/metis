// Package tui owns the dashboard's UI-state components: navigation,
// session projection, terminal presentation, guidance, Muse feed,
// panels, refresh coordination, and rendering. It depends only on
// the standard library (plus internal/termview for text math); it
// must never import the application's concrete persistence,
// process, or domain implementations. The root application adapts
// its domain services to this package's port interfaces, so
// forbidden dependencies fail at compilation.
//
// Migration is one component at a time: navigation first. Each
// component moves as state + pure reducers + tests; main keeps a
// thin adapter until its component moves.
package tui

// Mode is the UI interaction mode. Navigation owns it: the shell
// routes keys through EnterInput/EnterAttach/ToNav, never assigns
// the field. The zero value ("") routes as nav, matching the
// historical flat-model default (empty string fell through to nav
// handling); explicit transitions record ModeNav/ModeInput/ModeAttach.
type Mode string

const (
	ModeNav    Mode = "nav"
	ModeInput  Mode = "input"
	ModeAttach Mode = "attach"
)

// Nav is the navigation component's private state: selected worker
// cursor, Muse-tab membership, feed cursor, rail viewport offset,
// and interaction mode. Only navigation transitions mutate it;
// every other component reads resolved identities, never these
// fields.
type Nav struct {
	Selected   int
	OnMuse     bool
	FeedCursor int
	TabOffset  int
	Mode       Mode
}

// EnterInput moves to guidance-input mode, preserving the cursor.
func (n Nav) EnterInput() Nav {
	n.Mode = ModeInput
	return n
}

// EnterAttach moves to terminal-attach mode, preserving the cursor.
func (n Nav) EnterAttach() Nav {
	n.Mode = ModeAttach
	return n
}

// ToNav returns to navigation mode, preserving the cursor.
func (n Nav) ToNav() Nav {
	n.Mode = ModeNav
	return n
}

// OnMuseTab returns the initial navigation state for the pinned
// Muse orchestrator tab: Muse membership set, cursors zero (the
// shared Select/Clamp gates resolve them against live data, so
// stored zeros never escape as invalid indexes).
func OnMuseTab() Nav {
	return Nav{OnMuse: true}
}

// Clamp resolves the cursor against count rows: empty lists report
// -1 (no selection); out-of-range cursors pin to the nearest row.
// Renderers and operation requests share this gate, so an invalid
// cursor can never reach them (finding F).
func (n Nav) Clamp(count int) Nav {
	if count <= 0 {
		n.Selected = -1
		return n
	}
	if n.Selected < 0 {
		n.Selected = 0
	}
	if n.Selected >= count {
		n.Selected = count - 1
	}
	return n
}

// Select returns the selected row or -1 when there is none.
func (n Nav) Select(count int) int {
	return n.Clamp(count).Selected
}

// Restore re-binds the cursor by stable identity after the session
// list changes: the previously viewed name wins; a vanished name
// falls back to the top row. Index 0 with no names is "no rows",
// not "first row".
func (n Nav) Restore(names []string, prev string) Nav {
	n.Selected = 0
	for i, name := range names {
		if name == prev {
			n.Selected = i
			break
		}
	}
	return n.Clamp(len(names))
}

// Up moves the cursor or feed cursor up. On the Muse tab it moves
// the feed cursor; from the top worker it enters the Muse tab with
// the worker selection preserved. Reselect reports whether the
// worker selection changed (the caller repaints from fresh data).
func (n Nav) Up(feedLen int) (Nav, bool) {
	if n.OnMuse {
		if n.FeedCursor > 0 {
			n.FeedCursor--
		}
		return n, false
	}
	if n.Selected > 0 {
		n.Selected--
		return n, true
	}
	n.OnMuse = true
	n.FeedCursor = feedLen - 1
	return n, false
}

// Down moves the cursor or feed cursor down. Down past the oldest
// feed event returns to the worker list. Reselect reports whether
// the worker selection changed.
func (n Nav) Down(count, feedLen int) (Nav, bool) {
	if n.OnMuse {
		if n.FeedCursor < feedLen-1 {
			n.FeedCursor++
			return n, false
		}
		n.OnMuse = false
		n.Selected = 0
		return n.Clamp(count), true
	}
	if n.Selected < count-1 {
		n.Selected++
		return n, true
	}
	return n, false
}

// JumpToFeedTarget resolves a feed event's worker against the
// current list: the cursor moves only when the target is listed.
// The caller decides what an unlisted target means for status.
func (n Nav) JumpToFeedTarget(names []string, target string) (Nav, bool) {
	if target == "" {
		return n, false
	}
	for i, name := range names {
		if name == target {
			n.OnMuse = false
			n.Selected = i
			return n, true
		}
	}
	return n, false
}

// ClampFeedCursor pins the feed cursor to a changed feed: a refused
// generation never paints a stale feed as live, and a miss keeps
// last-known data — the cursor follows the data it actually has.
func (n Nav) ClampFeedCursor(feedLen int) Nav {
	if n.FeedCursor >= feedLen {
		n.FeedCursor = feedLen - 1
	}
	if n.FeedCursor < 0 && feedLen > 0 {
		n.FeedCursor = feedLen - 1
	}
	return n
}

// FollowTabs anchors the rail viewport on the cursor so large
// fleets scroll instead of clip. railH is the visible worker-row
// budget (already adjusted for the pinned Muse row by the caller).
func (n Nav) FollowTabs(railH int) Nav {
	if railH < 1 {
		railH = 1
	}
	if n.Selected < n.TabOffset {
		n.TabOffset = n.Selected
	}
	if n.Selected >= n.TabOffset+railH {
		n.TabOffset = n.Selected - railH + 1
	}
	if n.TabOffset < 0 {
		n.TabOffset = 0
	}
	return n
}

// RailWindow resolves the visible worker-row window. The Muse row
// never scrolls out: the window covers worker rows only.
func (n Nav) RailWindow(tabCount, railH int) (off, end int) {
	off = n.TabOffset
	if off > n.Selected {
		off = n.Selected
	}
	if off < 0 {
		off = 0
	}
	if off+railH <= n.Selected && tabCount > 0 {
		off = n.Selected - railH + 1
		if off < 0 {
			off = 0
		}
	}
	end = off + railH
	if end > tabCount {
		end = tabCount
	}
	return off, end
}

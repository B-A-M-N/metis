package main

import "fmt"

// tui_install.go: per-component refresh installers.
//
// Each UI component decides how a valid projection changes its own
// state; applyRefresh (tui_state.go) keeps only the freshness gates
// and the cross-component composition (input-absent flush,
// detach, data status) that no single component owns. No installer
// writes another component's fields.

// applySessionsRefresh installs the session projection: list,
// liveness, registry entry, and rail identity blocks. An
// untrustworthy list (registry/PTY failure) installs nothing:
// wiping the panes with an incomplete list would strand the UI on
// a phantom empty state while the sessions are still alive.
func (m *model) applySessionsRefresh(d refreshData) {
	if !d.sessionsOk {
		return
	}
	m.names = d.names
	m.running = d.running
	m.entry = d.entry
	if d.tabInfo != nil {
		m.tabInfo = d.tabInfo
	}
	if d.identities != nil {
		m.identities = d.identities
	}
}

// applyFeedRefresh installs the orchestration feed and its derived
// activity projection. A failed feed read keeps last-known data: a
// miss must not masquerade as empty.
func (m *model) applyFeedRefresh(d refreshData) {
	if d.feedOk {
		m.feed = d.feed
		m.controllers = d.controllers
	}
	m.museState = d.museState
	m.museOpen = d.museOpen
	m.museBlocked = d.museBlocked
	m.museFacts = d.museFacts
}

// applyPanelsRefresh installs the path-panel content.
func (m *model) applyPanelsRefresh(d refreshData) {
	m.panelRun = d.panelRun
	m.panel = d.panel
}

// applyNavRefresh reconciles navigation cursors with the data they
// actually have: the feed cursor pins to a shrunk feed, worker
// selection re-binds by stable identity across list changes, and
// the rail viewport anchors on the cursor. It returns the resolved
// selection for the shell composition.
func (m *model) applyNavRefresh(prev string) string {
	m.nav = m.nav.ClampFeedCursor(len(m.feed))
	m.nav = m.nav.Restore(m.names, prev)
	m.followTabs()
	return m.selName()
}

// applyTerminalRefresh installs terminal presentation state: live
// lines, event lines, the Git pane (fresh data wins; a moved
// selection with no fresh data clears rather than showing the
// previous session's state), and output-generation tracking. A
// jumped generation means the log rotated underneath us; the
// returned notice lets the shell say so on the status line instead
// of silently showing a discontinuous tail. The generation map is
// replaced after the comparison, so the next refresh compares
// against what this one saw.
//
// Ownership note: this installer reads the resolved selection but
// never writes navigation; the shell passes both names in.
func (m *model) applyTerminalRefresh(d refreshData, prev, newSel string) string {
	m.live = d.live
	m.events = d.events
	if d.hasGit {
		m.gitLines = d.git
	} else if newSel != prev {
		m.gitLines = nil
	}
	if d.logGen == nil || newSel == "" {
		return ""
	}
	notice := ""
	if old, had := m.logGen[newSel]; had {
		if ng, ok := d.logGen[newSel]; ok && ng > old {
			notice = fmt.Sprintf("output history rotated for %s (gen %d); older lines discarded", newSel, ng)
		}
	}
	m.logGen = d.logGen
	return notice
}

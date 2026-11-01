package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// tui_viewstate.go: navigation transitions plus terminal/panel
// toggles.
//
// The dashboard keeps ONE Bubble Tea program (one model, one
// Update, one View). Navigation state lives in the extracted
// internal/tui component (Nav); the methods here are the thin
// adapter: they delegate cursor/viewport transitions to Nav,
// reset navigation-scoped scroll intent, and return the refresh
// command that repaints from freshly collected data. Mode dispatch
// stays in tui.go Update; session/terminal/feed/panel installation
// stays in applyRefresh (tui_state.go).

// navReselect bumps the refresh generation and returns a full
// refresh command for the current selection, so worker moves always
// repaint from freshly collected data.
func (m *model) navReselect() tea.Cmd {
	var gen int
	m.refreshCtl, gen = m.refreshCtl.Reselect()
	return m.refreshCmd(gen, m.selName(), true)
}

// navUp moves the selection or feed cursor up (Nav.Up); a changed
// worker selection repaints from fresh data. Navigation-scoped
// scroll intent resets on every move.
func (m *model) navUp() tea.Cmd {
	var cmd tea.Cmd
	var reselect bool
	m.nav, reselect = m.nav.Up(len(m.feed))
	if reselect {
		cmd = m.navReselect()
	}
	m.scroll = 0
	return cmd
}

// navDown moves the selection or feed cursor down (Nav.Down); a
// changed worker selection repaints from fresh data.
func (m *model) navDown() tea.Cmd {
	var cmd tea.Cmd
	var reselect bool
	m.nav, reselect = m.nav.Down(len(m.names), len(m.feed))
	if reselect {
		cmd = m.navReselect()
	}
	m.scroll = 0
	return cmd
}

// toggleDirect flips live-send mode and reports the new posture.
func (m *model) toggleDirect() {
	m.direct = !m.direct
	if m.direct {
		m.status = "direct mode ON (live input, one writer at a time)"
	} else {
		m.status = "direct mode OFF (file only)"
	}
}

// toggleGraphPanel pops the MD path-traversal banner into the Git
// pane's slot and back out. Same height, same layout math, graph
// lineage in place of workdir state.
func (m *model) toggleGraphPanel() {
	m.graphPanel = !m.graphPanel
	if m.graphPanel {
		m.status = "path panel ON — graph lineage for " + orDash(m.panelRun) + " (g hides)"
	} else {
		m.status = "path panel off"
	}
}

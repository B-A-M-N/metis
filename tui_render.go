package main

import (
	"strings"
)

// tui_render.go: View composition over the focused render units.
//
// Presentation code lives here only as composition: text measurement
// and sanitization (tui_text.go), viewport geometry and box/scroll
// primitives (tui_layout.go), the worker rail plus pinned tab
// (tui_render_tabs.go), the Muse glyph map and orchestration feed
// (tui_render_muse.go), the terminal/Git/graph panes
// (tui_render_worker.go), and the header/status/footer chrome
// (tui_render_chrome.go). This file owns no measurement, no labels,
// and no pane bodies — it only selects which panes show and zips
// them into rows.
//
// The render path stays PURE: View renders only the cached model
// projection installed by applyRefresh. No DB queries, no process
// operations, no blocking effects.

func (m model) View() string {
	if m.width < 80 || m.height < 24 {
		return "Terminal too small: need at least 80x24. Resize or q to quit.\n"
	}
	lay := computeLayout(m.width, m.height)
	sel, running, mode := selectedSummary(m)
	var b strings.Builder
	b.WriteString(renderHeaderLine(m, sel, running, mode, lay.w) + "\n")

	left := renderRailBox(m, lay)
	// Worker output is untrusted: pane renderers sanitize control
	// sequences before painting so a session cannot repaint the
	// dashboard.
	var live, git []string
	if m.nav.OnMuse {
		// Muse tab: the orchestration feed replaces the terminal
		// and Git panes.
		live = renderMusePane(m, lay)
		git = []string{}
	} else {
		live = renderLivePane(m, sel, lay)
		git = renderGitPane(m, sel, lay)
	}
	for _, row := range joinPanes(left, live, git, lay) {
		b.WriteString(row + "\n")
	}
	b.WriteString(renderEventsLine(m, sel, lay.w) + "\n")
	b.WriteString(renderPromptLine(m, lay.w) + "\n")
	b.WriteString(renderStatusLine(m, lay.w) + "\n")
	return b.String()
}

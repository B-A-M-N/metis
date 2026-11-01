package main

import (
	"fmt"

	"tui-dash/internal/termview"
)

// tui_render_worker.go: the right-stack worker panes — live terminal
// output and the Git/path pane. Worker output is untrusted:
// sanitized before rendering so a session cannot repaint the
// dashboard. Pure: renders only the cached projection.

// renderLivePane renders the live terminal box for the selected
// session.
func renderLivePane(m model, sel string, lay renderLayout) []string {
	entry := boundEntry(m, sel)
	rawLive := sliceLines(boundLines(m, sel, m.live), lay.liveH, m.scroll)
	liveContent := make([]string, len(rawLive))
	for i, l := range rawLive {
		liveContent[i] = termview.SanitizeLine(l)
	}
	return boxLines(fmt.Sprintf("Live: %s (%s/%s, PgUp/PgDn scroll, r=tail)", sel,
		orDash(entry.Harness), orDash(entry.Model)), liveContent, lay.rightW, lay.liveH)
}

// renderGitPane renders the lower right box: either the MD path
// banner (same height, same layout math, graph lineage instead of
// workdir state; lines are pre-sanitized markdown) or the Git pane.
func renderGitPane(m model, sel string, lay renderLayout) []string {
	if m.graphPanel {
		title := "Path"
		if m.panelRun != "" {
			title = "Path: " + m.panelRun
		}
		return boxLines(title, sliceLines(m.panel, lay.gitH, 0), lay.rightW, lay.gitH)
	}
	entry := boundEntry(m, sel)
	return boxLines(fmt.Sprintf("Git: %s", entry.Workdir), sliceLines(boundLines(m, sel, m.gitLines), lay.gitH, 0), lay.rightW, lay.gitH)
}

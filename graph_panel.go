package main

// graph_panel.go: the pop-in MD banner that shows path traversal on
// the right side of the TUI. All content is built here as plain
// data; tui.go only toggles, truncates, and boxes it. Lines are
// markdown source (`#`, `##`, `-`) with ANSI bold on headers only;
// worker-influenced text (objectives) is sanitized before render so
// a session cannot repaint the dashboard through the panel.

import (
	"strings"
)

const graphPanelCap = 60

// panelCollectCap bounds one collected banner; View slices it down
// to the pane height. Refresh collects the full cap so a taller
// terminal never shows a banner shorter than the pane.
const panelCollectCap = 64

// resolvePanelRun picks the run the banner traverses: the selected
// worker's newest run, or the newest run overall on the Muse tab.
// Run resolution itself lives in task_readmodel.go (latestRunForAgent
// / latestRun); the panel consumes the projection, never SQL.
func resolvePanelRun(muse bool, sel string) string {
	if !muse && sel != "" {
		if run := latestRunForAgent(sel); run != "" {
			return run
		}
	}
	return latestRun()
}

// mdBold marks a markdown header line bold. Headers are static
// panel text, never worker content, so the escape is trusted.
func mdBold(s string) string {
	return "\x1b[1m" + s + "\x1b[0m"
}

// graphMDLines builds the banner body for a run: ready frontier,
// blocked chains with dependency paths, coverage counts. Capped;
// the tail notes what did not fit. The panel owns layout and text
// shaping only: acquisition lives in the read model
// (readGraphBanner), one open per render. The panel never imports
// persistence or opens the database (boundary tests pin this).
func graphMDLines(run string, maxLines int) []string {
	if maxLines <= 0 {
		maxLines = graphPanelCap
	}
	if strings.TrimSpace(run) == "" {
		return []string{"(no runs yet — delegating through Metis records the graph)"}
	}
	return readGraphBanner(run, maxLines)
}

func shortID(id string) string {
	if len(id) > 18 {
		return id[:18] + "…"
	}
	return id
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

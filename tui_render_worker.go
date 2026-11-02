package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"metis/internal/termview"
)

// tui_render_worker.go: the right-stack worker panes — live terminal
// output and the Git/path pane. Worker output is untrusted:
// sanitized before rendering so a session cannot repaint the
// dashboard. Pure: renders only the cached projection.

// renderLivePane renders the live terminal box for the selected
// session.
func renderLivePane(m model, sel string, lay renderLayout) []string {
	entry := boundEntry(m, sel)
	detail := m.tabInfo[sel]
	provider, model := entry.Provider, entry.Model
	if provider == "" {
		provider = detail.Provider
	}
	if model == "" {
		model = detail.Model
	}
	rawLive := sliceLines(boundLines(m, sel, m.live), lay.contentH, m.scroll)
	liveContent := make([]string, len(rawLive))
	for i, l := range rawLive {
		liveContent[i] = termview.SanitizeLine(l)
	}
	title := fmt.Sprintf("Text snapshot: %s (%s/%s; F5 opens full terminal)", sel,
		orDash(provider), orDash(model))
	if m.isNativeSelection(sel) {
		title = "Native activity: " + sel + " (i=structured steering)"
	}
	return boxLines(title, liveContent, lay.mainW, lay.contentH)
}

func renderInfoPane(m model, sel string, lay renderLayout) []string {
	entry := boundEntry(m, sel)
	detail := m.tabInfo[sel]
	label := detail.Label
	if label == "" {
		label = sel
	}
	health := "stopped"
	if m.running[sel] {
		health = "running"
	}
	provider := detail.Provider
	if provider == "" {
		provider = entry.Provider
	}
	model := detail.Model
	if model == "" {
		model = entry.Model
	}
	profile := detail.Profile
	if profile == "" {
		profile = entry.Profile
	}
	workdir := detail.Workdir
	if workdir == "" {
		workdir = entry.Workdir
	}
	lines := []string{
		"Project: " + label,
		"Session ID: " + sel,
		"Provider: " + orDash(provider),
		"Model: " + orDash(model),
		"Profile: " + orDash(profile),
		"Process: " + health,
		"Working directory:",
		workdir,
	}
	if m.infoSession != sel {
		lines = append(lines, "Metrics/errors loading…")
	} else {
		res := m.infoResources
		lines = append(lines, fmt.Sprintf("Obs goroutines: %d", res.Goroutines),
			fmt.Sprintf("Obs heap: %.1f MiB", float64(res.HeapAlloc)/(1024*1024)))
		stream := "not recorded"
		if m.infoStreamFound {
			stream = "unavailable"
			if m.infoStream.Bytes >= 0 {
				stream = fmt.Sprintf("%d / %d bytes", m.infoStream.Bytes, res.StreamCap)
			}
		}
		lines = append(lines, "Stream: "+strings.ReplaceAll(stream, " bytes", "B"), "Recent ops errors:")
		if len(m.infoDiagnostics) == 0 {
			lines = append(lines, "(none observed)")
		} else {
			for _, diagnostic := range m.infoDiagnostics {
				stamp := diagnostic.At.Local().Format("15:04:05")
				lines = append(lines, sanitizeLine(fmt.Sprintf("%s %s/%s: %s", stamp,
					diagnostic.Phase, diagnostic.ErrorClass, diagnostic.Detail)))
			}
		}
		lines = append(lines, "Ops source: "+m.infoDiagnosticSource)
	}
	lines = append(lines, "Recent session events:")
	ev := boundLines(m, sel, m.events)
	var selectedEvents []string
	for _, line := range ev {
		var event struct {
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && (event.Text == sel || strings.HasPrefix(event.Text, sel+":")) {
			selectedEvents = append(selectedEvents, sanitizeLine(line))
		}
	}
	if len(selectedEvents) == 0 {
		lines = append(lines, "(none recorded)")
	} else {
		start := len(selectedEvents) - 2
		if start < 0 {
			start = 0
		}
		lines = append(lines, selectedEvents[start:]...)
	}
	return boxLines("Info · F6 closes", sliceLines(lines, lay.contentH, 0), lay.infoW, lay.contentH)
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

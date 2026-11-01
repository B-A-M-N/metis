package main

import (
	tea "github.com/charmbracelet/bubbletea"
)

// tui_projection.go: immutable refresh snapshots and the async
// refresh command.
//
// Data collection lives behind the refreshSource interface
// (tui_source.go): this file owns no controller queries and no
// formatting. collectRefreshData is the production entry point — it
// reads through the active refreshSource and assembles the
// refreshData projection. Presentation helpers (tab palettes,
// labels, feed rendering) live in tui_render.go.

type refreshData struct {
	gen int
	// sel binds the projection to the selected session it was
	// collected for ("" when nothing was selected). applyRefresh
	// refuses data collected for a different session, so a slow
	// collect can never paint another session's detail panes.
	sel     string
	names   []string
	running map[string]bool
	// logGen carries each PTY session's output generation so the UI
	// can report discarded history instead of showing a silent gap.
	logGen map[string]int
	// tabInfo holds the current rail identity blocks by session name.
	tabInfo map[string]tabDetail
	entry   Session
	live    []string
	git     []string
	hasGit  bool
	events  []string
	// feed carries the global orchestration projection: latest
	// events plus the Muse activity state and outstanding counts.
	// Unlike the session panes it is not bound to sel.
	// feedOk says whether this collection actually read the feed:
	// applyRefresh keeps last-known data when a read fails instead
	// of painting the miss as an empty feed.
	feed        []OrchEvent
	feedOk      bool
	museState   string
	museOpen    int
	museBlocked int
	// panelRun/panel carry the MD path-traversal banner: the graph
	// lineage projection for the selected session's run (or the
	// newest run when nothing resolves). Collected with the feed so
	// the banner stays live without a worker selected.
	panelRun string
	panel    []string
}

// tabDetail is one terminal tab's identity block.
type tabDetail struct {
	Harness string
	Model   string
	Title   string
}

type refreshMsg struct{ data refreshData }

// refreshCmd is a model method so the command captures this
// instance's source (finding D): concurrent dashboards never share
// acquisition. A nil source (zero-value model literals in tests)
// reads live, matching newModel.
func (m model) refreshCmd(gen int, sel string, everyGit bool) tea.Cmd {
	src := m.source
	if src == nil {
		src = liveRefreshSource{}
	}
	return func() tea.Msg {
		return refreshMsg{data: collectRefreshData(src, gen, sel, everyGit)}
	}
}

// collectRefreshData gathers the unified session list and the
// selected session's content off the UI goroutine through src, then
// assembles the immutable projection. View() only renders what this
// returns. The source is an explicit parameter: no package global
// decides what a refresh reads. Stream retention is NOT enforced
// here: it is owned by retention.go (MaintainRetention) and the
// stream read path, so headless execution stays bounded with no
// dashboard refreshing.
func collectRefreshData(src refreshSource, gen int, sel string, everyGit bool) refreshData {
	if src == nil {
		src = liveRefreshSource{}
	}
	return assembleRefresh(src.snapshots(sel, everyGit), gen, sel)
}

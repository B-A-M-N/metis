package main

import (
	"fmt"
	"strings"

	tuistate "metis/internal/tui"
)

// tui_state.go: selection, attachment, and refresh-generation state transitions. Extracted from tui.go; behavior unchanged.

func newModel(preselect string) model {
	return newModelWithSource(liveRefreshSource{}, preselect)
}

// newModelWithSource builds a model reading through src: the
// instance-scoped injection seam (finding D). Production passes the
// live source; tests pass scripted fakes with no shared global.
func newModelWithSource(src refreshSource, preselect string) model {
	if src == nil {
		src = liveRefreshSource{}
	}
	// No explicit target: open on the pinned Muse orchestrator tab.
	// An explicit session preselects that worker instead.
	nav := tuistate.Nav{}
	if preselect == "" {
		nav = tuistate.OnMuseTab()
	}
	return model{source: src, preselect: preselect, nav: nav.ToNav(),
		status: "enter=attach live  i=guidance  g=path panel  d=direct toggle  q=quit  PgUp/PgDn=scroll"}
}

// selName resolves the selected session name, or "" when there is
// no selection. The single shared gate (finding F): every reader
// uses the navigation component's clamped cursor, so no caller
// re-derives bounds.
func (m model) selName() string {
	if i := m.nav.Select(len(m.names)); i >= 0 {
		return m.names[i]
	}
	return ""
}

func (m *model) attachTarget() (string, bool) {
	// The Muse tab is the orchestration feed, never a terminal:
	// Enter there jumps to the event's worker instead of attaching.
	if m.nav.OnMuse {
		return "", false
	}
	sel := m.selName()
	if sel == "" || !m.running[sel] || m.isNativeSelection(sel) {
		return "", false
	}
	return sel, true
}

func (m model) canonicalSession(display string) string {
	if id, ok := m.identities[display]; ok && id.Canonical != "" {
		return id.Canonical
	}
	if strings.HasPrefix(display, nativePrefix) {
		return strings.TrimPrefix(display, nativePrefix)
	}
	return display
}

func (m model) displayIdentity(canonical string) string {
	if _, ok := m.identities[canonical]; ok {
		return canonical
	}
	for key, id := range m.identities {
		if id.Canonical == canonical && id.Backend == TargetMuseNative {
			return key
		}
	}
	return canonical
}

func (m model) isNativeSelection(display string) bool {
	return strings.HasPrefix(display, nativePrefix) || m.identities[display].Backend == TargetMuseNative
}

// jumpToFeedTarget follows the feed cursor to its worker tab when
// that worker is still listed. Enter on the Muse tab never attaches
// a terminal: the feed is navigation, not a PTY.
func (m *model) jumpToFeedTarget() {
	if len(m.feed) == 0 {
		m.status = "orchestration feed is empty; select a worker tab to attach"
		return
	}
	cursor := m.nav.FeedCursor
	if cursor < 0 || cursor >= len(m.feed) {
		m.status = "orchestration feed is empty; select a worker tab to attach"
		return
	}
	target := m.displayIdentity(m.feed[cursor].TargetAgent)
	if target == "" {
		m.status = "feed event has no worker target; select a worker tab to attach"
		return
	}
	if nav, ok := m.nav.JumpToFeedTarget(m.names, target); ok {
		m.nav = nav
		if m.isNativeSelection(target) {
			m.direct = false
		}
		m.status = "jumped to worker " + target
		return
	}
	m.status = "event target " + target + " is not in the worker list"
}

// attachKey routes a keypress to the attached session. The update
// path never blocks: submissions enqueue on the session's ordered
// sender and the outcome returns as an inputResultMsg. A full lane
// refuses immediately with the reason in the status line.
const staleProjectionPrefix = "stale projection refused"

// installStaleMarker records the exact stale text this refresher
// installed, so a later fresh apply clears only its own marker. It
// is the sole writer of staleMarker; the status line is display
// only and is never parsed for control decisions.
func (m *model) installStaleMarker(marker string) {
	m.status = marker
	m.staleMarker = marker
}

// applyRefresh installs collected data, preserving selection by name
// across list changes. Shared by the sync initial paint and async
// refreshMsg delivery so both paths render identical state.
//
// Freshness gates: a projection whose generation or selected session
// does not match the model is refused outright. The last-known panes
// stay on screen, but only with an explicit stale marker — never
// presented as fresh. Refusal touches nothing else: per-session
// failure state (inputPending) and pending guidance survive intact.
func (m *model) applyRefresh(d refreshData) {
	prev := m.selName()
	if d.gen != m.refreshCtl.Gen {
		m.installStaleMarker(fmt.Sprintf("%s (gen %d, want %d); showing last-known data", staleProjectionPrefix, d.gen, m.refreshCtl.Gen))
		return
	}
	if d.sel != "" && prev != "" && prev != d.sel {
		m.installStaleMarker(fmt.Sprintf("%s (session %q, viewing %q); showing last-known data", staleProjectionPrefix, d.sel, prev))
		return
	}
	// Clear only the marker this refresher installed: a foreign
	// status that merely shares the prefix is someone else's
	// message (guidance outcome, input notice) and must survive.
	// Control state lives in staleMarker, never in parsed text.
	if m.staleMarker != "" {
		if m.status == m.staleMarker {
			m.status = ""
		}
		m.staleMarker = ""
	}
	// Record whose panes these are before installing them: panes
	// collected for d.sel must never render as another session's.
	m.paneSel = d.sel
	if !d.sessionsOk {
		// Partial acquisition: healthy panes still install below,
		// but the session list may be incomplete, so say so
		// instead of silently presenting last-known as live.
		m.installStaleMarker(fmt.Sprintf("session list unavailable (gen %d); showing last-known data", d.gen))
	}
	// Each component installs its own slice of the valid
	// projection (tui_install.go); this shell keeps the freshness
	// gates above and the cross-component detach below, which no
	// single component owns.
	m.applySessionsRefresh(d)
	m.applyFeedRefresh(d)
	m.applyPanelsRefresh(d)
	newSel := m.applyNavRefresh(prev)
	if notice := m.applyTerminalRefresh(d, prev, newSel); notice != "" {
		m.status = notice
	}
	// A disconnected tab cannot stay attached: the single terminal
	// viewport only ever shows the selected live session.
	if m.nav.Mode == tuistate.ModeAttach {
		if _, ok := m.attachTarget(); !ok {
			m.nav = m.nav.ToNav()
			m.status = "detached: session stopped"
		}
	}
}

// followTabs keeps the tab-rail viewport anchored on the selection so
// newly connected sessions appear and fleets scroll instead of clip.
// The geometry decision lives in the navigation component
// (Nav.FollowTabs); this adapter only supplies the rail budget:
// worker rows share the rail with the pinned Muse row, so the
// scroll window is one row shorter than the rail height.
func (m *model) followTabs() {
	tabH := m.height - 7
	if tabH < 1 {
		tabH = 1
	}
	workerH := tabH - 1
	if workerH < 1 {
		workerH = 1
	}
	m.nav = m.nav.FollowTabs(workerH)
}

// refresh repaints synchronously from freshly collected data; used
// only on the startup path, never inside Update. The rail glyph map
// lives with the presentation code (museGlyph, tui_render.go), which
// keeps a monochrome text fallback alongside every glyph.
func (m *model) refresh() {
	if m.preselect != "" {
		m.nav.Selected = -1
	}
	var gen int
	m.refreshCtl, gen = m.refreshCtl.Startup()
	sel := m.preselect
	if sel == "" {
		sel = m.selName()
	}
	m.applyRefresh(collectRefreshData(m.source, gen, sel, true))
	if m.preselect != "" {
		for i, n := range m.names {
			if n == m.preselect || (isPtySession(n) && ptyBare(n) == m.preselect) {
				m.nav.Selected = i
				break
			}
		}
		m.nav = m.nav.Clamp(len(m.names))
		m.preselect = ""
	}
}

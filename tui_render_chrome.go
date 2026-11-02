package main

import (
	"fmt"
	"strings"

	tuistate "metis/internal/tui"
)

// tui_render_chrome.go: the frame around the panes — header status,
// footer events, guidance prompt, and status lines. Pure formatting
// over cached view state.

// selectedSummary resolves the header identity: selected session
// name, its liveness, and the mode word. An invalid cursor never
// escapes: empty lists resolve to "(none)", out-of-range cursors
// clamp to the nearest live row (finding F). Renderers and
// operation requests share this gate, so no caller re-derives it.
func selectedSummary(m model) (sel string, running bool, mode string) {
	if m.nav.OnMuse {
		return "Meta.ai controller", false, "MCP"
	}
	sel = m.selName()
	if sel == "" {
		sel = "(none)"
	} else {
		running = m.running[sel]
	}
	mode = "NAV"
	if m.nav.Mode == tuistate.ModeInput {
		mode = "INPUT"
	} else if m.nav.Mode == tuistate.ModeAttach {
		mode = "ATTACHED"
	}
	return sel, running, mode
}

// boundEntry returns the cached session metadata only when it was
// collected for the viewed session; otherwise the zero Session, so
// chrome never mixes one worker's harness/model/workdir into
// another worker's header (P0 pane binding via model.paneSel,
// recorded by applyRefresh alongside the installs).
func boundEntry(m model, sel string) Session {
	if m.paneSel != sel {
		return Session{}
	}
	return m.entry
}

// boundLines returns cached pane lines only when they were
// collected for the viewed session; otherwise nil, so one
// worker's output never paints as another's (P0).
func boundLines(m model, sel string, lines []string) []string {
	if m.paneSel != sel {
		return nil
	}
	return lines
}

// renderHeaderLine renders the top dashboard status line.
func renderHeaderLine(m model, sel string, running bool, mode string, w int) string {
	if m.nav.OnMuse {
		return pad(" Metis  Meta.ai Orchestrator  controller=MCP  web-chat=not-connected ", w)
	}
	entry := boundEntry(m, sel)
	detail := m.tabInfo[sel]
	provider, model := entry.Provider, entry.Model
	if provider == "" {
		provider = detail.Provider
	}
	if model == "" {
		model = detail.Model
	}
	top := fmt.Sprintf(" Metis  session=%s provider=%s model=%s running=%v mode=%s direct=%v ",
		sel, orDash(provider), orDash(model), running, mode, m.direct)
	return pad(top, w)
}

// renderEventsLine renders the footer events summary (last two,
// sanitized).
func renderEventsLine(m model, sel string, w int) string {
	ev := "(no events)"
	if evLines := boundLines(m, sel, m.events); len(evLines) > 0 {
		tail := evLines
		if len(tail) > 2 {
			tail = tail[len(tail)-2:]
		}
		clean := make([]string, len(tail))
		for i, e := range tail {
			clean[i] = sanitizeLine(e)
		}
		ev = strings.Join(clean, " | ")
	}
	return pad("Events: "+ev, w)
}

// renderPromptLine renders the guidance/attach prompt row.
func renderPromptLine(m model, w int) string {
	if m.nav.OnMuse {
		return pad("[CONTROLLER] use MCP tools · no linked chat stream", w)
	}
	kind := "FILE"
	if m.direct {
		kind = "DIRECT"
	}
	prompt := fmt.Sprintf("[%s] prompt> %s", kind, m.guideCtl.Buf)
	if m.nav.Mode == tuistate.ModeAttach {
		prompt = "[LIMITED INPUT] keys go to session  (text snapshot only; F5 opens full terminal; esc detaches)"
	}
	return pad(prompt, w)
}

// renderStatusLine renders the bottom status row.
func renderStatusLine(m model, w int) string {
	return pad(m.status, w)
}

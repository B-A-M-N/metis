package main

import "strings"

// tui_render_tabs.go: worker rail rows plus the pinned Muse row and
// the rail scroll window. Pure presentation over the cached session
// list: no controller queries, no tmux, no filesystem.

// tabPalettes are dark, low-saturation backgrounds, one per harness.
// Raw ANSI keeps the rail dependency-free; the overview sanitizer
// never sees these because they wrap our own labels, not worker output.
var tabPalettes = []string{
	"\x1b[48;5;17m", "\x1b[48;5;18m", "\x1b[48;5;23m", "\x1b[48;5;24m",
	"\x1b[48;5;52m", "\x1b[48;5;53m", "\x1b[48;5;58m", "\x1b[48;5;60m",
}

// harnessBG maps a harness name deterministically to a tab background.
// Empty harness gets no background.
func harnessBG(harness string) string {
	if harness == "" {
		return ""
	}
	h := 0
	for i := 0; i < len(harness); i++ {
		h = h*31 + int(harness[i])
	}
	if h < 0 {
		h = -h
	}
	return tabPalettes[h%len(tabPalettes)]
}

// providerModelBG colors Muse Code tabs from the configured provider/model,
// never from the Muse Code harness name.
func providerModelBG(provider, model string) string {
	family := strings.ToLower(model)
	switch {
	case strings.HasPrefix(family, "claude"):
		return "\x1b[48;5;52m"
	case strings.HasPrefix(family, "agy"):
		return "\x1b[48;5;60m"
	case strings.HasPrefix(family, "gpt"), strings.HasPrefix(family, "chatgpt"):
		return "\x1b[48;5;236m"
	}
	switch strings.ToLower(provider) {
	case "meta":
		return "\x1b[48;5;18m"
	case "claude":
		return "\x1b[48;5;52m"
	case "agy":
		return "\x1b[48;5;60m"
	case "chatgpt", "openai":
		return "\x1b[48;5;236m"
	case "local":
		return "\x1b[48;5;238m"
	default:
		return ""
	}
}

// tabLabel builds one rail row: connection mark, session id, model,
// and title — background-colored by provider/model first (Muse
// identity: same harness, different backends), harness second,
// plain when neither resolves.
func tabLabel(prefix, mark, name string, detail tabDetail, innerW int) string {
	displayName := detail.Label
	if displayName == "" {
		displayName = name
	}
	plain := prefix + mark + " " + displayName
	if displayName != name {
		// Ordinal labels ("Locus · 01") shift as the population
		// changes; the session name is the persistent instance
		// identity and always rides along.
		plain += " · " + name
	}
	if detail.Model != "" {
		plain += " [" + detail.Model + "]"
	}
	if detail.Title != "" {
		plain += " — " + detail.Title
	}
	plain = pad(truncW(sanitizeLine(plain), innerW), innerW)
	bg := providerModelBG(detail.Provider, detail.Model)
	if bg == "" {
		bg = harnessBG(detail.Harness)
	}
	if bg != "" {
		return bg + plain + "\x1b[0m"
	}
	return plain
}

// buildWorkerTabs renders one rail row per session, stacked
// vertically on the left. ● is connected, ○ is disconnected; the
// single viewport on the right shows the selected tab. Worker rows
// are never mistaken for the orchestrator: they render only from
// m.names, never the feed.
func buildWorkerTabs(m model, innerW int) []string {
	var tabs []string
	sel := m.nav.Select(len(m.names))
	for i, n := range m.names {
		mark := "○"
		if m.running[n] {
			mark = "●"
		}
		prefix := "  "
		if i == sel && !m.nav.OnMuse {
			prefix = "> "
		}
		tabs = append(tabs, tabLabel(prefix, mark, n, m.tabInfo[n], innerW))
	}
	if len(tabs) == 0 {
		tabs = []string{"(none: use metis up)"}
	}
	return tabs
}

// railWindow resolves the scroll window over worker rows so large
// fleets scroll instead of clip. The Muse row never scrolls out: the
// window covers worker rows only. The geometry decision lives in the
// navigation component; this is the thin adapter.
func railWindow(m model, tabCount int, lay renderLayout) (off, end int) {
	return m.nav.RailWindow(tabCount, lay.workerH)
}

// renderRailBox renders the left "Terminals" pane: the pinned Muse
// row first, then the visible worker window.
func renderRailBox(m model, lay renderLayout) []string {
	tabs := buildWorkerTabs(m, lay.leftW-2)
	off, end := railWindow(m, len(tabs), lay)
	visTabs := tabs
	if off < len(tabs) {
		visTabs = tabs[off:end]
	}
	museRow := museTabLabel(m.nav.OnMuse, m.museState, lay.leftW-2)
	visTabs = append([]string{museRow}, visTabs...)
	return boxLines("Terminals", visTabs, lay.leftW, lay.contentH)
}

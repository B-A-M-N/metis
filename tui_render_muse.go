package main

import (
	"fmt"
	"strings"
	"time"
)

// tui_render_muse.go: the pinned Muse orchestrator tab and its
// global event feed. Presentation only: indicator states arrive
// pre-derived from the typed presence facts (deriveMuseState), so
// this file maps state words to glyphs and rows — it never infers
// liveness itself. Payload text is worker-influenced, so it is
// sanitized like live output before rendering.

// museTabLabel renders the pinned first rail row: glyph, name, and
// state word, color-wrapped like worker harness backgrounds.
func museTabLabel(selected bool, state string, innerW int) string {
	glyph, color := museGlyph(state)
	prefix := "  "
	if selected {
		prefix = "> "
	}
	plain := pad(truncW(sanitizeLine(prefix+glyph+" META.AI · "+state), innerW), innerW)
	if color != "" {
		return color + plain + "\x1b[0m"
	}
	return plain
}

func museGlyph(state string) (glyph, color string) {
	switch state {
	case MuseActive:
		return "◉", "\x1b[32;1m"
	case MuseActiveEstimated:
		// Labeled estimate, not verified activity: same
		// active glyph in non-bold so traffic-only presence
		// reads as weaker than a verified turn.
		return "◉", "\x1b[32m"
	case MuseConnected:
		return "◍", "\x1b[36m"
	case MuseDelegated:
		return "●", "\x1b[32m"
	case MuseAttention:
		return "▲", "\x1b[33m"
	case MuseError:
		return "✖", "\x1b[31m"
	case MuseDisconnected:
		return "◌", "\x1b[2m"
	default: // MuseIdle and unknown
		return "○", "\x1b[2m"
	}
}

// firstPayloadLine reduces an event payload to its first line for
// the feed row. Payloads are exact recorded content, truncated at
// the ledger bound; the row shows the head, never a reconstruction.
func firstPayloadLine(p string) string {
	if i := strings.Index(p, "\n"); i >= 0 {
		return strings.TrimSpace(p[:i])
	}
	return strings.TrimSpace(p)
}

// shortEventTime renders the event timestamp as clock time; unparseable
// stamps are shown raw rather than hidden.
func shortEventTime(ts string) string {
	if t, err := parseOrchTime(ts); err == nil {
		return time.Unix(0, t).Format("15:04:05")
	}
	if len(ts) >= 19 {
		return ts[11:19]
	}
	return ts
}

// renderFeedRow formats one orchestration event as a sanitized,
// width-bound feed row.
func renderFeedRow(e OrchEvent, cursor string, innerW int) string {
	from := e.OriginPrincipal
	if from == "" {
		from = "metis"
	}
	to := e.TargetAgent
	if to == "" {
		to = "metis"
	}
	head := firstPayloadLine(e.Payload)
	line := fmt.Sprintf("%s%s %s → %s | %s | %s | %s", cursor,
		shortEventTime(e.Created), from, to, e.EventType, head, e.Disposition)
	return sanitizeLine(pad(truncW(line, innerW), innerW))
}

// renderControllersRow formats one presence lease as an honest
// recently-seen row: verified transport identity plus source and
// age. A lease proves recent authentication, never a persistent
// live connection, and the label says so.
func renderControllersRow(p musePresence, now int64) string {
	age := now - p.LastSeen
	if age < 0 {
		age = 0
	}
	seen := fmt.Sprintf("%ds", age/int64(time.Second))
	if age >= int64(time.Minute) {
		seen = fmt.Sprintf("%dm", age/int64(time.Minute))
	}
	who := p.Principal
	if who == "" {
		who = "unknown"
	}
	src := p.Source
	if src == "" {
		src = "unknown-transport"
	}
	return fmt.Sprintf("  ● %s via %s · recently seen %s ago (not a live connection)", who, src, seen)
}

// renderMusePane renders the orchestration feed box that replaces
// the terminal and Git panes on the Muse tab.
func renderMusePane(m model, lay renderLayout) []string {
	feedTitle := fmt.Sprintf("Meta.ai Orchestrator · activity feed, not a chat · %s — open: %d · blocked: %d (up/down=event, enter=jump to worker)",
		m.museState, m.museOpen, m.museBlocked)
	var head []string
	now := time.Now().UnixNano()
	if len(m.controllers) == 0 {
		head = append(head, pad(truncW(sanitizeLine("Controllers: (none recently seen)"), lay.mainW-4), lay.mainW-4))
	} else {
		head = append(head, pad(truncW(sanitizeLine("Controllers (verified identities, recently seen — not live connections):"), lay.mainW-4), lay.mainW-4))
		for _, c := range m.controllers {
			head = append(head, pad(truncW(sanitizeLine(renderControllersRow(c, now)), lay.mainW-4), lay.mainW-4))
		}
	}
	head = append(head, pad(truncW(sanitizeLine("—"), lay.mainW-4), lay.mainW-4))
	var rows []string
	feedCursor := m.nav.FeedCursor
	for i, e := range m.feed {
		cursor := "  "
		if i == feedCursor {
			cursor = "> "
		}
		rows = append(rows, renderFeedRow(e, cursor, lay.mainW-4))
	}
	if len(rows) == 0 {
		rows = []string{"(no controller events yet)", "", "This tab is a dedicated MCP controller slot.", "It does not mirror a Meta.ai web conversation."}
	}
	// Window the event rows around the cursor instead of trimming
	// to the bottom: the selected event stays visible while
	// scrolling through retained history.
	avail := lay.feedH - 2 - len(head)
	if avail < 1 {
		avail = 1
	}
	start := 0
	if len(rows) > avail {
		start = feedCursor - avail/2
		if start < 0 {
			start = 0
		}
		if start+avail > len(rows) {
			start = len(rows) - avail
		}
		if feedCursor < start {
			start = feedCursor
		}
		if feedCursor >= start+avail {
			start = feedCursor - avail + 1
		}
	}
	feedLines := append(head, rows[start:min(start+avail, len(rows))]...)
	return boxLines(feedTitle, feedLines, lay.mainW, lay.contentH)
}

package main

import (
	"strings"
	"testing"
)

// Switching worker tabs shows the newly selected session's output
// and never the previous session's: the pane is bound to the
// selection, not to last-rendered content.
func TestWorkerPaneSwitchShowsSelectedSession(t *testing.T) {
	lay := renderLayout{mainW: 80, contentH: 10}
	m := model{live: []string{"w1-line"}, paneSel: "w1"}
	first := strings.Join(renderLivePane(m, "w1", lay), "\n")
	if !strings.Contains(first, "w1-line") {
		t.Fatalf("selected session output missing:\n%s", first)
	}
	m.live = []string{"w2-line"}
	m.paneSel = "w2"
	second := strings.Join(renderLivePane(m, "w2", lay), "\n")
	if !strings.Contains(second, "w2-line") {
		t.Fatalf("switched session output missing:\n%s", second)
	}
	if strings.Contains(second, "w1-line") {
		t.Fatalf("previous session output leaked across the switch:\n%s", second)
	}
	// A stale paneSel never paints another session's lines.
	m.paneSel = "w1"
	stale := strings.Join(renderLivePane(m, "w2", lay), "\n")
	if strings.Contains(stale, "w2-line") {
		t.Fatalf("mismatched selection must not paint:\n%s", stale)
	}
}

// Ordinal rail labels shift as the population changes; the session
// name is the persistent instance identity and must ride along,
// exactly once, at exact width.
func TestRailLabelCarriesPersistentInstanceID(t *testing.T) {
	row := tabLabel("> ", "●", "qw3",
		tabDetail{Label: "Locus · 01", Harness: "muse", Model: "opus"}, 40)
	for _, want := range []string{"Locus", "01", "opus", "qw3"} {
		if !strings.Contains(row, want) {
			t.Fatalf("rail row missing %q: %q", want, row)
		}
	}
	if strWidth(row) != 40 {
		t.Fatalf("rail row width %d, want 40: %q", strWidth(row), row)
	}
	plain := tabLabel("  ", "○", "qw3", tabDetail{}, 40)
	if c := strings.Count(plain, "qw3"); c != 1 {
		t.Fatalf("ungrouped label must name the session exactly once, got %d: %q", c, plain)
	}
}

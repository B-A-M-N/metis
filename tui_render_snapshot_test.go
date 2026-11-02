package main

// tui_render_snapshot_test.go: deterministic render snapshots for the
// decomposed View path (tui_render.go composition over tui_text,
// tui_layout, tui_render_tabs, tui_render_muse, tui_render_worker,
// tui_render_chrome). All inputs are canned model projections: no DB,
// no processes, no blocking effects.

import (
	"fmt"
	"strings"
	"testing"
)

func snapshotModel() model {
	m := newModel("")
	m.nav.OnMuse = false
	m.names = []string{"w0", "w1"}
	m.running = map[string]bool{"w0": true, "w1": false}
	m.nav.Selected = 0
	m.tabInfo = map[string]tabDetail{
		"w0": {Harness: "muse", Model: "opus", Title: "fix login"},
		"w1": {Harness: "codex", Model: "gpt", Title: "migrate db"},
	}
	m.entry = Session{Name: "w0", Workdir: "/w/w0", Harness: "muse", Model: "opus"}
	m.live = []string{"hello", "world"}
	m.gitLines = []string{"* main abc123"}
	m.events = []string{"ev-a", "ev-b"}
	// The fixture models an installed w0 projection: the pane
	// binding matches the viewed session, exactly as applyRefresh
	// records it. Tests that move the cursor without reinstalling
	// exercise the unbound path deliberately.
	m.paneSel = "w0"
	m.width, m.height = 100, 30
	return m
}

func viewLines(t *testing.T, m model) []string {
	t.Helper()
	out := m.View()
	if out == "" {
		t.Fatal("View must not render empty")
	}
	return strings.Split(strings.TrimSuffix(out, "\n"), "\n")
}

// The frame invariant: every row is exactly one terminal width wide
// and the frame holds header + content + three footer rows.
func TestRenderFrameGeometrySnapshot(t *testing.T) {
	for _, wh := range [][2]int{{80, 24}, {100, 30}, {160, 50}} {
		m := snapshotModel()
		m.width, m.height = wh[0], wh[1]
		rows := viewLines(t, m)
		if len(rows) != wh[1]-1 {
			t.Fatalf("%dx%d: got %d rows, want %d", wh[0], wh[1], len(rows), wh[1]-1)
		}
		for i, r := range rows {
			if got := strWidth(r); got != wh[0] {
				t.Fatalf("%dx%d row %d width %d, want %d: %q", wh[0], wh[1], i, got, wh[0], r)
			}
		}
	}
}

func TestLayoutGeometrySnapshot(t *testing.T) {
	lay := computeLayout(100, 30)
	if lay.leftW != 25 || lay.rightW != 75 {
		t.Fatalf("pane widths wrong: %+v", lay)
	}
	if lay.liveH != 14 || lay.gitH != 11 {
		t.Fatalf("pane heights wrong: %+v", lay)
	}
	if lay.tabH != 23 || lay.workerH != 22 {
		t.Fatalf("rail window wrong: %+v", lay)
	}
	if lay.contentH != 25 || lay.feedH != 25 {
		t.Fatalf("stack heights wrong: %+v", lay)
	}
	// Narrow floor: left rail clamps to 24 columns.
	small := computeLayout(80, 24)
	if small.leftW != 24 || small.rightW != 56 {
		t.Fatalf("narrow clamp wrong: %+v", small)
	}
	if small.liveH != 10 || small.gitH != 9 {
		t.Fatalf("narrow heights wrong: %+v", small)
	}
}

func TestViewNarrowTerminalSnapshot(t *testing.T) {
	for _, wh := range [][2]int{{60, 20}, {79, 30}, {100, 23}} {
		m := snapshotModel()
		m.width, m.height = wh[0], wh[1]
		if got := m.View(); !strings.Contains(got, "Terminal too small") {
			t.Fatalf("%dx%d must refuse: %q", wh[0], wh[1], got)
		}
	}
}

func TestRenderLongSessionListSnapshot(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false
	for i := 0; i < 30; i++ {
		m.names = append(m.names, fmt.Sprintf("worker-%02d", i))
	}
	if m.running == nil {
		m.running = map[string]bool{}
	}
	for _, n := range m.names {
		m.running[n] = true
	}
	m.nav.Selected = len(m.names) - 1
	m.width, m.height = 100, 30
	first := m.View()
	if first != m.View() {
		t.Fatal("long-list View must be deterministic")
	}
	// The selected worker stays visible: the rail scrolls to it and
	// the pinned Muse row stays on top.
	sel := m.selName()
	want := tabLabel("> ", "●", sel, tabDetail{}, 23)
	if !strings.Contains(first, want) {
		t.Fatalf("selected worker row must stay visible: %q", want)
	}
	if !strings.Contains(first, "MUSE") {
		t.Fatal("pinned Muse row must stay visible on long lists")
	}
	// The scroll window clips: not every worker row fits.
	if strings.Contains(first, "worker-00") {
		t.Fatalf("rail window must clip the head of a 30-session list")
	}
}

func TestRenderUnicodeWidthsSnapshot(t *testing.T) {
	m := snapshotModel()
	m.tabInfo["w0"] = tabDetail{Harness: "muse", Model: "opus", Title: "登录修复日本語"}
	m.live = []string{"hello 世界", "emoji 👍 here", "カタカナ output"}
	m.width, m.height = 100, 30
	first := m.View()
	if first != m.View() {
		t.Fatal("unicode View must be deterministic")
	}
	for i, r := range strings.Split(strings.TrimSuffix(first, "\n"), "\n") {
		if got := strWidth(r); got != 100 {
			t.Fatalf("unicode row %d width %d, want 100: %q", i, got, r)
		}
	}
	if !strings.Contains(first, "登录") {
		t.Fatal("CJK title must survive the rail")
	}
}

func TestRenderANSISanitizedSnapshot(t *testing.T) {
	m := snapshotModel()
	m.live = []string{"\x1b[2Jwiped", "\x1b]0;title\x07osc-leak", "ok\x00line", "plain"}
	m.events = []string{"\x1b[31mevil-ev"}
	m.width, m.height = 100, 30
	out := m.View()
	for _, leak := range []string{"\x1b[2J", "\x1b]0;", "\x1b[31mevil-ev"} {
		if strings.Contains(out, leak) {
			t.Fatalf("worker escape leaked into View: %q", leak)
		}
	}
	if !strings.Contains(out, "wiped") || !strings.Contains(out, "osc-leak") || !strings.Contains(out, "okline") {
		t.Fatalf("sanitized text must stay readable:\n%s", out)
	}
}

func TestRenderScrollOffsetsLiveSnapshot(t *testing.T) {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, "line-"+string(rune('A'+i)))
	}
	top := snapshotModel()
	top.live = lines
	top.scroll = 0
	topOut := top.View()
	mid := snapshotModel()
	mid.live = lines
	mid.scroll = 5
	midOut := mid.View()
	if topOut == midOut {
		t.Fatal("scroll must move the live tail")
	}
	// Tail shows the newest lines; scrolled view hides them.
	if !strings.Contains(topOut, "line-T") {
		t.Fatal("tail must show the newest live line")
	}
	if strings.Contains(midOut, "line-T") {
		t.Fatal("scrolled view must hide the newest live line")
	}
	if !strings.Contains(midOut, "line-O") {
		t.Fatalf("scrolled view must show the windowed line:\n%s", midOut)
	}
}

// A selection change must never present the previous session's
// panes as the new session's: entry metadata, live output, events,
// and Git state are bound to the session they were collected for.
// Until the new session's own projection arrives, its panes stay
// empty rather than showing another worker's data (P0).
func TestRenderSelectionChangeHidesPreviousSession(t *testing.T) {
	m := snapshotModel()
	// Viewing w1 while every cached pane still belongs to w0: the
	// w0 collection predates the switch (or w1's own collection is
	// still in flight). The rail still lists w0 (legitimate), but no
	// w1 pane chrome or body may carry w0's data.
	m.nav.Selected = 1
	out := m.View()
	lines := strings.Split(out, "\n")
	var header, liveTitle, gitTitle, eventsLine string
	for _, l := range lines {
		switch {
		case strings.Contains(l, "Metis"):
			header = l
		case strings.Contains(l, "Live:"):
			liveTitle = l
		case strings.Contains(l, "Git:"):
			gitTitle = l
		case strings.Contains(l, "Events:"):
			eventsLine = l
		}
	}
	if !strings.Contains(header, "session=w1") || strings.Contains(header, "muse") || strings.Contains(header, "opus") {
		t.Fatalf("header must bind metadata to w1, got %q", header)
	}
	if !strings.Contains(liveTitle, "Live: w1 (-/-") {
		t.Fatalf("live pane must not carry w0's harness/model, got %q", liveTitle)
	}
	if !strings.Contains(gitTitle, "Git:") || strings.Contains(gitTitle, "/w/w0") {
		t.Fatalf("git pane must not carry w0's workdir, got %q", gitTitle)
	}
	if !strings.Contains(eventsLine, "Events: (no events)") {
		t.Fatalf("events line must not carry w0's events, got %q", eventsLine)
	}
	for _, leaked := range []string{"hello", "world", "* main abc123", "/w/w0", "ev-a", "ev-b"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("viewing w1 must not show w0's pane bodies, leaked %q", leaked)
		}
	}
}

func TestRenderStaleProjectionSnapshot(t *testing.T) {
	m := snapshotModel()
	m.status = staleProjectionPrefix + " (gen 3, want 4); showing last-known data"
	out := m.View()
	if !strings.Contains(out, staleProjectionPrefix) {
		t.Fatal("stale marker must reach the screen verbatim")
	}
	if out != m.View() {
		t.Fatal("stale View must be deterministic")
	}
}

func TestMuseFeedRenderSnapshot(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = true
	m.names = []string{"w0"}
	m.running = map[string]bool{"w0": true}
	m.feed = []OrchEvent{
		{Seq: 1, EventType: OrchDelegationDispatched, OriginPrincipal: "metis", TargetAgent: "w0", Payload: "do it\nsecond line", Disposition: "open", Created: "2026-01-01T00:00:00Z"},
		{Seq: 2, EventType: OrchWorkerReported, OriginPrincipal: "w0", TargetAgent: "metis", Payload: "\x1b[31mdone ✓", Disposition: "closed", Created: "not-a-timestamp"},
	}
	m.museState = MuseActive
	m.museOpen, m.museBlocked = 1, 0
	m.nav.FeedCursor = 1
	m.width, m.height = 100, 30
	first := m.View()
	if first != m.View() {
		t.Fatal("muse feed View must be deterministic")
	}
	if !strings.Contains(first, "MUSE Orchestrator") {
		t.Fatal("muse tab must show the feed title")
	}
	if !strings.Contains(first, "do it") {
		t.Fatal("feed head line must render")
	}
	if strings.Contains(first, "second line") {
		t.Fatal("feed row must show only the payload head")
	}
	if strings.Contains(first, "\x1b[31mdone") {
		t.Fatal("feed payload escape must be sanitized")
	}
	if !strings.Contains(first, "✓") {
		t.Fatal("feed payload glyph must survive sanitization")
	}
	if !strings.Contains(first, "not-a-timestamp") {
		t.Fatal("unparseable stamps must show raw, never hidden")
	}
}

func TestTabLabelRenderSnapshot(t *testing.T) {
	// Harness escapes can never become rail styling: the label is
	// sanitized before the background wraps it.
	evil := tabLabel("> ", "●", "w0",
		tabDetail{Harness: "\x1b[31m", Model: "m\x1b[0m", Title: "t"}, 40)
	if strings.Contains(evil, "\x1b[31m") {
		t.Fatalf("harness escape leaked: %q", evil)
	}
	if strWidth(evil) != 40 {
		t.Fatalf("evil label width %d, want 40", strWidth(evil))
	}
	plain := tabLabel("  ", "○", "w0", tabDetail{}, 10)
	if strWidth(plain) != 10 {
		t.Fatalf("plain label width %d, want 10", strWidth(plain))
	}
}

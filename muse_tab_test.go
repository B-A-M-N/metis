package main

// muse_tab_test.go: the pinned Muse orchestrator tab.
//
// The Muse tab is the durable communication feed, not a worker
// terminal: pinned first, selected by default, fed only by committed
// events, and Enter jumps to the event's worker instead of attaching.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "metis/internal/tui"
)

// museKeyMsg builds nav key messages the way the existing TUI tests do.
func museKeyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
	}
}

func museTestSetup(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "musetok")
	seedOwnedSession(t, "w", "musetok")
}

// Default opening selection is the Muse tab, with no worker attached.
func TestMuseTabSelectedByDefault(t *testing.T) {
	m := newModel("")
	if !m.nav.OnMuse {
		t.Fatal("default selection must be the Muse tab")
	}
	if _, ok := m.attachTarget(); ok {
		t.Fatal("Muse tab must never yield an attach target")
	}
}

// An explicit session target selects that worker instead.
func TestMuseTabExplicitTargetSelectsWorker(t *testing.T) {
	m := newModel("w0")
	if m.nav.OnMuse {
		t.Fatal("explicit target must not open on the Muse tab")
	}
}

// The Muse row is always first and never scrolls out, even with a
// worker list far longer than the rail viewport.
func TestMuseTabPinnedFirst(t *testing.T) {
	museTestSetup(t)
	m := newModel("")
	m.width, m.height = 120, 30
	for i := 0; i < 60; i++ {
		name := "w" + string(rune('a'+i/26)) + string(rune('0'+i%10))
		m.names = append(m.names, name)
	}
	m.nav.Selected = 59
	m.running = map[string]bool{}
	m.followTabs()
	v := m.View()
	// Compare rail rows, not raw text: the top bar legitimately
	// names the preserved worker selection.
	selectedName := m.selName()
	museLine := -1
	for i, line := range strings.Split(v, "\n") {
		if strings.Contains(line, "CONTROLLER") && museLine < 0 {
			museLine = i
		}
	}
	if museLine < 0 {
		t.Fatal("CONTROLLER row missing from rail")
	}
	// Every rendered worker row must come after the CONTROLLER row.
	prefix := selectedName[:2]
	for i, line := range strings.Split(v, "\n") {
		if strings.Contains(line, prefix) && strings.Contains(line, "○") && i < museLine {
			t.Fatalf("worker row above the pinned CONTROLLER row: %q", line)
		}
	}
	if !strings.Contains(v, selectedName) {
		t.Fatal("selected worker scrolled out of view")
	}
}

// A genuine delegation through dispatch appears in the feed with its
// exact destination and message; a worker reply appears with correct
// source attribution.
func TestMuseFeedShowsDelegationAndReply(t *testing.T) {
	museTestSetup(t)
	tk, err := createTask("run-muse", "w", "inspect authentication and identify failure causes", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("muse")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := recordOrchEvent(OrchEvent{
		RunID: "run-muse", OriginPrincipal: "w",
		TargetAgent: "muse", TaskID: tk.ID,
		EventType: OrchWorkerReported, Payload: "digest=abc source=test",
		Disposition: "filed",
	}); err != nil {
		t.Fatal(err)
	}
	m := newModel("")
	m.width, m.height = 120, 30
	m.refreshCtl.Gen = 1
	m.applyRefresh(collectRefreshData(liveRefreshSource{}, 1, "", false))
	if len(m.feed) < 2 {
		t.Fatalf("feed must carry delegation + reply, got %d", len(m.feed))
	}
	v := m.View()
	if !strings.Contains(v, "MCP Controller") {
		t.Fatal("Muse pane missing when Muse tab selected")
	}
	if !strings.Contains(v, "inspect authentication") {
		t.Fatal("delegation instruction missing from feed")
	}
	// Attribution: delegation is muse → w; the reply names w.
	if !strings.Contains(v, "muse → w | delegation.dispatched") {
		t.Fatalf("delegation attribution wrong:\n%s", v)
	}
	found := false
	for _, line := range strings.Split(v, "\n") {
		if strings.Contains(line, "worker.reported") && strings.Contains(line, "w") {
			found = true
		}
	}
	if !found {
		t.Fatal("worker reply missing from feed")
	}
}

// Enter on the Muse tab jumps to the event's worker; it never
// attaches a terminal.
func TestMuseEnterJumpsToWorker(t *testing.T) {
	museTestSetup(t)
	m := newModel("")
	m.width, m.height = 120, 30
	m.names = []string{"w"}
	m.nav.Selected = 0
	m.running = map[string]bool{"w": true}
	m.feed = []OrchEvent{{TargetAgent: "w", EventType: OrchDelegationDispatched}}
	m.nav.FeedCursor = 0
	updated, _ := m.Update(museKeyMsg("enter"))
	got := updated.(model)
	if got.nav.OnMuse {
		t.Fatal("enter on Muse must leave the Muse tab")
	}
	if got.nav.Mode == tuistate.ModeAttach {
		t.Fatal("enter on Muse must never attach a terminal")
	}
	if len(got.names) == 0 || got.selName() != "w" {
		t.Fatal("enter must jump to the event's worker")
	}
}

// Up from the top worker enters Muse; down past the oldest feed
// event returns to the worker list. Worker selection survives.
func TestMuseTabNavigationCycle(t *testing.T) {
	m := newModel("")
	m.width, m.height = 120, 30
	m.names = []string{"a", "b"}
	m.nav.Selected = 0
	m.feed = []OrchEvent{
		{EventType: OrchDelegationDispatched, TargetAgent: "a"},
		{EventType: OrchWorkerReported, TargetAgent: "b"},
	}
	m.nav.FeedCursor = 1
	updated, _ := m.Update(museKeyMsg("up"))
	got := updated.(model)
	if !got.nav.OnMuse {
		t.Fatal("up from top worker must enter the Muse tab")
	}
	if got.nav.FeedCursor != 0 {
		t.Fatal("up on Muse must move the feed cursor")
	}
	updated2, _ := got.Update(museKeyMsg("down"))
	got2 := updated2.(model)
	if !got2.nav.OnMuse || got2.nav.FeedCursor != 1 {
		t.Fatal("down on Muse must move the feed cursor")
	}
	updated3, _ := got2.Update(museKeyMsg("down"))
	got3 := updated3.(model)
	if got3.nav.OnMuse {
		t.Fatal("down past the oldest feed event must return to workers")
	}
	if len(got3.names) == 0 || got3.selName() != "a" {
		t.Fatal("worker selection must survive the Muse excursion")
	}
}

// Stale generations refuse the feed exactly like session panes:
// last-known data is never presented as live.
func TestMuseFeedStaleRefused(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.feed = []OrchEvent{{EventType: OrchMessageSent, Payload: "old"}}
	d := refreshData{sessionsOk: true, gen: 4, feed: []OrchEvent{{EventType: OrchMessageSent, Payload: "new"}}}
	m.applyRefresh(d)
	if len(m.feed) != 1 || m.feed[0].Payload != "old" {
		t.Fatal("stale generation must not replace the feed")
	}
	if !strings.Contains(m.status, "stale") {
		t.Fatalf("stale refusal needs a marker, got %q", m.status)
	}
}

// The controller tab is provider-agnostic: no provider name is
// hardcoded, and the pane shows verified controller identities plus
// the activity feed (targeted workers and dispositions).
func TestControllerTabProviderAgnostic(t *testing.T) {
	museTestSetup(t)
	m := newModel("")
	m.width, m.height = 120, 30
	m.controllers = []musePresence{{Principal: "op-1", Source: "mcp-stdio", LastSeen: 0}}
	m.museState = MuseConnected
	v := m.View()
	if strings.Contains(v, "Meta.ai") || strings.Contains(v, "META.AI") || strings.Contains(v, "Meta AI") {
		t.Fatalf("controller surface must not hardcode a provider name:\n%s", v)
	}
	if !strings.Contains(v, "CONTROLLER") {
		t.Fatalf("controller rail row missing:\n%s", v)
	}
	if !strings.Contains(v, "op-1") || !strings.Contains(v, "mcp-stdio") {
		t.Fatalf("verified controller identity must be displayed:\n%s", v)
	}
	if !strings.Contains(v, "MCP Controller") {
		t.Fatalf("controller feed title missing:\n%s", v)
	}
	// Header identity is the selected controller surface, not a provider.
	sel, _, mode := selectedSummary(m)
	if sel != "Controller" || mode != "MCP" {
		t.Fatalf("selectedSummary = (%q, %q), want (Controller, MCP)", sel, mode)
	}
}

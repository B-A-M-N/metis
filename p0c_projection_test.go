package main

// P0-C: projection freshness + retention ownership.
//
// Freshness: collected data is bound to a refresh generation and an
// explicit selected-session ID. applyRefresh refuses mismatched
// projections — last-known panes stay, but only with a clear stale
// marker — clears the Git pane when the selection moves without
// fresh Git data, and never disturbs independent per-session failure
// state. Retention: the stream-cap sweep is owned by
// MaintainRetention (no dashboard required); the persisted rotation
// counter wires discarded history into gap reporting on reopen.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A projection from an older generation must not install: panes keep
// last-known data with a stale marker, and per-session failure state
// survives the refusal.
func TestRefreshStaleGenerationRefused(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.entry = Session{Name: "a"}
	m.live = []string{"old-live"}
	m.gitLines = []string{"git-a"}
	m.events = []string{"ev"}
	m.logGen = map[string]int{"a": 1}
	m.inputPending = map[string]string{"a": "in-1 rejected: lane closed"}
	m.status = "ready"
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen: 4, sel: "a",
		names:   []string{"x"},
		running: map[string]bool{"x": true},
		entry:   Session{Name: "x"},
		live:    []string{"new-live"},
		logGen:  map[string]int{"x": 9},
	}})
	got := updated.(model)
	if len(got.names) != 1 || got.names[0] != "a" {
		t.Fatalf("stale gen installed names: %v", got.names)
	}
	if len(got.live) != 1 || got.live[0] != "old-live" {
		t.Fatalf("stale gen installed live pane: %v", got.live)
	}
	if len(got.gitLines) != 1 || got.gitLines[0] != "git-a" {
		t.Fatalf("stale gen disturbed git pane: %v", got.gitLines)
	}
	if got.entry.Name != "a" {
		t.Fatalf("stale gen installed entry: %+v", got.entry)
	}
	if got.logGen["a"] != 1 {
		t.Fatalf("stale gen moved generation watermark: %v", got.logGen)
	}
	if !strings.Contains(got.status, staleProjectionPrefix) ||
		!strings.Contains(got.status, "last-known") {
		t.Fatalf("refusal needs a stale marker, got %q", got.status)
	}
	if got.inputPending["a"] != "in-1 rejected: lane closed" {
		t.Fatalf("refusal disturbed per-session failure state: %v", got.inputPending)
	}
}

// Data collected for another session must not paint this session's
// panes; a matching projection for the viewed session applies and
// clears the stale marker.
func TestRefreshStaleSessionRefused(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a", "b"}
	m.nav.Selected = 1 // viewing "b"
	m.running = map[string]bool{"a": true, "b": true}
	m.entry = Session{Name: "b"}
	m.live = []string{"live-b"}
	m.gitLines = []string{"git-b"}
	m.status = "ready"
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "a",
		names:   []string{"a", "b"},
		running: map[string]bool{"a": true, "b": true},
		entry:   Session{Name: "a"},
		live:    []string{"live-a"},
	}})
	got := updated.(model)
	if got.entry.Name != "b" || len(got.live) != 1 || got.live[0] != "live-b" {
		t.Fatalf("cross-session projection painted wrong panes: %+v %v", got.entry, got.live)
	}
	if !strings.Contains(got.status, staleProjectionPrefix) ||
		!strings.Contains(got.status, `"a"`) {
		t.Fatalf("refusal must name the mismatched session, got %q", got.status)
	}
	// The projection actually collected for "b" installs cleanly and
	// clears only our own stale marker.
	updated2, _ := got.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "b",
		names:   []string{"a", "b"},
		running: map[string]bool{"a": true, "b": true},
		entry:   Session{Name: "b"},
		live:    []string{"live-b2"},
		hasGit:  true,
		git:     []string{"git-b2"},
	}})
	got2 := updated2.(model)
	if len(got2.live) != 1 || got2.live[0] != "live-b2" {
		t.Fatalf("matching projection did not apply: %v", got2.live)
	}
	if strings.Contains(got2.status, staleProjectionPrefix) {
		t.Fatalf("fresh apply left a stale marker behind: %q", got2.status)
	}
}

// When the selection moves without fresh Git data, the pane must be
// cleared — never show the previous session's Git state as current.
// Same-session refreshes without Git data keep the cached pane.
func TestGitPaneClearedOnSessionChange(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.gitLines = []string{"git-a"}
	m.live = []string{"live-a"}
	// "a" left the fleet; now viewing "b" with no Git collected.
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "a",
		names:   []string{"b"},
		running: map[string]bool{"b": true},
	}})
	got := updated.(model)
	if len(got.gitLines) != 0 {
		t.Fatalf("stale git pane survived session change: %v", got.gitLines)
	}
	// Same session, no Git in this tick: the cached pane survives.
	m2 := newModel("")
	m2.refreshCtl.Gen = 5
	m2.names = []string{"a"}
	m2.nav.Selected = 0
	m2.gitLines = []string{"git-a"}
	updated2, _ := m2.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "a",
		names:   []string{"a"},
		running: map[string]bool{"a": true},
	}})
	if got2 := updated2.(model); len(got2.gitLines) != 1 || got2.gitLines[0] != "git-a" {
		t.Fatalf("same-session refresh clobbered cached git pane: %v", got2.gitLines)
	}
	// Fresh Git data always replaces, even across a session change.
	m3 := newModel("")
	m3.refreshCtl.Gen = 6
	m3.names = []string{"a", "b"}
	m3.nav.Selected = 1 // navigated to "b"; selection already moved
	m3.gitLines = []string{"git-a"}
	updated3, _ := m3.Update(refreshMsg{data: refreshData{
		gen: 6, sel: "b",
		names:   []string{"a", "b"},
		running: map[string]bool{"a": true, "b": true},
		entry:   Session{Name: "b"},
		hasGit:  true,
		git:     []string{"git-b"},
	}})
	if got3 := updated3.(model); len(got3.gitLines) != 1 || got3.gitLines[0] != "git-b" {
		t.Fatalf("fresh git data did not replace pane: %v", got3.gitLines)
	}
}

// A high-output worker with no dashboard refreshing must still get
// bounded retention: the owner caps the stream without any refresh.
func TestRetentionBoundedWithoutRefresh(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "p0c")
	seedOwnedSession(t, "w", "p0c")
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'x'
	}
	copy(big[len(big)-6:], "tail\n")
	if err := os.MkdirAll(filepath.Dir(streamPath("w")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath("w"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	// No collectRefreshData anywhere: headless execution only.
	if n := MaintainRetention(); n != 1 {
		t.Fatalf("owner should rotate exactly one stream, rotated %d", n)
	}
	fi, err := os.Stat(streamPath("w"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > tmuxStreamCap {
		t.Fatalf("stream file must be capped at %d, got %d", tmuxStreamCap, fi.Size())
	}
	data, err := os.ReadFile(streamPath("w"))
	if err != nil || !strings.Contains(string(data), "older output discarded") {
		t.Fatalf("rotation must leave a gap marker: %v %q", err, string(data))
	}
	if streamGeneration("w") != 1 {
		t.Fatalf("rotation must bump the generation counter, got %d", streamGeneration("w"))
	}
	// A second over-cap burst bumps the counter again.
	if err := os.WriteFile(streamPath("w"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	MaintainRetention()
	if streamGeneration("w") != 2 {
		t.Fatalf("second rotation must reach gen 2, got %d", streamGeneration("w"))
	}
}

// Reopening after headless rotations identifies the discarded
// history: the persisted counter surfaces through the projection and
// the model reports the gap instead of a silent discontinuity.
func TestSweepReportsGenerationGap(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "p0c2")
	seedOwnedSession(t, "w", "p0c2")
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.MkdirAll(filepath.Dir(streamPath("w")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath("w"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	MaintainRetention() // headless rotation while dashboard closed
	// Reopen: a fresh model collects for the session.
	m := newModel("")
	m.refreshCtl.Gen = 1
	d := collectRefreshData(liveRefreshSource{}, 1, "w", false)
	if d.logGen["w"] != 1 {
		t.Fatalf("projection must carry the persisted generation, got %v", d.logGen)
	}
	if !strings.Contains(strings.Join(d.live, "\n"), "older output discarded") {
		t.Fatalf("reopened live pane must show the gap marker: %v", d.live)
	}
	m.names = []string{"w"}
	m.nav.Selected = 0
	m.running = map[string]bool{"w": true}
	m.logGen = map[string]int{"w": 0}
	m.applyRefresh(d)
	if !strings.Contains(m.status, "rotated") {
		t.Fatalf("generation jump must report rotation, got %q", m.status)
	}
}

// Per-session failure state is independent: refusals and session
// changes never drop another session's parked outcome.
func TestTabPerSessionFailurePreserved(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a", "b"}
	m.nav.Selected = 0
	m.inputPending = map[string]string{
		"a": "in-1 rejected: lane closed",
		"b": "in-2 queued",
	}
	// Stale generation refused.
	updated, _ := m.Update(refreshMsg{data: refreshData{gen: 4, sel: "a"}})
	got := updated.(model)
	// Session change without git.
	updated2, _ := got.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "a", names: []string{"b"},
	}})
	got2 := updated2.(model)
	if got2.inputPending["a"] != "in-1 rejected: lane closed" ||
		got2.inputPending["b"] != "in-2 queued" {
		t.Fatalf("per-session failures lost: %v", got2.inputPending)
	}
}

// The stale marker is owned state, not parsed text: a fresh apply
// clears only the marker it installed. A foreign status that merely
// shares the marker's prefix is someone else's message and must
// survive the install.
func TestRefreshApplyPreservesForeignStatus(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.status = staleProjectionPrefix + " (forged by another writer)"
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen: 5, sel: "a",
		names:   []string{"a"},
		running: map[string]bool{"a": true},
		entry:   Session{Name: "a"},
		live:    []string{"live-a"},
	}})
	got := updated.(model)
	if got.status != staleProjectionPrefix+" (forged by another writer)" {
		t.Fatalf("fresh apply clobbered a status it did not install, got %q", got.status)
	}
}

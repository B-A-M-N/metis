package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	tuistate "metis/internal/tui"
)

func TestTUIListsNativeOnlySession(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	ws, err := canonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := claimLogicalSession(logicalSession{Name: "qa", Backend: TargetMuseNative,
		Workspace: ws, Provider: "echo", Profile: "muse-fi"}); err != nil || !claimed {
		t.Fatalf("seed native session: claimed=%v err=%v", claimed, err)
	}
	snap := (liveRefreshSource{}).snapshots(context.Background(), "native:qa", false)
	if !snap.sessionsOk {
		t.Fatal("native-only session listing should be trustworthy")
	}
	d := assembleRefresh(snap, 1, "native:qa")
	if len(d.names) != 1 || d.names[0] != "native:qa" {
		t.Fatalf("native identity missing from TUI rail: %v", d.names)
	}
	id := d.identities["native:qa"]
	if id.Canonical != "qa" || id.Backend != TargetMuseNative || id.Lifecycle != logicalActive {
		t.Fatalf("native rail identity lost backend scope: %+v", id)
	}
	if !strings.Contains(strings.Join(d.live, "\n"), "Provider: echo") {
		t.Fatalf("native activity projection missing durable facts: %v", d.live)
	}
}

func TestTUISelectNativeDoesNotAttachTmux(t *testing.T) {
	m := model{names: []string{"native:qa"}, running: map[string]bool{"native:qa": true},
		identities: map[string]sessionIdentity{"native:qa": {Canonical: "qa", Backend: TargetMuseNative}},
		nav:        tuistate.Nav{Selected: 0}}
	if cmd := m.requestAttach(); cmd != nil {
		t.Fatal("native selection must not create an attach command")
	}
	if got := m.canonicalSession("native:qa"); got != "qa" {
		t.Fatalf("display identity must resolve to canonical native name, got %q", got)
	}
	if m.nav.Mode == tuistate.ModeAttach || !strings.Contains(m.status, "terminal attach unavailable") {
		t.Fatalf("native selection entered terminal attach: mode=%s status=%q", m.nav.Mode, m.status)
	}
}

func TestTUIRejectsTerminalBytesForNative(t *testing.T) {
	before := liveInputSenderCount()
	m := model{names: []string{"native:qa"}, running: map[string]bool{"native:qa": true},
		identities: map[string]sessionIdentity{"native:qa": {Canonical: "qa", Backend: TargetMuseNative}},
		nav:        tuistate.Nav{Selected: 0, Mode: tuistate.ModeAttach}}
	got, cmd := m.attachKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cmd != nil || got.nav.Mode == tuistate.ModeAttach || !strings.Contains(got.status, "no terminal input") {
		t.Fatalf("native terminal byte was not refused: mode=%s status=%q cmd=%v", got.nav.Mode, got.status, cmd != nil)
	}
	if after := liveInputSenderCount(); after != before {
		t.Fatalf("native keypress created input sender: before=%d after=%d", before, after)
	}
}

func TestFeedJumpResolvesNativeIdentity(t *testing.T) {
	m := model{names: []string{"native:qa"},
		identities: map[string]sessionIdentity{"native:qa": {Canonical: "qa", Backend: TargetMuseNative}},
		feed:       []OrchEvent{{TargetAgent: "qa"}}, nav: tuistate.Nav{OnMuse: true, FeedCursor: 0}}
	m.jumpToFeedTarget()
	if got := m.selName(); got != "native:qa" {
		t.Fatalf("feed target must resolve to backend-qualified native identity, got %q", got)
	}
}

func TestNativeProjectionCannotLeakAcrossTabs(t *testing.T) {
	m := model{names: []string{"native:qa", "native:qb"},
		identities: map[string]sessionIdentity{
			"native:qa": {Canonical: "qa", Backend: TargetMuseNative},
			"native:qb": {Canonical: "qb", Backend: TargetMuseNative},
		},
		paneSel: "native:qa", live: []string{"Provider: qa-only"}}
	if got := boundLines(m, "native:qb", m.live); len(got) != 0 {
		t.Fatalf("qa activity leaked into qb pane: %v", got)
	}
}

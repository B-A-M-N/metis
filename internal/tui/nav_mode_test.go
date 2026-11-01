package tui

import "testing"

// Mode transitions are pure Nav reducers: the shell routes keys to
// them, never assigns the field. Zero value is ModeNav, matching
// the historical flat-model default (empty string fell through to
// nav handling).

func TestNavModeTransitions(t *testing.T) {
	var n Nav
	if n.Mode != "" {
		t.Fatalf("zero Nav must carry no explicit mode, got %q", n.Mode)
	}
	n = n.ToNav()
	if n.Mode != ModeNav {
		t.Fatalf("ToNav must record nav mode, got %q", n.Mode)
	}
	n = n.EnterInput()
	if n.Mode != ModeInput {
		t.Fatalf("EnterInput must set input mode, got %q", n.Mode)
	}
	n = n.EnterAttach()
	if n.Mode != ModeAttach {
		t.Fatalf("EnterAttach must set attach mode, got %q", n.Mode)
	}
	n = n.ToNav()
	if n.Mode != ModeNav {
		t.Fatalf("ToNav must restore nav mode, got %q", n.Mode)
	}
}

func TestNavModePreservesCursor(t *testing.T) {
	n := Nav{Selected: 3, OnMuse: true, FeedCursor: 2, TabOffset: 1}
	moved := n.EnterAttach()
	if moved.Selected != 3 || !moved.OnMuse || moved.FeedCursor != 2 || moved.TabOffset != 1 {
		t.Fatalf("mode transition must preserve navigation state, got %+v", moved)
	}
}

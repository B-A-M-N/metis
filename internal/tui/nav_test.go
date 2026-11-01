package tui

import "testing"

// Navigation reducers are pure: no tmux, registry, database, or
// event loop. Every transition below runs against row counts and
// names only.

func TestNavClampPinsInvalidCursor(t *testing.T) {
	if got := (Nav{Selected: 5}).Clamp(0).Selected; got != -1 {
		t.Fatalf("empty list must report no selection, got %d", got)
	}
	if got := (Nav{Selected: 5}).Clamp(2).Selected; got != 1 {
		t.Fatalf("over-range must pin to last row, got %d", got)
	}
	if got := (Nav{Selected: -3}).Clamp(2).Selected; got != 0 {
		t.Fatalf("negative must pin to first row, got %d", got)
	}
	if got := (Nav{Selected: 1}).Clamp(3).Selected; got != 1 {
		t.Fatalf("valid cursor must survive, got %d", got)
	}
}

func TestNavRestoreBindsByIdentity(t *testing.T) {
	names := []string{"a", "b", "c"}
	if got := (Nav{Selected: 2}).Restore(names, "b"); got.Selected != 1 {
		t.Fatalf("restore must follow the name, got %d", got.Selected)
	}
	if got := (Nav{Selected: 2}).Restore(names, "gone"); got.Selected != 0 {
		t.Fatalf("vanished name must fall back to top, got %d", got.Selected)
	}
	if got := (Nav{}).Restore(nil, "a"); got.Selected != -1 {
		t.Fatalf("empty list must report no selection, got %d", got.Selected)
	}
}

func TestNavUpDownWithMuseTab(t *testing.T) {
	n := Nav{Selected: 1}
	up, reselect := n.Up(4)
	if !reselect || up.Selected != 0 {
		t.Fatalf("up must move and reselect: %+v %v", up, reselect)
	}
	top, reselect := up.Up(4)
	if reselect || !top.OnMuse || top.FeedCursor != 3 {
		t.Fatalf("up from top must enter Muse tab at feed tail: %+v %v", top, reselect)
	}
	moved, _ := top.Up(4)
	if moved.FeedCursor != 2 {
		t.Fatalf("up on Muse tab must move feed cursor, got %+v", moved)
	}
	down, _ := moved.Down(3, 4)
	if down.FeedCursor != 3 {
		t.Fatalf("down on Muse tab must move feed cursor, got %+v", down)
	}
	back, reselect := down.Down(3, 4)
	if !reselect || back.OnMuse || back.Selected != 0 {
		t.Fatalf("down past oldest feed must return to workers: %+v %v", back, reselect)
	}
	end, reselect := Nav{Selected: 2}.Down(3, 4)
	if reselect || end.Selected != 2 {
		t.Fatalf("down at bottom must hold: %+v %v", end, reselect)
	}
}

func TestNavJumpToFeedTarget(t *testing.T) {
	names := []string{"a", "b"}
	n, ok := (Nav{OnMuse: true}).JumpToFeedTarget(names, "b")
	if !ok || n.OnMuse || n.Selected != 1 {
		t.Fatalf("listed target must select: %+v %v", n, ok)
	}
	if _, ok := (Nav{OnMuse: true}).JumpToFeedTarget(names, "ghost"); ok {
		t.Fatal("unlisted target must not move the cursor")
	}
	if _, ok := (Nav{OnMuse: true}).JumpToFeedTarget(names, ""); ok {
		t.Fatal("empty target must not move the cursor")
	}
}

func TestNavFeedCursorFollowsData(t *testing.T) {
	n := (Nav{FeedCursor: 9}).ClampFeedCursor(3)
	if n.FeedCursor != 2 {
		t.Fatalf("cursor must pin to new tail, got %d", n.FeedCursor)
	}
	n = (Nav{FeedCursor: -1}).ClampFeedCursor(3)
	if n.FeedCursor != 2 {
		t.Fatalf("negative cursor with data must go to tail, got %d", n.FeedCursor)
	}
	n = (Nav{FeedCursor: 0}).ClampFeedCursor(0)
	if n.FeedCursor != -1 {
		t.Fatalf("empty feed must report no cursor, got %d", n.FeedCursor)
	}
}

func TestNavFollowTabsScrollsFleet(t *testing.T) {
	n := (Nav{Selected: 10, TabOffset: 0}).FollowTabs(5)
	if n.TabOffset != 6 {
		t.Fatalf("viewport must anchor on cursor, got %+v", n)
	}
	n = (Nav{Selected: 2, TabOffset: 6}).FollowTabs(5)
	if n.TabOffset != 2 {
		t.Fatalf("viewport must scroll back up, got %+v", n)
	}
	off, end := (Nav{Selected: 1, TabOffset: 0}).RailWindow(9, 5)
	if off != 0 || end != 5 {
		t.Fatalf("window must cover head rows, got %d:%d", off, end)
	}
}

package main

import (
	tuistate "metis/internal/tui"
	"strings"
	"testing"
	"time"
)

// The Muse tab names each recently-seen controller by verified
// transport identity — and labels the lease honestly as recently
// seen, never as a live connection.
func TestRenderMusePaneListsControllers(t *testing.T) {
	now := time.Now().UnixNano()
	m := model{
		museState: MuseConnected,
		controllers: []musePresence{
			{Principal: "jwt:chatgpt-1", Source: "mcp-http", LastSeen: now - 12*int64(time.Second)},
			{Principal: "local-operator", Source: "mcp-local", LastSeen: now - 3*int64(time.Minute)},
		},
	}
	lay := renderLayout{mainW: 100, contentH: 20, feedH: 12}
	joined := strings.Join(renderMusePane(m, lay), "\n")
	if !strings.Contains(joined, "jwt:chatgpt-1") || !strings.Contains(joined, "local-operator") {
		t.Fatalf("controller identities missing:\n%s", joined)
	}
	if !strings.Contains(joined, "not a live connection") && !strings.Contains(joined, "not live connections") {
		t.Fatalf("presence must be labeled recently-seen, not live:\n%s", joined)
	}
	if strings.Contains(joined, "no controllers") {
		t.Fatalf("populated leases must not show the empty text:\n%s", joined)
	}
}

// TestRenderMusePaneWindowsAroundCursor proves retained history
// stays navigable: with the cursor on the first event the window
// shows the head (not the bottom-trimmed tail), and with the
// cursor on the last event the tail stays visible with its mark.
func TestRenderMusePaneWindowsAroundCursor(t *testing.T) {
	mkfeed := func() []OrchEvent {
		var out []OrchEvent
		for i := 0; i < 10; i++ {
			out = append(out, OrchEvent{
				EventType: OrchMessageSent, RequestID: "r",
				Payload: strings.Repeat("x", 4) + string(rune('a'+i)),
				Created: "2026-11-02T00:00:00Z",
			})
		}
		return out
	}
	m := model{museState: MuseConnected, feed: mkfeed()}
	lay := renderLayout{mainW: 100, contentH: 20, feedH: 8}
	m.nav = tuistate.Nav{FeedCursor: 0}
	top := strings.Join(renderMusePane(m, lay), "\n")
	if !strings.Contains(top, "> ") {
		t.Fatalf("cursor mark must be visible at head:\n%s", top)
	}
	if !strings.Contains(top, "xxxxa") || strings.Contains(top, "xxxxj") {
		t.Fatalf("cursor-at-head must window the head, not the tail:\n%s", top)
	}
	m.nav = tuistate.Nav{FeedCursor: 9}
	bottom := strings.Join(renderMusePane(m, lay), "\n")
	if !strings.Contains(bottom, "> ") {
		t.Fatalf("cursor mark must be visible at tail:\n%s", bottom)
	}
	if strings.Contains(bottom, "xxxxa") || !strings.Contains(bottom, "xxxxj") {
		t.Fatalf("cursor-at-tail must window the tail:\n%s", bottom)
	}
}

func TestRenderMusePaneEmptyControllersHonest(t *testing.T) {
	m := model{museState: MuseDisconnected}
	lay := renderLayout{mainW: 100, contentH: 20, feedH: 12}
	joined := strings.Join(renderMusePane(m, lay), "\n")
	if !strings.Contains(joined, "none recently seen") {
		t.Fatalf("empty leases must say so explicitly:\n%s", joined)
	}
}

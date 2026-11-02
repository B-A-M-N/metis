package main

// Phase 4 slice 1: single input writer on every supported entry
// point (TD-061, TD-063, CB-03/CB-11). PTY input already holds an
// exclusive TTL lease; the tmux direct-input path (TUI attach keys,
// guidance --direct) historically sent keystrokes with ownership
// verified but no writer exclusion — two controllers could type
// into the same session concurrently.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "metis/internal/tui"
)

func swapControllerID(t *testing.T, id string) {
	t.Helper()
	old := controllerIDValue
	controllerIDValue = id
	t.Cleanup(func() { controllerIDValue = old })
}

// While controller A holds the tmux input channel, controller B's
// keystrokes and direct guidance MUST be refused — never interleaved
// (TD-061). The holder re-acquiring keeps working.
func TestTmuxInputSingleWriter(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4a")
	seedOwnedSession(t, "w", "p4a")
	swapControllerID(t, "ctl-A")
	if err := sendTerminalInput("w", "echo from A", "Enter"); err != nil {
		t.Fatalf("holder input refused: %v", err)
	}
	swapControllerID(t, "ctl-B")
	if err := sendTerminalInput("w", "echo from B"); err == nil {
		t.Fatal("rival input during a held lease must be refused (TD-061)")
	} else if !strings.Contains(err.Error(), "held by") {
		t.Fatalf("refusal must name the holder: %v", err)
	}
	if err := injectGuidance("w", "rival guidance", true); err == nil {
		t.Fatal("rival direct guidance during a held lease must be refused")
	}
	swapControllerID(t, "ctl-A")
	if err := sendTerminalInput("w", "echo again"); err != nil {
		t.Fatalf("holder re-acquire refused: %v", err)
	}
}

// Guidance submission MUST NOT run on the Bubble Tea update path
// (TD-102/103): pressing enter returns a command that submits off
// the UI goroutine. Nothing is written synchronously — the status
// only reports the outcome when the result message arrives.
func TestGuidanceSubmitOffUpdatePath(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4c")
	seedOwnedSession(t, "w", "p4c")
	m := newModel("")
	m.names = []string{"w"}
	m.nav.Selected = 0
	m.running = map[string]bool{"w": true}
	m.nav = m.nav.EnterInput()
	m.guideCtl.Buf = "review the diff"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("guidance submit must return an async command, not run inline (TD-102/103)")
	}
	if gl, _ := listGuidance("w"); len(gl) != 0 {
		t.Fatalf("guidance must not be written on the update path, got %d rows", len(gl))
	}
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeNav || got.guideCtl.Buf != "" {
		t.Fatalf("input must still clear immediately, got mode=%q input=%q", got.nav.Mode, got.guideCtl.Buf)
	}
	msg := cmd()
	updated2, _ := got.Update(msg)
	if gl, _ := listGuidance("w"); len(gl) != 1 {
		t.Fatalf("executing the command must submit exactly once, got %d rows", len(gl))
	}
	if st := updated2.(model).status; !strings.Contains(st, "guidance ") || !strings.Contains(st, "sent (file)") {
		t.Fatalf("result message must report the send, got %q", st)
	}
}

// Two rapid enters MUST NOT produce two submissions: while one
// submission is in flight the second enter is held, preserving
// per-session delivery order without blocking the event loop.
func TestGuidanceDoubleEnterSerializes(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4d")
	seedOwnedSession(t, "w", "p4d")
	m := newModel("")
	m.names = []string{"w"}
	m.nav.Selected = 0
	m.running = map[string]bool{"w": true}
	m.nav = m.nav.EnterInput()
	m.guideCtl.Buf = "first"
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("first enter must return the submit command")
	}
	got := updated.(model)
	got.nav = got.nav.EnterInput()
	got.guideCtl.Buf = "second"
	updated2, cmd2 := got.Update(tea.KeyMsg{Type: tea.KeyEnter})
	_ = updated2
	if cmd2 != nil {
		if msg := cmd2(); msg != nil {
			_, _ = got.Update(msg)
		}
		if gl, _ := listGuidance("w"); len(gl) != 0 {
			t.Fatalf("second enter during in-flight submit must not submit, got %d rows", len(gl))
		}
	}
	if msg := cmd(); msg != nil {
		if u, _ := got.Update(msg); u != nil {
			got = u.(model)
		}
	}
	if gl, _ := listGuidance("w"); len(gl) != 1 {
		t.Fatalf("exactly one submission must land, got %d rows", len(gl))
	}
}

// A high-output tmux worker MUST NOT grow its stream file without
// bound (TD-066/TD-106, CB-10): collection caps the file every
// refresh, so one animated worker cannot exhaust disk or starve the
// fleet's observation.
func TestStreamCapEnforced(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4e")
	seedOwnedSession(t, "w", "p4e")
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
	collectRefreshData(liveRefreshSource{}, 1, "w", false)
	fi, err := os.Stat(streamPath("w"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() > 1<<20 {
		t.Fatalf("stream file must be capped at 1MB, got %d bytes", fi.Size())
	}
	ls, ok := streamTail("w", 10)
	if !ok || len(ls) == 0 {
		t.Fatalf("rotated stream must stay readable: %v %v", ls, ok)
	}
}

// The tail reader returns the last lines in order, even across many
// lines of output.
func TestStreamTailLastLines(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4f")
	seedOwnedSession(t, "w", "p4f")
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "line-%04d\n", i)
	}
	if err := os.MkdirAll(filepath.Dir(streamPath("w")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath("w"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ls, ok := streamTail("w", 500)
	if !ok || len(ls) != 500 {
		t.Fatalf("expected last 500 lines, got %d ok=%v", len(ls), ok)
	}
	if ls[0] != "line-2500" || ls[499] != "line-2999" {
		t.Fatalf("tail order wrong: first=%q last=%q", ls[0], ls[499])
	}
}

// Expiry frees the tmux channel: a disconnected holder (TD-062,
// CB-11) MUST NOT permanently lock input for another authorized
// controller.
func TestTmuxLeaseExpiryFreesChannel(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "p4b")
	seedOwnedSession(t, "w", "p4b")
	swapControllerID(t, "ctl-A")
	if err := sendTerminalInput("w", "echo hold"); err != nil {
		t.Fatalf("holder input refused: %v", err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE input_leases SET expires = 0 WHERE owner = 'ctl-A'`); err != nil {
		t.Fatalf("expiring the lease (red before the channel exists): %v", err)
	}
	swapControllerID(t, "ctl-B")
	if err := sendTerminalInput("w", "echo recovered"); err != nil {
		t.Fatalf("expired lease must free the channel: %v", err)
	}
}

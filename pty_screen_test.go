package main

// pty_screen_test.go: PTY log screen rendering (failing-first).
//
// Raw PTY bytes overwrite via CR/erase/cursor moves; splitting on
// '\n' stacks every fragment as a line. These tests pin that the
// TUI pane renders the final screen text for the common cases.

import (
	"os"
	"strings"
	"testing"
)

func screenWant(t *testing.T, in string, want []string) {
	t.Helper()
	got := renderPTYScreen([]byte(in), 0, 500)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("renderPTYScreen(%q) = %q, want %q", in, got, want)
	}
}

func TestPTYScreenCROverwrite(t *testing.T) {
	screenWant(t, "abc\rX\r\nnext\r\n", []string{"Xbc", "next"})
	// Progress bar: same line rewritten, only the last frame shows.
	screenWant(t, "downloading 10%\rdownloading 99%\r\n", []string{"downloading 99%"})
}

func TestPTYScreenEraseLine(t *testing.T) {
	screenWant(t, "abcdef\r\x1b[Kxy\r\n", []string{"xy"})
	screenWant(t, "abcdef\x1b[2D\x1b[K\r\n", []string{"abcd"})
	screenWant(t, "abcdef\x1b[2D\x1b[1K\r\n", []string{"     f"})
}

func TestPTYScreenCursorMoves(t *testing.T) {
	screenWant(t, "ab\bX\r\n", []string{"aX"})
	screenWant(t, "line1\r\nline2\x1b[AZ\r\n", []string{"line1Z", "line2"})
	screenWant(t, "hello\x1b[3DXY\r\n", []string{"heXYo"})
	screenWant(t, "hello\x1b[1;1H>> \r\n", []string{">> lo"})
}

func TestPTYScreenStripsStylesAndOSC(t *testing.T) {
	screenWant(t, "\x1b[31mred\x1b[0m plain\r\n", []string{"red plain"})
	screenWant(t, "\x1b]0;title\x07body\r\n", []string{"body"})
	screenWant(t, "\x1b]8;;http://x\x07link\x1b]8;;\x07\r\n", []string{"link"})
}

func TestPTYScreenEraseDisplay(t *testing.T) {
	screenWant(t, "one\r\ntwo\r\n\x1b[H\x1b[2Jthree\r\n", []string{"three"})
}

func TestPTYScreenPlainLinesUnchanged(t *testing.T) {
	screenWant(t, "a\r\nb\r\nc\r\n", []string{"a", "b", "c"})
	screenWant(t, "", nil)
}

func TestPTYScreenTabAndWide(t *testing.T) {
	got := renderPTYScreen([]byte("a\tb\r\n"), 0, 10)
	if len(got) != 1 || !strings.HasPrefix(got[0], "a") || !strings.HasSuffix(got[0], "b") {
		t.Fatalf("tab must advance, got %q", got)
	}
}

func TestPTYKeyBytesExtended(t *testing.T) {
	cases := map[string]string{
		"home": "\x1b[H", "end": "\x1b[F", "insert": "\x1b[2~", "delete": "\x1b[3~",
		"ctrl+a": "\x01", "ctrl+e": "\x05", "ctrl+k": "\x0b", "ctrl+u": "\x15",
		"ctrl+w": "\x17", "ctrl+l": "\x0c", "ctrl+d": "\x04", "ctrl+z": "\x1a",
		"ctrl+left": "\x1b[1;5D", "ctrl+right": "\x1b[1;5C", "shift+tab": "\x1b[Z",
	}
	for key, want := range cases {
		got, send := ptyKeyBytes(key, nil)
		if !send || got != want {
			t.Fatalf("ptyKeyBytes(%q) = %q,%v want %q,true", key, got, send, want)
		}
	}
	if got, send := ptyKeyBytes("alt+b", []rune{'b'}); !send || got != "\x1bb" {
		t.Fatalf("alt+b must be ESC+b, got %q %v", got, send)
	}
	// Local keys stay local.
	for _, local := range []string{"esc", "pgup", "pgdn"} {
		if _, send := ptyKeyBytes(local, nil); send {
			t.Fatalf("local key %q must not be sent", local)
		}
	}
}

func TestPTYResizeCmdGates(t *testing.T) {
	m := newModel("")
	m.width, m.height = 120, 30
	m.names = []string{"w"}
	m.running = map[string]bool{"w": true, "pty:p": true}
	m.nav.OnMuse = false
	m.nav.Selected = 0
	if cmd := m.ptyResizeCmd("w"); cmd != nil {
		t.Fatal("tmux session must not get a PTY resize cmd")
	}
	if cmd := m.ptyResizeCmd("pty:p"); cmd == nil {
		t.Fatal("running PTY session must get a resize cmd")
	}
	if cmd := m.ptyResizeCmd("pty:dead"); cmd != nil {
		t.Fatal("non-running PTY session must not get a resize cmd")
	}
	m.width = 40
	if cmd := m.ptyResizeCmd("pty:p"); cmd != nil {
		t.Fatal("too-small dashboard must not resize")
	}
}

// Adversarial bounds: malformed/untrusted worker output must not
// force unbounded allocation (Q-pass items 1).
func TestPTYScreenAdversarialBounds(t *testing.T) {
	// Huge CSI cursor jumps clamp instead of allocating.
	got := renderPTYScreen([]byte("x\x1b[999999999C"+"y\x1b[999999999B"+"z\r\n"), 0, 500)
	for _, l := range got {
		if len([]rune(l)) > ptyScreenMaxCol+2 {
			t.Fatalf("line exceeded col bound: %d runes", len([]rune(l)))
		}
	}
	if len(got) > ptyScreenMaxLines {
		t.Fatalf("lines exceeded bound: %d", len(got))
	}
	// Absurd CSI row/column positioning.
	got = renderPTYScreen([]byte("\x1b[999999999;999999999Hhi\r\n"), 0, 500)
	if len(got) > ptyScreenMaxLines {
		t.Fatalf("positioning exceeded line bound: %d", len(got))
	}
	// Tab flood cannot expand one line without bound.
	var sb strings.Builder
	sb.WriteString("a")
	for i := 0; i < 100000; i++ {
		sb.WriteByte('\t')
	}
	sb.WriteString("b\r\n")
	got = renderPTYScreen([]byte(sb.String()), 0, 500)
	if len(got) != 1 || len([]rune(got[0])) > ptyScreenMaxCol+2 {
		t.Fatalf("tab flood must stay bounded, got %d lines / %d runes", len(got), len([]rune(got[0])))
	}
	// Malformed CSI (no final byte, huge digits) is inert.
	got = renderPTYScreen([]byte("ab\x1b[999999999999999999999999999999"), 0, 500)
	if len(got) == 0 || got[0] != "ab" {
		t.Fatalf("malformed CSI must be inert tail, got %q", got)
	}
	// Truncated log segment without any newline is real output,
	// never "(no output yet)" (Q-pass item 4).
	big := strings.Repeat("x", 300000)
	ls, _ := boundedPTYScreenLines(ptyMeta{LogPath: writeTempLog(t, big)}, 500)
	if len(ls) == 0 || strings.Contains(ls[0], "no output") {
		t.Fatalf("newline-free truncated log must render, got %q", ls)
	}
	if len([]rune(ls[0])) > ptyScreenMaxCol+2 {
		t.Fatalf("single huge line must stay bounded: %d runes", len([]rune(ls[0])))
	}
}

func writeTempLog(t *testing.T, content string) string {
	t.Helper()
	path := t.TempDir() + "/pty.log"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Resize ordering (Q-pass item 3): sequence gate is latest-wins.
func TestResizeSeqLatestWins(t *testing.T) {
	name := "seqtest-" + t.Name()
	if !resizeSeqAdvance(name, 1) {
		t.Fatal("seq 1 must advance")
	}
	if !resizeSeqAdvance(name, 3) {
		t.Fatal("seq 3 must advance")
	}
	if resizeSeqAdvance(name, 2) {
		t.Fatal("seq 2 after 3 must be superseded")
	}
	if resizeSeqAdvance(name, 3) {
		t.Fatal("duplicate seq 3 must be superseded")
	}
	if !resizeSeqAdvance(name, 4) {
		t.Fatal("seq 4 must advance")
	}
	// Independent sessions do not share sequence state.
	if !resizeSeqAdvance(name+"-other", 1) {
		t.Fatal("other session seq 1 must advance independently")
	}
}

// Live resizeLatest: applies in order, superseded seq is a no-op,
// and a stopped session refuses instead of resizing a replacement.
func TestPTYResizeLatestLive(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	dir := t.TempDir()
	if err := ptyStart("rlive", dir, "cat", "demo", ""); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ptyStop("rlive") }()
	if err := ptyResizeLatest("rlive", 1, 30, 100); err != nil {
		t.Fatalf("seq1 resize: %v", err)
	}
	// Superseded duplicate: no-op success, worker untouched.
	if err := ptyResizeLatest("rlive", 1, 99, 99); err != nil {
		t.Fatalf("superseded resize must be a no-op success: %v", err)
	}
	if err := ptyResizeLatest("rlive", 2, 40, 120); err != nil {
		t.Fatalf("seq2 resize: %v", err)
	}
	if err := ptyStop("rlive"); err != nil {
		t.Fatal(err)
	}
	// Stopped session: newer seq still refuses (not running).
	if err := ptyResizeLatest("rlive", 3, 24, 80); err == nil {
		t.Fatal("resize on stopped session must refuse")
	}
}

// Model sequences increase monotonically per session across calls
// (pointer receiver persistence), and stale results are ignored.
func TestPTYResizeCmdSequencesIncrease(t *testing.T) {
	m := newModel("")
	m.width, m.height = 120, 30
	m.names = []string{"pty:p"}
	m.running = map[string]bool{"pty:p": true}
	m.nav.OnMuse = false
	if cmd := m.ptyResizeCmd("pty:p"); cmd == nil {
		t.Fatal("running PTY must get a resize cmd")
	}
	if cmd := m.ptyResizeCmd("pty:p"); cmd == nil {
		t.Fatal("second resize must get a cmd")
	}
	if m.ptyResizeSeq["pty:p"] != 2 {
		t.Fatalf("issued sequences must persist on the model, got %d", m.ptyResizeSeq["pty:p"])
	}
	// Stale result (seq 1 after seq 2 issued) must not clobber status.
	m.status = "keep"
	next, _ := m.Update(ptyResizeResultMsg{session: "pty:p", seq: 1, err: nil})
	if next.(model).status != "keep" {
		t.Fatal("stale resize result must be ignored")
	}
}

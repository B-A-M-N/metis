package termview

import (
	"strings"
	"testing"
)

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
	}{
		{'a', 1},
		{0, 0},
		{'\n', 0},
		{0x7f, 0},
		{0x0301, 0}, // combining acute
		{0x4e2d, 2}, // CJK unified
		{0xac00, 2}, // Hangul syllable
		{0xff21, 2}, // fullwidth A
		{'—', 1},    // em dash stays narrow
	}
	for _, c := range cases {
		if got := RuneWidth(c.r); got != c.want {
			t.Errorf("RuneWidth(%U) = %d, want %d", c.r, got, c.want)
		}
	}
}

func TestWidth(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"ab", 2},
		{"日本", 4},
		{"a\u0301", 1},            // base + combining mark
		{"\x1b[31mred\x1b[0m", 3}, // sequences occupy nothing
		{"\x1b]8;;http://x\x07link\x1b]8;;\x07", 4}, // OSC hyperlink wrapped
	}
	for _, c := range cases {
		if got := Width(c.s); got != c.want {
			t.Errorf("Width(%q) = %d, want %d", c.s, got, c.want)
		}
	}
}

func TestSeqLen(t *testing.T) {
	if got := SeqLen("\x1b[31m"); got != 5 {
		t.Errorf("CSI SeqLen = %d, want 5", got)
	}
	if got := SeqLen("\x1b]8;;u\x07rest"); got != 7 {
		t.Errorf("OSC SeqLen = %d, want 7", got)
	}
	if got := SeqLen("ab"); got != 0 {
		t.Errorf("plain SeqLen = %d, want 0", got)
	}
	if got := SeqLen("\x1b"); got != 0 {
		t.Errorf("lone ESC SeqLen = %d, want 0", got)
	}
}

func TestTrunc(t *testing.T) {
	if got := Trunc("日本語", 5); Width(got) != 4 {
		t.Fatalf("wide trunc columns = %d, want 4 (%q)", Width(got), got)
	}
	if got := Trunc("abcdef", 3); got != "abc" {
		t.Fatalf("Trunc = %q, want abc", got)
	}
	if got := Trunc("ab", 0); got != "" {
		t.Fatalf("Trunc w=0 = %q, want empty", got)
	}
	// A styled span cut mid-span keeps its escape bytes.
	styled := "\x1b[31mred\x1b[0m"
	if got := Trunc(styled, 2); Width(got) != 2 || !strings.Contains(got, "\x1b") {
		t.Fatalf("styled trunc = %q", got)
	}
	// Truncation never splits a rune: the cut lands on a boundary.
	if got := Trunc("日本語", 3); got != "日" {
		t.Fatalf("rune-boundary trunc = %q, want %q", got, "日")
	}
}

func TestPadExactWidth(t *testing.T) {
	inputs := []string{"ab", "日本", "\x1b[31mred\x1b[0m", "", "— title —"}
	for _, in := range inputs {
		got := Pad(in, 10)
		if w := Width(got); w != 10 {
			t.Errorf("Pad(%q, 10) columns = %d, want 10 (%q)", in, w, got)
		}
	}
	// A cut inside a styled span is reset, never bleeding.
	styled := "\x1b[31mred text here\x1b[0m"
	got := Pad(styled, 5)
	if !strings.HasSuffix(got, "\x1b[0m"+strings.Repeat(" ", 5-Width(Trunc(styled, 5)))) {
		t.Fatalf("styled pad leaks: %q", got)
	}
	if strings.Count(got, "\x1b[0m") < 1 {
		t.Fatalf("styled pad missing reset: %q", got)
	}
}

func TestSanitizeLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"\x1b[31mred\x1b[0m", "red"},
		{"a\x1bb", "ab"},       // lone ESC dies with the regex; b survives
		{"a\x00b\x7fc", "abc"}, // C0/DEL stripped
		{"plain", "plain"},
		{"\x1b]8;;http://x\x07link\x1b]8;;\x07", "link"},
		{"\tindent", "indent"}, // tab is a control: stripped, not expanded
	}
	for _, c := range cases {
		if got := SanitizeLine(c.in); got != c.want {
			t.Errorf("SanitizeLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

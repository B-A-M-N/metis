// Package termview owns pure terminal text measurement, ANSI/width
// handling, and sanitization for the TUI.
//
// The functions are pure over already-collected strings: no
// controller queries, no tmux, no filesystem, no database, no Bubble
// Tea. Callers pass presentation data (strings, widths) in and get
// rows out; the package never reaches back into package main (it
// cannot: internal packages are imported by main, never the reverse)
// and imports only the standard library.
package termview

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// RuneWidth is a compact wcwidth: combining marks and controls take
// no columns, East Asian wide/fullwidth runes take two, everything
// else takes one.
func RuneWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0
	case r >= 0x300 && r <= 0x36f || r >= 0x1dc0 && r <= 0x1dff ||
		r >= 0x20d0 && r <= 0x20ff || r >= 0xfe20 && r <= 0xfe2f:
		return 0
	case r >= 0x1100 && r <= 0x115f || r >= 0x2e80 && r <= 0xa4cf ||
		r >= 0xac00 && r <= 0xd7a3 || r >= 0xf900 && r <= 0xfaff ||
		r >= 0xfe30 && r <= 0xfe4f || r >= 0xff00 && r <= 0xff60 ||
		r >= 0xffe0 && r <= 0xffe6 || r >= 0x20000 && r <= 0x3fffd:
		return 2
	}
	return 1
}

// SeqLen returns the byte length of the escape sequence starting at
// s[0] (which must be ESC), or 0 when s holds no sequence there.
func SeqLen(s string) int {
	if len(s) < 2 || s[0] != 0x1b {
		return 0
	}
	switch s[1] {
	case '[':
		// CSI: parameter/intermediate bytes, then a final byte.
		for i := 2; i < len(s); i++ {
			if s[i] >= 0x40 && s[i] <= 0x7e {
				return i + 1
			}
		}
		return len(s)
	case ']':
		// OSC: terminated by BEL or ESC\.
		for i := 2; i < len(s); i++ {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
		}
		return len(s)
	case '(', ')', '#', '%':
		if len(s) >= 3 {
			return 3
		}
		return len(s)
	default:
		return 2
	}
}

// Width counts terminal columns: escape sequences occupy none.
func Width(s string) int {
	w := 0
	for i := 0; i < len(s); {
		if n := SeqLen(s[i:]); n > 0 {
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		w += RuneWidth(r)
		i += size
	}
	return w
}

// Trunc cuts s to at most w columns without splitting a rune or an
// escape sequence; sequences ride along for free.
func Trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cols := 0
	for i := 0; i < len(s); {
		if n := SeqLen(s[i:]); n > 0 {
			// Sequences occupy no columns and always ride along,
			// so a reset is never stranded outside the cut.
			i += n
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if rw := RuneWidth(r); cols+rw > w {
			return s[:i]
		} else {
			cols += rw
		}
		i += size
	}
	return s
}

// Pad extends s to exactly w columns, truncating first. A cut inside
// a styled span is reset so the style cannot bleed into the border.
func Pad(s string, w int) string {
	s = Trunc(s, w)
	if strings.Contains(s, "\x1b") && !strings.HasSuffix(s, "\x1b[0m") {
		// A cut inside a styled span must not bleed into the border.
		s += "\x1b[0m"
	}
	return s + strings.Repeat(" ", w-Width(s))
}

// ANSIRe matches CSI, OSC, and legacy escape sequences. Terminal
// output is untrusted: the overview renders it sanitized so a worker
// cannot repaint the dashboard or hide content with control codes.
var ANSIRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]|\x1b\\][^\x07]*(?:\x07|\x1b\\\\)|\x1b[()][0-9A-B]|\x1b[MX78cH=>]")

// SanitizeLine strips terminal control sequences and C0 controls so
// worker output is inert text in the overview.
func SanitizeLine(s string) string {
	s = ANSIRe.ReplaceAllString(s, "")
	var b strings.Builder
	for _, r := range s {
		if r < 32 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

package main

import (
	"tui-dash/internal/termview"
)

// tui_text.go: thin adapter over internal/termview.
//
// All measurement, ANSI/width, and sanitization logic lives in
// internal/termview as dependency-free pure functions. The lowercase
// names below are kept so existing call sites (tui_layout.go,
// tui_render_*.go, graph_panel.go) and tests compile unchanged; new
// code should import termview directly.

func runeWidth(r rune) int { return termview.RuneWidth(r) }

// ansiSeqLen returns the byte length of the escape sequence starting
// at s[0] (which must be ESC), or 0 when s holds no sequence there.
func ansiSeqLen(s string) int { return termview.SeqLen(s) }

// strWidth counts terminal columns: escape sequences occupy none.
func strWidth(s string) int { return termview.Width(s) }

// truncW cuts s to at most w columns without splitting a rune or an
// escape sequence; sequences ride along for free.
func truncW(s string, w int) string { return termview.Trunc(s, w) }

func pad(s string, w int) string { return termview.Pad(s, w) }

// ansiRe matches CSI, OSC, and legacy escape sequences. Terminal
// output is untrusted: the overview renders it sanitized so a worker
// cannot repaint the dashboard or hide content with control codes.
var ansiRe = termview.ANSIRe

// sanitizeLine strips terminal control sequences and C0 controls so
// worker output is inert text in the overview.
func sanitizeLine(s string) string { return termview.SanitizeLine(s) }

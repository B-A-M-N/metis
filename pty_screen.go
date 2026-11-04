package main

// pty_screen.go: PTY log screen rendering for the TUI pane.
//
// The raw PTY log is a byte stream, not lines: progress bars,
// spinners, and prompts overwrite via CR / backspace / CSI cursor
// and erase sequences. Splitting that stream on '\n' and sanitizing
// each fragment (the old ptyLogLines path) paints every overwrite
// as a new line — unreadable for exactly the interactive workers
// the pane exists for.
//
// renderPTYScreen replays the tail bytes through a small screen
// model: a scrollback of rune lines plus a cursor, handling CR, LF,
// BS, TAB, CSI cursor movement (A B C D E F G H ` d), erase in line
// (K) and erase in display (J), and stripping SGR/OSC/other escape
// sequences as inert. Styles are not reproduced (text pane), and
// alternate-screen full-screen applications are NOT emulated —
// their bytes render linearly, exactly as before, and the pane
// title says so. This fixes overwrite rendering; it does not claim
// to be a VT emulator. cols<=0 means no wrapping (lines grow to
// their natural width and are truncated by the pane renderer).

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

const ptyScreenMaxLines = 2000

// Bounds for untrusted worker output (Q-pass): a hostile or buggy
// worker must not be able to force unbounded allocation through
// the screen model. Cursor movement is clamped to the retained
// grid, line length is capped, and CSI parameters are already
// bounded by Atoi failure -> 0 plus these clamps. Input itself is
// the bounded 128 KiB tail; these caps bound the *expansion*
// (tab/cursor padding) of that input.
const (
	ptyScreenMaxCol = 2048
)

type ptyScreen struct {
	lines [][]rune
	row   int
	col   int
	cols  int
}

func (s *ptyScreen) ensureRow() {
	// Clamp the cursor into the retained grid before growing it:
	// an unclamped CSI row jump (e.g. ESC[999999999B) would
	// otherwise loop-append millions of nil lines.
	if s.row < 0 {
		s.row = 0
	}
	if s.row > ptyScreenMaxLines-1 {
		s.row = ptyScreenMaxLines - 1
	}
	if s.col < 0 {
		s.col = 0
	}
	if s.col > ptyScreenMaxCol {
		s.col = ptyScreenMaxCol
	}
	for s.row >= len(s.lines) {
		s.lines = append(s.lines, nil)
	}
	if len(s.lines) > ptyScreenMaxLines {
		excess := len(s.lines) - ptyScreenMaxLines
		s.lines = s.lines[excess:]
		s.row -= excess
		if s.row < 0 {
			s.row = 0
		}
	}
}

func (s *ptyScreen) cur() []rune {
	s.ensureRow()
	return s.lines[s.row]
}

func (s *ptyScreen) put(r rune) {
	line := s.cur()
	if s.cols > 0 && s.col >= s.cols {
		s.row++
		s.col = 0
		line = s.cur()
	}
	for len(line) <= s.col {
		line = append(line, ' ')
	}
	line[s.col] = r
	s.lines[s.row] = line
	s.col++
}

func (s *ptyScreen) eraseLine(mode int) {
	line := s.cur()
	switch mode {
	case 0: // cursor to end of line
		if s.col < len(line) {
			s.lines[s.row] = line[:s.col]
		}
	case 1: // start of line to cursor (inclusive)
		for i := 0; i <= s.col && i < len(line); i++ {
			line[i] = ' '
		}
		s.lines[s.row] = line
	case 2: // whole line
		s.lines[s.row] = nil
	}
}

func (s *ptyScreen) eraseDisplay(mode int) {
	switch mode {
	case 0: // cursor to end of display
		s.eraseLine(0)
		for i := s.row + 1; i < len(s.lines); i++ {
			s.lines[i] = nil
		}
	case 1: // start of display to cursor
		for i := 0; i < s.row && i < len(s.lines); i++ {
			s.lines[i] = nil
		}
		s.eraseLine(1)
	case 2: // whole display (scrollback kept count simple: clear all)
		s.lines = nil
		s.row, s.col = 0, 0
	}
}

func csiParams(body string) []int {
	if body == "" {
		return nil
	}
	parts := strings.Split(body, ";")
	out := make([]int, len(parts))
	for i, p := range parts {
		if p == "" {
			out[i] = 0
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			out[i] = 0
		} else {
			out[i] = n
		}
	}
	return out
}

func paramOr(params []int, i, def int) int {
	if i < len(params) && params[i] > 0 {
		// Cap at the grid bounds: an absurd CSI parameter
		// (ESC[999999999C) must clamp, never allocate.
		if params[i] > ptyScreenMaxCol {
			return ptyScreenMaxCol
		}
		if params[i] > ptyScreenMaxLines {
			return ptyScreenMaxLines
		}
		return params[i]
	}
	return def
}

// renderPTYScreen replays data into display lines (last maxLines).
func renderPTYScreen(data []byte, cols, maxLines int) []string {
	s := &ptyScreen{cols: cols}
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case b == 0x1b && i+1 < len(data) && data[i+1] == '[':
			// CSI: params/intermediates until final byte 0x40-0x7e.
			j := i + 2
			for j < len(data) && !(data[j] >= 0x40 && data[j] <= 0x7e) {
				j++
			}
			if j >= len(data) {
				i = len(data)
				break
			}
			final := data[j]
			params := csiParams(string(data[i+2 : j]))
			switch final {
			case 'A':
				s.row -= paramOr(params, 0, 1)
				if s.row < 0 {
					s.row = 0
				}
			case 'B':
				s.row += paramOr(params, 0, 1)
				s.ensureRow()
			case 'C':
				s.col += paramOr(params, 0, 1)
			case 'D':
				s.col -= paramOr(params, 0, 1)
				if s.col < 0 {
					s.col = 0
				}
			case 'E':
				s.row += paramOr(params, 0, 1)
				s.col = 0
				s.ensureRow()
			case 'F':
				s.row -= paramOr(params, 0, 1)
				if s.row < 0 {
					s.row = 0
				}
				s.col = 0
			case 'G', '`':
				s.col = paramOr(params, 0, 1) - 1
				if s.col < 0 {
					s.col = 0
				}
			case 'H', 'f':
				s.row = paramOr(params, 0, 1) - 1
				s.col = paramOr(params, 1, 1) - 1
				if s.row < 0 {
					s.row = 0
				}
				if s.col < 0 {
					s.col = 0
				}
				s.ensureRow()
			case 'd':
				s.row = paramOr(params, 0, 1) - 1
				if s.row < 0 {
					s.row = 0
				}
				s.ensureRow()
			case 'J':
				mode := 0
				if len(params) > 0 {
					mode = params[0]
				}
				s.eraseDisplay(mode)
			case 'K':
				mode := 0
				if len(params) > 0 {
					mode = params[0]
				}
				s.eraseLine(mode)
			default:
				// SGR, modes, device queries: inert in a text pane.
			}
			i = j + 1
		case b == 0x1b && i+1 < len(data) && data[i+1] == ']':
			// OSC: skip to BEL or ST.
			j := i + 2
			for j < len(data) {
				if data[j] == 0x07 {
					j++
					break
				}
				if data[j] == 0x1b && j+1 < len(data) && data[j+1] == '\\' {
					j += 2
					break
				}
				j++
			}
			i = j
		case b == 0x1b:
			// Two/three-byte escape: skip it.
			if i+1 < len(data) && (data[i+1] == '(' || data[i+1] == ')' || data[i+1] == '#' || data[i+1] == '%') {
				i += 3
			} else {
				i += 2
			}
			if i > len(data) {
				i = len(data)
			}
		case b == '\r':
			s.col = 0
			i++
		case b == '\n':
			s.row++
			s.ensureRow()
			i++
		case b == '\b':
			if s.col > 0 {
				s.col--
			}
			i++
		case b == '\t':
			s.col = (s.col/8 + 1) * 8
			i++
		case b < 0x20 || b == 0x7f:
			i++
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			s.put(r)
			i += size
		}
	}
	out := make([]string, 0, len(s.lines))
	for _, l := range s.lines {
		out = append(out, strings.TrimRight(string(l), " "))
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	if maxLines > 0 && len(out) > maxLines {
		out = out[len(out)-maxLines:]
	}
	return out
}

// boundedPTYScreenLines tails the raw log bytes (bounded) and
// renders them through renderPTYScreen, so CR/erase overwrites
// paint as the final screen text instead of stacked fragments.
// Sentinels match boundedPTYLogLines.
func boundedPTYScreenLines(m ptyMeta, lines int) ([]string, bool) {
	data, cut := tailSegment(m.LogPath, boundedLogLinesMaxBytes)
	if len(data) == 0 {
		if ptyWorkerAlive(m) {
			return []string{"(no output yet)"}, false
		}
		return []string{"(session not running)"}, false
	}
	if cut {
		// Drop the partial head: bytes before the first newline
		// belong to a line that started before the window; a
		// mid-escape head is inert garbage otherwise. But a
		// segment with NO newline at all is one huge line whose
		// tail is real output — dropping it would misreport
		// live output as "(no output yet)". Keep it (the
		// truncation flag still marks the head loss).
		if idx := indexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	ls := renderPTYScreen(data, 0, lines)
	if len(ls) == 0 {
		return []string{"(no output yet)"}, false
	}
	return ls, cut
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

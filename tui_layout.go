package main

import (
	"strings"
)

// tui_layout.go: viewport sizing and pane geometry plus the generic
// box/scroll primitives. Pure: dimensions in, rows out.

// renderLayout is the resolved pane geometry for one View pass.
type renderLayout struct {
	w, h          int
	leftW, rightW int
	liveH, gitH   int
	tabH, workerH int
	contentH      int
	feedH         int
}

// computeLayout resolves the window into pane dimensions. Thresholds
// mirror the original View math exactly: narrow windows are rejected
// by the caller before layout runs.
func computeLayout(width, height int) renderLayout {
	leftW := width / 4
	if leftW < 24 {
		leftW = 24
	}
	rightW := width - leftW
	liveH := (height - 8) * 2 / 3
	if liveH < 8 {
		liveH = 8
	}
	gitH := height - 5 - liveH
	if gitH < 4 {
		gitH = 4
	}
	tabH := height - 7
	if tabH < 1 {
		tabH = 1
	}
	// One rail row belongs to the pinned Muse tab.
	workerH := tabH - 1
	if workerH < 1 {
		workerH = 1
	}
	return renderLayout{
		w: width, h: height,
		leftW: leftW, rightW: rightW,
		liveH: liveH, gitH: gitH,
		tabH: tabH, workerH: workerH,
		contentH: height - 5,
		feedH:    liveH + gitH,
	}
}

func boxLines(title string, content []string, w, h int) []string {
	if w < 4 {
		w = 4
	}
	if h < 3 {
		h = 3
	}
	out := make([]string, 0, h)
	top := "+" + strings.Repeat("-", w-2) + "+"
	if len(title) > 0 {
		t := truncW(" "+title+" ", w-4)
		top = "+" + "-" + t + strings.Repeat("-", w-3-strWidth(t)) + "+"
		// Keep total width exact even when title is short.
		if strWidth(top) < w {
			top += strings.Repeat("-", w-strWidth(top))
		}
		top = truncW(top, w)
	}
	out = append(out, top)
	bodyH := h - 2
	for i := 0; i < bodyH; i++ {
		var line string
		if i < len(content) {
			line = content[i]
		}
		out = append(out, "|"+pad(line, w-2)+"|")
	}
	out = append(out, "+"+strings.Repeat("-", w-2)+"+")
	return out
}

func sliceLines(lines []string, h int, scroll int) []string {
	bodyH := h - 2
	if bodyH < 1 {
		bodyH = 1
	}
	if scroll <= 0 {
		if len(lines) > bodyH {
			return lines[len(lines)-bodyH:]
		}
		return lines
	}
	end := len(lines) - scroll
	if end < 0 {
		end = 0
	}
	start := end - bodyH
	if start < 0 {
		start = 0
	}
	if start > end {
		start = end
	}
	return lines[start:end]
}

// joinPanes zips the left rail with the right stack (live rows then
// git rows) into contentH terminal rows.
func joinPanes(left, live, git []string, lay renderLayout) []string {
	rows := make([]string, 0, lay.contentH)
	for i := 0; i < lay.contentH; i++ {
		l := ""
		if i < len(left) {
			l = left[i]
		} else {
			l = strings.Repeat(" ", lay.leftW)
		}
		r := ""
		if i < len(live) {
			r = live[i]
		} else if i-lay.liveH < len(git) {
			r = git[i-lay.liveH]
		} else {
			r = strings.Repeat(" ", lay.rightW)
		}
		rows = append(rows, pad(l, lay.leftW)+pad(r, lay.rightW))
	}
	return rows
}

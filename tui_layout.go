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
	mainW, infoW  int
	liveH, gitH   int
	tabH, workerH int
	contentH      int
	feedH         int
}

// computeLayout resolves the window into pane dimensions. Thresholds
// mirror the original View math exactly: narrow windows are rejected
// by the caller before layout runs.
func computeLayout(width, height int) renderLayout {
	return computeLayoutWithInfo(width, height, false)
}

func computeLayoutWithInfo(width, height int, infoOpen bool) renderLayout {
	leftW := width / 4
	if leftW < 24 {
		leftW = 24
	}
	rightW := width - leftW
	mainW, infoW := rightW, 0
	if infoOpen {
		infoW = rightW / 3
		if infoW < 22 {
			infoW = 22
		}
		if infoW > rightW/2 {
			infoW = rightW / 2
		}
		mainW = rightW - infoW
	}
	contentH := height - 5
	if contentH < 3 {
		contentH = 3
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
		leftW: leftW, rightW: rightW, mainW: mainW, infoW: infoW,
		liveH: contentH, gitH: 0,
		tabH: tabH, workerH: workerH,
		contentH: contentH,
		feedH:    contentH,
	}
}

func joinSessionPanes(left, main, info []string, lay renderLayout) []string {
	rows := make([]string, 0, lay.contentH)
	for i := 0; i < lay.contentH; i++ {
		l, c, r := "", "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(main) {
			c = main[i]
		}
		if i < len(info) {
			r = info[i]
		}
		row := pad(l, lay.leftW) + pad(c, lay.mainW)
		if lay.infoW > 0 {
			row += pad(r, lay.infoW)
		}
		rows = append(rows, row)
	}
	return rows
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

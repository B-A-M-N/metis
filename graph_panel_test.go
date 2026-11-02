package main

// graph_panel_test.go: the banner is a pure projection — same
// lineage in, same markdown out, worker-influenced text sanitized,
// output bounded.

import (
	"fmt"
	"strings"
	"testing"
)

func TestGraphPanelLines(t *testing.T) {
	run, ids := seedGraphRun(t)
	lines := graphMDLines(run, 60)
	text := strings.Join(lines, "\n")
	for _, want := range []string{"# path: " + run, "## ready", "## blocked", "## coverage"} {
		if !strings.Contains(text, want) {
			t.Fatalf("banner missing %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, shortID(ids["c"])) || !strings.Contains(text, "waits on") {
		t.Fatalf("banner must name the blocked task and its gate:\n%s", text)
	}
	if !strings.Contains(text, "path:") || !strings.Contains(text, "<-") {
		t.Fatalf("banner must show a dependency path:\n%s", text)
	}
	// Deterministic: rebuild and compare exactly.
	again := strings.Join(graphMDLines(run, 60), "\n")
	if again != text {
		t.Fatalf("banner not deterministic:\n%s\n---\n%s", text, again)
	}
	// Empty store and unknown run stay honest, never empty.
	if got := graphMDLines("", 60); len(got) == 0 {
		t.Fatal("empty run must still render a line")
	}
}

func TestGraphPanelSanitizesObjectives(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "graphxss")
	seedOwnedSession(t, "w", "graphxss")
	if _, err := createTask("run-xss", "w", "do evil \x1b[2J\x1b[H and \x1b]0;pwned\x07 stuff", "x"); err != nil {
		t.Fatal(err)
	}
	lines := graphMDLines("run-xss", 60)
	for _, l := range lines {
		if strings.Contains(l, "\x1b[2J") || strings.Contains(l, "\x07") || strings.Contains(l, "pwned\x1b") {
			t.Fatalf("worker escape leaked into banner: %q", l)
		}
	}
}

func TestGraphPanelCap(t *testing.T) {
	run, _ := seedGraphRun(t)
	lines := graphMDLines(run, 5)
	if len(lines) > 5 {
		t.Fatalf("banner exceeded cap: %d lines", len(lines))
	}
}

func TestResolvePanelRun(t *testing.T) {
	run, _ := seedGraphRun(t)
	if got := resolvePanelRun(false, "no-such-session"); got != run {
		t.Fatalf("unknown session must fall back to newest run, got %q", got)
	}
}

// The banner pops into the Git pane's slot on `g` and pops back out
// on the second `g`: same height, same layout math, lineage in
// place of workdir state.
func TestGraphPanelToggle(t *testing.T) {
	run, ids := seedGraphRun(t)
	m := newModel("w")
	// Tall enough that the Git-slot pane fits the whole banner;
	// narrow panes slice to their tail by design (see sliceLines).
	m.width, m.height = 120, 60
	m.refresh()
	if m.nav.OnMuse {
		t.Fatal("explicit target must open on the worker tab")
	}
	if m.panelRun != run {
		t.Fatalf("refresh must collect the worker's run, got %q", m.panelRun)
	}
	// Panel off: Git pane, no lineage.
	if v := m.View(); strings.Contains(v, "waits on") || strings.Contains(v, "Path:") {
		t.Fatalf("banner must be off by default:\n%s", v)
	}
	// Panel on: banner with the blocked task and its gate.
	um, _ := m.Update(museKeyMsg("g"))
	m = um.(model)
	if !m.graphPanel {
		t.Fatal("g must open the path panel")
	}
	v := m.View()
	if !strings.Contains(v, "Path: "+run) {
		t.Fatalf("banner title must name the run:\n%s", v)
	}
	if !strings.Contains(v, shortID(ids["c"])) || !strings.Contains(v, "waits on") {
		t.Fatalf("banner must show the blocked task and its gate:\n%s", v)
	}
	// Panel off again: Git pane returns, lineage leaves.
	um, _ = m.Update(museKeyMsg("g"))
	m = um.(model)
	if m.graphPanel {
		t.Fatal("second g must close the path panel")
	}
	if v := m.View(); strings.Contains(v, "waits on") || strings.Contains(v, "Path:") {
		t.Fatalf("banner must be gone after toggle-off:\n%s", v)
	}
}

// A banner render over N tasks must not scale its DB opens with N.
// graphMDLines fans out per task (ready/blockers/coverage each scan
// every task; depChain walks per blocker), so one render over a
// 40-task chain costs hundreds of opens — once per refresh tick in
// the live TUI (livePanelSnapshot). The render must hoist a single
// handle and share it: exactly one open per graphMDLines call.
func TestGraphRenderHoistsOneOpen(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	const n = 40
	run := "run-hoist"
	prev := ""
	for i := 0; i < n; i++ {
		tk, err := createTask(run, "w", fmt.Sprintf("hoist %d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		if prev != "" {
			if err := addTaskDep(tk.ID, prev, testReq("orch")); err != nil {
				t.Fatal(err)
			}
		}
		prev = tk.ID
	}
	before := dbOpenCount.Load()
	lines := graphMDLines(run, 2000)
	if len(lines) == 0 {
		t.Fatal("hoisted render must still produce a banner")
	}
	text := strings.Join(lines, "\n")
	if !strings.Contains(text, "## ready") || !strings.Contains(text, "## blocked") || !strings.Contains(text, "## coverage") {
		t.Fatalf("hoisted render must keep all three sections:\n%s", text)
	}
	if got := dbOpenCount.Load() - before; got != 1 {
		t.Fatalf("graphMDLines over %d tasks opened the DB %d times, want exactly 1 (hoist one handle per render)", n, got)
	}
}

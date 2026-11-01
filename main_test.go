package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "tui-dash/internal/tui"
)

// TestMain re-execs into supervisor mode when the test binary is
// spawned as a detached supervisor (ptyStart re-executes
// os.Executable()). Without this, the child would run the test suite
// instead of supervising the PTY.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "pty-supervise" {
		main()
		os.Exit(0)
	}
	// Cross-process ID-collision probe: a child test binary re-execed
	// with TUI_DASH_IDGEN_PROBE set prints fresh generator output and
	// exits without running the suite. See
	// task_idgen_crossprocess_test.go.
	if os.Getenv("TUI_DASH_IDGEN_PROBE") != "" {
		runIDGenProbe(os.Getenv("TUI_DASH_IDGEN_PROBE"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPublicJSONNoOwner(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	s := Session{Name: "n", Tmux: "tui-n", Workdir: "/w", Command: "c", Created: "t", Owner: "secret-token", Harness: "muse", Model: "m"}
	data, err := json.Marshal(s.Public())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-token") || strings.Contains(string(data), "owner") {
		t.Fatalf("public projection leaks owner: %s", data)
	}
	if !strings.Contains(string(data), `"harness":"muse"`) {
		t.Fatalf("public projection drops labels: %s", data)
	}
	// Persistence round-trip must keep the credential.
	reg := map[string]Session{"n": s}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if got := loadRegistry()["n"]; got.Owner != "secret-token" {
		t.Fatalf("registry lost owner: %+v", got)
	}
	m := ptyMeta{Name: "p", Owner: "pty-secret", Status: "running"}
	pdata, err := json.Marshal(ptyView{Session: m.publicPty(), State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pdata), "pty-secret") {
		t.Fatalf("pty view leaks owner: %s", pdata)
	}
}

func TestStatePerms(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TUI_DASH_STATE", state)
	if err := saveRegistry(map[string]Session{}); err != nil {
		t.Fatal(err)
	}
	checkMode := func(path string, want os.FileMode) {
		t.Helper()
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Fatalf("%s mode=%o want %o", path, got, want)
		}
	}
	checkMode(registryPath(), 0o600)
	// Repair pass tightens pre-existing permissive files.
	lax := filepath.Join(state, "streams")
	_ = os.MkdirAll(lax, 0o755)
	probe := filepath.Join(lax, "x.log")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hardenStatePerms()
	checkMode(state, 0o700)
	checkMode(probe, 0o600)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	checkMode(tasksDBPath(), 0o600)
}

func TestAdoptExplicit(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	state := t.TempDir()
	t.Setenv("TUI_DASH_STATE", state)
	work := t.TempDir()
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tui-leg", "-c", work, "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("tmux start: %v %s", out, err)
	}
	defer exec.Command("tmux", "kill-session", "-t", "tui-leg").Run()
	reg := map[string]Session{
		"leg": {Name: "leg", Tmux: "tui-leg", Workdir: work, Command: "sleep 60", Created: "now"},
	}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	// Destructive paths must refuse WITHOUT adopting as a side effect.
	if err := stopSession("leg"); err == nil {
		t.Fatal("stop of unowned session should fail")
	}
	if got := loadRegistry()["leg"]; got.Owner != "" {
		t.Fatalf("stop conferred ownership as side effect: %+v", got)
	}
	if err := sendTerminalInput("leg", "x"); err == nil {
		t.Fatal("input to unowned session should fail")
	}
	if got := loadRegistry()["leg"]; got.Owner != "" {
		t.Fatalf("input conferred ownership as side effect: %+v", got)
	}
	// Explicit adoption confers authority once, and is auditable.
	if _, err := adoptSession("leg"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if got := loadRegistry()["leg"]; got.Owner == "" {
		t.Fatal("adopt did not persist credential")
	}
	if _, err := adoptSession("leg"); err == nil {
		t.Fatal("re-adopt of credentialed session should fail")
	}
	if _, err := adoptSession("missing"); err == nil {
		t.Fatal("adopt of unknown session should fail")
	}
	// After adoption, control works.
	if err := stopSession("leg"); err != nil {
		t.Fatalf("stop after adopt: %v", err)
	}
	if _, err := exec.Command("tmux", "has-session", "-t", "tui-leg").CombinedOutput(); err == nil {
		t.Fatal("adopted session survived stop")
		exec.Command("tmux", "kill-session", "-t", "tui-leg").Run()
	}
	// Dead records cannot be adopted.
	reg["deadleg"] = Session{Name: "deadleg", Tmux: "tui-deadleg", Workdir: work, Command: "x", Created: "now"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := adoptSession("deadleg"); err == nil {
		t.Fatal("adopt of dead session should fail")
	}
}

func TestCLIAliases(t *testing.T) {
	// Group forms reach the canonical command with flags intact.
	cases := map[string]string{
		"task list": "task-list", "task create": "task-create",
		"run finalize": "run-finalize", "guide pending": "guidance-outstanding",
		"report compare": "compare-reports", "agent work": "worker-loop",
		"agent list": "agents", "tasks": "task-list", "send": "inject",
		"watch": "run", "workers": "agents", "sessions": "list",
	}
	for in, want := range cases {
		args := append([]string{"tui-dash"}, strings.Fields(in)...)
		args = append(args, "--run", "R")
		got := aliasArgs(args)
		if got[1] != want {
			t.Fatalf("%q -> %q, want %q", in, got[1], want)
		}
		if got[len(got)-2] != "--run" || got[len(got)-1] != "R" {
			t.Fatalf("%q dropped flags: %v", in, got)
		}
	}
	// Legacy meanings survive: bare `run` still opens the TUI, and
	// canonical names pass through untouched.
	if got := aliasArgs([]string{"tui-dash", "run", "--session", "x"}); got[1] != "run" {
		t.Fatalf("bare run rewritten: %v", got)
	}
	for _, canon := range []string{"task-list", "inject", "mcp", "graph", "muse", "pty-log"} {
		if got := aliasArgs([]string{"tui-dash", canon}); got[1] != canon {
			t.Fatalf("canonical %q rewritten: %v", canon, got)
		}
	}
	// Typos never land on a wrong command.
	if got := aliasArgs([]string{"tui-dash", "task", "bogus"}); got[1] != "task" {
		t.Fatalf("typo rewritten: %v", got)
	}
	// Bare nouns offer their verbs; real commands do not.
	if help, ok := groupHelp("task"); !ok || !strings.Contains(help, "task list") {
		t.Fatalf("bare task should list verbs: %q %v", help, ok)
	}
	for _, w := range []string{"run", "tasks", "bogus", ""} {
		if _, ok := groupHelp(w); ok {
			t.Fatalf("groupHelp(%q) should be false", w)
		}
	}
}

func TestStartValidation(t *testing.T) {
	if err := startSession("bad name!", t.TempDir(), "echo hi", "", ""); err == nil {
		t.Fatal("bad name should fail")
	}
	if err := startSession("ok", "/nonexistent-workdir-xyz", "echo hi", "", ""); err == nil {
		t.Fatal("missing workdir should fail")
	}
}

func TestInjectAndSnapshot(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TUI_DASH_STATE", state)
	work := t.TempDir()
	reg := map[string]Session{
		"unit": {Name: "unit", Tmux: "tui-unit", Workdir: work, Command: "echo hi"},
	}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := injectGuidance("unit", "UNIT-GUIDANCE-1", false); err != nil {
		t.Fatal(err)
	}
	g, _ := os.ReadFile(filepath.Join(work, "guidance.md"))
	if !strings.Contains(string(g), "UNIT-GUIDANCE-1") {
		t.Fatalf("guidance.md missing text: %s", g)
	}
	snap := snapshotText("unit")
	if !strings.Contains(snap, "UNIT-GUIDANCE-1") {
		t.Fatalf("snapshot missing guidance: %s", snap)
	}
	if err := injectGuidance("missing", "x", false); err == nil {
		t.Fatal("inject to missing session should fail")
	}
}

func TestDemoGuidanceAck(t *testing.T) {
	work := t.TempDir()
	_ = os.WriteFile(filepath.Join(work, "guidance.md"), []byte("# Guidance\n\n"), 0o644)
	go func() {
		time.Sleep(50 * time.Millisecond)
		f, _ := os.OpenFile(filepath.Join(work, "guidance.md"), os.O_APPEND|os.O_WRONLY, 0o644)
		if f != nil {
			_, _ = f.WriteString("\nDEMO-GUIDANCE-1\n")
			_ = f.Close()
		}
	}()
	if err := runDemo(work, 10, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ack, _ := os.ReadFile(filepath.Join(work, "guidance_ack.md"))
	if !strings.Contains(string(ack), "DEMO-GUIDANCE-1") {
		t.Fatalf("demo did not ack guidance, ack=%s", ack)
	}
}

func TestStopOwnership(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	state := t.TempDir()
	t.Setenv("TUI_DASH_STATE", state)
	work := t.TempDir()
	// Foreign session: live tmux session with no registry entry.
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tui-foreign", "-c", work, "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("tmux start: %v %s", err, out)
	}
	defer exec.Command("tmux", "kill-session", "-t", "tui-foreign").Run()
	if err := stopSession("foreign"); err == nil {
		t.Fatal("stop of unregistered session should fail")
	}
	if _, err := exec.Command("tmux", "has-session", "-t", "tui-foreign").CombinedOutput(); err != nil {
		t.Fatal("foreign session was killed; ownership check failed")
	}
	// Registry entry with mismatched owner.
	reg := map[string]Session{
		"foreign": {Name: "foreign", Tmux: "tui-foreign", Workdir: work, Command: "sleep 60", Created: "now", Owner: "someone-else"},
	}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := stopSession("foreign"); err == nil {
		t.Fatal("stop with mismatched owner should fail")
	}
	if _, err := exec.Command("tmux", "has-session", "-t", "tui-foreign").CombinedOutput(); err != nil {
		t.Fatal("foreign session was killed despite owner mismatch")
	}
	// Own session: full lifecycle works.
	if err := startSession("own", work, "sleep 60", "muse", "test-model"); err != nil {
		t.Fatalf("start own: %v", err)
	}
	reread := loadRegistry()
	if reread["own"].Owner == "" {
		t.Fatal("owned session missing owner token")
	}
	if reread["own"].Harness != "muse" || reread["own"].Model != "test-model" {
		t.Fatalf("harness/model labels not persisted: %+v", reread["own"])
	}
	if err := stopSession("own"); err != nil {
		t.Fatalf("stop own: %v", err)
	}
	if _, err := exec.Command("tmux", "has-session", "-t", "tui-own").CombinedOutput(); err == nil {
		t.Fatal("own session still running after stop")
		exec.Command("tmux", "kill-session", "-t", "tui-own").Run()
	}
	// Unknown name is refused, never kills.
	if err := stopSession("nope-missing"); err == nil {
		t.Fatal("stop of unknown session should fail")
	}
}

func TestRefreshMsgStaleIgnored(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 2
	m.names = []string{"a"}
	m.nav.Selected = 0
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen:   1,
		names: []string{"x", "y"},
	}})
	got, ok := updated.(model)
	if !ok {
		t.Fatal("Update did not return model")
	}
	if len(got.names) != 1 || got.names[0] != "a" {
		t.Fatalf("stale refreshMsg mutated model: %v", got.names)
	}
}

func TestRefreshMsgAppliesPreservesSelectionAndGit(t *testing.T) {
	m := newModel("")
	m.refreshCtl.Gen = 5
	m.names = []string{"a", "b"}
	m.nav.Selected = 1 // "b"
	m.gitLines = []string{"git: cached"}
	m.live = []string{"old"}
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen:     5,
		names:   []string{"a", "b", "c"},
		running: map[string]bool{"a": true, "b": false, "c": true},
		entry:   Session{Name: "b", Workdir: "/tmp"},
		live:    []string{"new"},
		events:  []string{"e1"},
		// hasGit false: cached gitLines must survive.
	}})
	got := updated.(model)
	if len(got.names) != 3 {
		t.Fatalf("names not applied: %v", got.names)
	}
	if got.selName() != "b" {
		t.Fatalf("selection not preserved by name: %+v %v", got.nav, got.names)
	}
	if len(got.gitLines) != 1 || got.gitLines[0] != "git: cached" {
		t.Fatalf("gitLines clobbered on non-git refresh: %v", got.gitLines)
	}
	if !got.running["a"] || got.running["b"] || !got.running["c"] {
		t.Fatalf("running map not applied: %v", got.running)
	}
	if got.entry.Name != "b" || len(got.live) != 1 || got.live[0] != "new" {
		t.Fatalf("entry/live not applied: %+v %v", got.entry, got.live)
	}
	// View must render from cache without touching tmux/registry.
	got.width, got.height = 100, 30
	v := got.View()
	if !strings.Contains(v, "session=b") {
		t.Fatalf("View missing selected session: %q", string(v[:minInt(200, len(v))]))
	}
}

func TestTerminalTabsRail(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false // rail selection renders in worker view
	m.names = []string{"a", "b", "c"}
	m.running = map[string]bool{"a": true, "b": false, "c": true}
	m.nav.Selected = 1
	m.width, m.height = 100, 30
	v := m.View()
	for _, want := range []string{"Terminals", "● a", "○ b", "● c", "> ○ b"} {
		if !strings.Contains(v, want) {
			t.Fatalf("tab rail missing %q", want)
		}
	}
}

func TestTabDisconnectDropsAttach(t *testing.T) {
	m := newModel("")
	m.width, m.height = 100, 30
	m.refreshCtl.Gen = 1
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.nav = m.nav.EnterAttach()
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen:     1,
		names:   []string{"a"},
		running: map[string]bool{"a": false},
	}})
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeNav {
		t.Fatal("attach survived the tab disconnecting")
	}
	if !strings.Contains(got.status, "stopped") {
		t.Fatalf("expected stopped status, got %q", got.status)
	}
}

func TestTabViewportScrolls(t *testing.T) {
	m := newModel("")
	m.width, m.height = 100, 30
	var names []string
	for i := 0; i < 60; i++ {
		names = append(names, fmt.Sprintf("s%02d", i))
	}
	m.names = names
	m.nav.Selected = 59
	m.running = map[string]bool{}
	updated, _ := m.Update(refreshMsg{data: refreshData{gen: 0, names: names, running: map[string]bool{}}})
	got := updated.(model)
	v := got.View()
	if !strings.Contains(v, "s59") {
		t.Fatal("selected tab scrolled out of view")
	}
	if strings.Contains(v, "s00") {
		t.Fatal("tab rail did not scroll; first tab still visible with selection at end")
	}
}

func TestSanitizedOverview(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false // sanitized worker overview, not the Muse feed
	m.width, m.height = 100, 30
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.live = []string{"\x1b[2J\x1b[Hhello", "plain", "\x1b]0;title\x07x"}
	m.events = []string{"e\x1b[31mred"}
	// Fixture models an installed projection for the viewed
	// session, exactly as applyRefresh records it.
	m.paneSel = "a"
	v := m.View()
	// Worker escapes must be inert. Own chrome (harness tab
	// backgrounds, the MUSE row) intentionally carries escapes,
	// so the assertion names worker payloads, not "\x1b".
	for _, leaked := range []string{"\x1b[2J", "\x1b[H", "\x1b]0;", "\x1b[31m"} {
		if strings.Contains(v, leaked) {
			t.Fatalf("worker escape leaked into overview: %q", leaked)
		}
	}
	for _, want := range []string{"hello", "plain", "x"} {
		if !strings.Contains(v, want) {
			t.Fatalf("sanitization ate content %q", want)
		}
	}
}

func TestUnicodeWidth(t *testing.T) {
	if strWidth("日本") != 4 {
		t.Fatalf("wide width: got %d want 4", strWidth("日本"))
	}
	if got := pad("ab", 5); got != "ab   " {
		t.Fatalf("ascii pad: %q", got)
	}
	if got := pad("日本", 5); got != "日本 " {
		t.Fatalf("wide pad: %q", got)
	}
	if got := truncW("日本語", 5); strWidth(got) != 4 {
		t.Fatalf("wide trunc: %q", got)
	}
	if got := sanitizeLine("\x1b[31mred\x1b[0m"); got != "red" {
		t.Fatalf("sanitize: %q", got)
	}
}

func TestLogRotationGapStatus(t *testing.T) {
	m := newModel("")
	m.width, m.height = 100, 30
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.logGen = map[string]int{"a": 3}
	updated, _ := m.Update(refreshMsg{data: refreshData{
		gen:     0,
		names:   []string{"a"},
		running: map[string]bool{"a": true},
		logGen:  map[string]int{"a": 4},
	}})
	got := updated.(model)
	if !strings.Contains(got.status, "rotated") {
		t.Fatalf("rotation gap not reported: %q", got.status)
	}
	// No jump, no noise: the status line stays quiet.
	m2 := newModel("")
	m2.width, m2.height = 100, 30
	m2.names = []string{"a"}
	m2.nav.Selected = 0
	m2.running = map[string]bool{"a": true}
	m2.logGen = map[string]int{"a": 4}
	updated2, _ := m2.Update(refreshMsg{data: refreshData{
		gen:     0,
		names:   []string{"a"},
		running: map[string]bool{"a": true},
		logGen:  map[string]int{"a": 4},
	}})
	if strings.Contains(updated2.(model).status, "rotated") {
		t.Fatal("steady generation reported as rotation")
	}
}

func TestTabIdentityColors(t *testing.T) {
	// Harness color is deterministic; empty harness gets none.
	if harnessBG("muse") != harnessBG("muse") || harnessBG("muse") == "" {
		t.Fatal("harness color not deterministic")
	}
	if harnessBG("") != "" {
		t.Fatal("empty harness should have no background")
	}
	label := tabLabel("> ", "●", "w0",
		tabDetail{Harness: "muse", Model: "opus", Title: "fix bug"}, 40)
	for _, want := range []string{"w0", "opus", "fix bug", harnessBG("muse"), "\x1b[0m"} {
		if !strings.Contains(label, want) {
			t.Fatalf("tab label missing %q: %q", want, label)
		}
	}
	if strWidth(label) != 40 {
		t.Fatalf("tab label width %d, want 40: %q", strWidth(label), label)
	}
	// Escape smuggled in harness/model/title is inert, and the row
	// still measures exactly innerW columns.
	evil := tabLabel("  ", "○", "w1",
		tabDetail{Harness: "\x1b[31m", Model: "m\x1b[0m", Title: "t"}, 40)
	if strings.Contains(evil, "\x1b[31m") {
		t.Fatalf("harness escape leaked: %q", evil)
	}
	if strWidth(evil) != 40 {
		t.Fatalf("evil label width %d, want 40", strWidth(evil))
	}
	// A colored rail row survives box padding without bleeding or
	// clipping visible text.
	boxed := boxLines("Terminals", []string{label}, 30, 5)
	if !strings.Contains(boxed[1], "w0") || !strings.Contains(boxed[1], "\x1b[0m") {
		t.Fatalf("boxed tab lost content or reset: %q", boxed[1])
	}
	if strWidth(boxed[1]) != 30 {
		t.Fatalf("boxed row width %d, want 30", strWidth(boxed[1]))
	}
}

func TestTabTitlesFromTasks(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "ot")
	seedOwnedSession(t, "w0", "ot")
	if _, err := createTaskIdem("run-t", "muse", "repair login", "", "k1"); err != nil {
		t.Fatal(err)
	}
	// Retarget the task at the session: titles key on task.Agent.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET agent = 'w0' WHERE run = 'run-t'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_TMUX_LIST", "tui-w0")
	d := collectRefreshData(liveRefreshSource{}, 1, "", false)
	got, ok := d.tabInfo["w0"]
	if !ok {
		t.Fatalf("no tab info for w0: %+v", d.tabInfo)
	}
	if got.Title != "repair login" {
		t.Fatalf("tab title %q, want newest objective", got.Title)
	}
	if !d.running["w0"] {
		t.Fatal("seeded session should read running from batch list")
	}
}

func TestAttachRefusesStopped(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false // attach targets a worker tab, never the Muse feed
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeNav {
		t.Fatal("attached to a stopped session")
	}
	if !strings.Contains(got.status, "read-only") {
		t.Fatalf("expected read-only status, got %q", got.status)
	}
}

func TestAttachDetachCycle(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false // attach targets a worker tab, never the Muse feed
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("enter did not attach: mode=%q status=%q", got.nav.Mode, got.status)
	}
	updated2, _ := got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got2 := updated2.(model)
	if got2.nav.Mode != tuistate.ModeNav {
		t.Fatal("esc did not detach")
	}
}

func TestSendTerminalInputOwnership(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	state := t.TempDir()
	t.Setenv("TUI_DASH_STATE", state)
	work := t.TempDir()
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", "tui-foreign", "-c", work, "sleep", "60").CombinedOutput(); err != nil {
		t.Fatalf("tmux start: %v %s", out, err)
	}
	defer exec.Command("tmux", "kill-session", "-t", "tui-foreign").Run()
	// Unknown and foreign sessions are refused before any input is sent.
	if err := sendTerminalInput("nope-missing", "x"); err == nil {
		t.Fatal("input to unknown session should fail")
	}
	if err := sendTerminalInput("foreign", "x"); err == nil {
		t.Fatal("input to foreign session should fail")
	}
	// Owned session receives keystrokes (shell echoes them in the pane).
	if err := startSession("kbd", work, "sleep 60", "demo", ""); err != nil {
		t.Fatalf("start own: %v", err)
	}
	defer stopSession("kbd")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if sessionRunning("kbd") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owned session never came up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := sendTerminalInput("kbd", "echo hi-typed", "Enter"); err != nil {
		t.Fatalf("input to owned session failed: %v", err)
	}
	time.Sleep(time.Second)
	out, _ := tmux("capture-pane", "-p", "-t", "tui-kbd")
	if !strings.Contains(out, "echo hi-typed") {
		t.Fatalf("typed input missing from pane: %q", out)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Integration test, run only after `go build -o tui-dash .` in this folder.
func TestIntegrationTmux(t *testing.T) {
	bin, err := filepath.Abs("tui-dash")
	if err != nil || bin == "" {
		t.Skip("binary path unavailable")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skip("build ./tui-dash first: go build -o tui-dash .")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not available")
	}
	state := t.TempDir()
	work := t.TempDir()
	_ = exec.Command("git", "-C", work, "init", "-q").Run()
	env := append(os.Environ(), "TUI_DASH_STATE="+state)
	run := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	defer func() { _, _ = run("stop", "--name", "gotest") }()
	if _, err := run("start", "--name", "gotest", "--workdir", work, "--cmd", bin+" demo --workdir "+work+" --steps 120 --delay 1"); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(3 * time.Second)
	snap, _ := run("snapshot", "--name", "gotest")
	if !strings.Contains(snap, "step") {
		t.Fatalf("snapshot missing step output: %s", snap)
	}
	if _, err := run("inject", "--name", "gotest", "--text", "GO-TEST-GUIDANCE-123"); err != nil {
		t.Fatalf("inject: %v", err)
	}
	acked := false
	for i := 0; i < 10; i++ {
		time.Sleep(time.Second)
		snap, _ = run("snapshot", "--name", "gotest")
		if strings.Contains(snap, "GUIDANCE RECEIVED: GO-TEST-GUIDANCE-123") {
			acked = true
			break
		}
	}
	if !acked {
		t.Fatalf("guidance not acknowledged: %s", snap)
	}
	if !strings.Contains(snap, "step-") {
		t.Fatalf("git pane missing step files: %s", snap)
	}
}

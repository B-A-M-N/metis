package main

// tui_decompose_test.go: decomposition seams for the TUI application
// model (review P0-D) — view-state components, the read-model
// interface, the async attach command, and rune-aware input editing.

import (
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "tui-dash/internal/tui"
)

// Backspace in guidance-input mode must remove one rune, not one
// byte: multi-byte input (accents, CJK, emoji) must stay valid UTF-8
// and lose exactly the last character.
func TestInputBackspaceRuneAware(t *testing.T) {
	m := newModel("")
	m.nav = m.nav.EnterInput()
	m.guideCtl.Buf = "abé👍"
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	got := updated.(model)
	if got.guideCtl.Buf != "abé" {
		t.Fatalf("backspace must remove one rune, got %q", got.guideCtl.Buf)
	}
	updated, _ = got.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	got = updated.(model)
	if got.guideCtl.Buf != "ab" {
		t.Fatalf("backspace must remove the 2-byte rune whole, got %q", got.guideCtl.Buf)
	}
	// A lone multi-byte rune clears to empty, and backspace on empty
	// input is a no-op rather than a slice panic.
	got.guideCtl.Buf = "é"
	updated, _ = got.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	got = updated.(model)
	if got.guideCtl.Buf != "" {
		t.Fatalf("backspace must clear a lone rune, got %q", got.guideCtl.Buf)
	}
	updated, _ = got.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if got = updated.(model); got.guideCtl.Buf != "" {
		t.Fatalf("backspace on empty input must stay empty, got %q", got.guideCtl.Buf)
	}
}

// fakeRefreshSource is a scripted refreshSource: assembly tests
// exercise collectRefreshData/assembleRefresh with no tmux,
// filesystem, or database.
type fakeRefreshSource struct {
	snaps refreshSnapshots
}

func (f fakeRefreshSource) snapshots(sel string, everyGit bool) refreshSnapshots {
	return f.snaps
}

func cannedSnapshots() refreshSnapshots {
	return refreshSnapshots{
		sessions: []sessionSnapshot{
			{name: "a", running: true,
				entry:  Session{Name: "a", Workdir: "/w/a", Harness: "muse", Model: "opus"},
				detail: tabDetail{Harness: "muse", Model: "opus", Title: "fix login"}},
			{name: "b", running: false,
				entry:  Session{Name: "b", Workdir: "/w/b"},
				detail: tabDetail{}},
		},
		content: contentSnapshot{found: true,
			entry:     Session{Name: "a", Workdir: "/w/a", Harness: "muse", Model: "opus"},
			live:      []string{"canned-live"},
			git:       []string{"canned-git"},
			hasGit:    true,
			events:    []string{"canned-ev"},
			streamGen: 4, hasStreamGen: true},
		feed: feedSnapshot{
			feed: []OrchEvent{{Seq: 11, EventType: OrchDelegationDispatched, TargetAgent: "a"}},
			ok:   true, state: MuseActive, open: 1},
		panel: panelSnapshot{run: "run-canned", lines: []string{"# path: run-canned"}},
	}
}

// countingSource is a fakeRefreshSource that counts query rounds so
// refresh scheduling is observable without timing.
type countingSource struct {
	snaps refreshSnapshots
	calls int
}

func (c *countingSource) snapshots(sel string, everyGit bool) refreshSnapshots {
	c.calls++
	return c.snaps
}

// runBatch executes a command tree one level: a Batch's members run,
// a lone command runs. Returned messages are the command outcomes.
// Timer reschedules (tea.Tick) block for their full duration, so
// members are drained with a bound: production acquisition returns
// immediately on the fake source, while a tick member times out and
// is skipped. Without the bound every batch would burn a 500ms tick.
func runBatch(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	if top, ok := drainCmd(cmd); ok {
		if bm, ok := top.(tea.BatchMsg); ok {
			var out []tea.Msg
			for _, c := range bm {
				if c == nil {
					continue
				}
				if m, ok := drainCmd(c); ok {
					out = append(out, m)
				}
			}
			return out
		}
	}
	return nil
}

// drainCmd runs one command with a bound, reporting whether it
// produced a message in time. Timer-only commands exceed the bound
// and are skipped.
func drainCmd(cmd tea.Cmd) (tea.Msg, bool) {
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m, true
	case <-time.After(100 * time.Millisecond):
		return nil, false
	}
}

// Overlapping refreshes must coalesce to one in flight (Phase 5): a
// tick while a refresh is outstanding must not dispatch a second
// collection. Delivering the outstanding refresh re-arms dispatch.
func TestRefreshSkipsWhileOneIsInFlight(t *testing.T) {
	src := &countingSource{snaps: cannedSnapshots()}
	m := newModelWithSource(src, "")
	var held []tea.Msg
	for i := 0; i < 3; i++ {
		updated, cmd := m.Update(tickMsg{})
		m = updated.(model)
		held = append(held, runBatch(cmd)...)
	}
	if src.calls != 1 {
		t.Fatalf("overlapping refresh dispatched while one in flight: calls=%d, want 1", src.calls)
	}
	var r1 refreshMsg
	for _, msg := range held {
		if r, ok := msg.(refreshMsg); ok {
			r1 = r
			break
		}
	}
	updated, _ := m.Update(r1)
	m = updated.(model)
	// Next odd tick re-arms: suppression must not stick.
	for i := 0; i < 2; i++ {
		updated, cmd := m.Update(tickMsg{})
		m = updated.(model)
		runBatch(cmd)
	}
	if src.calls != 2 {
		t.Fatalf("delivery must re-arm dispatch: calls=%d, want 2", src.calls)
	}
}

// The read-model seam: assembly maps each snapshot component onto
// the refreshData projection with no controller contact.
func TestAssembleRefreshFromSource(t *testing.T) {
	d := assembleRefresh(cannedSnapshots(), 7, "a")
	if d.gen != 7 || d.sel != "a" {
		t.Fatalf("generation/selection not bound: %+v", d)
	}
	if len(d.names) != 2 || d.names[0] != "a" || d.names[1] != "b" {
		t.Fatalf("session order not preserved: %v", d.names)
	}
	if !d.running["a"] || d.running["b"] {
		t.Fatalf("liveness not mapped: %v", d.running)
	}
	if d.entry.Workdir != "/w/a" || len(d.live) != 1 || d.live[0] != "canned-live" {
		t.Fatalf("content not installed: %+v", d.entry)
	}
	if !d.hasGit || len(d.git) != 1 || d.git[0] != "canned-git" {
		t.Fatalf("git not installed: %v %v", d.git, d.hasGit)
	}
	if len(d.events) != 1 || d.events[0] != "canned-ev" {
		t.Fatalf("events not installed: %v", d.events)
	}
	if d.logGen["a"] != 4 {
		t.Fatalf("stream generation not mapped: %v", d.logGen)
	}
	if got := d.tabInfo["a"]; got.Title != "fix login" || got.Model != "opus" {
		t.Fatalf("tab identity not mapped: %+v", got)
	}
	if !d.feedOk || len(d.feed) != 1 || d.feed[0].Seq != 11 {
		t.Fatalf("feed not installed: %+v", d.feed)
	}
	if d.museState != MuseActive || d.museOpen != 1 {
		t.Fatalf("activity not mapped: %q %d", d.museState, d.museOpen)
	}
	if d.panelRun != "run-canned" || len(d.panel) != 1 {
		t.Fatalf("panel not installed: %q %v", d.panelRun, d.panel)
	}
}

// collectRefreshData reads through the given source: with a fake,
// marker values flow through and no controller is contacted (this
// test installs no fake tmux, registry, or DB).
func TestCollectRefreshDataUsesReadModel(t *testing.T) {
	d := collectRefreshData(fakeRefreshSource{snaps: cannedSnapshots()}, 9, "a", true)
	if d.gen != 9 || d.sel != "a" {
		t.Fatalf("projection not bound: gen=%d sel=%q", d.gen, d.sel)
	}
	if len(d.names) != 2 || d.live[0] != "canned-live" || d.panelRun != "run-canned" {
		t.Fatalf("fake source did not flow through: %+v", d.names)
	}
	if !d.feedOk || d.feed[0].Seq != 11 {
		t.Fatalf("fake feed did not flow through: %+v", d.feed)
	}
}

// The refresh source is instance-scoped (finding D): two models
// with different sources collect independently with no shared
// global to overwrite. Under a package-global source, the second
// install would decide what both dashboards read.
func TestRefreshSourceIsInstanceScoped(t *testing.T) {
	other := cannedSnapshots()
	other.sessions = []sessionSnapshot{{name: "z", running: true}}
	a := newModelWithSource(fakeRefreshSource{snaps: cannedSnapshots()}, "")
	b := newModelWithSource(fakeRefreshSource{snaps: other}, "")
	ma := a.refreshCmd(1, "a", true)()
	mb := b.refreshCmd(1, "z", true)()
	da, ok := ma.(refreshMsg)
	if !ok {
		t.Fatalf("instance A command must deliver refreshMsg, got %T", ma)
	}
	db, ok := mb.(refreshMsg)
	if !ok {
		t.Fatalf("instance B command must deliver refreshMsg, got %T", mb)
	}
	if len(da.data.names) != 2 || da.data.names[0] != "a" {
		t.Fatalf("instance A must read its own source: %+v", da.data.names)
	}
	if len(db.data.names) != 1 || db.data.names[0] != "z" {
		t.Fatalf("instance B must read its own source: %+v", db.data.names)
	}
}

// Enter on a running worker flips to attach mode immediately and
// heals the stream off the update path: the returned command carries
// the blocking work, and its typed outcome confirms the attachment.
func TestAttachEnterUsesAsyncCommand(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "projattach")
	seedOwnedSession(t, "w", "projattach")
	m := newModel("w")
	m.names = []string{"w"}
	m.running = map[string]bool{"w": true}
	m.nav.Selected = 0
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("enter must attach: mode=%q status=%q", got.nav.Mode, got.status)
	}
	if cmd == nil {
		t.Fatal("enter must return the async attach command")
	}
	msg := cmd()
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "pipe-pane") {
		t.Fatalf("stream healing must run in the command, log:\n%s\nerr=%v", log, err)
	}
	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("attach command must deliver attachResultMsg, got %T", msg)
	}
	if !res.attached || res.session != "w" {
		t.Fatalf("attach outcome wrong: %+v", res)
	}
	updated, _ = got.Update(res)
	got = updated.(model)
	if got.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("confirmed attach must hold: mode=%q status=%q", got.nav.Mode, got.status)
	}
	if !strings.Contains(got.status, "attached to w") {
		t.Fatalf("confirmed attach needs a status, got %q", got.status)
	}
}

// A refusal for the attached session detaches with the reason; a
// stale outcome for any other session never touches this UI state.
func TestAttachRefusalDetaches(t *testing.T) {
	m := newModel("")
	m.nav.OnMuse = false
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	// Establish the attempt fence the way requestAttach does: the
	// refusal must carry the live attempt's generation.
	var gen int
	m.attachCtl, gen = m.attachCtl.Request("a")
	m.nav = m.nav.EnterAttach()
	m.status = "attached to a"
	updated, _ := m.Update(attachResultMsg{session: "a", gen: gen, reason: "ownership lost"})
	got := updated.(model)
	if got.nav.Mode != tuistate.ModeNav {
		t.Fatal("refusal must detach to nav")
	}
	if !strings.Contains(got.status, "ownership lost") {
		t.Fatalf("refusal must carry the reason, got %q", got.status)
	}
	// Stale refusal for another session: untouched.
	m.nav = m.nav.EnterAttach()
	m.status = "attached to a"
	updated, _ = m.Update(attachResultMsg{session: "b", reason: "gone"})
	got = updated.(model)
	if got.nav.Mode != tuistate.ModeAttach || got.status != "attached to a" {
		t.Fatalf("stale outcome must not touch this UI: %+v", got.status)
	}
}

// A superseded attempt's late refusal must not detach the newer
// attach (finding G): Enter, Esc, Enter re-issues the heal, and the
// first attempt's refusal arrives after the second attempt's success.
// Correlation by session name alone detaches the live attach.
func TestAttachStaleRefusalCannotClobberNewerAttach(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "projattachfence")
	seedOwnedSession(t, "w", "projattachfence")
	m := newModel("w")
	m.names = []string{"w"}
	m.running = map[string]bool{"w": true}
	m.nav.Selected = 0
	updated, cmd1 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.nav.Mode != tuistate.ModeAttach || cmd1 == nil {
		t.Fatalf("first enter must attach, mode=%q", m.nav.Mode)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(model)
	if m.nav.Mode != tuistate.ModeNav {
		t.Fatalf("esc must detach, mode=%q", m.nav.Mode)
	}
	updated, cmd2 := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(model)
	if m.nav.Mode != tuistate.ModeAttach || cmd2 == nil {
		t.Fatalf("second enter must re-attach, mode=%q", m.nav.Mode)
	}
	msg2 := cmd2()
	res2, ok := msg2.(attachResultMsg)
	if !ok || !res2.attached {
		t.Fatalf("second attempt must succeed, got %T %+v", msg2, msg2)
	}
	updated, _ = m.Update(res2)
	m = updated.(model)
	if m.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("second attempt success must hold, mode=%q", m.nav.Mode)
	}
	// First attempt's refusal lands late: it must not detach the
	// newer confirmed attach.
	updated, _ = m.Update(attachResultMsg{session: "w", reason: "superseded heal failed"})
	m = updated.(model)
	if m.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("stale refusal must not detach newer attach, mode=%q status=%q", m.nav.Mode, m.status)
	}
	if !strings.Contains(m.status, "attached to w") {
		t.Fatalf("newer attach status must hold, got %q", m.status)
	}
}

// View is pure: repeated renders are identical and render nothing
// the model does not already hold (no tmux/registry contact).
func TestViewRendersFromCacheOnly(t *testing.T) {
	m := newModel("")
	m.names = []string{"a"}
	m.nav.Selected = 0
	m.running = map[string]bool{"a": true}
	m.width, m.height = 100, 30
	first := m.View()
	second := m.View()
	if first != second {
		t.Fatal("View must be a pure function of the model")
	}
	if !strings.Contains(first, "session=a") {
		t.Fatalf("View must render cached selection: %q", first[:minInt(200, len(first))])
	}
}

// An invalid selected index must never escape into renderers or
// operation requests: empty lists resolve to "(none)", and an
// out-of-range cursor clamps to the nearest live row instead of
// panicking (finding F).
func TestSelectedSummaryClampsInvalidIndex(t *testing.T) {
	m := newModel("")
	m.names = nil
	m.running = map[string]bool{}
	m.nav.Selected = 5
	if sel, _, _ := selectedSummary(m); sel != "(none)" {
		t.Fatalf("empty list must resolve to (none), got %q", sel)
	}
	m.names = []string{"a", "b"}
	m.running = map[string]bool{"a": true, "b": true}
	m.nav.Selected = 5
	if sel, _, _ := selectedSummary(m); sel != "b" {
		t.Fatalf("over-range cursor must clamp to last row, got %q", sel)
	}
	m.nav.Selected = -3
	if sel, _, _ := selectedSummary(m); sel != "a" {
		t.Fatalf("negative cursor must clamp to first row, got %q", sel)
	}
}

// A guidance outcome stays correlated with its origin session: the
// selection may move while the submit is in flight, and the status
// must name the session that actually received it (finding G).
func TestGuidanceResultNamesOriginSession(t *testing.T) {
	m := newModel("")
	m.names = []string{"a", "b"}
	m.running = map[string]bool{"a": true, "b": true}
	m.nav.Selected = 1
	m.guideCtl.Busy = true
	updated, _ := m.Update(guidanceResultMsg{session: "a", gid: "g1"})
	got := updated.(model)
	// The assertion pins the explicit correlation marker, not a
	// substring that "guidance" itself would satisfy.
	if !strings.Contains(got.status, "→ a") {
		t.Fatalf("guidance outcome must name its origin session, got %q", got.status)
	}
	if strings.Contains(got.status, "→ b") {
		t.Fatalf("guidance outcome must not imply the viewed session, got %q", got.status)
	}
}

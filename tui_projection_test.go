package main

// tui_projection_test.go: the TUI collection path is read-only and
// failures degrade to last-known data, never to fabricated or
// cleared panes.

import (
	"os"
	"strings"
	"testing"
	"time"

	tuistate "tui-dash/internal/tui"
)

// Background refresh must not heal streams: a missing pipe is
// observed, and re-attachment belongs to the attach path.
func TestCollectLeavesMissingStreamAlone(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "projro")
	seedOwnedSession(t, "w", "projro")
	if _, err := os.Stat(streamPath("w")); !os.IsNotExist(err) {
		t.Fatal("stream must start missing for this test")
	}
	collectRefreshData(liveRefreshSource{}, 1, "w", false)
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "pipe-pane") {
		t.Fatalf("collection must not run pipe-pane; log:\n%s", log)
	}
}

// Attaching is the owner action: it re-attaches the stream through
// the single async lifecycle (requestAttach + attachCmd). The
// command carries the blocking heal off the update path; its typed
// outcome confirms the attachment.
func TestAttachHealsStream(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "projown")
	seedOwnedSession(t, "w", "projown")
	m := newModel("w")
	m.names = []string{"w"}
	m.running = map[string]bool{"w": true}
	m.nav.Selected = 0
	cmd := m.requestAttach()
	if m.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("attach refused: %s", m.status)
	}
	if cmd == nil {
		t.Fatal("requestAttach must return the async attach command")
	}
	msg := cmd()
	res, ok := msg.(attachResultMsg)
	if !ok {
		t.Fatalf("attach command must deliver attachResultMsg, got %T", msg)
	}
	if !res.attached || res.session != "w" {
		t.Fatalf("attach outcome wrong: %+v", res)
	}
	log, _ := os.ReadFile(logPath)
	if !strings.Contains(string(log), "pipe-pane") {
		t.Fatalf("attach must re-attach the stream; log:\n%s", log)
	}
	updated, _ := m.Update(res)
	if got := updated.(model); got.nav.Mode != tuistate.ModeAttach {
		t.Fatalf("confirmed attach must hold: mode=%q status=%q", got.nav.Mode, got.status)
	}
}

// A failed feed read keeps last-known data; only a successful read
// replaces it.
func TestFeedFailureKeepsLastKnown(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	m := newModel("")
	before := []OrchEvent{{Seq: 7, EventType: OrchMessageSent}}
	m.feed = before
	m.refreshCtl.Gen = 3
	m.applyRefresh(refreshData{sessionsOk: true, gen: 3, feedOk: false})
	if len(m.feed) != 1 || m.feed[0].Seq != 7 {
		t.Fatalf("miss must keep last-known feed, got %+v", m.feed)
	}
	after := []OrchEvent{{Seq: 9, EventType: OrchMessageDelivered}}
	m.applyRefresh(refreshData{sessionsOk: true, gen: 3, feed: after, feedOk: true})
	if len(m.feed) != 1 || m.feed[0].Seq != 9 {
		t.Fatalf("successful read must replace the feed, got %+v", m.feed)
	}
}

// An old disconnect followed by live traffic is history, not
// state. Only a recent, unrecovered disconnect reads as Error.
func TestMuseActivityDisconnectHistory(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	// recordOrchEvent always stamps now: seed genuinely old
	// history with a direct insert, then recent traffic on top.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO orchestration_events
		(run_id, event_type, created) VALUES ('', ?, ?)`,
		OrchTransportDisconnected, old); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchMessageDelivered, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state == MuseError {
		t.Fatal("disconnect superseded by traffic must not read as Error")
	}
}

func TestMuseActivityRecentDisconnect(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchTransportDisconnected, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state != MuseError {
		t.Fatalf("recent lone disconnect must read as Error, got %s", state)
	}
}

// A recent disconnect after older traffic means the transport is
// down now: older traffic must not mask it. (The scan must treat
// newer events as superseding older ones.)
func TestMuseActivityRecentDisconnectAfterTraffic(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO orchestration_events
		(run_id, event_type, created) VALUES ('', ?, ?)`,
		OrchMessageDelivered, old); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchTransportDisconnected, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state != MuseError {
		t.Fatalf("recent disconnect after old traffic must read as Error, got %s", state)
	}
}

// A historical approval.required with no pending approval rows is
// resolved history, not live attention — especially when newer
// traffic exists.
func TestMuseActivityStaleApprovalIgnored(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO orchestration_events
		(run_id, event_type, created) VALUES ('', ?, ?)`,
		OrchApprovalRequired, old); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchMessageDelivered, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state == MuseAttention {
		t.Fatal("resolved historical approval must not read as Attention")
	}
}

// Attention must derive from current pending state: an approval
// that is still pending holds Attention.
func TestMuseActivityPendingApprovalHoldsAttention(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMuseApprovalTables(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO muse_approvals
		(session, native_id, approval_id, item_id, requirement_id, choices,
		decide_command_id, status, decision, decided_by, requested_at, resolved_at)
		VALUES ('w', 'n1', 'ap-1', '', '', '[]', '', 'pending', '', '', ?, '')`,
		nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchApprovalRequired, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	if state, _, _ := museActivity(); state != MuseAttention {
		t.Fatalf("pending approval must hold Attention, got %s", state)
	}
}

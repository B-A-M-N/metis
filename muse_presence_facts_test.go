package main

// muse_presence_facts_test.go: F-14 — Muse presence is a traffic
// projection, not verified execution state.
//
// The indicator must distinguish connection from execution
// progress: connection (live lease), current operation (committed
// open work), and blocking reason are separate typed facts with
// explicit Unknowns. Verified Active needs a live lease PLUS open
// work; traffic recency alone is only the labeled estimate
// "Active (est)", never bare Active.

import (
	"testing"
	"time"
)

func presenceFactsSetup(t *testing.T) {
	t.Helper()
	t.Setenv("TUI_DASH_STATE", t.TempDir())
}

func seedOpenTask(t *testing.T, status string) {
	t.Helper()
	tk, err := createTask("run-presence", "w", "objective for presence test", "acc")
	if err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE tasks SET status = ? WHERE id = ?`, status, tk.ID); err != nil {
		t.Fatal(err)
	}
}

func seedOldEvent(t *testing.T, typ string) {
	t.Helper()
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
		(run_id, event_type, created) VALUES ('', ?, ?)`, typ, old); err != nil {
		t.Fatal(err)
	}
}

// Idle connection: a live lease with no work and no traffic is
// Connected (present), not Idle and not Active.
func TestPresenceIdleConnection(t *testing.T) {
	presenceFactsSetup(t)
	if err := recordMusePresence("idle-principal", "mcp", 10*time.Minute); err != nil {
		t.Fatalf("recordMusePresence: %v", err)
	}
	f := collectMuseFacts()
	if f.Lease != MuseLeaseLive {
		t.Fatalf("lease fact = %v, want Live", f.Lease)
	}
	if f.Operation != MuseOpNone {
		t.Fatalf("operation fact = %v, want None", f.Operation)
	}
	if state, _, _ := museActivity(); state != MuseConnected {
		t.Fatalf("state = %q, want %q", state, MuseConnected)
	}
}

// Expired lease: with no work and no traffic the indicator falls
// back to Idle and the lease is pruned, not lingered.
func TestPresenceExpiredLease(t *testing.T) {
	presenceFactsSetup(t)
	if err := recordMusePresence("gone-principal", "mcp", -time.Minute); err != nil {
		t.Fatalf("recordMusePresence: %v", err)
	}
	f := collectMuseFacts()
	if f.Lease != MuseLeaseAbsent {
		t.Fatalf("lease fact = %v, want Absent", f.Lease)
	}
	if state, _, _ := museActivity(); state != MuseIdle {
		t.Fatalf("state = %q, want %q", state, MuseIdle)
	}
	for _, p := range activeMusePresence() {
		if p.Principal == "gone-principal" {
			t.Fatalf("expired lease still active: %+v", p)
		}
	}
}

// Live turn without recent traffic: lease plus committed open work
// is verified Active even when the feed has only stale history —
// execution progress must not depend on fresh MCP traffic.
func TestPresenceLiveTurnWithoutRecentTraffic(t *testing.T) {
	presenceFactsSetup(t)
	if err := recordMusePresence("turn-principal", "mcp", 10*time.Minute); err != nil {
		t.Fatalf("recordMusePresence: %v", err)
	}
	seedOpenTask(t, TaskWorking)
	seedOldEvent(t, OrchMessageDelivered)
	f := collectMuseFacts()
	if f.Lease != MuseLeaseLive {
		t.Fatalf("lease fact = %v, want Live", f.Lease)
	}
	if f.Operation != MuseOpInFlight {
		t.Fatalf("operation fact = %v, want InFlight", f.Operation)
	}
	if f.Progress != MuseProgressStale {
		t.Fatalf("progress fact = %v, want Stale", f.Progress)
	}
	if state, open, _ := museActivity(); state != MuseActive || open != 1 {
		t.Fatalf("state = %q open = %d, want %q open = 1", state, open, MuseActive)
	}
}

// Blocked approval: a still-pending approval holds Attention with
// the approval blocking reason, even without fresh traffic.
func TestPresenceBlockedApproval(t *testing.T) {
	presenceFactsSetup(t)
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
		VALUES ('w', 'n1', 'ap-f14', '', '', '[]', '', 'pending', '', '', ?, '')`,
		nowUTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchApprovalRequired, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	f := collectMuseFacts()
	if f.Blocking != MuseBlockingApproval {
		t.Fatalf("blocking fact = %v, want Approval", f.Blocking)
	}
	if state, _, _ := museActivity(); state != MuseAttention {
		t.Fatalf("state = %q, want %q", state, MuseAttention)
	}
}

// Abandoned work: open work with no live lease is Delegated — the
// indicator must not imply the orchestrator is present or active.
func TestPresenceAbandonedWork(t *testing.T) {
	presenceFactsSetup(t)
	seedOpenTask(t, TaskDispatched)
	seedOldEvent(t, OrchMessageDelivered)
	f := collectMuseFacts()
	if f.Lease != MuseLeaseAbsent {
		t.Fatalf("lease fact = %v, want Absent", f.Lease)
	}
	if f.Operation != MuseOpInFlight {
		t.Fatalf("operation fact = %v, want InFlight", f.Operation)
	}
	if state, _, _ := museActivity(); state != MuseDelegated {
		t.Fatalf("state = %q, want %q", state, MuseDelegated)
	}
}

// Traffic alone is never verified activity: recent MCP traffic
// with no lease and no open work reads as the labeled estimate,
// never bare Active.
func TestPresenceTrafficAloneIsEstimate(t *testing.T) {
	presenceFactsSetup(t)
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchMessageSent, Created: nowUTC()}); err != nil {
		t.Fatal(err)
	}
	f := collectMuseFacts()
	if f.Lease != MuseLeaseAbsent {
		t.Fatalf("lease fact = %v, want Absent", f.Lease)
	}
	if f.Operation != MuseOpNone {
		t.Fatalf("operation fact = %v, want None", f.Operation)
	}
	if f.Progress != MuseProgressFresh {
		t.Fatalf("progress fact = %v, want Fresh", f.Progress)
	}
	state, _, _ := museActivity()
	if state == MuseActive {
		t.Fatal("traffic alone must never read as verified Active")
	}
	if state != MuseActiveEstimated {
		t.Fatalf("state = %q, want labeled estimate %q", state, MuseActiveEstimated)
	}
}

// The estimate renders distinctly from verified Active: same glyph
// family, weaker color, and the label carries the estimate marker.
func TestPresenceEstimateRendersLabeled(t *testing.T) {
	glyph, color := museGlyph(MuseActiveEstimated)
	if glyph == "" || color == "" {
		t.Fatal("estimated state needs a glyph")
	}
	if _, activeColor := museGlyph(MuseActive); color == activeColor {
		t.Fatal("estimate must render weaker than verified Active")
	}
	label := museTabLabel(false, MuseActiveEstimated, 40)
	if !contains(label, "est") {
		t.Fatalf("estimate label must carry its marker: %q", label)
	}
}

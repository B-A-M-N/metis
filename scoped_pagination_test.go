package main

// scoped_pagination_test.go: Z-02 pagination defects (failing-first).
//
// 1. Cursor must name the last RETURNED event when the visible
//    limit is reached (never the end of the fetched page), or
//    advance past scanned foreign rows when none are returned.
// 2. Forward scanning has a finite page budget per request with
//    an advancing continuation cursor.
// 3. Status latest-event lookup finds the NEWEST authorized
//    event (bounded backward scan), not the oldest in window.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// seedForeignEvents bulk-inserts n foreign worker-report events
// (controller B's session) directly, returning the last seq.
func seedForeignEvents(t *testing.T, n int) int64 {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := claimProvisionOwner(db, "sharedB", "jwt:controller-b"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var last int64
	for i := 0; i < n; i++ {
		res, err := tx.Exec(`INSERT INTO orchestration_events
			(run_id, origin_principal, origin_harness, muse_session, target_agent, host_id,
			 task_id, attempt_no, event_type, request_id, payload, disposition, created)
			VALUES ('', 'sharedB', '', '', 'metis', '', '', 0, 'worker.reported', '', 'foreign', 'filed', ?)`, nowUTC())
		if err != nil {
			t.Fatal(err)
		}
		if id, _ := res.LastInsertId(); id > last {
			last = id
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return last
}

func seedAuthEvent(t *testing.T, payload string) int64 {
	t.Helper()
	seq, err := recordOrchEvent(OrchEvent{
		OriginPrincipal: "sharedA", TargetAgent: "metis",
		EventType: OrchWorkerReported, Payload: payload, Disposition: "filed",
	})
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestMCPHTTPEventsCursorNamesLastReturned(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Five authorized events in ONE database page.
	var seqs []int64
	for i := 0; i < 5; i++ {
		seqs = append(seqs, seedAuthEvent(t, fmt.Sprintf("auth-%d", i)))
	}
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	// Page with limit 1 five times: every authorized event reachable.
	cursor := int64(0)
	for i := 0; i < 5; i++ {
		code, out := f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 1, "after_seq": cursor}))
		if code != http.StatusOK {
			t.Fatalf("page %d: %d", i, code)
		}
		text := mcpHTTPText(out)
		want := fmt.Sprintf("auth-%d", i)
		if !strings.Contains(text, want) {
			t.Fatalf("page %d must return %q (cursor must name last returned event): %s", i, want, text)
		}
		cursor = parseCursor(t, out)
		if cursor != seqs[i] {
			t.Fatalf("cursor after page %d = %d, want last returned seq %d", i, cursor, seqs[i])
		}
	}
}

func parseCursor(t *testing.T, out map[string]any) int64 {
	t.Helper()
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	var payload struct {
		Cursor int64 `json:"cursor"`
	}
	if len(content) == 0 {
		t.Fatalf("no content: %v", out)
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("cursor payload: %v (%s)", err, text)
	}
	return payload.Cursor
}

func TestMCPHTTPEventsScanBudgetAdvancesCursor(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	// Far more foreign rows than any per-request budget (10 pages),
	// then one authorized event at the very end.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	lastForeign := seedForeignEvents(t, eventsScanBudgetPages*maxOrchPage+1)
	seedAuthEvent(t, "authorized-after-budget")
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	code, out := f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 10}))
	if code != http.StatusOK {
		t.Fatalf("budget request: %d", code)
	}
	text := mcpHTTPText(out)
	if strings.Contains(text, "authorized-after-budget") {
		t.Fatal("authorized event beyond the scan budget must not be returned in one request")
	}
	cursor := parseCursor(t, out)
	if cursor == 0 {
		t.Fatal("budget-exhausted response must carry an advancing continuation cursor, got 0")
	}
	if cursor <= 0 || cursor > lastForeign {
		t.Fatalf("continuation cursor %d out of scanned range (last foreign %d)", cursor, lastForeign)
	}
	// Continuing from the cursor eventually reaches the authorized event.
	code, out = f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 10, "after_seq": cursor}))
	if code != http.StatusOK || !strings.Contains(mcpHTTPText(out), "authorized-after-budget") {
		t.Fatalf("continuation must reach authorized event: %d %s", code, mcpHTTPText(out))
	}
}

func TestMCPHTTPStatusLatestScopedEvent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	authSeq := seedAuthEvent(t, "old-authorized")
	// 501 foreign events NEWER than the authorized one: a
	// newest-window-only lookup finds no authorized event; the
	// bounded backward scan must still name authSeq.
	seedForeignEvents(t, maxOrchPage+1)
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	code, out := f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_status", map[string]any{}))
	if code != http.StatusOK {
		t.Fatalf("status: %d", code)
	}
	text := mcpHTTPText(out)
	want := fmt.Sprintf(`"latest_event_seq":%d`, authSeq)
	if !strings.Contains(text, want) {
		t.Fatalf("status must name latest authorized seq %d: %s", authSeq, text)
	}
}


package main

// event_limit_test.go: bounded event-response limits (failing-first).
//
// Omitted/zero limit defaults to 100, limits above 500 clamp to
// 500 — normalized before scoped/unscoped paths diverge, for
// HTTP scoped, fleet-authorized, and local stdio readers alike.

import (
	"fmt"
	"strings"
	"testing"
)

func seedNEvents(t *testing.T, prefix string, n int) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO orchestration_events
			(run_id, origin_principal, origin_harness, muse_session, target_agent, host_id,
			 task_id, attempt_no, event_type, request_id, payload, disposition, created)
			VALUES ('', 'sharedA', '', '', 'metis', '', '', 0, 'worker.reported', '', ?, 'filed', ?)`,
			prefix+fmt.Sprint(i), nowUTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func countEventsPayload(t *testing.T, res mcpToolResult) int {
	t.Helper()
	text := res.Content[0].Text
	// Count payload markers of the seeded prefix.
	return strings.Count(text, `"payload":"L-`)
}

// claimSharedA seeds the sharedA owner for HTTP-scoped reads.
func claimSharedA(t *testing.T) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
}

func TestMCPEventsLimitDefaultsAndClamps(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	claimSharedA(t)
	seedNEvents(t, "L-", 600)
	// Omitted limit -> 100 (stdio unscoped path).
	res, err := mcpToolEventsScope(rawOrEmpty(t, `{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content[0].Text, `"payload":"L-`); n != 100 {
		t.Fatalf("omitted limit must default to 100, got %d events", n)
	}
	// Zero limit -> 100.
	res, err = mcpToolEventsScope(rawOrEmpty(t, `{"limit":0}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content[0].Text, `"payload":"L-`); n != 100 {
		t.Fatalf("zero limit must default to 100, got %d", n)
	}
	// Above 500 clamps to 500.
	res, err = mcpToolEventsScope(rawOrEmpty(t, `{"limit":9999}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content[0].Text, `"payload":"L-`); n != 500 {
		t.Fatalf("limit 9999 must clamp to 500, got %d", n)
	}
	// Valid limit preserved exactly.
	res, err = mcpToolEventsScope(rawOrEmpty(t, `{"limit":7}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(res.Content[0].Text, `"payload":"L-`); n != 7 {
		t.Fatalf("limit 7 must return exactly 7, got %d", n)
	}
}

func rawOrEmpty(t *testing.T, s string) []byte {
	t.Helper()
	return []byte(s)
}

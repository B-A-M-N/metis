package main

// hierarchy_lineage_test.go: package 5 failing-first (parent-turn
// lineage, observe-only).
//
// worker.reported must join its task attempt to the native session
// that executed it: OrchEvent.muse_session carries the turn intent's
// native_id. Today no producer sets the column, so the feed cannot
// join a report (or a later child turn) to its native session.

import (
	"testing"
)

func TestWorkerReportedCarriesNativeSession(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "lin")
	seedOwnedSession(t, "w", "lin")
	tk, err := createTask("run-lin", "w", "do lineage", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureTurnIntent(tk.ID, got.Attempt, "w", "nativesess-1", "", "echo", t.TempDir(), "do lineage", testReq("orch")); err != nil {
		t.Fatalf("turn intent: %v", err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "lineage done", Source: SourceWorkerClaim, Reporter: "w"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	events, err := listOrchEvents("run-lin", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.EventType == OrchWorkerReported && e.TaskID == tk.ID {
			if e.MuseSession != "nativesess-1" {
				t.Fatalf("worker.reported muse_session = %q, want nativesess-1", e.MuseSession)
			}
			return
		}
	}
	t.Fatal("no worker.reported event for the task")
}

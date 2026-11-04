package main

// scoped_regression_test.go: read-scope regression defects (failing-first).
//
// 1. Project inspection must filter sessions, tasks, AND runs by
//    ownership before constructing the response (previously tasks
//    and runs leaked across controllers sharing a workdir).
// 2. Scoped event pagination must scan forward with a continuation
//    cursor past inaccessible rows (previously a 500-row window of
//    foreign events stalled authorized readers at cursor 0).

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func seedSharedWorkdir(t *testing.T) string {
	t.Helper()
	work := t.TempDir()
	reg := loadRegistry()
	reg["sharedA"] = Session{Name: "sharedA", Workdir: work, Owner: "jwt:controller-a"}
	reg["sharedB"] = Session{Name: "sharedB", Workdir: work, Owner: "jwt:controller-b"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedB", "jwt:controller-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := createTask("run-shared-a", "sharedA", "objective-alpha-secret", "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := createTask("run-shared-b", "sharedB", "objective-beta-secret", "done"); err != nil {
		t.Fatal(err)
	}
	return work
}

func TestMCPHTTPProjectsInspectScoped(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	work := seedSharedWorkdir(t)
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	tokB := f.mint("controller-b", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	code, out := f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_projects", map[string]any{"workdir": work}))
	if code != http.StatusOK {
		t.Fatalf("projects inspect B: %d %v", code, out)
	}
	text := mcpHTTPText(out)
	for _, leak := range []string{"sharedA", "objective-alpha-secret", "run-shared-a"} {
		if strings.Contains(text, leak) {
			t.Fatalf("controller B must not see A's inspect data (%q): %s", leak, text)
		}
	}
	for _, want := range []string{"sharedB", "objective-beta-secret", "run-shared-b"} {
		if !strings.Contains(text, want) {
			t.Fatalf("controller B must see its own inspect data (%q): %s", want, text)
		}
	}
	// List form: A's workdir project must not be exposed via B's sessions either.
	_, out = f.call(tokB, "2024-11-05", mcpCallBody(1, "metis_projects", map[string]any{}))
	if strings.Contains(mcpHTTPText(out), "sharedA") {
		t.Fatalf("controller B list must not contain A's session: %s", mcpHTTPText(out))
	}
}

func TestMCPHTTPEventsScopedPagination(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	// 501 foreign events for controller B (session sharedB owned by B),
	// then one authorized worker event for controller A.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedB", "jwt:controller-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := claimProvisionOwner(db, "sharedA", "jwt:controller-a"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for i := 0; i < maxOrchPage+1; i++ {
		if _, err := recordOrchEvent(OrchEvent{
			OriginPrincipal: "sharedB", TargetAgent: "metis",
			EventType: OrchWorkerReported, Payload: "foreign-payload", Disposition: "filed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := recordOrchEvent(OrchEvent{
		OriginPrincipal: "sharedA", TargetAgent: "metis",
		EventType: OrchWorkerReported, Payload: "authorized-payload", Disposition: "filed",
	}); err != nil {
		t.Fatal(err)
	}
	tokA := f.mint("controller-a", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	_, out := f.call(tokA, "2024-11-05", mcpCallBody(1, "metis_events", map[string]any{"limit": 10}))
	text := mcpHTTPText(out)
	if !strings.Contains(text, "authorized-payload") {
		t.Fatalf("authorized controller must retrieve its event past 500 foreign rows: %s", text[:min(500, len(text))])
	}
	if strings.Contains(text, "foreign-payload") {
		t.Fatalf("foreign event content must not leak: %s", text[:min(500, len(text))])
	}
}

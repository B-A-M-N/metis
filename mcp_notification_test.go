package main

// mcp_notification_test.go: P0-D — a JSON-RPC notification (a
// tools/call frame with no request id) must never execute a tool.
// There is nobody to receive the result, so executing it would be a
// fire-and-forget mutation: unauditable and uncorrelatable.

import (
	"testing"
)

// TestMCPNotificationToolsCallPerformsNoMutation feeds mcpHandle a
// notification-form tools/call for a state-changing tool and
// requires: no reply AND no state change. The same call WITH an id
// must execute (control arm), proving the test distinguishes
// refused-notification from broken-tool.
func TestMCPNotificationToolsCallPerformsNoMutation(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	args := `{"run":"r-notif","session":"w","objective":"x","actor":"a","request_id":"n1","key":"nk1"}`
	notif := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"metis_delegate","arguments":` + args + `}}`
	if reply := mcpHandle([]byte(notif)); reply != nil {
		t.Fatalf("notification tools/call must yield no reply, got %s", reply)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 0 {
		t.Fatalf("notification tools/call executed a mutation: %d task rows", n)
	}

	// Control: the identical call WITH an id executes and leaves a row.
	req := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"metis_delegate","arguments":` + args + `}}`
	if reply := mcpHandle([]byte(req)); reply == nil {
		t.Fatal("identified tools/call must yield a reply")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM tasks`); n != 1 {
		t.Fatalf("control call must leave exactly one task row, got %d", n)
	}
}

// TestMCPNotificationUnknownMethodStaysSilent pins the adjacent,
// already-correct behavior: unknown methods without an id stay
// silent (no fabricated reply).
func TestMCPNotificationUnknownMethodStaysSilent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if reply := mcpHandle([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`)); reply != nil {
		t.Fatalf("unknown-method notification must yield no reply, got %s", reply)
	}
}

package main

import (
	"testing"
	"time"
)

// backdateFleetHeartbeat forces a stale heartbeat without sleeping.
func backdateFleetHeartbeat(t *testing.T, nodeID string, ns int64) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE fleet_nodes SET last_heartbeat_ns = ? WHERE node_id = ?`, ns, nodeID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// Enrollment mints a stable node identity that heartbeats keep
// alive. No node, no fleet routing.
func TestFleetNodeEnrollHeartbeatAlive(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	id, err := enrollFleetNode("host-a", `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if id == "" {
		t.Fatal("empty node id")
	}
	if err := heartbeatFleetNode(id); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	nodes, err := listFleetNodes(10 * time.Minute)
	if err != nil {
		t.Fatalf("listFleetNodes: %v", err)
	}
	found := false
	for _, n := range nodes {
		if n.NodeID == id {
			found = true
			if !n.Alive {
				t.Fatalf("fresh heartbeat not alive: %+v", n)
			}
			if n.Host != "host-a" {
				t.Fatalf("host = %q, want host-a", n.Host)
			}
		}
	}
	if !found {
		t.Fatalf("enrolled node %q not listed", id)
	}
}

// A heartbeat for an unknown node is refused, not auto-registered:
// identity comes from enrollment, never from first contact.
func TestFleetNodeUnknownHeartbeatRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := heartbeatFleetNode("node-does-not-exist"); err == nil {
		t.Fatal("heartbeat for unknown node succeeded, want refusal")
	}
}

// A node whose heartbeat lapsed stays listed but is not alive.
// Stale liveness must never read as ready.
func TestFleetNodeStaleNotAlive(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	id, err := enrollFleetNode("host-stale", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	backdateFleetHeartbeat(t, id, time.Now().Add(-time.Hour).UnixNano())
	nodes, err := listFleetNodes(10 * time.Minute)
	if err != nil {
		t.Fatalf("listFleetNodes: %v", err)
	}
	for _, n := range nodes {
		if n.NodeID == id && n.Alive {
			t.Fatalf("stale node reads alive: %+v", n)
		}
	}
}

// Two enrollments never share an identity, even for the same host.
func TestFleetNodeEnrollDistinctIdentity(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	a, err := enrollFleetNode("host-same", `{}`)
	if err != nil {
		t.Fatalf("enroll a: %v", err)
	}
	b, err := enrollFleetNode("host-same", `{}`)
	if err != nil {
		t.Fatalf("enroll b: %v", err)
	}
	if a == b {
		t.Fatalf("duplicate node identity %q for two enrollments", a)
	}
}

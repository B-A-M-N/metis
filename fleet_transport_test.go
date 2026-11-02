package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Routing to an unknown node is refused: the fleet never executes
// against an identity it did not enroll.
func TestFleetRouteUnknownNodeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if _, err := fleetRouteSession("node-missing", "any-session", 10*time.Minute); err == nil {
		t.Fatal("routed to unknown node, want refusal")
	} else if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("error = %q, want unknown-node refusal", err)
	}
}

// A stale node is known but not routable: liveness gates routing,
// and the error must say so.
func TestFleetRouteStaleNodeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	backdateFleetHeartbeat(t, id, time.Now().Add(-time.Hour).UnixNano())
	if _, err := fleetRouteSession(id, "any-session", 10*time.Minute); err == nil {
		t.Fatal("routed to stale node, want refusal")
	} else if !strings.Contains(err.Error(), "stale") {
		t.Fatalf("error = %q, want stale-liveness refusal", err)
	}
}

// A live node on another host is not silently executed locally:
// remote transport does not exist yet, so routing must refuse and
// name the missing leg.
func TestFleetRouteRemoteNodeRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	id, err := enrollFleetNode("definitely-not-this-host", `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := heartbeatFleetNode(id); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	if _, err := fleetRouteSession(id, "any-session", 10*time.Minute); err == nil {
		t.Fatal("routed to remote node, want refusal")
	} else if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("error = %q, want remote-transport refusal", err)
	}
}

// A live local node delegates to the session backend: with a bogus
// session the local resolver's own error must surface, proving the
// request reached the local leg rather than being swallowed.
func TestFleetRouteLocalNodeDelegatesToSession(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := heartbeatFleetNode(id); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	if _, err := fleetRouteSession(id, "session-does-not-exist", 10*time.Minute); err == nil {
		t.Fatal("bogus session routed, want resolver error")
	} else if !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("error = %q, want local resolver error", err)
	}
}

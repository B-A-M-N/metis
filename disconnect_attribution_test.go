package main

// disconnect_attribution_test.go: P0 disconnect attribution (failing-first).
//
// A process-global last-principal must never attribute a disconnect:
// HTTP server shutdown is not a client's disconnect, one controller's
// disconnect must not mark another controller disconnected, and a
// reconnected/live lease recovers the connection projection.
// Stdio disconnects bind to the authenticated identity of that
// specific transport session; HTTP disconnects are observed only
// via presence-lease expiry, never inferred.

import (
	"net/http"
	"testing"
	"time"
)

// Interleaved stdio sessions: each session's disconnect names ITS
// authenticated principal, even when requests interleave.
func TestStdioSessionDisconnectAttribution(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	sessA, sessB := newStdioSession(), newStdioSession()
	sessA.recordPrincipal("jwt:controller-a")
	sessB.recordPrincipal("jwt:controller-b")
	// Interleave: B acts after A, so a global last-principal would
	// wrongly attribute A's disconnect to B.
	sessA.emitDisconnect()
	sessB.emitDisconnect()
	evs := orchEventsOfType(t, OrchTransportDisconnected)
	if len(evs) != 2 {
		t.Fatalf("disconnect events = %d, want 2", len(evs))
	}
	origins := map[string]bool{}
	for _, e := range evs {
		origins[e.OriginPrincipal] = true
		if e.Payload != "mcp-local" {
			t.Fatalf("stdio disconnect payload must name the transport, got %q", e.Payload)
		}
	}
	if !origins["jwt:controller-a"] || !origins["jwt:controller-b"] {
		t.Fatalf("disconnects must be attributed per session, got %v", origins)
	}
	// Unauthenticated session: no disconnect event (nothing to attribute).
	sessC := newStdioSession()
	sessC.emitDisconnect()
	if n := len(orchEventsOfType(t, OrchTransportDisconnected)); n != 2 {
		t.Fatalf("unauthenticated disconnect must emit nothing, total %d", n)
	}
}

// An authenticated HTTP request completing must NOT record a
// disconnect: HTTP is per-request stateless; disconnect is only
// lease expiry. Shutdown/request-completion infer nothing.
func TestHTTPRequestsEmitNoDisconnect(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, nil)
	code, _ := f.call(f.goodToken(), "2024-11-05", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	// A second call (another completed request) still emits nothing.
	code, _ = f.call(f.goodToken(), "2024-11-05", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{},
	})
	if code != http.StatusOK {
		t.Fatalf("list2: %d", code)
	}
	if n := len(orchEventsOfType(t, OrchTransportDisconnected)); n != 0 {
		t.Fatalf("HTTP request completion must never emit disconnect, got %d", n)
	}
}

// One controller's disconnect must not mark another controller with
// a live lease disconnected; a same-principal reconnect (lease
// newer than the disconnect) recovers; expiry yields absent, not down.
func TestDisconnectPresenceRecovery(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	// Controller B holds a live lease; controller A disconnects.
	if err := recordMusePresence("jwt:controller-b", "mcp-http", time.Hour); err != nil {
		t.Fatal(err)
	}
	sessA := newStdioSession()
	sessA.recordPrincipal("jwt:controller-a")
	sessA.emitDisconnect()
	f := collectMuseFacts()
	if f.Lease != MuseLeaseLive {
		t.Fatalf("B's lease must be live, got %v", f.Lease)
	}
	if f.Connection == MuseConnDown {
		t.Fatal("A's disconnect must not mark live-lease controller B disconnected")
	}
	// Same-principal reconnection via HTTP: lease newer than
	// the disconnect recovers (HTTP has no session rows).
	if err := recordMusePresence("jwt:controller-a", "mcp-http", time.Hour); err != nil {
		t.Fatal(err)
	}
	f = collectMuseFacts()
	if f.Connection == MuseConnDown {
		t.Fatal("reconnected principal (lease newer than disconnect) must recover connection")
	}
	// Same-principal reconnection via stdio: a NEW session row,
	// not a refreshed principal lease, recovers.
	sessR := newStdioSession()
	sessR.recordPrincipal("jwt:controller-a")
	if f := collectMuseFacts(); f.Connection == MuseConnDown {
		t.Fatal("reconnected stdio session must recover connection via session registry")
	}
	_ = sessR
	// Lease expiry: expired-lease principal yields Absent, never Down.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE muse_presence_leases SET expires_ns = 1 WHERE principal = 'jwt:controller-b'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	f = collectMuseFacts()
	if f.Connection == MuseConnDown {
		t.Fatal("expired lease must yield absent/unknown, never disconnected-down")
	}
}

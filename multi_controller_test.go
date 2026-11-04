package main

// multi_controller_test.go: concurrent-controller presence (failing-first).
//
// Two stdio sessions sharing the local-operator principal: B stays
// connected but idle, A disconnects. The global connection
// projection must NOT report Down while B's session is active.
// Transport-session identity is distinct from principal identity.

import (
	"testing"
	"time"
)

func TestSamePrincipalIdleControllerSurvivesPeerDisconnect(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	sessA, sessB := newStdioSession(), newStdioSession()
	sessB.recordPrincipal("local-operator") // B authenticates, then goes idle
	sessA.recordPrincipal("local-operator") // A authenticates
	// A disconnects (EOF). B remains connected, idle.
	sessA.emitDisconnect()

	f := collectMuseFacts()
	if f.Connection == MuseConnDown {
		t.Fatal("B idle-but-connected must not be marked disconnected by A's disconnect")
	}
	// B's session is the surviving active session in the durable registry.
	if n := activeStdioSessions(); n != 1 {
		t.Fatalf("active stdio sessions = %d, want 1 (B)", n)
	}
	// B disconnects too: now Down is legitimate.
	sessB.emitDisconnect()
	f = collectMuseFacts()
	if f.Connection != MuseConnDown {
		t.Fatalf("both sessions disconnected must project Down, got %v", f.Connection)
	}
	if n := activeStdioSessions(); n != 0 {
		t.Fatalf("active stdio sessions after both disconnect = %d, want 0", n)
	}
}

func TestDifferentPrincipalsDisconnectIsolation(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	sessA, sessB := newStdioSession(), newStdioSession()
	sessA.recordPrincipal("jwt:controller-a")
	sessB.recordPrincipal("jwt:controller-b")
	sessB.recordPrincipal("jwt:controller-b") // B refreshes, stays connected
	sessA.emitDisconnect()
	if f := collectMuseFacts(); f.Connection == MuseConnDown {
		t.Fatal("A's disconnect must not mark B (different principal, active session) disconnected")
	}
	evs := orchEventsOfType(t, OrchTransportDisconnected)
	if len(evs) != 1 || evs[0].OriginPrincipal != "jwt:controller-a" {
		t.Fatalf("exactly A's disconnect, attributed to A: %+v", evs)
	}
}

func TestUnauthenticatedAndExpiredSessions(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	// Unauthenticated session: no event, no session row.
	sessU := newStdioSession()
	sessU.emitDisconnect()
	if n := len(orchEventsOfType(t, OrchTransportDisconnected)); n != 0 {
		t.Fatalf("unauthenticated session must emit nothing, got %d", n)
	}
	if n := activeStdioSessions(); n != 0 {
		t.Fatalf("unauthenticated session must register no row, got %d", n)
	}
	// Expired session row (crash without disconnect): bounded stale
	// window, then absent — never a fabricated Down from expiry.
	sessX := newStdioSession()
	sessX.recordPrincipal("local-operator")
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE muse_transport_sessions SET expires_ns = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if n := activeStdioSessions(); n != 0 {
		t.Fatalf("expired session rows must prune, got %d", n)
	}
	if f := collectMuseFacts(); f.Connection == MuseConnDown {
		t.Fatal("session expiry must yield absent/unknown, never disconnected-down")
	}
}

func TestInterleavedSessionsNoContamination(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	sessA, sessB := newStdioSession(), newStdioSession()
	// Interleave concurrent requests: A, B, A, B.
	sessA.recordPrincipal("local-operator")
	sessB.recordPrincipal("local-operator")
	sessA.recordPrincipal("local-operator")
	sessB.recordPrincipal("local-operator")
	if n := activeStdioSessions(); n != 2 {
		t.Fatalf("interleaved sessions must register independently, got %d", n)
	}
	// Concurrent disconnects (B first, then A).
	sessB.emitDisconnect()
	if f := collectMuseFacts(); f.Connection == MuseConnDown {
		t.Fatal("A still active after B's disconnect: must not be Down")
	}
	sessA.emitDisconnect()
	if f := collectMuseFacts(); f.Connection != MuseConnDown {
		t.Fatalf("both disconnected (interleaved): must be Down, got %v", f.Connection)
	}
	evs := orchEventsOfType(t, OrchTransportDisconnected)
	if len(evs) != 2 {
		t.Fatalf("two disconnect events, got %d", len(evs))
	}
	for _, e := range evs {
		if e.OriginPrincipal != "local-operator" {
			t.Fatalf("disconnect origin contaminated: %q", e.OriginPrincipal)
		}
	}
	// Reconnection: a fresh session recovers the projection.
	sessC := newStdioSession()
	sessC.recordPrincipal("local-operator")
	if f := collectMuseFacts(); f.Connection == MuseConnDown {
		t.Fatal("reconnected session must recover connection projection")
	}
	_ = time.Now()
}

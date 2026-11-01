package main

import (
	"testing"
	"time"
)

// A live authenticated lease with no recent traffic is Connected,
// not Idle: the orchestrator is present even when no turn is in
// flight.
func TestMusePresenceLeaseHoldsConnected(t *testing.T) {
	if err := recordMusePresence("test-principal", "mcp", 10*time.Minute); err != nil {
		t.Fatalf("recordMusePresence: %v", err)
	}
	state, _, _ := museActivity()
	if state != MuseConnected {
		t.Fatalf("lease alive, no traffic: state = %q, want %q", state, MuseConnected)
	}
}

// An expired lease is not presence: with no work and no traffic the
// indicator must fall back to Idle, not linger Connected.
func TestMusePresenceExpiredLeaseFallsBack(t *testing.T) {
	if err := recordMusePresence("stale-principal", "mcp", -time.Minute); err != nil {
		t.Fatalf("recordMusePresence: %v", err)
	}
	// The test database is shared across tests in this package, so
	// other tests' live leases may exist; assert only that the
	// expired principal itself was pruned.
	for _, p := range activeMusePresence() {
		if p.Principal == "stale-principal" {
			t.Fatalf("expired lease still active: %+v", p)
		}
	}
}

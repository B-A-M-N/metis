package main

import (
	"testing"
	"time"
)

// An authenticated HTTP call must record controller presence under
// the verified JWT subject — otherwise a working cloud controller
// never lights the Muse tab. Unauthenticated calls record nothing.
func TestMCPHTTPRecordsControllerPresence(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, nil)
	tok := f.mint("chatgpt-1", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	if code, _ := f.call(tok, "2024-11-05", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping", "params": map[string]any{}}); code != 200 {
		t.Fatalf("ping: %d", code)
	}
	found := false
	for _, p := range activeMusePresence() {
		if p.Principal == "jwt:chatgpt-1" && p.Source == "mcp-http" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no mcp-http lease for jwt:chatgpt-1: %+v", activeMusePresence())
	}
	// A refused call leaves no lease behind.
	bad := f.mint("mallory", f.res, "metis:message", time.Now().Add(-time.Hour), nil)
	f.call(bad, "2024-11-05", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping", "params": map[string]any{}})
	for _, p := range activeMusePresence() {
		if p.Principal == "jwt:mallory" {
			t.Fatalf("refused caller gained presence: %+v", p)
		}
	}
}

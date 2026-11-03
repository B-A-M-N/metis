package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Principal-scoped controller policy: a provision binds its
// worker to the verified JWT subject; remote session-targeting
// tools admit only that owner. Leases arbitrate, they do not
// authorize. Local transport bypasses the check.

func TestProvisionOwnerRecordLookup(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, found, err := lookupProvisionOwner(db, "ghost"); err != nil || found {
		t.Fatalf("missing row must report not-found, got found=%v err=%v", found, err)
	}
	if other, err := claimProvisionOwner(db, "own1", "jwt:op-1"); err != nil || other {
		t.Fatalf("record: %v", err)
	}
	owner, found, err := lookupProvisionOwner(db, "own1")
	if err != nil || !found || owner != "jwt:op-1" {
		t.Fatalf("lookup must return the recording principal, got %q found=%v err=%v", owner, found, err)
	}
	// PTY display names normalize to the same binding.
	owner, found, err = lookupProvisionOwner(db, "pty:own1")
	if err != nil || !found || owner != "jwt:op-1" {
		t.Fatalf("pty:-prefixed lookup must hit the same binding, got %q found=%v err=%v", owner, found, err)
	}
	// A rival claim loses without rebinding: first writer wins,
	// so a conflicting admission refuses instead of silently
	// reassigning ownership.
	if other, err := claimProvisionOwner(db, "pty:own1", "jwt:op-2"); err != nil || !other {
		t.Fatalf("rival claim must report owned-by-other, got other=%v err=%v", other, err)
	}
	owner, found, err = lookupProvisionOwner(db, "own1")
	if err != nil || !found || owner != "jwt:op-1" {
		t.Fatalf("lost claim race must not rebind the owner, got %q found=%v err=%v", owner, found, err)
	}
}

func TestMCPHTTPOwnerPolicy(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedLiveSelfPtyRow(t, "own1")
	seedLiveSelfPtyRow(t, "orphan")
	seedPtyRow(t, "dead1", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if other, err := claimProvisionOwner(db, "own1", "jwt:op-1"); err != nil || other {
		t.Fatal(err)
	}
	if other, err := claimProvisionOwner(db, "dead1", "jwt:op-1"); err != nil || other {
		t.Fatal(err)
	}
	db.Close()

	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin,tasks"})
	scopes := "metis:provision metis:input metis:stop metis:message"
	tok1 := f.mint("op-1", f.res, scopes, time.Now().Add(time.Hour), nil)
	tok2 := f.mint("op-2", f.res, scopes, time.Now().Add(time.Hour), nil)
	call := func(tok string, id any, name string, args map[string]any) map[string]any {
		_, out := f.call(tok, "2024-11-05", mcpCallBody(id, name, args))
		return out
	}
	rawOf := func(out map[string]any) string {
		raw, _ := json.Marshal(out)
		return string(raw)
	}

	// Owner reads its own worker.
	out := call(tok1, 1, "metis_terminal", map[string]any{"session": "pty:own1", "op": "status"})
	if raw := rawOf(out); !strings.Contains(raw, "own1") {
		t.Fatalf("owner must observe its own worker, got: %s", raw)
	}
	// A different controller — valid JWT, full scopes, full
	// capabilities — is refused on read, input, send, and stop.
	out = call(tok2, 2, "metis_terminal", map[string]any{"session": "pty:own1", "op": "status"})
	if raw := rawOf(out); !strings.Contains(raw, "another controller") {
		t.Fatalf("foreign read must name the ownership refusal, got: %s", raw)
	}
	openFifoReader(t, "own1")
	out = call(tok2, 3, "metis_terminal_input", map[string]any{
		"session": "pty:own1", "text": "intrude", "actor": "cloud", "request_id": "own-1"})
	if raw := rawOf(out); !strings.Contains(raw, "another controller") {
		t.Fatalf("foreign input must name the ownership refusal, got: %s", raw)
	}
	out = call(tok2, 4, "metis_send", map[string]any{
		"session": "own1", "text": "intrude", "actor": "cloud", "request_id": "own-2"})
	if raw := rawOf(out); !strings.Contains(raw, "another controller") {
		t.Fatalf("foreign send must name the ownership refusal, got: %s", raw)
	}
	out = call(tok2, 5, "metis_session_stop", map[string]any{
		"session": "pty:own1", "actor": "cloud", "request_id": "own-3"})
	if raw := rawOf(out); !strings.Contains(raw, "another controller") {
		t.Fatalf("foreign stop must name the ownership refusal, got: %s", raw)
	}
	// No owner row: even the session's seeder is refused
	// fail-closed, never grandfathered in.
	out = call(tok1, 6, "metis_terminal", map[string]any{"session": "pty:orphan", "op": "status"})
	if raw := rawOf(out); !strings.Contains(raw, "no recorded remote owner") {
		t.Fatalf("unowned session must be refused fail-closed, got: %s", raw)
	}
	// Owner input passes the ownership gate and reaches the
	// lease authority (queued through the held-open FIFO).
	out = call(tok1, 7, "metis_terminal_input", map[string]any{
		"session": "pty:own1", "text": "owner-followup", "actor": "cloud", "request_id": "own-4"})
	if raw := rawOf(out); !strings.Contains(raw, "queued") {
		t.Fatalf("owner input must pass the ownership gate, got: %s", raw)
	}
	// Owner stop ends its worker through the domain path.
	out = call(tok1, 8, "metis_session_stop", map[string]any{
		"session": "pty:dead1", "actor": "cloud", "request_id": "own-5", "generation": 0})
	if raw := rawOf(out); !strings.Contains(raw, "stopped") {
		t.Fatalf("owner stop must report stopped, got: %s", raw)
	}

	// Local transport bypasses owner scoping: the machine owner
	// retains full control, including sessions with no row.
	localRaw, _ := json.Marshal(map[string]any{"session": "pty:orphan", "op": "status"})
	res, err := mcpToolTerminal(localRaw)
	if err != nil {
		t.Fatalf("local read must bypass owner scoping, got: %v", err)
	}
	payload, _ := json.Marshal(res)
	if !strings.Contains(string(payload), "orphan") {
		t.Fatalf("local read must name the worker, got: %s", payload)
	}
}

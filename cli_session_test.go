package main

// cli_session_test.go: session list/stop CLI consistency.
//
// Text and JSON list output must agree on the session universe:
// every PTY-backed session shown in text appears in --json with
// its backend, and backend-qualified stop identities round-trip.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedPTYRow(t *testing.T, name string) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, owner, status, heartbeat, harness, model)
		VALUES (?, ?, 'sleep 60', 'o', 'running', ?, 'muse-code', 'opus')`,
		name, "/tmp/"+name, nowUTC()); err != nil {
		t.Fatal(err)
	}
}

// TestListJSONIncludesPTYBackends proves list --json covers the
// same universe as text list: the registry session and the
// PTY-backed session both appear, tagged by backend, with the
// PTY name carrying the pty: prefix so it round-trips through
// stopSession and matches the TUI selection keys.
func TestListJSONIncludesPTYBackends(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "list")
	seedOwnedSession(t, "reg1", "list")
	seedPTYRow(t, "worker1")
	code, out := captureStdout(t, func() int { return cliList([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("list --json must succeed, got code %d: %s", code, out)
	}
	var sessions []PublicSession
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("list --json must emit a session array: %v\n%s", err, out)
	}
	byName := map[string]PublicSession{}
	for _, s := range sessions {
		byName[s.Name] = s
	}
	reg, ok := byName["reg1"]
	if !ok {
		t.Fatalf("registry session missing from JSON list: %s", out)
	}
	if reg.Backend != "tmux" || reg.Status == "" {
		t.Fatalf("registry entry must carry backend tmux and a status: %+v", reg)
	}
	pty, ok := byName["pty:worker1"]
	if !ok {
		t.Fatalf("PTY session missing from JSON list (want pty:worker1): %s", out)
	}
	if pty.Backend != "pty" {
		t.Fatalf("PTY entry must carry backend pty: %+v", pty)
	}
	if pty.Status == "" || pty.Harness != "muse-code" {
		t.Fatalf("PTY entry must carry status and harness: %+v", pty)
	}
	// Text and JSON agree on names: the text list shows the same
	// pty:-qualified display name.
	_, text := captureStdout(t, func() int { return cliList(nil) })
	if !strings.Contains(text, "pty:worker1") || !strings.Contains(text, "reg1") {
		t.Fatalf("text list must show both sessions:\n%s", text)
	}
}

// TestStopQualifiedIdentitiesRoundTrip proves the qualified names
// emitted by list route back through stop: pty: names stop only
// the PTY twin and unknown qualified names fail closed.
func TestStopQualifiedIdentitiesRoundTrip(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedPTYRow(t, "solo")
	if err := stopSession("pty:solo"); err != nil {
		t.Fatalf("pty-qualified stop must route to the PTY path, got: %v", err)
	}
	if err := stopSession("tmux:solo"); err == nil {
		t.Fatal("tmux-qualified stop of a PTY-only name must fail")
	}
	if err := stopSession("pty:ghost"); err == nil {
		t.Fatal("qualified stop of an unknown PTY name must fail")
	} else if !strings.Contains(err.Error(), "no such PTY session") {
		t.Fatalf("unknown PTY stop must say so, got: %v", err)
	}
}

// TestListReportsPTYRegistryUnavailable proves an unreadable PTY
// registry fails the list instead of presenting a partial
// universe as complete.
func TestListReportsPTYRegistryUnavailable(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "list")
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_STATE", blocker)
	if code, _ := captureStdout(t, func() int { return cliList(nil) }); code != 1 {
		t.Fatalf("list with an unreadable PTY registry must exit 1, got %d", code)
	}
	if code, _ := captureStdout(t, func() int { return cliList([]string{"--json"}) }); code != 1 {
		t.Fatalf("list --json with an unreadable PTY registry must exit 1, got %d", code)
	}
}

// TestListMarksGoneWorkers proves the JSON list reports observed
// liveness: a registry row never marked stopped whose tmux
// session is gone reports "gone", never "running".
func TestListMarksGoneWorkers(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "list")
	seedOwnedSession(t, "reg1", "list")
	// Kill the tmux session behind the registry row's back: the
	// row still says running (Stopped=false).
	fakeDir := os.Getenv("FAKE_TMUX_DIR")
	if fakeDir == "" {
		t.Fatal("fake tmux dir not set")
	}
	if err := os.WriteFile(filepath.Join(fakeDir, "dead-metis-reg1"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := captureStdout(t, func() int { return cliList([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("list --json must succeed, got code %d: %s", code, out)
	}
	var sessions []PublicSession
	if err := json.Unmarshal([]byte(out), &sessions); err != nil {
		t.Fatalf("list --json must emit a session array: %v\n%s", err, out)
	}
	found := false
	for _, s := range sessions {
		if s.Name == "reg1" {
			found = true
			if s.Status != "gone" {
				t.Fatalf("absent worker must report gone, got %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("registry session missing from JSON list: %s", out)
	}
}

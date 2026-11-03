package main

// provision_pty_default_test.go: managed-PTY default for interactive
// Muse Code workers.
//
// New muse-code provisions land on the managed PTY backend unless
// tmux is explicitly requested; tmux stays supported for existing
// sessions, anything else is refused, and PTY replays only adopt a
// row recorded for the identical curated launcher.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeMuse places a stub muse executable on PATH so the
// curated launcher resolves without the real binary. The stub never
// runs: tests drive the service with a stub launcher.
func installFakeMuse(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "muse")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

type stubProvisionLauncher struct {
	last *mcpProvisionRecord
	err  error
}

func (s *stubProvisionLauncher) LaunchProvisionSession(want mcpProvisionRecord) error {
	c := want
	s.last = &c
	return s.err
}

type stubProvisionLiveness struct {
	live     bool
	verified bool
}

func (s stubProvisionLiveness) ProvisionSessionLive(mcpProvisionRecord) bool { return s.live }
func (s stubProvisionLiveness) ProvisionLaunchVerified(mcpProvisionRecord) bool {
	return s.verified
}

func testProvisionService(l *stubProvisionLauncher, live, verified bool) *ProvisionService {
	return &ProvisionService{
		OpenDB:   openTasksDB,
		Launcher: l,
		Liveness: stubProvisionLiveness{live: live, verified: verified},
		MkdirAll: os.MkdirAll,
	}
}

func TestProvisionMuseCodeDefaultsToPTY(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeMuse(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	l := &stubProvisionLauncher{}
	got, err := testProvisionService(l, false, true).Provision(ProvisionRequest{
		Actor: "orch", RequestID: "pty-def-1", Session: "mc1",
		Workdir: work, Harness: "muse-code", Provider: "echo",
	})
	if err != nil {
		t.Fatalf("muse-code provision: %v", err)
	}
	if got.Backend != provisionBackendPTY {
		t.Fatalf("muse-code must default to pty, got %q", got.Backend)
	}
	if l.last == nil || l.last.Backend != provisionBackendPTY {
		t.Fatalf("launcher must see the pty backend: %+v", l.last)
	}
	if !strings.Contains(l.last.Command, "muse") || !strings.Contains(l.last.Command, work) {
		t.Fatalf("curated launcher must name muse and the workdir: %q", l.last.Command)
	}
}

func TestProvisionMuseCodeExplicitTmuxCompat(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeMuse(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	l := &stubProvisionLauncher{}
	got, err := testProvisionService(l, false, true).Provision(ProvisionRequest{
		Actor: "orch", RequestID: "pty-tmux-1", Session: "mc2",
		Workdir: work, Backend: provisionBackendTMUX, Harness: "muse-code",
		Provider: "echo",
	})
	if err != nil {
		t.Fatalf("explicit tmux muse-code must stay supported: %v", err)
	}
	if got.Backend != provisionBackendTMUX {
		t.Fatalf("explicit tmux must be honored, got %q", got.Backend)
	}
}

func TestProvisionMuseCodeRefusesNativeBackend(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeMuse(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	l := &stubProvisionLauncher{}
	_, err := testProvisionService(l, false, true).Provision(ProvisionRequest{
		Actor: "orch", RequestID: "pty-nat-1", Session: "mc3",
		Workdir: work, Backend: provisionBackendNative, Harness: "muse-code",
		Provider: "echo",
	})
	if err == nil {
		t.Fatal("muse-code on a native backend must be refused")
	}
	if l.last != nil {
		t.Fatal("refused provision must never reach the launcher")
	}
}

func TestProvisionMuseCodeReplayConflictOnProviderChange(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeMuse(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	l := &stubProvisionLauncher{}
	svc := testProvisionService(l, false, true)
	base := ProvisionRequest{
		Actor: "orch", RequestID: "pty-conf-1", Session: "mc4",
		Workdir: work, Harness: "muse-code", Provider: "echo",
	}
	if _, err := svc.Provision(base); err != nil {
		t.Fatalf("first provision: %v", err)
	}
	changed := base
	changed.Provider = "meta"
	if _, err := svc.Provision(changed); err == nil {
		t.Fatal("reused request_id with a different provider must conflict")
	} else if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("conflict must say conflict, got: %v", err)
	}
}

func TestProvisionPtyCommandMatches(t *testing.T) {
	match := mcpProvisionRecord{Harness: "muse-code", Command: "exec muse --x"}
	if !provisionPtyCommandMatches(ptyMeta{Command: "exec muse --x"}, match) {
		t.Fatal("identical curated launcher must match")
	}
	if provisionPtyCommandMatches(ptyMeta{Command: "exec muse --other"}, match) {
		t.Fatal("a row recorded for another configuration must not match")
	}
	other := mcpProvisionRecord{Harness: "demo", Command: "a"}
	if !provisionPtyCommandMatches(ptyMeta{Command: "b"}, other) {
		t.Fatal("non-muse-code harnesses keep existing behavior")
	}
}

func TestStopSessionRoutesPtyOnlyName(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	// A PTY-only row with no live supervisor: no registry entry, no
	// tmux session. stopSession must route to the PTY stop (which
	// records gone) instead of reporting absence.
	if _, err := db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, owner, status, heartbeat, harness, model)
		VALUES ('pr1', '/tmp/pr1', 'sleep 60', 'o', 'running', ?, '', '')`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := stopSession("pr1"); err != nil {
		t.Fatalf("pty-only stop must succeed, got: %v", err)
	}
	m, err := ptyGet("pr1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "gone" {
		t.Fatalf("pty-only stop must record gone, got %q", m.Status)
	}
}

func TestStopSessionRefusesAmbiguousName(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "prec")
	seedOwnedSession(t, "both", "prec")
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, owner, status, heartbeat, harness, model)
		VALUES ('both', '/tmp/both', 'sleep 60', 'o', 'running', ?, '', '')`, nowUTC()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	markFakeTmuxDead(t)
	// A bare name owned by both backends is refused instead of
	// silently killing the tmux twin: the error names both
	// backend-qualified identities.
	if err := stopSession("both"); err == nil {
		t.Fatal("ambiguous bare name must be refused")
	} else if !strings.Contains(err.Error(), "ambiguous") ||
		!strings.Contains(err.Error(), "tmux:both") ||
		!strings.Contains(err.Error(), "pty:both") {
		t.Fatalf("ambiguity error must name both qualified identities, got: %v", err)
	}
	if got := loadRegistry()["both"]; got.Stopped {
		t.Fatal("refused ambiguous stop must leave the registry entry live")
	}
	// Backend-qualified identities route exactly. The tmux twin
	// stops through the registry (dead fake tmux: records stopped)
	// while the PTY row is untouched.
	if err := stopSession("tmux:both"); err != nil {
		t.Fatalf("tmux-qualified stop must succeed, got: %v", err)
	}
	if got := loadRegistry()["both"]; !got.Stopped {
		t.Fatalf("registry entry must be marked stopped: %+v", got)
	}
	m, err := ptyGet("both")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "running" {
		t.Fatalf("tmux-qualified stop must not touch the PTY row, got %q", m.Status)
	}
	// The PTY twin stops through the generation-fenced PTY path.
	if err := stopSession("pty:both"); err != nil {
		t.Fatalf("pty-qualified stop must succeed, got: %v", err)
	}
	m, err = ptyGet("both")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "gone" {
		t.Fatalf("pty-qualified stop must record gone, got %q", m.Status)
	}
}

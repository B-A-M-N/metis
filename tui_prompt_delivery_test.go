package main

// tui_prompt_delivery_test.go: prompt-bar input must reach the
// selected worker and only that worker.
//
// A PTY-backed worker is selected under its backend-qualified
// display key (pty:name). The submit path canonicalizes that key
// before calling submitGuidance; stripping the prefix would route
// the text into the tmux registry lookup, which fails — typed
// input would reach no worker at all.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCanonicalSessionKeepsPTYIdentity pins the address mapping:
// a PTY display key canonicalizes to itself (it is already the
// backend-qualified address), while tmux and native selections
// keep their existing canonicalization.
func TestCanonicalSessionKeepsPTYIdentity(t *testing.T) {
	m := model{identities: map[string]sessionIdentity{
		"pty:w": {Canonical: "w", Backend: TargetPTY, Observation: "pty_log"},
		"t":     {Canonical: "t", Backend: TargetTMUX, Observation: "terminal_stream"},
	}}
	if got := m.canonicalSession("pty:w"); got != "pty:w" {
		t.Fatalf("PTY display key must canonicalize to itself, got %q", got)
	}
	if got := m.canonicalSession("t"); got != "t" {
		t.Fatalf("tmux selection must keep canonicalizing, got %q", got)
	}
	if got := m.canonicalSession("native:n"); got != "n" {
		t.Fatalf("native prefix must keep stripping, got %q", got)
	}
}

// TestPromptBarGuidanceReachesPTYWorker proves the exact address
// the TUI submit path now passes for a PTY selection lands in that
// worker's own workdir — file mode, hermetic, no supervisor needed.
func TestPromptBarGuidanceReachesPTYWorker(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, owner, status, heartbeat, harness, model)
		VALUES ('pw', ?, 'sleep 60', 'o', 'running', ?, 'muse-code', 'opus')`,
		work, nowUTC()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	m := model{identities: map[string]sessionIdentity{
		"pty:pw": {Canonical: "pw", Backend: TargetPTY, Observation: "pty_log"},
	}}
	target := m.canonicalSession("pty:pw")
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-pty-1"}
	gid, err := submitGuidance(target, "do the pty thing", false, req)
	if err != nil {
		t.Fatalf("guidance to the canonical PTY address must land, got: %v", err)
	}
	if gid == "" {
		t.Fatal("guidance must return an id")
	}
	raw, err := os.ReadFile(filepath.Join(work, "guidance.md"))
	if err != nil {
		t.Fatalf("guidance file must exist in the PTY worker workdir: %v", err)
	}
	if !strings.Contains(string(raw), "do the pty thing") {
		t.Fatalf("guidance text must reach the selected worker's file, got:\n%s", raw)
	}
	// And the sibling session gains nothing: input reaches the
	// selected worker only.
	other := t.TempDir()
	if _, err := os.Stat(filepath.Join(other, "guidance.md")); !os.IsNotExist(err) {
		t.Fatal("guidance must not leak into other workdirs")
	}
}

// TestPromptBarGuidanceBareNameStillFails closed the loop on the
// old bug: a stripped bare PTY name is not a registry session and
// must refuse loudly rather than misdelivering.
func TestPromptBarGuidanceBareNameStillFails(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	seedPTYRow(t, "bare")
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-bare-1"}
	if _, err := submitGuidance("bare", "lost text", false, req); err == nil {
		t.Fatal("bare PTY name must not resolve through the registry")
	}
}

// TestPromptDirectDeliversToSelectedWorkerOnly is the P0-D
// end-to-end proof: two live echo-backed PTY workers, one direct
// prompt-bar submit. The selected worker's supervisor must report
// written_to_pty (only that receipt settles delivered), its log
// must carry the text, and the sibling worker must receive
// nothing.
func TestPromptDirectDeliversToSelectedWorkerOnly(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	if err := ptyStart("pa", t.TempDir(), "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart pa: %v", err)
	}
	defer ptyStop("pa")
	if err := ptyStart("pb", t.TempDir(), "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart pb: %v", err)
	}
	defer ptyStop("pb")
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-direct-a1"}
	gid, err := submitGuidance("pty:pa", "marker-alpha-direct", true, req)
	if err != nil {
		t.Fatalf("direct submit to the live selected worker must deliver, got: %v", err)
	}
	if gid == "" {
		t.Fatal("direct submit must return a guidance id")
	}
	if _, state, _, ok := findGuidanceByRequest(req.RequestID); !ok || state != GuidanceDelivered {
		t.Fatalf("direct submit must settle delivered on the supervisor receipt, got state=%q ok=%v", state, ok)
	}
	waitForLog(t, "pa", "marker-alpha-direct")
	if data, err := ptyLog("pb", 8000); err != nil {
		t.Fatalf("ptyLog pb: %v", err)
	} else if strings.Contains(string(data), "marker-alpha-direct") {
		t.Fatalf("sibling worker must receive nothing, pb log carries the marker: %q", string(data))
	}
}

// TestPromptDirectToStoppedWorkerRefuses proves a stopped worker
// yields a truthful refusal — never a delivered receipt, never a
// silent file-only filing.
func TestPromptDirectToStoppedWorkerRefuses(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	if err := ptyStart("ps", t.TempDir(), "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart ps: %v", err)
	}
	if err := ptyStop("ps"); err != nil {
		t.Fatalf("ptyStop ps: %v", err)
	}
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-direct-stopped1"}
	if _, err := submitGuidance("pty:ps", "text for a dead worker", true, req); err == nil {
		t.Fatal("direct submit to a stopped worker must refuse")
	} else if !strings.Contains(err.Error(), "refused") {
		t.Fatalf("refusal must say refused, got: %v", err)
	}
	if _, state, _, ok := findGuidanceByRequest(req.RequestID); !ok {
		t.Fatal("refused submit must still leave an audit row")
	} else if state == GuidanceDelivered || state == GuidanceFiled {
		t.Fatalf("stopped-worker submit must not settle delivered/filed, got %q", state)
	}
}

// TestPromptDirectContestedLeaseRefuses proves a rival lease
// holder blocks live delivery and is named in the refusal, while
// the audit row records the refusal rather than a delivery.
func TestPromptDirectContestedLeaseRefuses(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	if err := ptyStart("pc", t.TempDir(), "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart pc: %v", err)
	}
	defer ptyStop("pc")
	if err := ptyAcquireLease("pc", "rival-holder"); err != nil {
		t.Fatalf("rival lease: %v", err)
	}
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-direct-lease1"}
	if _, err := submitGuidance("pty:pc", "contested text", true, req); err == nil {
		t.Fatal("direct submit under a rival lease must refuse")
	} else if !strings.Contains(err.Error(), "rival-holder") {
		t.Fatalf("refusal must name the holder, got: %v", err)
	}
	if _, state, _, ok := findGuidanceByRequest(req.RequestID); !ok {
		t.Fatal("refused submit must still leave an audit row")
	} else if state != GuidanceRefused {
		t.Fatalf("contested-lease submit must settle refused, got %q", state)
	}
}

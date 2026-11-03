package main

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ptyEscStrip = regexp.MustCompile("\x1b\\][^\x07]*(?:\x07|\x1b\\\\)|\x1b\\[[0-9;? ]*[a-zA-Z]|\x1b[()][0-2A-Z]|\x1b[=>]|\x1b[0-9 ]+q")

// ptyScreenText returns the worker's log with control sequences
// stripped: cursor-addressed TUIs split one reply across escape
// sequences, so the raw bytes rarely contain a marker contiguously.
func ptyScreenText(t *testing.T, name string) string {
	t.Helper()
	data, err := ptyLog(name, 8000)
	if err != nil {
		t.Fatalf("ptyLog %s: %v", name, err)
	}
	return ptyEscStrip.ReplaceAllString(string(data), "")
}

func waitForScreen(t *testing.T, name, want string) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if strings.Contains(ptyScreenText(t, name), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker %s screen never showed %q", name, want)
		}
		time.Sleep(2 * time.Second)
	}
}

// TestMuseCodeEchoLifecycle qualifies the curated persistent
// interactive harness without spending inference: a muse-code
// worker on the echo provider must start in the requested
// directory, stay alive, carry its model/provider identity, accept
// follow-up input, and stop cleanly with its supervisor reaped.
func TestMuseCodeEchoLifecycle(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rmc", "session": "mc1", "objective": "echo lifecycle",
		"actor": "orch", "request_id": "mc-1", "backend": "pty",
		"workdir": work, "harness": "muse-code", "provider": "echo",
		"model": "spark",
	}))
	if err != nil {
		t.Fatalf("muse-code provision: %v", err)
	}
	if res.IsError {
		t.Fatalf("muse-code provision isError: %s", res.Content[0].Text)
	}
	// Hygiene: the supervisor daemonizes, so a failure after this
	// point must still stop what was started — otherwise the
	// orphan breaks later runs by holding the session name.
	t.Cleanup(func() { _ = ptyStop("mc1") })
	// Running with the curated launcher, not a substituted shell.
	meta, err := ptyGet("mc1")
	if err != nil {
		t.Fatal(err)
	}
	if !ptySupervisorAlive(meta) || !ptyWorkerAlive(meta) {
		t.Fatalf("muse-code worker must be alive: %+v", meta.Status)
	}
	if !strings.Contains(meta.Command, "muse") || !strings.Contains(meta.Command, "echo") {
		t.Fatalf("worker must run the curated muse echo launcher, got %q", meta.Command)
	}
	if meta.Model != "spark" {
		t.Fatalf("worker must report its model identity, got %q", meta.Model)
	}
	// Follow-up input reaches the live worker's terminal: gate on
	// the interactive prompt first (the echo TUI boots
	// asynchronously), then submit once through the human
	// prompt-bar path. Delivery settles on the supervisor receipt
	// and the echo reply renders the marker on screen.
	waitForScreen(t, "mc1", "❯")
	req := OperationRequest{Actor: "metis-operator", RequestID: "guide-mc-1"}
	if _, err := submitGuidance("pty:mc1", "follow-up-echo-marker", true, req); err != nil {
		t.Fatalf("follow-up input to the interactive worker: %v", err)
	}
	if _, state, _, ok := findGuidanceByRequest(req.RequestID); !ok || state != GuidanceDelivered {
		t.Fatalf("follow-up submit must settle delivered, got state=%q ok=%v", state, ok)
	}
	waitForScreen(t, "mc1", "follow-up-echo-marker")
	// Listed under its backend identity while alive.
	agents, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents.Content[0].Text, `"pty:mc1"`) {
		t.Fatalf("live muse-code worker missing from listing: %s", agents.Content[0].Text)
	}
	// Stop ends the worker; the supervisor must be reaped, not
	// left as a daemon orphan.
	if err := ptyStop("mc1"); err != nil {
		t.Fatalf("cleanup stop mc1: %v", err)
	}
	requireSupervisorReaped(t, meta)
}

// TestSameDirTwoEchoWorkersIsolatedSteering is the P1
// human-steering loop on real interactive workers: two muse-code
// echo workers share ONE directory (same project, same harness),
// each with a stable control identity. A human prompt-bar submit
// reaches the selected worker, is comprehended (echo reply
// rendered), and never leaks to the sibling; a rival cloud lease
// on the other worker serializes instead of interleaving; both
// survive until explicitly stopped.
func TestSameDirTwoEchoWorkersIsolatedSteering(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	provision := func(session, reqID string) {
		t.Helper()
		res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
			"run": "same", "session": session, "objective": "same-dir steering",
			"actor": "orch", "request_id": reqID, "backend": "pty",
			"workdir": work, "harness": "muse-code", "provider": "echo",
		}))
		if err != nil || res.IsError {
			t.Fatalf("provision %s: %v %v", session, err, res)
		}
	}
	provision("w1", "same-1")
	t.Cleanup(func() { _ = ptyStop("w1") })
	provision("w2", "same-2")
	t.Cleanup(func() { _ = ptyStop("w2") })
	// Both reach the interactive prompt in the same directory.
	waitForScreen(t, "w1", "❯")
	waitForScreen(t, "w2", "❯")
	// Human prompt-bar submit to the selected worker only: the
	// guidance-direct path submits with Enter, the echo worker
	// comprehends it, and the reply renders on its screen.
	req := OperationRequest{Actor: "metis-operator", RequestID: "same-human-1"}
	if _, err := submitGuidance("pty:w1", "same-dir-alpha", true, req); err != nil {
		t.Fatalf("human submit to w1: %v", err)
	}
	waitForScreen(t, "w1", "same-dir-alpha")
	if strings.Contains(ptyScreenText(t, "w2"), "same-dir-alpha") {
		t.Fatal("human input to w1 leaked onto w2's screen")
	}
	// The sibling is independently steerable.
	req2 := OperationRequest{Actor: "metis-operator", RequestID: "same-human-2"}
	if _, err := submitGuidance("pty:w2", "same-dir-beta", true, req2); err != nil {
		t.Fatalf("human submit to w2: %v", err)
	}
	waitForScreen(t, "w2", "same-dir-beta")
	if strings.Contains(ptyScreenText(t, "w1"), "same-dir-beta") {
		t.Fatal("human input to w2 leaked onto w1's screen")
	}
	// A rival cloud lease on w1 serializes the human instead of
	// interleaving: the submit refuses naming the holder. (The
	// earlier human submit still holds its own lease here, which
	// is itself the serialization working; clear it first so the
	// cloud lease is the only holder under test.)
	ptyClearLease("w1")
	if err := ptyAcquireLease("w1", "mcp:jwt:op-9:cloud"); err != nil {
		t.Fatalf("seed cloud lease: %v", err)
	}
	cReq := OperationRequest{Actor: "metis-operator", RequestID: "same-human-3"}
	if _, err := submitGuidance("pty:w1", "contested", true, cReq); err == nil {
		t.Fatal("human submit under a rival cloud lease must refuse")
	} else if !strings.Contains(err.Error(), "mcp:jwt:op-9:cloud") {
		t.Fatalf("refusal must name the lease holder, got: %v", err)
	}
	ptyClearLease("w1")
	// Both workers survive the whole exchange until stopped.
	for _, n := range []string{"w1", "w2"} {
		m, err := ptyGet(n)
		if err != nil || !ptyWorkerAlive(m) {
			t.Fatalf("worker %s must survive steering: %+v %v", n, m.Status, err)
		}
	}
	if err := ptyStop("w1"); err != nil {
		t.Fatalf("stop w1: %v", err)
	}
	if err := ptyStop("w2"); err != nil {
		t.Fatalf("stop w2: %v", err)
	}
}

package main

// guidance_native_test.go: metis_send must steer muse-native
// sessions through MSP turns, never terminal keystrokes. A native
// session with no binding is refused (not fake-filed); a bound
// session with no daemon fails closed; a bound session with a live
// daemon gets its text admitted as a queued turn.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Native sessions have no terminal/file channel their worker
// consumes: without a binding there is nowhere to steer, so the
// send is refused and recorded — never silently filed.
func TestNativeSteerWithoutBindingRefused(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "nsr")
	seedOwnedSession(t, "w", "nsr")
	reg := loadRegistry()
	e := reg["w"]
	e.Harness = "muse-native"
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	_, err := submitGuidance("w", "steer this", true, testReq("orch"))
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("native steer without binding must be refused, got %v", err)
	}
	list, err := listGuidance("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != GuidanceRefused {
		t.Fatalf("refusal must be recorded: %+v", list)
	}
}

// A bound native session with no live daemon fails closed: the
// turn is never submitted, and the row records the failure.
func TestNativeSteerWithoutDaemonFails(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("MUSEFI_DATA_HOME", t.TempDir())
	installFakeTmux(t, "nsd")
	seedOwnedSession(t, "w", "nsd")
	reg := loadRegistry()
	e := reg["w"]
	e.Harness = "muse-native"
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := bindMuseSession("w", "native-1", "c0", "echo", "musefi", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	_, err := submitGuidance("w", "steer this", true, testReq("orch"))
	if err == nil {
		t.Fatal("native steer without daemon must fail")
	}
	list, err := listGuidance("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != GuidanceFailed {
		t.Fatalf("failure must be recorded: %+v", list)
	}
}

// A bound native session with a live daemon gets the text
// admitted as a queued MSP turn: delivered with the admission
// (disposition + turn id), never terminal keystrokes.
func TestNativeSteerDeliversQueuedTurn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	installFakeTmux(t, "nso")
	seedOwnedSession(t, "w", "nso")
	reg := loadRegistry()
	e := reg["w"]
	e.Harness = "muse-native"
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	// Bind a real native session on the isolated daemon so the
	// steer has a live target.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	nativeID, cursor, err := museStartSession(ctx, h, "steer-target", loadRegistry()["w"].Workdir)
	h.Close()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := bindMuseSession("w", nativeID, cursor, "echo", "", testReq("orch")); err != nil {
		t.Fatalf("bind: %v", err)
	}
	id, err := submitGuidance("w", "steer this turn", true, testReq("orch"))
	if err != nil {
		t.Fatalf("steer: %v", err)
	}
	g, err := guidanceStatus(id)
	if err != nil {
		t.Fatal(err)
	}
	if g.State != GuidanceDelivered {
		t.Fatalf("admitted steer must settle delivered, got %+v", g)
	}
	if !strings.Contains(g.Detail, "turn=") || !strings.Contains(g.Detail, "admitted") {
		t.Fatalf("delivery must name the admission (turn id), got %q", g.Detail)
	}
}

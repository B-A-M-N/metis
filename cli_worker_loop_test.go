package main

// cli_worker_loop_test.go: the `worker-loop` CLI command must
// accept any harness with a managed execution path — argv or
// native turn — not just ManagedArgv harnesses. This exercises
// the real command entry (flag parsing, adapter lookup, gate),
// not the internal runWorkerOnce helper.

import (
	"context"
	"testing"
	"time"
)

func TestCliWorkerLoopAcceptsNativeTurnHarness(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	t.Setenv("METIS_MUSE_TURN_TIMEOUT", "2m")
	stopDaemon := startTestDaemon(t, home)
	defer stopDaemon()
	installFakeTmux(t, "nativeCLI")
	seedOwnedSession(t, "w", "nativeCLI")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	nativeID, cursor, err := museStartSession(ctx, h, "metis-qual-cli", loadRegistry()["w"].Workdir)
	h.Close()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := bindMuseSession("w", nativeID, cursor, "echo", "", testReq("orch")); err != nil {
		t.Fatalf("bind: %v", err)
	}

	tk, err := createTask("run-native-cli", "agent", "say DONE", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if rc := cliWorkerLoop([]string{
		"--session", "w",
		"--harness", "muse-native",
		"--once",
		"--requester", "test-cli",
	}); rc != 0 {
		t.Fatalf("cli worker-loop exited %d; muse-native must be accepted", rc)
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskReportReady {
		t.Fatalf("expected report_ready, got %s", got.Status)
	}
}

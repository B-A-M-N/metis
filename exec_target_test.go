package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The resolver must name a backend and prove THAT backend ready.
// Guessing from a bare session string conflates terminal liveness
// with daemon availability and native binding state.

// Unknown names resolve to nothing, never to a default backend.
func TestResolveExecTargetUnknown(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if _, err := resolveExecTarget("ghost"); err == nil ||
		!strings.Contains(err.Error(), "no such session") {
		t.Fatalf("unknown session must be refused, got %v", err)
	}
	if err := sessionBindable("ghost"); err == nil {
		t.Fatal("sessionBindable must refuse unknown sessions")
	}
}

// A pty:-prefixed name with no PTY behind it is unknown, not tmux.
func TestResolveExecTargetPtyMissing(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if _, err := resolveExecTarget("pty:ghost"); err == nil ||
		!strings.Contains(err.Error(), "no such session") {
		t.Fatalf("missing PTY must be refused, got %v", err)
	}
}

func TestResolveExecTargetDoesNotFallbackOnLogicalStoreFailure(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "exec-target")
	seedOwnedSession(t, "collision", "exec-target")
	ws, err := canonicalRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := claimLogicalSession(logicalSession{Name: "collision", Backend: TargetMuseNative,
		Workspace: ws, Provider: "echo", NativeID: "native-collision"}); err != nil || !claimed {
		t.Fatalf("seed native logical identity: claimed=%v err=%v", claimed, err)
	}
	if err := os.Remove(tasksDBPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tasksDBPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveExecTarget("collision"); err == nil || !strings.Contains(err.Error(), "logical session store is unreadable") {
		t.Fatalf("unreadable logical authority must not fall back to tmux: %v", err)
	}
}

// A plain registered session resolves to tmux when its terminal lives.
func TestResolveExecTargetTMUX(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "tok-1")
	seedOwnedSession(t, "t3", "tok-1")
	tgt, err := resolveExecTarget("t3")
	if err != nil {
		t.Fatalf("live tmux session must resolve: %v", err)
	}
	if tgt.Kind != TargetTMUX || tgt.Name != "t3" {
		t.Fatalf("want tmux target for t3, got %+v", tgt)
	}
}

// A muse-native session with a binding but no daemon must NOT fall
// back to tmux liveness: the daemon gate is the readiness proof.
func TestResolveExecTargetMuseNativeNeedsDaemon(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("MUSEFI_DATA_HOME", t.TempDir()) // no socket inside: daemon down
	reg := loadRegistry()
	reg["mx"] = Session{Name: "mx", Tmux: tmuxName("mx"), Workdir: t.TempDir(),
		Owner: "op", Harness: "muse-native"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := bindMuseSession("mx", "native-1", "c0", "echo", "musefi", testReq("op")); err != nil {
		t.Fatal(err)
	}
	_, err := resolveExecTarget("mx")
	if err == nil || !strings.Contains(err.Error(), "no live daemon") {
		t.Fatalf("native binding without daemon must be refused at dispatch, got %v", err)
	}
	if berr := sessionBindable("mx"); berr == nil {
		t.Fatal("sessionBindable must refuse native sessions with no daemon")
	}
}

// A muse-native session with a binding AND a live daemon socket
// resolves to the native backend carrying its binding identities.
// The socket is a real unix socket in an isolated store, so the
// test proves the daemon gate passes without a running daemon.
func TestResolveExecTargetMuseNative(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	store := t.TempDir()
	t.Setenv("MUSEFI_DATA_HOME", store)
	sockDir := filepath.Join(store, "muse", "session-server")
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(sockDir, "session-server.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	reg := loadRegistry()
	reg["mx"] = Session{Name: "mx", Tmux: tmuxName("mx"), Workdir: t.TempDir(),
		Owner: "op", Harness: "muse-native"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := bindMuseSession("mx", "native-7", "c0", "echo", "musefi", testReq("op")); err != nil {
		t.Fatal(err)
	}
	tgt, err := resolveExecTarget("mx")
	if err != nil {
		t.Fatalf("bound native session with live socket must resolve: %v", err)
	}
	if tgt.Kind != TargetMuseNative || tgt.NativeID != "native-7" ||
		tgt.Profile != "musefi" || tgt.Provider != "echo" {
		t.Fatalf("native target must carry binding identities, got %+v", tgt)
	}
	if err := sessionBindable("mx"); err != nil {
		t.Fatalf("sessionBindable must accept ready native target: %v", err)
	}
}

// A native harness session with NO binding must be refused, even
// when its tmux terminal is alive: tmux liveness is not a binding.
func TestResolveExecTargetNativeNeedsBinding(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "tok-1")
	reg := loadRegistry()
	reg["nb"] = Session{Name: "nb", Tmux: tmuxName("nb"), Workdir: t.TempDir(),
		Owner: "tok-1", Harness: "muse-native"}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	_, err := resolveExecTarget("nb")
	if err == nil || !strings.Contains(err.Error(), "no muse binding") {
		t.Fatalf("native harness without binding must be refused, got %v", err)
	}
}

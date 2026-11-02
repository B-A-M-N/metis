package main

// musefi_profile_test.go: package 6 slice (no-inference profile
// qualification).
//
// The musefi launcher (isolated XDG_CONFIG_HOME, real FI catalog)
// must serve a native session daemon that Metis can drive. The echo
// provider performs no inference: this proves launcher + isolated
// store + Metis commander resolution + session/start work as one
// path. A live FI-backed turn (real quota) remains unqualified by
// design and needs explicit authorization.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMuseFiProfileServesEchoSession(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (live FI profile); run without -short")
	}
	real, err := filepath.EvalSymlinks(os.ExpandEnv("$HOME/.local/bin/musefi"))
	if err != nil {
		t.Skipf("musefi launcher absent: %v", err)
	}
	bin := t.TempDir()
	if err := os.Symlink(real, filepath.Join(bin, "musefi")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	home := t.TempDir()
	t.Setenv("MUSEFI_DATA_HOME", home)
	c, err := resolveMuseCommander("musefi")
	if err != nil {
		t.Fatalf("musefi commander: %v", err)
	}
	if c.Binary != "musefi" || c.DataHome != home {
		t.Fatalf("musefi commander = %+v, want binary musefi store %s", c, home)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "musefi", "echo", []string{"XDG_DATA_HOME=" + home}, "--join")
	if err != nil {
		t.Fatalf("musefi host: %v", err)
	}
	defer h.Close()
	work := t.TempDir()
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatalf("command id: %v", err)
	}
	sid, _, err := museStartSessionWithID(ctx, h, "fi-qual", work, museSessionSelection{}, cmdID)
	if err != nil {
		t.Fatalf("musefi session/start: %v", err)
	}
	if sid == "" {
		t.Fatal("musefi served an empty native session id")
	}
}

// The daemon child must serve its commander's store, not the
// caller's ambient one: XDG_DATA_HOME pins the serve process to
// the profile root (live-found: without this the musefi daemon
// listened on ~/.local/share).
func TestMuseDaemonEnvPinsCommanderStore(t *testing.T) {
	got := museDaemonEnv(museCommander{Name: "musefi", Binary: "musefi", DataHome: "/tmp/fi-qual"})
	if len(got) != 1 || got[0] != "XDG_DATA_HOME=/tmp/fi-qual" {
		t.Fatalf("musefi commander must pin XDG_DATA_HOME, got %q", got)
	}
	if got := museDaemonEnv(museCommander{}); got != nil {
		t.Fatalf("empty DataHome must not override the environment, got %q", got)
	}
}

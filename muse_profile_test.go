package main

// muse_profile_test.go: profile-aware MSP binding. A profile names
// an isolated muse home (data + config); the binding row records
// which profile owns the native session, and turn/status refuse to
// resume a binding under a different profile or provider — the same
// fencing TD-052 demands for providers, extended to stores.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestMuseProfileResolve(t *testing.T) {
	// Ambient: today's behavior — the operator's own muse.
	amb, err := resolveMuseCommander("")
	if err != nil {
		t.Fatal(err)
	}
	if amb.Binary != "muse" || amb.DataHome != museHomeDir() {
		t.Fatalf("ambient commander must be today's muse: %+v", amb)
	}
	// musefi is a separate thing: its store comes from its own
	// environment, never from a layout Metis invents.
	store := filepath.Join(t.TempDir(), "muse-fi-store")
	t.Setenv("MUSEFI_DATA_HOME", store)
	p, err := resolveMuseCommander("musefi")
	if err != nil {
		t.Fatal(err)
	}
	if p.Binary != "musefi" || p.DataHome != store {
		t.Fatalf("musefi must resolve to its binary and its own store: %+v", p)
	}
	// Fail closed: no store, no commander.
	t.Setenv("MUSEFI_DATA_HOME", "")
	if _, err := resolveMuseCommander("musefi"); err == nil {
		t.Fatal("musefi without MUSEFI_DATA_HOME must be refused")
	}
	// Unknown labels are refused: admitting an unlisted name would
	// let a crafted label pick the executable Metis spawns.
	for _, bad := range []string{"codex", "../x", "muse;evil", "MUSEFI", "-lead"} {
		if _, err := resolveMuseCommander(bad); err == nil {
			t.Fatalf("commander %q must be refused", bad)
		}
	}
}

// A legacy binding row (bound before profiles existed) reads back
// with an empty profile: ambient, never a fabricated one.
func TestMuseBindingProfileFencing(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	seedOwnedSession(t, "w", "fence")
	req := testReq("orch")
	if err := bindMuseSession("w", "native-1", "c1", "echo", "muse-fi", req); err != nil {
		t.Fatal(err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.Profile != "muse-fi" || b.Provider != "echo" {
		t.Fatalf("binding must record profile and provider: %+v", b)
	}
	// Same profile + provider: allowed.
	if err := checkMuseBindingScope(b, "echo", "muse-fi"); err != nil {
		t.Fatalf("matching scope refused: %v", err)
	}
	// Provider switch: refused (TD-052, pre-existing).
	if err := checkMuseBindingScope(b, "meta", "muse-fi"); err == nil {
		t.Fatal("provider switch must be refused")
	}
	// Profile switch: refused — a different profile is a different
	// store, and resuming there would orphan the native session.
	if err := checkMuseBindingScope(b, "echo", "musefi-staging"); err == nil {
		t.Fatal("profile switch must be refused")
	}
	if err := checkMuseBindingScope(b, "echo", ""); err == nil {
		t.Fatal("ambient resume of a profiled binding must be refused")
	}
}

func TestMuseProfileMigration(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-profile schema exactly.
	if _, err := db.Exec(`DROP TABLE IF EXISTS muse_bindings`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE muse_bindings (
		session TEXT PRIMARY KEY, native_id TEXT NOT NULL,
		view_cursor TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL, updated TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	seedOwnedSession(t, "w", "legacy")
	if err := bindMuseSession("w", "native-9", "c9", "echo", "", testReq("orch")); err != nil {
		t.Fatalf("legacy-shaped row must migrate on write: %v", err)
	}
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	if b.NativeID != "native-9" || b.Profile != "" {
		t.Fatalf("migrated row must read back ambient: %+v", b)
	}
	if err := checkMuseBindingScope(b, "echo", ""); err != nil {
		t.Fatalf("ambient scope must match migrated row: %v", err)
	}
	if !strings.HasPrefix(b.Created, "20") {
		t.Fatalf("migrated row must carry timestamps: %+v", b)
	}
}

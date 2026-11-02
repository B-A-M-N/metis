package main

// session_registry.go: session identity persistence.
//
// The registry maps session names to their Session records. It is the
// durable half of session identity: liveness comes from tmux, ownership
// from the stamped token, and this file only loads, saves, and
// serializes cross-process read-modify-write sequences. No tmux
// control, no input, no guidance, no snapshots here. Extracted from
// session.go; behavior unchanged.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func stateDir() string {
	if d := os.Getenv("METIS_STATE"); d != "" {
		_ = os.MkdirAll(d, 0o700)
		return d
	}
	// Keep accepting the pre-rebrand override for existing scripts.
	if d := os.Getenv("TUI_DASH_STATE"); d != "" {
		_ = os.MkdirAll(d, 0o700)
		return d
	}
	home, _ := os.UserHomeDir()
	legacy := filepath.Join(home, ".local", "share", "tui-dash")
	d := filepath.Join(home, ".local", "share", "metis")
	// Reuse existing state in place; fresh installs use the Metis path.
	if _, err := os.Stat(legacy); err == nil || !os.IsNotExist(err) {
		d = legacy
	}
	_ = os.MkdirAll(d, 0o700)
	return d
}

// hardenStatePerms restricts pre-existing state to owner-only access.
// New files are created restrictive going forward (plus a restrictive
// umask in main); this pass covers state written by older versions.
func hardenStatePerms() {
	d := stateDir()
	_ = os.Chmod(d, 0o700)
	for _, p := range []string{
		registryPath(), tasksDBPath(),
		tasksDBPath() + "-wal", tasksDBPath() + "-shm",
	} {
		_ = os.Chmod(p, 0o600)
	}
	for _, sub := range []string{"streams", "pty"} {
		sd := filepath.Join(d, sub)
		_ = os.Chmod(sd, 0o700)
		entries, err := os.ReadDir(sd)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				_ = os.Chmod(filepath.Join(sd, e.Name()), 0o600)
			}
		}
	}
}

func registryPath() string { return filepath.Join(stateDir(), "sessions.json") }

func loadRegistry() map[string]Session {
	reg, _ := loadRegistryKnown()
	return reg
}

// loadRegistryKnown reports whether the registry read succeeded. A
// missing file is a genuine empty list (first run), not a failure;
// corrupt JSON or an unreadable file is a failure the caller must
// not mistake for "no sessions".
func loadRegistryKnown() (map[string]Session, bool) {
	reg := map[string]Session{}
	data, err := os.ReadFile(registryPath())
	if err != nil {
		return reg, os.IsNotExist(err)
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return map[string]Session{}, false
	}
	if reg == nil {
		reg = map[string]Session{}
	}
	return reg, true
}

func saveRegistry(reg map[string]Session) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := registryPath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, registryPath())
}

// withRegistryLock serializes cross-process registry read-modify-write
// sequences with an flock-held lock file. Load the registry fresh inside fn;
// a copy loaded before the lock may already be stale.
func withRegistryLock(fn func() error) error {
	lockPath := filepath.Join(stateDir(), "registry.lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("registry lock: %w", err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("registry lock: %w", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return fn()
}

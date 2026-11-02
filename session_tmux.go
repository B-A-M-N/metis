package main

// session_tmux.go: tmux backend transport and ownership.
//
// Raw tmux invocation, liveness probes, and the ownership checks that
// gate every destructive tmux operation. resolveOwnedTarget binds a
// verified session to its immutable tmux session ID so a session
// killed and recreated mid-operation never receives a stale action.
// Extracted from session.go; behavior unchanged.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

func tmuxName(name string) string       { return "metis-" + name }
func legacyTmuxName(name string) string { return "tui-" + name }

// sessionTmuxTarget preserves the concrete target stored in the registry.
// Records without a target probe the Metis name first, then the legacy name.
func sessionTmuxTarget(name, recorded string) string {
	if recorded != "" {
		return recorded
	}
	registry, known := loadRegistryKnown()
	if !known {
		return ""
	}
	if entry, ok := registry[name]; ok && entry.Tmux != "" {
		return entry.Tmux
	}
	for _, target := range []string{tmuxName(name), legacyTmuxName(name)} {
		if _, err := tmux("has-session", "-t", target); err == nil {
			return target
		}
	}
	return tmuxName(name)
}

func tmux(args ...string) (string, error) {
	return tmuxContext(context.Background(), args...)
}

func tmuxContext(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		// Return stdout even on error; callers check err for existence.
		return string(out), err
	}
	return string(out), nil
}

func sessionRunning(name string) bool {
	return sessionRunningContext(context.Background(), name)
}

func sessionRunningContext(ctx context.Context, name string) bool {
	if entry, ok := loadRegistry()[name]; ok && entry.Tmux != "" {
		_, err := tmuxContext(ctx, "has-session", "-t", entry.Tmux)
		return err == nil
	}
	for _, target := range []string{tmuxName(name), legacyTmuxName(name)} {
		if _, err := tmuxContext(ctx, "has-session", "-t", target); err == nil {
			return true
		}
	}
	return false
}

// sessionNameInUse checks both namespaces before launch so a stale record
// or foreign legacy session cannot be shadowed by a new Metis session.
func sessionNameInUse(name string) bool {
	for _, target := range []string{tmuxName(name), legacyTmuxName(name)} {
		if _, err := tmux("has-session", "-t", target); err == nil {
			return true
		}
	}
	return false
}

// tmuxLiveSessions lists live tmux session names in a single subprocess
// call. Fleet refresh uses this batch instead of one has-session check
// per session; on error callers fall back to per-session checks.
func tmuxLiveSessions() (map[string]bool, error) {
	return tmuxLiveSessionsContext(context.Background())
}

func tmuxLiveSessionsContext(ctx context.Context) (map[string]bool, error) {
	out, err := tmuxContext(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if n := strings.TrimSpace(line); n != "" {
			live[n] = true
		}
	}
	return live, nil
}

// resolveOwnedTarget verifies ownership and then binds the verified session
// to its immutable tmux session ID ($-prefixed). Destructive tmux operations
// must address this ID, not the replaceable session name: a session killed
// and recreated between verification and action must not receive the stale
// operation.
func resolveOwnedTarget(reg map[string]Session, name string) (Session, string, error) {
	entry, err := verifySessionOwnership(reg, name)
	if err != nil {
		return Session{}, "", err
	}
	target := sessionTmuxTarget(name, entry.Tmux)
	out, err := tmux("display-message", "-p", "-t", target, "#{session_id}")
	if err != nil {
		return Session{}, "", fmt.Errorf("cannot resolve session id for %q: %w", name, err)
	}
	id := strings.TrimSpace(out)
	if !strings.HasPrefix(id, "$") || strings.ContainsAny(id, " \t\n") {
		return Session{}, "", fmt.Errorf("cannot resolve session id for %q: bad id %q", name, out)
	}
	return entry, id, nil
}

// verifySessionOwnership ensures name refers to a live session this
// tool started. It never confers authority: a record without a
// credential is refused (use the explicit `adopt` command to claim a
// legacy session). A live session whose owner does not match (another
// tool's session sharing either managed prefix) is refused and never
// touched.
func verifySessionOwnership(reg map[string]Session, name string) (Session, error) {
	entry, ok := reg[name]
	if !ok {
		return Session{}, fmt.Errorf("no such session: %s", name)
	}
	if !sessionRunning(name) {
		return entry, nil
	}
	if entry.Owner == "" {
		return Session{}, fmt.Errorf("session %q has no ownership credential; it is read-only until explicitly adopted (see `adopt`)", name)
	}
	target := sessionTmuxTarget(name, entry.Tmux)
	live := sessionOwnerValue(target, "METIS_OWNER")
	if live == "" {
		live = sessionOwnerValue(target, "TUI_DASH_OWNER")
	}
	if live == "" || live != entry.Owner {
		return Session{}, fmt.Errorf("session %q is not owned by this tool (owner mismatch); refusing", name)
	}
	return entry, nil
}

func sessionOwnerValue(target, key string) string {
	out, err := tmux("show-environment", "-t", target, key)
	if err != nil {
		return ""
	}
	trimmed := strings.TrimSpace(out)
	prefix := key + "="
	if !strings.HasPrefix(trimmed, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
}

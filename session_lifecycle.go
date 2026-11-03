package main

// session_lifecycle.go: tmux session lifecycle — start, stop, adopt.
//
// startSession runs a reserved/start/confirm lifecycle: validation
// reserves nothing observable, the tmux session is started and
// ownership-stamped, and the registry insert confirms under the
// registry lock. A confirm failure compensates by killing the session
// just created, so a duplicate or unwritable registry can never leave
// an orphaned live tmux session behind (P0-B). stopSession and
// adoptSession keep their ownership-bound behavior. Extracted from
// session.go; behavior unchanged except the P0-B compensating
// cleanup on registry-confirm failure.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func startSession(name, workdir, command, harness, model string) error {
	return startSessionConfigured(name, workdir, command, harness, model, "", "")
}

// startSessionConfigured is the shared tmux lifecycle with display identity
// metadata recorded at the same confirmation point as the session itself.
func startSessionConfigured(name, workdir, command, harness, model, provider, profile string) error {
	// Reserve: validate everything validatable before any side effect.
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name must match [A-Za-z0-9_-]{1,64}")
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("workdir does not exist: %s", abs)
	}
	if sessionNameInUse(name) {
		return fmt.Errorf("session already running: %s", name)
	}
	guidance := filepath.Join(abs, "guidance.md")
	if _, err := os.Stat(guidance); err != nil {
		_ = os.WriteFile(guidance, []byte("# Guidance\n\nGuidance sent from Metis / orchestrator.\nThe worker reads this file; direct tmux typing is off by default.\n\n"), 0o600)
	}
	appendEvent(abs, "session_started", name+": "+command)
	// Start: tmux new-session with a shell string: use sh -c so the command
	// can be any shell command, as in the Python version.
	_, err = tmux("new-session", "-d", "-s", tmuxName(name), "-c", abs, "sh", "-c", command)
	if err != nil {
		return fmt.Errorf("tmux start failed: %w", err)
	}
	owner := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	if err := stampSessionOwner(tmuxName(name), owner); err != nil {
		_, _ = tmux("kill-session", "-t", tmuxName(name))
		return fmt.Errorf("failed to stamp session ownership: %w", err)
	}
	// Confirm: the registry insert under the lock is the atomicity
	// point. A concurrent start that won the name, or an unwritable
	// registry, fails here — after the tmux session is already live.
	entry := Session{Name: name, Tmux: tmuxName(name), Workdir: abs, Command: command, Created: time.Now().UTC().Format(time.RFC3339), Owner: owner, Harness: harness, Model: model, Provider: provider, Profile: profile}
	if err := withRegistryLock(func() error {
		locked := loadRegistry()
		if _, dup := locked[name]; dup {
			return fmt.Errorf("session already registered: %s", name)
		}
		locked[name] = entry
		return saveRegistry(locked)
	}); err != nil {
		// Compensating cleanup (P0-B): the tmux session we just
		// created and stamped is ours alone — no registry entry
		// exists for anyone else to adopt or act on — so killing it
		// by name cannot touch a foreign session.
		_, _ = tmux("kill-session", "-t", tmuxName(name))
		return err
	}
	ensureStreamPipe(name)
	return nil
}

// adoptSession explicitly claims a recorded-but-unowned live session.
// Unlike verification, adoption is a deliberate operator decision: it
// stamps a fresh token, persists it, and records an auditable event.
// It refuses unknown names, dead sessions, and sessions that already
// carry a credential (those need no adoption).
func adoptSession(name string) (Session, error) {
	// Check, stamp, and record under the registry lock so two concurrent
	// adopters cannot both stamp the same live session and diverge.
	var adopted Session
	if err := withRegistryLock(func() error {
		locked := loadRegistry()
		entry, ok := locked[name]
		if !ok {
			return fmt.Errorf("no such session: %s", name)
		}
		if entry.Owner != "" {
			return fmt.Errorf("session %q already has an ownership credential", name)
		}
		if !sessionRunning(name) {
			return fmt.Errorf("session %q is not running; nothing to adopt", name)
		}
		entry.Owner = fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
		if err := stampSessionOwner(sessionTmuxTarget(name, entry.Tmux), entry.Owner); err != nil {
			return fmt.Errorf("cannot stamp session ownership: %w", err)
		}
		locked[name] = entry
		if err := saveRegistry(locked); err != nil {
			return err
		}
		adopted = entry
		return nil
	}); err != nil {
		return Session{}, err
	}
	appendEvent(adopted.Workdir, "session_adopted", name)
	return adopted, nil
}

// stampSessionOwner writes the current and legacy tmux environment keys.
// The legacy key lets an older tui-dash process still recognize sessions
// created while both command names are in circulation.
func stampSessionOwner(target, owner string) error {
	if _, err := tmux("set-environment", "-t", target, "METIS_OWNER", owner); err != nil {
		return err
	}
	_, err := tmux("set-environment", "-t", target, "TUI_DASH_OWNER", owner)
	return err
}

// splitStopIdentity parses a backend-qualified session identity.
// "pty:foo" selects the PTY backend, "tmux:foo" selects the tmux
// registry, and a bare name selects whichever backend owns it —
// which must be exactly one (see stopSession).
func splitStopIdentity(name string) (backend, bare string) {
	if rest, ok := strings.CutPrefix(name, ptyPrefix); ok {
		return "pty", rest
	}
	if rest, ok := strings.CutPrefix(name, "tmux:"); ok {
		return "tmux", rest
	}
	return "", name
}

func stopSession(name string) error {
	// Backend-qualified identities route exactly: "pty:foo" stops
	// only the PTY session, "tmux:foo" only the registry session.
	// A bare name that exists on both backends is ambiguous and is
	// refused instead of stopping the wrong session — registry
	// precedence would silently kill the tmux twin when the
	// operator meant the PTY one.
	if backend, bare := splitStopIdentity(name); backend != "" {
		switch backend {
		case "pty":
			if _, perr := ptyGet(bare); perr != nil {
				return fmt.Errorf("no such PTY session: %s", name)
			}
			return ptyStop(bare)
		default:
			if _, ok := loadRegistry()[bare]; !ok {
				return fmt.Errorf("no such session: %s", name)
			}
			name = bare
		}
	} else if _, ok := loadRegistry()[name]; ok {
		if _, perr := ptyGet(name); perr == nil {
			return fmt.Errorf("ambiguous session %q: exists as both a tmux session and a PTY session; stop %q or %q",
				name, "tmux:"+name, ptyDisplay(name))
		}
	} else if _, perr := ptyGet(name); perr == nil {
		// PTY-backed sessions have no registry row: route them to
		// the generation-fenced PTY stop instead of reporting
		// absence.
		return ptyStop(name)
	}
	reg := loadRegistry()
	if !sessionRunning(name) {
		// Already dead: nothing to terminate. Confirm the absence
		// and record stopped so a finished worker never lingers
		// as live in the registry. No tmux addressing happens on
		// this path, so no live session can be mis-targeted; the
		// record is authoritative because the registry is written
		// only through this tool's locked paths.
		if _, ok := reg[name]; !ok {
			return fmt.Errorf("no such session: %s", name)
		}
		return markSessionStopped(reg[name])
	}
	entry, target, err := resolveOwnedTarget(reg, name)
	if err != nil {
		return err
	}
	disableStreamPipeTarget(target)
	// Terminate first and confirm the kill against the bound session ID.
	// The stopped state is recorded only after termination is confirmed, so
	// a failed kill never leaves a live session marked stopped.
	if _, err := tmux("kill-session", "-t", target); err != nil {
		return fmt.Errorf("kill session %q: %w", name, err)
	}
	for i := 0; i < 50; i++ {
		if !sessionRunning(name) {
			break
		}
		if i == 49 {
			return fmt.Errorf("session %q still running after kill", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return markSessionStopped(entry)
}

// markSessionStopped records the stopped state after termination is
// confirmed (live path) or absence is confirmed (dead path), then
// retires the session's input senders.
func markSessionStopped(entry Session) error {
	appendEvent(entry.Workdir, "session_stopped", entry.Name)
	retireInputSenders(entry.Name)
	return withRegistryLock(func() error {
		locked := loadRegistry()
		cur, ok := locked[entry.Name]
		if !ok {
			return fmt.Errorf("no such session: %s", entry.Name)
		}
		cur.Stopped = true
		locked[entry.Name] = cur
		return saveRegistry(locked)
	})
}

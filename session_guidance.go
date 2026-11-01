package main

// session_guidance.go: guidance delivery and direct terminal input.
//
// Guidance is durable first (guidance.md plus the events log) and live
// second (an explicit direct send). Direct keystrokes — guided or
// interactive — arbitrate through the single-writer lease authority
// (TD-061) and address the verified session ID, so two controllers
// never interleave on one terminal and a replaced session never
// receives a stale send. Extracted from session.go; behavior
// unchanged. The session manager stays out of task instruction
// generation, verification, and fleet placement: it only delivers
// caller-supplied text to an owned terminal.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func injectGuidance(name, text string, direct bool) error {
	workdir := ""
	if isPtySession(name) {
		mt, err := ptyGet(ptyBare(name))
		if err != nil {
			return err
		}
		workdir = mt.Workdir
	} else {
		reg := loadRegistry()
		entry, ok := reg[name]
		if !ok {
			return fmt.Errorf("no such session in registry: %s", name)
		}
		workdir = entry.Workdir
	}
	stamp := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	f, err := os.OpenFile(filepath.Join(workdir, "guidance.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(f, "\n## %s\n\n%s\n", stamp, text)
	_ = f.Close()
	kind := "guidance"
	if direct {
		kind = "guidance_direct"
	}
	appendEvent(workdir, kind, text)
	if direct {
		if isPtySession(name) {
			if _, err := ptyInput(ptyBare(name), controllerID(), text+"\n"); err != nil {
				return fmt.Errorf("guidance saved to file, but direct send refused: %w", err)
			}
			return nil
		}
		reg := loadRegistry()
		_, target, terr := resolveOwnedTarget(reg, name)
		if terr != nil {
			return fmt.Errorf("guidance saved to file, but direct send refused: %w", terr)
		}
		// CB-13: a tmux session running a known one-shot harness has
		// no live-input channel; keystrokes would vanish into a
		// process that cannot consume them. Refuse explicitly instead
		// of reporting the guidance as delivered. Unknown harnesses
		// (custom commands) keep the legacy attempt path.
		if h, herr := sessionHarness(name); herr == nil && h != "" {
			if a, aerr := lookupAdapter(h); aerr == nil && !a.AcceptsLiveInput {
				if a.OneShot {
					return fmt.Errorf("guidance saved to file, but direct send refused: harness %q is one-shot and does not consume live input", h)
				}
				return fmt.Errorf("guidance saved to file, but direct send refused: harness %q does not consume live input and cannot continue a conversation", h)
			}
		}
		// Single input writer (TD-061): guided keystrokes need the
		// same lease as interactive typing, or two controllers
		// interleave on one terminal.
		if err := tmuxAcquireLease(name, controllerID()); err != nil {
			return fmt.Errorf("guidance saved to file, but direct send refused: %w", err)
		}
		if _, err := tmux("send-keys", "-t", target, "-l", text); err != nil {
			return err
		}
		_, err = tmux("send-keys", "-t", target, "Enter")
		return err
	}
	return nil
}

// sendTerminalInput delivers keystrokes to an owned, running session.
// literal text goes via `send-keys -l` (no key parsing); named keys
// (Enter, BSpace, Up, C-c, ...) go as tmux key names. Foreign or dead
// sessions are refused before any input is sent.
func sendTerminalInput(name, literal string, keys ...string) error {
	reg := loadRegistry()
	// resolveOwnedTarget verifies ownership and binds to the live session ID;
	// a dead session fails resolution, so no separate liveness check is needed.
	_, target, err := resolveOwnedTarget(reg, name)
	if err != nil {
		return err
	}
	// Single input writer (TD-061): direct tmux keystrokes arbitrate
	// through the same lease authority as PTY input. A rival holder's
	// refusal names the holder; the TUI drops back to nav on refusal.
	if err := tmuxAcquireLease(name, controllerID()); err != nil {
		return fmt.Errorf("direct input refused: %w", err)
	}
	if literal != "" {
		if _, err := tmux("send-keys", "-t", target, "-l", literal); err != nil {
			return fmt.Errorf("send literal: %w", err)
		}
	}
	if len(keys) > 0 {
		args := append([]string{"send-keys", "-t", target}, keys...)
		if _, err := tmux(args...); err != nil {
			return fmt.Errorf("send keys: %w", err)
		}
	}
	return nil
}

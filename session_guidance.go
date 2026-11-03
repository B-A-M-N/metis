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
	// P1-F: a durable filing is only durable when the bytes and
	// the close both succeed. A full filesystem, permission
	// failure, or interrupted write must surface as a failure
	// receipt, never as a success.
	if _, err := fmt.Fprintf(f, "\n## %s\n\n%s\n", stamp, text); err != nil {
		_ = f.Close()
		return fmt.Errorf("guidance file write failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("guidance file close failed: %w", err)
	}
	kind := "guidance"
	if direct {
		kind = "guidance_direct"
	}
	appendEvent(workdir, kind, text)
	if direct {
		if isPtySession(name) {
			// Submit with Enter (\r), matching the tmux path's
			// literal-then-Enter: raw-mode TUIs read Enter as
			// \r, while \n arrives as Ctrl-J and never submits
			// the line the caller just typed.
			id, err := ptyInput(ptyBare(name), controllerID(), text+"\r")
			if err != nil {
				return fmt.Errorf("guidance saved to file, but direct send refused: %w", err)
			}
			// A queued input id proves nothing about delivery:
			// await the supervisor's typed receipt and report
			// its verdict — written, refused, or unknown —
			// instead of marking delivery at queue time.
			if derr := awaitPTYDeliveryReceipt(id); derr != nil {
				return fmt.Errorf("guidance saved to file, but live delivery %s", derr)
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

// ptyDeliveryReceiptTimeout bounds how long a direct PTY send
// waits for the supervisor's typed receipt. The supervisor is
// local and consumes fifo frames promptly; a receipt that never
// arrives is reported as unknown, never as delivered.
const ptyDeliveryReceiptTimeout = 10 * time.Second

// awaitPTYDeliveryReceipt polls the supervisor's disposition of a
// queued input id until it is written to the PTY, refused with a
// reason, or the wait expires. Only written_to_pty counts as
// delivered; an expired wait is unknown, not delivered.
func awaitPTYDeliveryReceipt(id string) error {
	deadline := time.Now().Add(ptyDeliveryReceiptTimeout)
	for {
		status, detail, err := ptyInputStatus(id)
		if err != nil {
			return fmt.Errorf("unverifiable (receipt lookup failed: %v; outcome unknown)", err)
		}
		switch status {
		case "written_to_pty":
			return nil
		case "error":
			return fmt.Errorf("refused by supervisor: %s", detail)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("unacknowledged (id %s still %s; outcome unknown)", id, status)
		}
		time.Sleep(50 * time.Millisecond)
	}
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

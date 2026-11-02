package main

// exec_target.go: one typed execution target per session name.
//
// Metis runs three backends with different lifecycles (tmux,
// managed PTY, Muse-native bindings) behind one logical session
// boundary, but dispatch and status paths pass a bare session
// string. Guessing the backend from liveness alone conflates "a
// terminal is alive" with "the daemon is up" or "a native session
// is bound". resolveExecTarget is the canonical resolver: it names
// the backend explicitly — the logical session record first when
// one exists — and verifies that backend's readiness. For
// muse-native that means the binding plus a live daemon, never just
// tmux/PTY liveness; a native-only session (no tmux row, no PTY row)
// resolves entirely from its logical record plus its binding. It
// does not replace any registry.

import (
	"fmt"
)

// Backend identities. The string values are stable for logs and
// receipts; compare against the constants, not literals.
const (
	TargetTMUX       = "tmux"
	TargetPTY        = "pty"
	TargetMuseNative = "muse-native"
)

// execTarget is one session resolved to the backend that will
// execute for it, with the identities that backend needs.
type execTarget struct {
	Kind string
	Name string
	// Muse-native only.
	NativeID string
	Profile  string
	Provider string
}

// resolveExecTarget maps a session name to its execution backend
// and proves that backend ready:
//   - A logical session record, when one exists, is the typed
//     authority: a muse-native record resolves through the Muse
//     binding (plus a live daemon) with no tmux/PTY row required,
//     and records for other backends defer to their own stores.
//   - PTY names resolve in the PTY store and must be running.
//   - Sessions whose registered harness offers native turns
//     resolve through the Muse binding and require a live daemon.
//   - Everything else resolves in the tmux registry and must be
//     a live, unstopped session.
//
// An unknown name, a stopped session, or a native binding without
// a daemon is an error, never a silent fallback to another
// backend.
func resolveExecTarget(session string) (execTarget, error) {
	ls, found, lerr := loadLogicalSession(session)
	if lerr != nil {
		return execTarget{}, fmt.Errorf("dispatch: logical session store is unreadable; refusing backend fallback: %w", lerr)
	}
	if found {
		if tgt, ok, err := resolveLogicalTarget(ls, session); err != nil || ok {
			return tgt, err
		}
	}
	if isPtySession(session) {
		if _, state, err := ptyStatus(ptyBare(session)); err != nil {
			return execTarget{}, fmt.Errorf("dispatch: no such session: %s", session)
		} else if state != "running" {
			return execTarget{}, fmt.Errorf("dispatch: session %s is %s, not running", session, state)
		}
		return execTarget{Kind: TargetPTY, Name: session}, nil
	}
	reg := loadRegistry()
	if entry, ok := reg[session]; ok {
		if a, err := lookupAdapter(entry.Harness); err == nil && a.ManagedTurn != nil {
			b, berr := getMuseBinding(session)
			if berr != nil || b.NativeID == "" {
				return execTarget{}, fmt.Errorf("dispatch: session %s runs native harness %q but has no muse binding; bind it first", session, entry.Harness)
			}
			c, cerr := resolveMuseCommander(b.Profile)
			if cerr != nil {
				return execTarget{}, cerr
			}
			if derr := museRequireDaemonFor(c); derr != nil {
				return execTarget{}, fmt.Errorf("dispatch: session %s has a muse binding but no live daemon: %v", session, derr)
			}
			return execTarget{Kind: TargetMuseNative, Name: session,
				NativeID: b.NativeID, Profile: b.Profile, Provider: b.Provider}, nil
		}
		if entry.Stopped {
			return execTarget{}, fmt.Errorf("dispatch: session %s is stopped", session)
		}
		if !sessionRunning(session) {
			return execTarget{}, fmt.Errorf("dispatch: session %s is not running", session)
		}
		return execTarget{Kind: TargetTMUX, Name: session}, nil
	}
	if b, err := getMuseBinding(session); err == nil && b.NativeID != "" {
		c, cerr := resolveMuseCommander(b.Profile)
		if cerr != nil {
			return execTarget{}, cerr
		}
		if derr := museRequireDaemonFor(c); derr != nil {
			return execTarget{}, fmt.Errorf("dispatch: session %s has a muse binding but no live daemon: %v", session, derr)
		}
		return execTarget{Kind: TargetMuseNative, Name: session,
			NativeID: b.NativeID, Profile: b.Profile, Provider: b.Provider}, nil
	}
	return execTarget{}, fmt.Errorf("dispatch: no such session: %s", session)
}

// resolveLogicalTarget resolves through a logical session record.
// It returns ok=false when the record defers to the shell stores (a
// non-native backend, whose live tmux/PTY rows stay authoritative
// over any record). A native record routes authoritatively: the
// binding plus a live daemon prove dispatchability, with no tmux
// row and no PTY row required — and no silent fallback to either
// when the binding or daemon is missing.
func resolveLogicalTarget(ls logicalSession, session string) (execTarget, bool, error) {
	if ls.Backend != TargetMuseNative {
		return execTarget{}, false, nil
	}
	if ls.State == logicalStopped {
		return execTarget{}, true, fmt.Errorf("dispatch: session %s is stopped", session)
	}
	b, berr := getMuseBinding(session)
	if berr != nil || b.NativeID == "" {
		return execTarget{}, true, fmt.Errorf("dispatch: session %s is a native session with no muse binding; bind it first", session)
	}
	if err := checkNativeLogicalScope(ls, b); err != nil {
		return execTarget{}, true, err
	}
	c, cerr := resolveMuseCommander(b.Profile)
	if cerr != nil {
		return execTarget{}, true, cerr
	}
	if derr := museRequireDaemonFor(c); derr != nil {
		return execTarget{}, true, fmt.Errorf("dispatch: session %s has a muse binding but no live daemon: %v", session, derr)
	}
	return execTarget{Kind: TargetMuseNative, Name: session,
		NativeID: b.NativeID, Profile: b.Profile, Provider: b.Provider}, true, nil
}

// checkNativeLogicalScope refuses execution when the binding no
// longer names the logical session's workspace or declared model: a
// re-homed or re-modelled session is a different execution
// environment, and turns never silently follow it. It mirrors
// checkMuseExecutionScope for names with no shell backing, where
// that check has no store to consult.
func checkNativeLogicalScope(ls logicalSession, b museBinding) error {
	if ls.Backend != TargetMuseNative {
		return fmt.Errorf("native session %s has logical backend %q; refusing execution", ls.Name, ls.Backend)
	}
	if ls.State != logicalActive {
		return fmt.Errorf("native session %s is %s; refusing execution", ls.Name, ls.State)
	}
	if ls.NativeID == "" || b.NativeID == "" || b.NativeID != ls.NativeID {
		return fmt.Errorf("dispatch: session %s bound to native session %s, logical record names %s; refusing identity swap", ls.Name, b.NativeID, ls.NativeID)
	}
	if ls.Workspace == "" || b.Workspace == "" || b.Workspace != ls.Workspace {
		return fmt.Errorf("dispatch: session %s now resolves to workspace %s, bound to %s; refusing turn on a re-homed session", ls.Name, ls.Workspace, b.Workspace)
	}
	if b.Model != ls.Model {
		return fmt.Errorf("dispatch: session %s now declares model %q, bound to %q; refusing turn on a re-modelled session", ls.Name, ls.Model, b.Model)
	}
	if ls.Provider == "" || b.Provider != ls.Provider {
		return fmt.Errorf("native session %s provider changed from %q to %q; refusing execution", ls.Name, ls.Provider, b.Provider)
	}
	if ls.Profile != b.Profile {
		return fmt.Errorf("native session %s profile changed from %q to %q; refusing execution", ls.Name, ls.Profile, b.Profile)
	}
	return nil
}

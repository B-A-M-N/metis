package main

// muse_profile.go: which commander serves MSP for a binding.
//
// Metis coordinates two separate commanders and never mixes them:
//   - "" (ambient): the operator's own `muse` binary and muse home.
//   - "musefi": the separate FI-profiled launcher. It owns its own
//     store; Metis only knows the store root through MUSEFI_DATA_HOME
//     (fail closed when unset) and invokes the binary on PATH.
//
// Metis does not own, lay out, or manage the commander's store. The
// binding row records which commander owns each native session so a
// resume under the wrong one is refused, never silently re-homed.

import (
	"fmt"
	"os"
)

// museCommander is how to reach one commander's MSP surface: the
// binary to spawn and the store root whose daemon socket proves a
// session daemon serves it.
type museCommander struct {
	Name     string `json:"name"`
	Binary   string `json:"binary"`
	DataHome string `json:"data_home"`
}

// resolveMuseCommander maps a profile label to its commander.
// Unknown labels are refused: admitting an unlisted binary would
// let a crafted label pick the executable Metis spawns.
func resolveMuseCommander(name string) (museCommander, error) {
	switch name {
	case "":
		return museCommander{Binary: "muse", DataHome: museHomeDir()}, nil
	case "musefi":
		home := os.Getenv("MUSEFI_DATA_HOME")
		if home == "" {
			return museCommander{}, fmt.Errorf("musefi profile needs MUSEFI_DATA_HOME set to its store root; refusing to guess the store")
		}
		return museCommander{Name: "musefi", Binary: "musefi", DataHome: home}, nil
	default:
		return museCommander{}, fmt.Errorf("unknown muse commander %q: want \"\" (muse) or \"musefi\"", name)
	}
}

// museSocketFor is the well-known session-server socket for an
// explicit store root. Daemon and joiners rendezvous here; a store
// with no daemon has no socket and fails closed.
func museSocketFor(dataHome string) string {
	if dataHome == "" {
		return ""
	}
	return dataHome + "/muse/session-server/session-server.sock"
}

// museDaemonReadyFor reports whether a session daemon serves the
// given store root. Commander-aware callers must use this, not the
// ambient museDaemonReady, or they will join the wrong daemon.
func museDaemonReadyFor(dataHome string) bool {
	sock := museSocketFor(dataHome)
	if sock == "" {
		return false
	}
	fi, err := os.Stat(sock)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSocket != 0
}

// museRequireDaemonFor refuses to proceed when no session daemon
// serves the commander's store. Without the daemon a host would
// serve in-process and its sessions would die with the call while
// the binding row pretends they are durable.
func museRequireDaemonFor(c museCommander) error {
	label := c.DataHome
	if c.Name != "" {
		label = fmt.Sprintf("commander %q (%s)", c.Name, c.DataHome)
	}
	if !museDaemonReadyFor(c.DataHome) {
		return fmt.Errorf("no muse session daemon serves %s; start one first", label)
	}
	return nil
}

// checkMuseExecutionScope refuses execution when the Metis session
// no longer resolves to the workspace (or declared model) the
// binding was scoped to. A re-homed or re-modelled session is a
// different execution environment; turns never silently follow it.
// The workspace resolves through the decoupled resolver: a live
// shell backing stays authoritative over the record, while a
// native-only session resolves from its logical record — scope that
// refuses native sessions could never turn, observe, or deliver.
func checkMuseExecutionScope(session string, b museBinding) error {
	workdir, err := resolveSessionWorkspace(session)
	if err != nil {
		return err
	}
	current, err := canonicalRoot(workdir)
	if err != nil {
		return err
	}
	if b.Workspace != "" && current != b.Workspace {
		return fmt.Errorf("session %s now resolves to workspace %s, bound to %s; refusing turn on a re-homed session (TD-050)", session, current, b.Workspace)
	}
	var model string
	if isPtySession(session) {
		if m, merr := ptyGet(ptyBare(session)); merr == nil {
			model = m.Model
		}
	} else if e, ok := loadRegistry()[session]; ok {
		model = e.Model
	}
	if b.Model != "" && model != "" && model != b.Model {
		return fmt.Errorf("session %s now declares model %q, bound to %q; refusing turn on a re-modelled session", session, model, b.Model)
	}
	return nil
}

// checkMuseBindingScope refuses provider OR commander switches:
// both select the native store, and a silent change lands in the
// wrong one. A profiled binding resumed ambient (or vice versa) is
// a switch, not a default.
func checkMuseBindingScope(b museBinding, provider, profile string) error {
	if b.Provider != provider {
		return fmt.Errorf("session %s bound with provider %q, not %q; refusing silent provider switch (TD-052)", b.Session, b.Provider, provider)
	}
	if b.Profile != profile {
		return fmt.Errorf("session %s bound under muse profile %q, not %q; refusing cross-profile resume", b.Session, b.Profile, profile)
	}
	return nil
}

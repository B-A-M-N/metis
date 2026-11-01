package main

// Session management for tui-dash: tmux sessions, registry, git info,
// guidance injection, and headless snapshots.
//
// Same model as the Python live-tui-dashboard, deliberately:
//   - tmux session names are tui-<name>, so the two tools can see
//     each other's sessions.
//   - guidance defaults to file mode: appended to <workdir>/guidance.md
//     and to events.jsonl. Direct mode also sends the text to the
//     tmux pane and can collide with agent input, so it is off by default.
//   - this file shows Muse's orchestrator view, not Muse's own chat
//     work. There is no TUI for that; the worker in tmux is what
//     the dashboard watches.
//
// This file holds only the Session identity/data record. Behavior
// lives in the cohesive components: session_registry.go (persistence),
// session_tmux.go (backend transport and ownership), session_lifecycle.go
// (start/stop/adopt), session_stream.go (stream pipes and events),
// session_guidance.go (guidance delivery and input), and
// session_snapshot.go (read-only projections).

import (
	"regexp"
)

type Session struct {
	Name    string `json:"name"`
	Tmux    string `json:"tmux"`
	Workdir string `json:"workdir"`
	Command string `json:"command"`
	Created string `json:"created"`
	Stopped bool   `json:"stopped,omitempty"`
	// Owner is a per-session token proving TUI-dash started this
	// session. Destructive operations (stop, direct input) verify it
	// against the live tmux session environment before acting, so a
	// name collision with another tool's session can never cause us
	// to kill or type into a session we do not own.
	Owner string `json:"owner,omitempty"`
	// Harness identifies the worker runtime (e.g. demo, muse, codex,
	// hermes) and Model names the loaded model, if known. Display-only:
	// they distinguish sessions sharing a provider, nothing derives
	// control or verification from them.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

// PublicSession is the display/JSON projection of a Session.
// Owner is a control credential: it is required for registry
// persistence (loadRegistry/saveRegistry round-trip the full
// Session) but must never cross the machine-readable boundary.
// Use Public() at every CLI/JSON emission point.
type PublicSession struct {
	Name    string `json:"name"`
	Tmux    string `json:"tmux"`
	Workdir string `json:"workdir"`
	Command string `json:"command"`
	Created string `json:"created"`
	Stopped bool   `json:"stopped,omitempty"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Public strips control credentials for external display.
func (s Session) Public() PublicSession {
	return PublicSession{
		Name: s.Name, Tmux: s.Tmux, Workdir: s.Workdir,
		Command: s.Command, Created: s.Created, Stopped: s.Stopped,
		Harness: s.Harness, Model: s.Model,
	}
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

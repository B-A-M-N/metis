package tui

// Attach tracks one terminal-attachment lifecycle: the current
// attempt generation and the session it targets. Only its
// transitions mutate it. Liveness and UI mode arrive as explicit
// inputs to Classify, so the generation fencing stays pure and
// unit-testable without tmux, registry, or the event loop:
// a superseded attempt's late outcome never touches a newer
// attempt's state, even when both name the same session.
type Attach struct {
	Gen     int
	Session string
}

// AttachDecision is what an attach outcome means for the current
// attempt: confirmed (still attached to that exact session),
// refused (detach with the reason), or ignored (a stale outcome
// that must change nothing).
type AttachDecision int

const (
	AttachIgnored AttachDecision = iota
	AttachConfirmed
	AttachRefused
)

// Request starts an attempt at session: fresh generation, recorded
// target. The caller builds the healing command with the returned
// generation pinned to the outcome.
func (a Attach) Request(session string) (Attach, int) {
	a.Gen++
	a.Session = session
	return a, a.Gen
}

// Classify decides one async outcome. Ignored unless the UI is
// still in attach mode, the generation matches the current attempt,
// the outcome names the attempted session, and that session is
// still the live attachment target. Anything else leaves every
// state untouched.
func (a Attach) Classify(mode Mode, resGen int, resSession string, attached bool, liveSession string, liveOK bool) AttachDecision {
	if mode != ModeAttach {
		return AttachIgnored
	}
	if resGen != a.Gen {
		return AttachIgnored
	}
	if resSession != a.Session {
		return AttachIgnored
	}
	if !liveOK || liveSession != resSession {
		return AttachIgnored
	}
	if attached {
		return AttachConfirmed
	}
	return AttachRefused
}

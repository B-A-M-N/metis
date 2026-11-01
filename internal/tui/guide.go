package tui

import (
	"strings"
	"unicode/utf8"
)

// Guide owns guidance editing and submit serialization: the input
// buffer, the one-in-flight busy flag, and the outcome status
// decision. Only its transitions mutate it. Mode changes, command
// construction, and delivery stay in the main-package adapter, so
// the busy/belongs-to policy is unit-testable without tmux, the
// registry, or the event loop: while one submit is in flight a
// second enter is held, never delivered out of order.
type Guide struct {
	Buf  string
	Busy bool
}

// Type appends typed runes to the buffer.
func (g Guide) Type(runes []rune) Guide {
	g.Buf += string(runes)
	return g
}

// Backspace removes one rune, not one byte, so multi-byte input
// stays valid UTF-8.
func (g Guide) Backspace() Guide {
	if g.Buf != "" {
		_, size := utf8.DecodeLastRuneInString(g.Buf)
		g.Buf = g.Buf[:len(g.Buf)-size]
	}
	return g
}

// Clear empties the buffer without touching the busy flag.
func (g Guide) Clear() Guide {
	g.Buf = ""
	return g
}

// GuideSubmit is what a submit keypress means for the current
// buffer: Drop (blank, nothing to send), Hold (a submit is already
// in flight, keep the text), or Send (dispatch Text now).
type GuideSubmit int

const (
	GuideDrop GuideSubmit = iota
	GuideHold
	GuideSend
)

// GuideIntent is the adapter's build order: the decision plus the
// trimmed text to submit when Kind is GuideSend (the kept text when
// GuideHold, empty when GuideDrop).
type GuideIntent struct {
	Kind GuideSubmit
	Text string
}

// Submit classifies one enter press. Send clears the buffer and
// marks busy; Hold changes nothing so the text survives; Drop
// clears a blank buffer without marking busy.
func (g Guide) Submit() (Guide, GuideIntent) {
	text := strings.TrimSpace(g.Buf)
	if text == "" {
		g.Buf = ""
		return g, GuideIntent{Kind: GuideDrop}
	}
	if g.Busy {
		return g, GuideIntent{Kind: GuideHold, Text: g.Buf}
	}
	g.Buf = ""
	g.Busy = true
	return g, GuideIntent{Kind: GuideSend, Text: text}
}

// GuideOutcome is one submit's typed result: the origin session,
// the durable guidance id, the delivery path, or the refusal.
type GuideOutcome struct {
	Session string
	GID     string
	Direct  bool
	Err     error
}

// Complete clears the busy flag and reports the outcome status. The
// status always names the origin session: the selection may have
// moved while the submit was in flight, and a bare "sent" would
// misattribute it to the newly viewed session.
func (g Guide) Complete(o GuideOutcome) (Guide, string) {
	g.Busy = false
	if o.Err != nil {
		return g, o.Err.Error()
	}
	if o.Direct {
		return g, "guidance " + o.GID + " sent (direct) → " + o.Session
	}
	return g, "guidance " + o.GID + " sent (file) → " + o.Session
}

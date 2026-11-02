package main

// muse_native_observe.go: the native observation adapter.
//
// A Muse-native session is observed exclusively through Muse
// session/turn events — session/read and session/resume status,
// view cursors, turn/completed terminal values, and completed
// agent-message text. This file projects those events into one
// typed observation; it never reads PTY logs, tmux capture panes,
// or terminal output, and it never relabels shell output as native
// history. Shell projections stay in their own files.
//
// Every helper takes an explicit *museHost: production passes a
// joined host, hermetic tests pass a stub. Read-only helpers
// (observe) never advance the durable cursor — the cursor moves
// only through the turn-terminal path (run), or unprocessed events
// between the stored cursor and head would be skipped. Helpers that
// need an idempotency key mint one per call; durable task flows
// that must survive crashes use the turn-intent path instead.

import (
	"context"
	"fmt"
	"time"
)

// nativeObservation is a point-in-time view of one bound native
// session, projected only from Muse events. Status and Cursor come
// from session/read (or resume); TurnID/Terminal/Text describe the
// last turn this adapter waited on (empty for pure reads); Events
// counts the notifications the wait consumed.
type nativeObservation struct {
	Session  string `json:"session"`
	NativeID string `json:"native_id"`
	Provider string `json:"provider"`
	Profile  string `json:"profile"`
	Status   string `json:"status"`
	Cursor   string `json:"cursor"`
	TurnID   string `json:"turn_id,omitempty"`
	Terminal string `json:"terminal,omitempty"`
	Text     string `json:"text,omitempty"`
	Events   int    `json:"events_observed"`
}

// requireNativeBinding resolves the durable binding behind a Metis
// session name. An unbound name is refused before any host traffic,
// never an empty identity a later call could mistake for a session.
func requireNativeBinding(session string) (museBinding, error) {
	b, err := getMuseBinding(session)
	if err != nil {
		return museBinding{}, fmt.Errorf("native observe %s: %v", session, err)
	}
	if b.NativeID == "" {
		return museBinding{}, fmt.Errorf("native observe %s: binding names no native session", session)
	}
	return b, nil
}

// requireNativeUseScope enforces the execution scope for native
// use: a native-only session is checked against its logical record,
// a shell-backed session against its live store. A re-homed or
// re-modelled session is refused, never silently followed.
func requireNativeUseScope(session string, b museBinding) error {
	if ls, found, lerr := loadLogicalSession(session); lerr != nil {
		return fmt.Errorf("native session %s logical scope is unreadable: %w", session, lerr)
	} else if found {
		return checkNativeLogicalScope(ls, b)
	}
	return checkMuseExecutionScope(session, b)
}

// observeNativeSession reads one bound native session without
// attaching: no lease, no load, no subscription, no cursor advance.
// Point-in-time only — immediately stale if a foreign host appends.
func observeNativeSession(ctx context.Context, h *museHost, session string) (nativeObservation, error) {
	var obs nativeObservation
	b, err := requireNativeBinding(session)
	if err != nil {
		return obs, err
	}
	if err := requireNativeUseScope(session, b); err != nil {
		return obs, err
	}
	status, cursor, err := museReadSession(ctx, h, b.NativeID)
	if err != nil {
		return obs, err
	}
	return nativeObservation{
		Session: session, NativeID: b.NativeID,
		Provider: b.Provider, Profile: b.Profile,
		Status: status, Cursor: cursor,
	}, nil
}

// runNativeObservedTurn submits one turn to the bound native session
// and waits for its terminal notification, projecting the observed
// events. Admission is never completion: only the server's terminal
// value ends the wait. The durable cursor advances on a terminal
// outcome (this is the turn-terminal writer); unknown outcomes
// return an error with no observation and retain the stored cursor.
func runNativeObservedTurn(ctx context.Context, h *museHost, session, text string, timeout time.Duration) (nativeObservation, error) {
	var obs nativeObservation
	b, err := requireNativeBinding(session)
	if err != nil {
		return obs, err
	}
	if err := requireNativeUseScope(session, b); err != nil {
		return obs, err
	}
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		return obs, fmt.Errorf("resume %s: %w (cursor %q retained)", b.NativeID, err, b.Cursor)
	}
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		return obs, verr
	}
	cmdID, err := newMuseCommandID()
	if err != nil {
		return obs, err
	}
	ack, err := museSubmitTurnWithID(ctx, h, b.NativeID, text, "", cmdID)
	if err != nil {
		return obs, fmt.Errorf("turn/start %s: %v (submission uncertain; cursor %q retained)", cmdID, err, b.Cursor)
	}
	var finalText string
	var events int
	observe := chainTurnObserver(ctx, h, session, b.NativeID, func(s string) { finalText = s })
	wctx, wcancel := context.WithTimeout(ctx, timeout)
	defer wcancel()
	terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, ack.TurnID, func(n museNotification) {
		events++
		observe(n)
	})
	if err != nil {
		return obs, err
	}
	if cursor != "" {
		_ = updateMuseCursor(session, cursor, ack.TurnID)
	} else {
		cursor = b.Cursor
	}
	return nativeObservation{
		Session: session, NativeID: b.NativeID,
		Provider: b.Provider, Profile: b.Profile,
		Cursor: cursor, TurnID: ack.TurnID, Terminal: terminal,
		Text: finalText, Events: events,
	}, nil
}

// cancelNativeObservedTurn asks the server to stop one exact turn
// and requires the authoritative terminal outcome. The cancel ack
// is not confirmation: any observed terminal value (cancelled,
// completed, failed) confirms the stop, while a failed admission,
// a lost terminal, or an expired wait stays uncertain for the
// operator to reconcile.
func cancelNativeObservedTurn(ctx context.Context, h *museHost, session, turnID string, wait time.Duration) (string, error) {
	b, err := requireNativeBinding(session)
	if err != nil {
		return ExecutionCancelUncertain, err
	}
	if err := requireNativeUseScope(session, b); err != nil {
		return ExecutionCancelUncertain, err
	}
	return stopNativeTurnOnHost(ctx, h, b.NativeID, turnID, wait)
}

// resumeNativeObserved re-attaches a fresh host to the bound native
// session after a restart and revalidates the served config against
// the binding. It proves recovery: the session survives its hosts,
// the stored cursor replays, and a drifted provider, model, or
// workspace refuses before anything is submitted. The stored cursor
// is positional, not progress — resume never advances it.
func resumeNativeObserved(ctx context.Context, h *museHost, session string) (nativeObservation, error) {
	var obs nativeObservation
	b, err := requireNativeBinding(session)
	if err != nil {
		return obs, err
	}
	if err := requireNativeUseScope(session, b); err != nil {
		return obs, err
	}
	status, cursor, err := museResumeSession(ctx, h, b.NativeID, b.Cursor)
	if err != nil {
		return obs, fmt.Errorf("resume %s: %w (cursor %q retained)", b.NativeID, err, b.Cursor)
	}
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		return obs, verr
	}
	return nativeObservation{
		Session: session, NativeID: b.NativeID,
		Provider: b.Provider, Profile: b.Profile,
		Status: status, Cursor: cursor,
	}, nil
}

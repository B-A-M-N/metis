package main

// muse_observation.go: live view subscription and terminal-event
// observation over a museHost.
//
// Cursor semantics mirrored from the wire: view cursors are opaque
// server-minted strings — never parse, construct, or compare them,
// only store and replay. Accepted-vs-completed stays distinct here:
// turn/start admission (see muse_turn_rpc.go) is never an outcome,
// and only a turn/completed (or turn/unqueued) notification carrying
// the authoritative `terminal` value ends a wait. Unknown outcomes
// (timeout, host close, overflow gap, missing terminal) are reported
// as errors naming the last observed cursor, never invented.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// museSubscribe attaches this connection's live view subscription at
// an explicit cursor. With after == "" the server sends from now (no
// replay); with a previously observed cursor it replays (after, head]
// gaplessly before any live event. It never loads a session, takes
// no lease, and writes nothing.
func museSubscribe(ctx context.Context, h *museHost, sessionID, after string) (string, error) {
	params := map[string]any{"sessionId": sessionID}
	if after != "" {
		params["after"] = after
	}
	raw, err := h.Call(ctx, "view/subscribe", params)
	if err != nil {
		return "", fmt.Errorf("view/subscribe: %w", err)
	}
	var res struct {
		ViewCursor string `json:"viewCursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("view/subscribe result: %w", err)
	}
	return res.ViewCursor, nil
}

// museWaitTurn consumes the host notification stream until the named
// turn reaches its terminal notification, and returns the server's
// authoritative `terminal` value (e.g. completed). Any other
// notification is handed to observe for projection; the wait never
// invents an outcome on timeout — it reports the timeout and the
// last observed cursor, leaving the outcome unknown.
func museWaitTurn(ctx context.Context, h *museHost, sessionID, turnID string, observe func(museNotification)) (terminal, cursor string, err error) {
	// The notification stream drops oldest-first under overflow and
	// counts via Dropped. A drop during this wait means the turn's
	// terminal event may have been among the discarded, so any
	// completion observed afterwards is untrustworthy: report an
	// explicit gap with the replay cursor instead.
	drops := h.Dropped()
	base := cursor
	for {
		select {
		case <-ctx.Done():
			return "", cursor, fmt.Errorf("wait turn %s: %w (outcome unknown; cursor %q)", turnID, ctx.Err(), cursor)
		case n, ok := <-h.Notifications():
			if !ok {
				return "", cursor, fmt.Errorf("wait turn %s: host closed (outcome unknown; cursor %q)", turnID, cursor)
			}
			if c := museViewCursor(n); c != "" {
				cursor = c
			}
			if d := h.Dropped(); d != drops {
				return "", base, fmt.Errorf("wait turn %s: notification overflow gap (%d dropped); outcome unknown, replay from cursor %q",
					turnID, d-drops, base)
			}
			if observe != nil {
				observe(n)
			}
			// turn/completed is the normal terminal path; a queued
			// turn that never runs ends as turn/unqueued instead.
			// Both are authoritative turn ends from the server.
			if n.Method != "turn/completed" && n.Method != "turn/unqueued" {
				continue
			}
			var p struct {
				SessionID string `json:"sessionId"`
				TurnID    string `json:"turnId"`
				Terminal  string `json:"terminal"`
			}
			if err := json.Unmarshal(n.Params, &p); err != nil {
				continue
			}
			if p.SessionID == sessionID && p.TurnID == turnID {
				if p.Terminal == "" {
					if n.Method == "turn/unqueued" {
						return "unqueued", cursor, nil
					}
					return "", cursor, fmt.Errorf("turn/completed without terminal value (outcome unknown; cursor %q)", cursor)
				}
				return p.Terminal, cursor, nil
			}
		}
	}
}

// museInputKind reports whether notification params carry model
// text, for projections that must never mistake structured events
// for terminal output.
func museInputKind(method string) string {
	if strings.HasPrefix(method, "item/") || strings.HasPrefix(method, "turn/") || strings.HasPrefix(method, "session/") {
		return "structured-event"
	}
	return "other"
}

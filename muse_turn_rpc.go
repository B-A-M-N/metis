package main

// muse_turn_rpc.go: native turn admission over a museHost — submit
// and cancel.
//
// Admission is never completion: turn/start returns the ack
// (turnId + disposition) and the terminal outcome arrives later as a
// turn/completed notification carrying the authoritative `terminal`
// value (see muse_observation.go). Callers must wait for that event
// before recording any execution result.
//
// Identity rules mirrored from the wire (verified on 1.4.14):
//   - commandId is client-minted UUIDv7 (see newMuseCommandID in
//     muse_session_rpc.go) and is the idempotency handle: a retried
//     submit with the same commandId is deduplicated by the server
//     to the same result.
//   - turnId derives from the submit commandId; always take it from
//     the ack, never derive it client-side.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// museTurnAck is the admission receipt for one turn/start. Disposition
// is one of started, queued, steered; status is always accepted on
// the verified wire. Neither field is an execution outcome.
type museTurnAck struct {
	CommandID      string `json:"commandId"`
	Disposition    string `json:"disposition"`
	StartedNewTurn bool   `json:"startedNewTurn"`
	Status         string `json:"status"`
	TurnID         string `json:"turnId"`
}

// museSubmitTurn submits one user input to a session. ifBusy selects
// the server-side disposition when a turn is already running
// (queue, steer, replace); empty means the server default (queue),
// which is the only safe choice for a client that has not inspected
// live session state. The returned ack is admission, not completion.
func museSubmitTurn(ctx context.Context, h *museHost, sessionID, text, ifBusy string) (museTurnAck, error) {
	cmdID, err := newMuseCommandID()
	if err != nil {
		return museTurnAck{}, err
	}
	return museSubmitTurnWithID(ctx, h, sessionID, text, ifBusy, cmdID)
}

// museSubmitTurnWithID submits with a caller-provided idempotency
// key. Durable flows persist the key before calling so a crash
// between accept and ack still names the same command on retry;
// the server dedups identical commandId+payload resubmissions.
func museSubmitTurnWithID(ctx context.Context, h *museHost, sessionID, text, ifBusy, cmdID string) (museTurnAck, error) {
	var ack museTurnAck
	if cmdID == "" {
		var err error
		cmdID, err = newMuseCommandID()
		if err != nil {
			return ack, err
		}
	}
	params := map[string]any{
		"commandId": cmdID,
		"sessionId": sessionID,
		"input":     []any{map[string]string{"type": "text", "text": text}},
	}
	if ifBusy != "" {
		params["ifBusy"] = ifBusy
	}
	raw, err := h.Call(ctx, "turn/start", params)
	if err != nil {
		return ack, fmt.Errorf("turn/start: %w", err)
	}
	if err := json.Unmarshal(raw, &ack); err != nil {
		return ack, fmt.Errorf("turn/start ack: %w", err)
	}
	if ack.TurnID == "" {
		return ack, fmt.Errorf("turn/start ack carried no turn id")
	}
	return ack, nil
}

// museCancelTurn asks the server to cancel one exact turn. Best
// effort by design: the ack only admits the request, and the turn's
// terminal notification remains authoritative. Callers must still
// wait for (or reconcile) the terminal outcome afterward.
func museCancelTurn(ctx context.Context, h *museHost, sessionID, turnID string) error {
	cmdID, err := newMuseCommandID()
	if err != nil {
		return err
	}
	_, err = h.Call(ctx, "turn/cancel", map[string]any{
		"commandId": cmdID, "sessionId": sessionID, "turnId": turnID,
	})
	if err != nil {
		return fmt.Errorf("turn/cancel: %w", err)
	}
	return nil
}

// nativeCancelWait bounds the terminal-observation window after a
// turn/cancel admission. The cancel ack is not confirmation — only
// a terminal outcome confirms the turn stopped — so expiry fails
// closed to uncertain and leaves reconciliation to the operator.
func nativeCancelWait() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("METIS_NATIVE_CANCEL_WAIT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 15 * time.Second
}

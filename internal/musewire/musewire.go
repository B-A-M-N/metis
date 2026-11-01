// Package musewire owns the pure MSP/JSON-RPC wire framing for one
// `muse serve` child: frame encode/decode and the request/reply
// correlation vocabulary.
//
// Wire facts (installed Muse 1.4.14, schema fingerprint recorded at
// handshake; verified against the echo provider, zero inference):
//   - initialize + `initialized` notification opens the connection;
//     anything else is refused `notInitialized` (-32600).
//   - Replies and notifications interleave on stdout; correlation is
//     by numeric id. Notification-only traffic gets no reply.
//   - Closing stdin initiates orderly host shutdown.
//   - commandId must be client-minted UUIDv7; the server rejects
//     anything else before any durable byte.
//
// This package knows nothing about processes, pipes, supervision,
// sessions, turns, or the task ledger: those stay in package main.
// It imports only the standard library, so it can never grow a
// DB/process/tmux/MCP dependency without the import list saying so.
package musewire

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	// ClientName is the MSP client identification. It is diagnostics
	// only, never an authority claim.
	ClientName = "metis"
	// ClientVersion is the MSP client version.
	ClientVersion = "0.1.0"
	// CallTimeout bounds one request/reply exchange.
	CallTimeout = 30 * time.Second
	// MaxLineBytes bounds one JSON-RPC frame. View events can be
	// large; overflow is a loud error, never a silent split.
	MaxLineBytes = 4 << 20
)

// Error carries a JSON-RPC error with the server's kind label when
// present, so callers can distinguish retryable overload (-32001)
// from terminal refusal without string matching.
type Error struct {
	Code    int
	Kind    string
	Message string
}

func (e *Error) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("muse %s (code %d): %s", e.Kind, e.Code, e.Message)
	}
	return fmt.Sprintf("muse error %d: %s", e.Code, e.Message)
}

// Notification is one server-initiated MSP message with its raw
// params: a notification, or a must-answer request
// (`approval/request`, `userInput/request`). RequestID is non-nil
// exactly for must-answer requests; the presentation receipt (and
// any decision) travels separately, so surfacing never implies
// answering. View cursors ride inside method-specific params; use
// ViewCursor to extract the cursor when the method carries one.
type Notification struct {
	Method    string
	Params    json.RawMessage
	RequestID *int64
}

// ViewCursor returns the view cursor from notification params when
// present, or "" when the method carries none.
func ViewCursor(n Notification) string {
	var p map[string]json.RawMessage
	if err := json.Unmarshal(n.Params, &p); err != nil {
		return ""
	}
	var c string
	if raw, ok := p["viewCursor"]; ok {
		_ = json.Unmarshal(raw, &c)
	}
	return c
}

// EncodeCall renders one request frame: method plus params under a
// numeric correlation id, newline-terminated. The object shape is the
// historical one (jsonrpc/id/method/params keys), so wire bytes are
// unchanged from the pre-extraction host.
func EncodeCall(id int64, method string, params any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// EncodeNotify renders one notification frame: no reply ever comes back.
func EncodeNotify(method string, params any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// EncodeResult renders one bare result frame answering a
// server-initiated request (a presentation receipt or similar). It is
// not a call: no waiter is registered and no reply is correlated.
func EncodeResult(id int64, result any) ([]byte, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

// Frame is one decoded wire line: either server-initiated traffic (a
// notification, or a must-answer request carrying both id and
// method), a reply to one pending call (id, no method), or neither
// (junk: a valid line with no id and no method, ignored like a bad
// line — untrusted framing is never a state change).
type Frame struct {
	ID     *int64
	Method string
	Params json.RawMessage
	Result json.RawMessage
	Err    *Error
}

// IsServerMessage reports server-initiated traffic: anything carrying
// a method surfaces, even when it also carries an id (a must-answer
// request). Replies never carry a method, so this branch can never
// steal a pending call's reply.
func (f Frame) IsServerMessage() bool { return f.Method != "" }

// IsReply reports a reply owning a correlation id. Late or foreign
// replies (no waiter owns the id) are dropped by the caller.
func (f Frame) IsReply() bool { return f.Method == "" && f.ID != nil }

// DecodeFrame classifies one raw wire line. ok=false means the line
// is not JSON at all and must be skipped; a well-formed line that is
// neither server traffic nor a reply decodes with ok=true but
// matches neither predicate, and is likewise ignored.
func DecodeFrame(line []byte) (f Frame, ok bool) {
	var msg struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int             `json:"code"`
			Message string          `json:"message"`
			Data    json.RawMessage `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return Frame{}, false
	}
	f.ID = msg.ID
	f.Method = msg.Method
	f.Params = msg.Params
	f.Result = msg.Result
	if msg.Error != nil {
		var data struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(msg.Error.Data, &data)
		f.Err = &Error{Code: msg.Error.Code, Kind: data.Kind, Message: msg.Error.Message}
	}
	return f, true
}

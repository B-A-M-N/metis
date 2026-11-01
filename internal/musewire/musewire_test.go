package musewire

import (
	"encoding/json"
	"strings"
	"testing"
)

// legacyFrame rebuilds the pre-extraction encoding independently:
// the old host marshaled the same map shape. Byte equality here is
// the proof the extraction preserved wire bytes exactly.
func legacyFrame(t *testing.T, v map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return append(body, '\n')
}

func TestEncodeCallByteExact(t *testing.T) {
	got, err := EncodeCall(7, "session/start", map[string]any{"commandId": "cmd-1", "title": "x"})
	if err != nil {
		t.Fatal(err)
	}
	want := legacyFrame(t, map[string]any{"jsonrpc": "2.0", "id": int64(7), "method": "session/start", "params": map[string]any{"commandId": "cmd-1", "title": "x"}})
	if string(got) != string(want) {
		t.Fatalf("call bytes differ:\n got %s\nwant %s", got, want)
	}
}

func TestEncodeNotifyByteExact(t *testing.T) {
	got, err := EncodeNotify("initialized", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := legacyFrame(t, map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}})
	if string(got) != string(want) {
		t.Fatalf("notify bytes differ:\n got %s\nwant %s", got, want)
	}
}

func TestEncodeResultByteExact(t *testing.T) {
	got, err := EncodeResult(9, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := legacyFrame(t, map[string]any{"jsonrpc": "2.0", "id": int64(9), "result": map[string]any{}})
	if string(got) != string(want) {
		t.Fatalf("result bytes differ:\n got %s\nwant %s", got, want)
	}
}

func TestErrorString(t *testing.T) {
	e := &Error{Code: -32001, Kind: "overloaded", Message: "busy"}
	if !strings.Contains(e.Error(), "overloaded") || !strings.Contains(e.Error(), "-32001") {
		t.Fatalf("kind error string lost label: %q", e.Error())
	}
	plain := &Error{Code: -32601, Message: "no such method"}
	if !strings.Contains(plain.Error(), "-32601") {
		t.Fatalf("plain error string lost code: %q", plain.Error())
	}
}

func TestViewCursor(t *testing.T) {
	n := Notification{Method: "turn/started", Params: json.RawMessage(`{"viewCursor":"v4:s:9"}`)}
	if got := ViewCursor(n); got != "v4:s:9" {
		t.Fatalf("cursor = %q", got)
	}
	bare := Notification{Method: "turn/started", Params: json.RawMessage(`{}`)}
	if got := ViewCursor(bare); got != "" {
		t.Fatalf("missing cursor = %q, want empty", got)
	}
	broken := Notification{Method: "x", Params: json.RawMessage(`not json`)}
	if got := ViewCursor(broken); got != "" {
		t.Fatalf("broken params cursor = %q, want empty", got)
	}
}

func TestDecodeFrameClassification(t *testing.T) {
	// Reply carrying a result.
	fr, ok := DecodeFrame([]byte(`{"jsonrpc":"2.0","id":3,"result":{"ok":true}}`))
	if !ok || !fr.IsReply() || fr.IsServerMessage() {
		t.Fatalf("result reply misclassified: %+v ok=%v", fr, ok)
	}
	if fr.Err != nil || string(fr.Result) != `{"ok":true}` {
		t.Fatalf("result payload lost: %+v", fr)
	}
	// Reply carrying an error with a kind label.
	fr, ok = DecodeFrame([]byte(`{"jsonrpc":"2.0","id":4,"error":{"code":-32001,"message":"busy","data":{"kind":"overloaded"}}}`))
	if !ok || !fr.IsReply() {
		t.Fatalf("error reply misclassified: %+v ok=%v", fr, ok)
	}
	if fr.Err == nil || fr.Err.Code != -32001 || fr.Err.Kind != "overloaded" {
		t.Fatalf("error kind lost: %+v", fr.Err)
	}
	// Error without a data/kind block: kind stays empty, never fails.
	fr, ok = DecodeFrame([]byte(`{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"nope"}}`))
	if !ok || fr.Err == nil || fr.Err.Kind != "" || fr.Err.Code != -32601 {
		t.Fatalf("kindless error mishandled: %+v ok=%v", fr, ok)
	}
	// Plain notification surfaces with no request id.
	fr, ok = DecodeFrame([]byte(`{"jsonrpc":"2.0","method":"turn/started","params":{"viewCursor":"v4:s:9"}}`))
	if !ok || !fr.IsServerMessage() || fr.IsReply() || fr.ID != nil {
		t.Fatalf("notification misclassified: %+v ok=%v", fr, ok)
	}
	if ViewCursor(Notification{Method: fr.Method, Params: fr.Params}) != "v4:s:9" {
		t.Fatalf("notification cursor lost: %s", fr.Params)
	}
	// Must-answer request: method AND id. It must surface (never
	// stealable as a reply) with its request id intact.
	fr, ok = DecodeFrame([]byte(`{"jsonrpc":"2.0","id":11,"method":"approval/request","params":{}}`))
	if !ok || !fr.IsServerMessage() || fr.IsReply() {
		t.Fatalf("must-answer request misclassified: %+v ok=%v", fr, ok)
	}
	if fr.ID == nil || *fr.ID != 11 {
		t.Fatalf("request id lost: %+v", fr.ID)
	}
	// Junk line: valid JSON, neither id nor method. Decodes cleanly
	// but matches nothing, so the caller ignores it.
	fr, ok = DecodeFrame([]byte(`{"foo":1}`))
	if !ok || fr.IsServerMessage() || fr.IsReply() {
		t.Fatalf("junk must be ignorable: %+v ok=%v", fr, ok)
	}
	// Bad line: not JSON. The caller skips it without state change.
	if _, ok = DecodeFrame([]byte(`{oops`)); ok {
		t.Fatal("bad line must not decode")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	// The classifier is directional-agnostic: our own encoded call
	// carries id+method, so it reads back as server-initiated
	// traffic — the same rule the host read loop applies to the
	// real stream.
	raw, err := EncodeCall(12, "session/list", map[string]any{"limit": 200})
	if err != nil {
		t.Fatal(err)
	}
	fr, ok := DecodeFrame(raw[:len(raw)-1])
	if !ok || !fr.IsServerMessage() || fr.Method != "session/list" {
		t.Fatalf("round trip misclassified: %+v ok=%v", fr, ok)
	}
	if fr.ID == nil || *fr.ID != 12 {
		t.Fatalf("round trip id lost: %+v", fr.ID)
	}
}

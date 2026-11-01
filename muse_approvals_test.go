package main

// muse_approvals_test.go: native approval observation (slice A).
//
// Approvals arrive two ways: server-initiated `approval/request`
// (must-answer, carries an id) and the `approval/requested` fold
// notification. The framing layer must surface both — never drop a
// must-answer request as a "late reply" — and record them durably.
// Deciding (approval/decide + presentation receipts) is slice B.

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// The read loop must not drop server-initiated requests: a message
// with BOTH an id and a method is a server request, not a reply to
// a pending call. Replies never carry a method.
func TestMuseServerRequestsSurfaced(t *testing.T) {
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	enc := json.NewEncoder(server)
	mustEncode := func(v any) {
		t.Helper()
		if err := enc.Encode(v); err != nil {
			t.Fatalf("stub write: %v", err)
		}
	}
	// Server-initiated must-answer request (id + method).
	mustEncode(map[string]any{"jsonrpc": "2.0", "id": 99, "method": "approval/request",
		"params": map[string]any{"approvalId": "ap-1"}})
	// Plain fold notification (method, no id).
	mustEncode(map[string]any{"jsonrpc": "2.0", "method": "approval/requested",
		"params": map[string]any{"approvalId": "ap-1"}})
	// Ordinary reply to our own call still correlates.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	callDone := make(chan struct{})
	go func() {
		defer close(callDone)
		_, _ = h.call(ctx, "session/list", map[string]any{})
	}()
	deadline := time.After(5 * time.Second)
	gotReq, gotNotif, gotCall := false, false, false
	for !(gotReq && gotNotif && gotCall) {
		select {
		case n := <-h.Notifications():
			switch {
			case n.Method == "approval/request" && n.RequestID != nil && *n.RequestID == 99:
				gotReq = true
			case n.Method == "approval/requested" && n.RequestID == nil:
				gotNotif = true
			}
		case <-callDone:
			gotCall = true
		case <-deadline:
			t.Fatalf("missing server messages: request=%v notif=%v call=%v", gotReq, gotNotif, gotCall)
		}
	}
}

// Requested folds upsert a pending row; resolved marks it with the
// server's terminal decision. Unknown approvals never fabricate rows.
func TestMuseApprovalTable(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	req := []byte(`{"approvalId":"ap-9","itemId":"it-3","currentRequirementId":{"sourceIndex":0},
		"availableChoices":[{"choiceId":"allow","label":"Allow"},{"choiceId":"deny","label":"Deny"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", req); err != nil {
		t.Fatal(err)
	}
	// Re-request (updated fold) refreshes, never duplicates.
	if err := recordMuseApprovalRequested("w", "native-1", req); err != nil {
		t.Fatal(err)
	}
	pending, err := listMuseApprovals("w", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ApprovalID != "ap-9" || pending[0].Status != "pending" {
		t.Fatalf("expected one pending approval, got %+v", pending)
	}
	if len(pending[0].Choices) != 2 {
		t.Fatalf("choices must be preserved for decide: %+v", pending[0])
	}
	if err := recordMuseApprovalResolved("w", "ap-9",
		[]byte(`{"approvalId":"ap-9","decision":"allow","decidedByCommandId":"cmd-1"}`)); err != nil {
		t.Fatal(err)
	}
	pending, err = listMuseApprovals("w", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("resolved approval must leave the pending set: %+v", pending)
	}
	all, err := listMuseApprovals("w", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Status != "resolved" || all[0].Decision != "allow" {
		t.Fatalf("resolved row must keep the terminal decision: %+v", all)
	}
	// Resolving an unknown approval is an error, not a new row.
	if err := recordMuseApprovalResolved("w", "ap-nope", []byte(`{}`)); err == nil {
		t.Fatal("resolving an unknown approval must fail")
	}
	// Malformed folds are refused without rows.
	if err := recordMuseApprovalRequested("w", "native-1", []byte(`{"noId":1}`)); err == nil {
		t.Fatal("fold without approvalId must fail")
	}
	if n, _ := listMuseApprovals("w", false); len(n) != 1 {
		t.Fatalf("refused folds must not add rows: %+v", n)
	}
}

// Deciding sends exactly one offered choice with a fresh UUIDv7
// commandId and the stored requirement guard. Anything else is
// refused locally, before any byte reaches the server.
func TestMuseDecideApproval(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	var mu sync.Mutex
	var decideCalls []map[string]any
	failDecide := 0
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "approval/decide" {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			mu.Lock()
			decideCalls = append(decideCalls, p)
			fail := failDecide
			mu.Unlock()
			if fail == 1 {
				return nil, &museError{Code: -32053, Kind: "precondition", Message: "stale requirement"}
			}
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seed := []byte(`{"approvalId":"ap-d","itemId":"it-1",
		"currentRequirementId":{"sourceIndex":0},
		"availableChoices":[{"choiceId":"allow","label":"Allow"},{"choiceId":"deny","label":"Deny"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", seed); err != nil {
		t.Fatal(err)
	}
	req := OperationRequest{Actor: "orch", RequestID: "req-1"}
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-d", "allow", "", req); err != nil {
		t.Fatalf("decide: %v", err)
	}
	mu.Lock()
	nDecide := len(decideCalls)
	var sent map[string]any
	if nDecide == 1 {
		sent = decideCalls[0]
	}
	mu.Unlock()
	if nDecide != 1 {
		t.Fatalf("expected exactly one decide call, got %d", nDecide)
	}
	if sent["approvalId"] != "ap-d" || sent["choiceId"] != "allow" {
		t.Fatalf("bad decide params: %v", sent)
	}
	cmdID, _ := sent["commandId"].(string)
	if len(cmdID) != 36 || cmdID[14] != '7' {
		t.Fatalf("decide must carry a UUIDv7 commandId: %v", sent)
	}
	if _, ok := sent["requirementId"]; !ok {
		t.Fatalf("decide must carry the stored requirement guard: %v", sent)
	}
	rows, _ := listMuseApprovals("w", false)
	if len(rows) != 1 || rows[0].Decision != "allow" || rows[0].Status != "pending" {
		t.Fatalf("decide records the decision but stays pending until the resolved fold: %+v", rows)
	}
	// Unoffered choice: refused locally, zero wire calls.
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-d", "rm-rf", "", req); err == nil {
		t.Fatal("unoffered choice must be refused")
	}
	// Unknown approval: refused locally, zero wire calls.
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-nope", "allow", "", req); err == nil {
		t.Fatal("unknown approval must be refused")
	}
	mu.Lock()
	nDecide = len(decideCalls)
	mu.Unlock()
	if nDecide != 1 {
		t.Fatalf("refusals must not touch the wire: %d calls", nDecide)
	}
	// Stale requirement: server rejection surfaces, row stays pending.
	mu.Lock()
	failDecide = 1
	mu.Unlock()
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-d", "deny", "", req); err == nil {
		t.Fatal("stale requirement rejection must surface")
	}
	rows, _ = listMuseApprovals("w", true)
	if len(rows) != 1 {
		t.Fatalf("failed decide must leave the row pending: %+v", rows)
	}
}

// The wait-loop observer records approval traffic and answers
// must-answer requests once recorded — receipt means durably
// recorded and operator-visible, never a decision.
func TestMuseApprovalObserverReceipt(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	client, server := net.Pipe()
	// Receipts arrive on the stub-server goroutine; the test goroutine
	// consumes them through this channel, which is the happens-before
	// edge the race detector needs. Buffered so the stub never blocks
	// on stray duplicates while the test is busy elsewhere.
	receipts := make(chan int64, 8)
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "" {
			receipts <- id // result frame for our receipt
			return nil, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	enc := json.NewEncoder(server)
	go func() {
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "approval/request",
			"params": map[string]any{"approvalId": "ap-r", "availableChoices": []any{}}})
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "approval/requested",
			"params": map[string]any{"approvalId": "ap-r", "availableChoices": []any{}}})
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/completed",
			"params": map[string]any{"sessionId": "native-1", "turnId": "t-1", "terminal": "completed"}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	term, _, err := museWaitTurn(ctx, h, "native-1", "t-1",
		museApprovalObserver(ctx, h, "w", "native-1"))
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if term != "completed" {
		t.Fatalf("wait must still terminate on the turn: %q", term)
	}
	rows, _ := listMuseApprovals("w", true)
	if len(rows) != 1 || rows[0].ApprovalID != "ap-r" {
		t.Fatalf("observer must record the approval: %+v", rows)
	}
	// The receipt travels host->stub after the observer records the
	// row; wait for it on a bounded context rather than assuming it
	// already landed when the turn wait returned.
	receiptCtx, receiptCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer receiptCancel()
	var got []int64
	for len(got) < 1 {
		select {
		case id := <-receipts:
			got = append(got, id)
		case <-receiptCtx.Done():
			t.Fatalf("must-answer request never receipted: %v", receiptCtx.Err())
		}
	}
	if got[0] != 7 {
		t.Fatalf("must-answer request must get exactly one receipt: %v", got)
	}
	select {
	case extra := <-receipts:
		got = append(got, extra)
		t.Fatalf("must-answer request must get exactly one receipt: %v", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// approval/listPending is a lease-free fold read: sessionId in,
// pending approvals + prompts out.
func TestMuseListPendingPassthrough(t *testing.T) {
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "approval/listPending" {
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(params, &p)
			if p.SessionID != "native-7" {
				return nil, &museError{Code: -32602, Message: "wrong session"}
			}
			return map[string]any{
				"approvals":  []any{map[string]any{"approvalId": "ap-2"}},
				"userInputs": []any{},
			}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := museListPendingApprovals(ctx, h, "native-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Approvals) != 1 || got.Approvals[0]["approvalId"] != "ap-2" {
		t.Fatalf("bad pending fold: %+v", got)
	}
}

// A conflicting decide retry must not reuse the command id bound
// to a different decision payload: same choice reuses the id for
// server dedup, a different choice mints a fresh one.
func TestDecideBindsCommandIDToPayload(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	var mu sync.Mutex
	var decideCmds []string
	client, server := net.Pipe()
	stubMuseServer(t, server, func(id int64, method string, params json.RawMessage) (any, *museError) {
		if method == "initialize" {
			return map[string]any{
				"serverInfo": map[string]string{"name": "stub", "version": "0"},
				"schema":     map[string]any{"fingerprint": "test", "version": 1},
			}, nil
		}
		if method == "approval/decide" {
			var p map[string]any
			_ = json.Unmarshal(params, &p)
			cmd, _ := p["commandId"].(string)
			mu.Lock()
			decideCmds = append(decideCmds, cmd)
			mu.Unlock()
			return map[string]any{}, nil
		}
		return map[string]any{}, nil
	})
	h, err := newMuseHostIO(client, client)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	seed := []byte(`{"approvalId":"ap-fp","itemId":"it-1",
		"currentRequirementId":{"sourceIndex":0},
		"availableChoices":[{"choiceId":"allow","label":"Allow"},{"choiceId":"deny","label":"Deny"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", seed); err != nil {
		t.Fatal(err)
	}
	req := OperationRequest{Actor: "orch", RequestID: "req-1"}
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-fp", "allow", "", req); err != nil {
		t.Fatalf("decide allow: %v", err)
	}
	// Identical retry reuses the id (server dedup).
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-fp", "allow", "", req); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	mu.Lock()
	cmds := append([]string(nil), decideCmds...)
	mu.Unlock()
	if len(cmds) != 2 || cmds[0] == "" || cmds[0] != cmds[1] {
		t.Fatalf("identical retry must reuse the command id, got %v", cmds)
	}
	// Conflicting retry mints a fresh id, never the allow-bound one.
	if err := museDecideApprovalOnHost(ctx, h, "w", "native-1", "ap-fp", "deny", "", req); err != nil {
		t.Fatalf("conflicting retry: %v", err)
	}
	mu.Lock()
	cmds = append([]string(nil), decideCmds...)
	mu.Unlock()
	if len(cmds) != 3 || cmds[2] == cmds[0] {
		t.Fatalf("conflicting retry must mint a fresh command id, got %v", cmds)
	}
}

package main

import (
	"testing"
)

// Enqueue then replay: a reconnecting node replays its pending ops
// and gets back exactly what was enqueued, in order.
func TestFleetOutboxEnqueueReplay(t *testing.T) {
	id, err := enrollFleetNode("host-outbox", `{"muse":true}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	op1, err := enqueueFleetOp(id, "Offer", `{"task":"t1"}`, req)
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	req2 := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if _, err := enqueueFleetOp(id, "Offer", `{"task":"t2"}`, req2); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	pending, err := listPendingFleetOps(id)
	if err != nil {
		t.Fatalf("listPendingFleetOps: %v", err)
	}
	if len(pending) != 2 || pending[0].OpID != op1 {
		t.Fatalf("replay = %+v, want 2 ops starting with %q", pending, op1)
	}
}

// The request ID is the idempotency key: retrying the same enqueue
// returns the original op, never a duplicate row.
func TestFleetOutboxEnqueueIdempotent(t *testing.T) {
	id, err := enrollFleetNode("host-idem", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	a, err := enqueueFleetOp(id, "Claim", `{}`, req)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	b, err := enqueueFleetOp(id, "Claim", `{}`, req)
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if a != b {
		t.Fatalf("idempotent re-enqueue minted %q, want %q", b, a)
	}
	if pending, err := listPendingFleetOps(id); err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, err = %v; want exactly 1", pending, err)
	}
}

// Acking an unknown op is refused, not absorbed: an ack for
// something never enqueued is a protocol error, never success.
func TestFleetOutboxAckUnknownRefused(t *testing.T) {
	if err := ackFleetOp("op-does-not-exist"); err == nil {
		t.Fatal("acked unknown op, want refusal")
	}
}

// An acked op leaves the pending set exactly once: replay after
// acknowledgment must not redeliver it.
func TestFleetOutboxAckRemovesFromPending(t *testing.T) {
	id, err := enrollFleetNode("host-ack", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	op, err := enqueueFleetOp(id, "Report", `{}`, req)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := ackFleetOp(op); err != nil {
		t.Fatalf("ackFleetOp: %v", err)
	}
	pending, err := listPendingFleetOps(id)
	if err != nil {
		t.Fatalf("listPendingFleetOps: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("acked op redelivered: %+v", pending)
	}
}

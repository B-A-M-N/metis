package main

import (
	"testing"
)

// Capacity gates admission: a node with one slot takes one
// reservation, and the second waiter is refused, not queued
// silently.
func TestFleetResourcesCapacityGates(t *testing.T) {
	id, err := enrollFleetNode("host-cap", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := setFleetNodeCapacity(id, 1); err != nil {
		t.Fatalf("setFleetNodeCapacity: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot(id, "task-a", "atm-a", req); err != nil {
		t.Fatalf("first reservation: %v", err)
	}
	req2 := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot(id, "task-b", "atm-b", req2); err == nil {
		t.Fatal("over-capacity reservation admitted, want refusal")
	}
}

// Release frees exactly the released slot: the next waiter fits,
// and the released identity cannot be double-released into a
// negative load.
func TestFleetResourcesReleaseFrees(t *testing.T) {
	id, err := enrollFleetNode("host-rel", `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := setFleetNodeCapacity(id, 1); err != nil {
		t.Fatalf("setFleetNodeCapacity: %v", err)
	}
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot(id, "task-a", "atm-a", req); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := releaseFleetSlot(id, "task-a", "atm-a"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := releaseFleetSlot(id, "task-a", "atm-a"); err == nil {
		t.Fatal("double release succeeded, want refusal")
	}
	req2 := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot(id, "task-b", "atm-b", req2); err != nil {
		t.Fatalf("post-release reservation refused: %v", err)
	}
}

// Reserving on an unknown node is refused: capacity is a property
// of enrolled nodes, never of bare hostnames.
func TestFleetResourcesUnknownNodeRefused(t *testing.T) {
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := reserveFleetSlot("node-missing", "t", "a", req); err == nil {
		t.Fatal("reserved on unknown node, want refusal")
	}
}

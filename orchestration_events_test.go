package main

// orchestration_events_test.go: the durable feed is append-only,
// cursor-stable, idempotent on request identity, and bounded.

import (
	"fmt"
	"strings"
	"testing"
)

func orchSetup(t *testing.T) {
	t.Helper()
	t.Setenv("TUI_DASH_STATE", t.TempDir())
}

func TestOrchAppendAndCursorPages(t *testing.T) {
	orchSetup(t)
	for i := 1; i <= 3; i++ {
		if _, err := recordOrchEvent(OrchEvent{
			RunID: "run-1", OriginPrincipal: "muse",
			TargetAgent: fmt.Sprintf("w%d", i),
			EventType: OrchDelegationDispatched,
			RequestID: fmt.Sprintf("req-%d", i),
			Payload:   fmt.Sprintf("msg %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := listOrchEvents("run-1", 0, 2)
	if err != nil || len(page1) != 2 {
		t.Fatalf("page1: %v %d", err, len(page1))
	}
	if page1[0].Seq >= page1[1].Seq || page1[0].TargetAgent != "w1" {
		t.Fatalf("unstable order: %+v", page1)
	}
	page2, err := listOrchEvents("run-1", page1[1].Seq, 2)
	if err != nil || len(page2) != 1 || page2[0].TargetAgent != "w3" {
		t.Fatalf("cursor page2: %v %+v", err, page2)
	}
	caughtUp, err := listOrchEvents("run-1", page2[0].Seq, 2)
	if err != nil || len(caughtUp) != 0 {
		t.Fatalf("caught-up page must be empty: %v %+v", err, caughtUp)
	}
	// Run scoping: other runs are excluded.
	other, err := listOrchEvents("run-9", 0, 10)
	if err != nil || len(other) != 0 {
		t.Fatalf("run scope leaked: %v %+v", err, other)
	}
}

func TestOrchIdempotentAppend(t *testing.T) {
	orchSetup(t)
	e := OrchEvent{RunID: "r", OriginPrincipal: "muse",
		EventType: OrchDelegationDispatched, RequestID: "idem-1", Payload: "x"}
	s1, err := recordOrchEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := recordOrchEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatalf("retry must return original seq, got %d then %d", s1, s2)
	}
	all, err := listOrchEvents("r", 0, 10)
	if err != nil || len(all) != 1 {
		t.Fatalf("duplicate row stored: %+v", all)
	}
	// Empty request IDs never dedup.
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchMessageSent}); err != nil {
		t.Fatal(err)
	}
	if _, err := recordOrchEvent(OrchEvent{EventType: OrchMessageSent}); err != nil {
		t.Fatal(err)
	}
	msgs, err := listOrchEvents("", 0, 10)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("empty request IDs must always append: %+v", msgs)
	}
}

func TestOrchRejectsUnknownType(t *testing.T) {
	orchSetup(t)
	if _, err := recordOrchEvent(OrchEvent{EventType: "muse.dreamed"}); err == nil {
		t.Fatal("unknown event type accepted")
	}
}

func TestOrchPayloadTruncatedWithMarker(t *testing.T) {
	orchSetup(t)
	big := strings.Repeat("x", maxOrchPayload+100)
	seq, err := recordOrchEvent(OrchEvent{EventType: OrchMessageSent, Payload: big})
	if err != nil {
		t.Fatal(err)
	}
	got, err := listOrchEvents("", seq-1, 1)
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	if !strings.Contains(got[0].Payload, "[truncated 100 bytes]") {
		t.Fatal("oversize payload lacks explicit truncation marker")
	}
	if len(got[0].Payload) > maxOrchPayload+64 {
		t.Fatal("payload not bounded")
	}
}

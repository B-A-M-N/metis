package main

// P0-B: terminal input leaves the Bubble Tea update path, keeps
// per-session order, and never lets a stale result touch another
// session's UI state.

import (
	"fmt"
	"sync"
	"testing"
)

// Jobs for one session complete in enqueue order even when the
// first blocks: a single bounded sender per session, not one
// goroutine per keypress.
func TestInputSenderPreservesOrder(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var order []string
	execInputJob = func(job inputJob) (string, string) {
		if job.opID == "op-1" {
			<-release
		}
		mu.Lock()
		order = append(order, job.opID)
		mu.Unlock()
		return DispositionConfirmed, "ok"
	}
	defer func() { execInputJob = execInputJobReal }()
	t.Cleanup(func() { retireInputSenders("sess") })
	s := newInputSender("sess")
	j1, _ := s.enqueue("op-1", false, "", nil, "", "owner")
	j2, _ := s.enqueue("op-2", false, "", nil, "", "owner")
	j3, _ := s.enqueue("op-3", false, "", nil, "", "owner")
	close(release)
	for i, j := range []inputJob{j1, j2, j3} {
		res := <-j.reply
		if res.opID != fmt.Sprintf("op-%d", i+1) {
			t.Fatalf("reply %d out of order: %+v", i, res)
		}
		if res.session != "sess" || res.disposition != DispositionConfirmed {
			t.Fatalf("bad result envelope: %+v", res)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for i, want := range []string{"op-1", "op-2", "op-3"} {
		if order[i] != want {
			t.Fatalf("execution order %v, want FIFO", order)
		}
	}
}

// A full queue rejects immediately on the update path instead of
// blocking the UI or dropping the keystroke silently.
func TestInputSenderBounded(t *testing.T) {
	block := make(chan struct{})
	execInputJob = func(job inputJob) (string, string) {
		<-block
		return DispositionConfirmed, "ok"
	}
	defer func() { execInputJob = execInputJobReal }()
	t.Cleanup(func() { retireInputSenders("sess") })
	s := newInputSenderWithCap("sess", 1)
	j1, ok := s.enqueue("op-1", false, "", nil, "", "owner")
	if !ok {
		t.Fatal("first enqueue must fit")
	}
	// 1 in flight + 1 queued = full; the next must refuse now.
	if _, ok := s.enqueue("op-2", false, "", nil, "", "owner"); ok {
		t.Fatal("over-cap enqueue must be refused, not block")
	}
	// Release the stub and drain the in-flight job before returning:
	// the reply send happens after the execInputJob read, so
	// receiving it proves the stub is no longer referenced and the
	// deferred restore cannot race the sender goroutine.
	close(block)
	<-j1.reply
}

// A result for another session never touches this UI state; a
// result for the attached session lands on the status line.
func TestInputResultSessionBinding(t *testing.T) {
	m := newModel("w")
	m.names = []string{"a", "b"}
	m.running = map[string]bool{"a": true, "b": true}
	m.nav.Selected = 0
	m.nav = m.nav.EnterAttach()
	m.status = ""
	m = applyInputResult(m, inputResultMsg{session: "b", opID: "op-9", disposition: DispositionConfirmed})
	if m.status != "" {
		t.Fatalf("stale-session result must not touch status, got %q", m.status)
	}
	if m.inputPending["b"] == "" {
		t.Fatal("stale result must be parked per-session, not dropped silently")
	}
	m = applyInputResult(m, inputResultMsg{session: "a", opID: "op-1", disposition: DispositionRejected, detail: "lease held"})
	if m.status == "" {
		t.Fatal("attached-session result must reach the status line")
	}
}

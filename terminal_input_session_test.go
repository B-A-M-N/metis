package main

// Extended session-binding coverage for the terminal input split:
// stale results for another session must never display as the
// outcome for the selected session, and every typed result must
// carry its own session identity. Tea-free by design: the
// terminal boundary is exercised without Bubble Tea.

import (
	"testing"
)

// A parked stale result survives a later attached result, nav-mode
// results park without touching the status line, and detail text
// reaches the status line for the attached session only.
func TestTerminalInputResultStaysBoundToSelectedSession(t *testing.T) {
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

	m = applyInputResult(m, inputResultMsg{session: "a", opID: "op-1", disposition: DispositionRejected, detail: "lease held"})
	if m.status == "" {
		t.Fatal("attached-session result must reach the status line")
	}
	if got := m.inputPending["b"]; got == "" {
		t.Fatal("earlier parked result for b must survive the attached result for a")
	}

	m.nav = m.nav.ToNav()
	m.status = ""
	m = applyInputResult(m, inputResultMsg{session: "a", opID: "op-2", disposition: DispositionConfirmed})
	if m.status != "" {
		t.Fatalf("nav-mode result must park, not display, got %q", m.status)
	}
	if m.inputPending["a"] == "" {
		t.Fatal("nav-mode result must be parked per-session")
	}
}

// Each job's typed receipt carries its own session identity, even
// with two sessions draining concurrently.
func TestTerminalInputResultCarriesSessionIdentity(t *testing.T) {
	execInputJob = func(job inputJob) (string, string) {
		return DispositionConfirmed, "ok"
	}
	defer func() { execInputJob = execInputJobReal }()
	sa := newInputSender("sess-a")
	sb := newInputSender("sess-b")
	ja, ok := sa.enqueue("op-a", false, "", nil, "", "owner")
	if !ok {
		t.Fatal("enqueue on sess-a must fit")
	}
	jb, ok := sb.enqueue("op-b", false, "", nil, "", "owner")
	if !ok {
		t.Fatal("enqueue on sess-b must fit")
	}
	ra := <-ja.reply
	rb := <-jb.reply
	if ra.session != "sess-a" || ra.opID != "op-a" || ra.disposition != DispositionConfirmed {
		t.Fatalf("bad receipt envelope for sess-a: %+v", ra)
	}
	if rb.session != "sess-b" || rb.opID != "op-b" || rb.disposition != DispositionConfirmed {
		t.Fatalf("bad receipt envelope for sess-b: %+v", rb)
	}
}

// TestTerminalInputResultSurvivesTabSwitch pins the P1-E
// selection-change rule: a job queued while attached to A, then a
// tab switch to B, then A's receipt — the receipt parks under A
// and the status line never presents A's outcome as B's.
func TestTerminalInputResultSurvivesTabSwitch(t *testing.T) {
	m := newModel("w")
	m.names = []string{"a", "b"}
	m.running = map[string]bool{"a": true, "b": true}
	m.nav.Selected = 0
	m.nav = m.nav.EnterAttach()
	m.status = ""
	// Switch tabs while A's job is in flight.
	m.nav.Selected = 1
	m = applyInputResult(m, inputResultMsg{session: "a", opID: "op-7", disposition: DispositionConfirmed})
	if m.status != "" {
		t.Fatalf("switched-away result must not touch status, got %q", m.status)
	}
	if got := m.inputPending["a"]; got == "" {
		t.Fatal("A's receipt must park under A's identity, not the viewed tab")
	}
	if _, ok := m.inputPending["b"]; ok {
		t.Fatalf("B must gain no receipt from A's job: %+v", m.inputPending)
	}
}

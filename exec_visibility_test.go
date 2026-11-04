package main

// exec_visibility_test.go: execution-stage visibility (failing-first).
//
// Dispatched is assignment, not execution. A worker.claimed event
// is durable only after the claim's ownership checks pass, and
// the CONTROLLER pane shows the execution progression (dispatched
// / working / report_ready / verified / failed counts from
// committed task rows) so pickup-pending is never shown as
// execution.

import (
	"strings"
	"testing"
)

func TestWorkerClaimEmitsClaimedEvent(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pClaim")
	seedOwnedSession(t, "w", "pClaim")
	tk := mustPhaseTask(t, "claim-ev")
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	// Before any claim: dispatched only, no claimed event.
	if n := len(orchEventsOfType(t, OrchWorkerClaimed)); n != 0 {
		t.Fatalf("claimed event before claim: %d", n)
	}
	ad := fakeWorkerAdapter("sh", "-c", "echo hi")
	asn, idle, err := workerResolveAssignment("w", ad, testReq("op"))
	if err != nil || idle {
		t.Fatalf("resolve: idle=%v err=%v", idle, err)
	}
	if _, err := workerClaimAttempt("w", asn); err != nil {
		t.Fatalf("claim: %v", err)
	}
	evs := orchEventsOfType(t, OrchWorkerClaimed)
	if len(evs) != 1 {
		t.Fatalf("worker.claimed events = %d, want 1", len(evs))
	}
	e := evs[0]
	if e.TaskID != tk.ID || e.Attempt != asn.task.Attempt {
		t.Fatalf("claimed event must name exact task/attempt: %+v", e)
	}
	if e.TargetAgent != "w" {
		t.Fatalf("claimed target must be the claiming session: %+v", e)
	}
}

func TestControllerPaneShowsExecutionProgression(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "pExec")
	seedOwnedSession(t, "w", "pExec")
	tk := mustPhaseTask(t, "exec-prog")
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	m := newModel("")
	m.nav.OnMuse = true
	m.width, m.height = 120, 30
	snap := liveFeedSnapshot()
	m.feed = snap.feed
	m.museState = snap.state
	m.museFacts = snap.facts
	m.execCounts = snap.execCounts
	v := m.View()
	for _, want := range []string{"execution:", "dispatched=", "working="} {
		if !strings.Contains(v, want) {
			t.Fatalf("controller pane must show execution progression (%q missing):\n%s", want, v)
		}
	}
	// Dispatched-but-unclaimed reads as dispatched=1, working=0:
	// assignment is never presented as execution.
	if !strings.Contains(v, "dispatched=1") || !strings.Contains(v, "working=0") {
		t.Fatalf("parked dispatch must show dispatched=1 working=0:\n%s", v)
	}
}

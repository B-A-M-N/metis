package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestOpsDiagObserveOK(t *testing.T) {
	opsReset()
	observeOps(opsPhaseDispatch, "req-1", "task-1", 2, "sess-1", nil)
	evs := opsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Phase != opsPhaseDispatch || ev.RequestID != "req-1" || ev.TaskID != "task-1" ||
		ev.Attempt != 2 || ev.Session != "sess-1" || ev.Disposition != opsDispositionOK {
		t.Fatalf("event fields wrong: %+v", ev)
	}
	if ev.ErrorClass != "" || ev.Detail != "" {
		t.Fatalf("ok event must carry no error class/detail: %+v", ev)
	}
}

func TestOpsDiagObserveError(t *testing.T) {
	opsReset()
	observeOps(opsPhaseVerification, "req-v", "task-v", 1, "sess-v",
		errors.New("task task-v is failed; only a reported task can be verified or rejected"))
	evs := opsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Disposition != opsDispositionError {
		t.Fatalf("want error disposition: %+v", ev)
	}
	if ev.ErrorClass == "" || ev.Detail == "" {
		t.Fatalf("error event must carry class and detail: %+v", ev)
	}
}

func TestOpsDiagErrorClasses(t *testing.T) {
	cases := []struct{ in, want string }{
		{"task t is working, expected pending (concurrent dispatch won)", "conflict"},
		{"task t already has a report for attempt 1; retry instead (TD-087)", "conflict"},
		{"task t is terminal (verified); use followup for more work", "terminal"},
		{"retry budget exhausted: task t already has 3 attempts (max 3); (TD-035)", "budget"},
		{`run "r" at concurrency limit 2 (1 live); resolve a task (TD-032)`, "budget"},
		{"no such task: t", "not_found"},
		{"no such session: s", "not_found"},
		{"artifact x failed evidence integrity for task t attempt 1", "verification"},
		{"criterion c1 has no verdict; evidence integrity alone cannot verify (TD-100)", "verification"},
		{`remote principal "mallory" cannot record a pass verdict`, "auth"},
		{"attempt a1 may still be active (delivery submitted); reconcile it first (TD-118)", "reconcile_refused"},
		{"some truly novel failure mode", "unknown"},
	}
	for _, c := range cases {
		if got := classifyOpsError(errors.New(c.in)); got != c.want {
			t.Errorf("classifyOpsError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := classifyOpsError(nil); got != "" {
		t.Errorf("classifyOpsError(nil) = %q, want empty", got)
	}
}

// Typed error codes beat substring matching: a conflict-typed
// error classifies "conflict" even when its text matches no known
// substring, including through a wrapping layer. Untyped errors
// keep the substring fallback.
func TestOpsDiagTypedCodeBeatsSubstring(t *testing.T) {
	typed := &ProvisionError{Message: "totally novel failure mode", Conflict: true}
	if got := classifyOpsError(typed); got != "conflict" {
		t.Errorf("typed conflict classified %q, want conflict", got)
	}
	wrapped := fmt.Errorf("outer context: %w", typed)
	if got := classifyOpsError(wrapped); got != "conflict" {
		t.Errorf("wrapped typed conflict classified %q, want conflict", got)
	}
	untyped := errors.New("totally novel failure mode")
	if got := classifyOpsError(untyped); got != "unknown" {
		t.Errorf("untyped novel error classified %q, want unknown fallback", got)
	}
}

func TestOpsDiagRedaction(t *testing.T) {
	secrets := []string{
		"dispatch failed: api_key=hunter2-secret-value",
		"claim refused: token: abcdef123456",
		"auth error: password=s3cr3t-p@ss",
		"remote failed: Bearer abcdef1234567890",
		"leaked key sk-abcdef1234567890 in output",
		"secret=shhh-dont-log-me please",
	}
	for _, in := range secrets {
		got := redactOpsDetail(in)
		for _, leak := range []string{"hunter2", "abcdef123456", "s3cr3t", "shhh-dont-log-me"} {
			if strings.Contains(got, leak) {
				t.Errorf("redactOpsDetail leaked secret: in=%q out=%q", in, got)
			}
		}
		if !strings.Contains(got, "<redacted") {
			t.Errorf("redactOpsDetail left no redaction marker: in=%q out=%q", in, got)
		}
	}
	// Non-secret text passes through so failure classes stay visible.
	plain := "task t is working, expected pending (concurrent dispatch won)"
	if got := redactOpsDetail(plain); got != plain {
		t.Errorf("redactOpsDetail altered clean text: %q", got)
	}
}

func TestOpsDiagRedactsObservedErrors(t *testing.T) {
	opsReset()
	observeOps(opsPhaseClaim, "req-r", "task-r", 1, "sess-r",
		errors.New("claim refused: api_key=hunter2-secret-value"))
	evs := opsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %d", len(evs))
	}
	if strings.Contains(evs[0].Detail, "hunter2") {
		t.Fatalf("stored detail leaked credential: %q", evs[0].Detail)
	}
}

func TestOpsDiagRingBounded(t *testing.T) {
	opsReset()
	for i := 0; i < opsRingCap+50; i++ {
		observeOps(opsPhaseReport, "req", "task", i, "", nil)
	}
	evs := opsSnapshot()
	if len(evs) != opsRingCap {
		t.Fatalf("ring must stay bounded at %d, got %d", opsRingCap, len(evs))
	}
	if evs[len(evs)-1].Attempt != opsRingCap+49 {
		t.Fatalf("ring must keep newest events, last=%+v", evs[len(evs)-1])
	}
}

func TestOpsDiagNeedsNoDatabase(t *testing.T) {
	// Observation must work without any ledger access: a bogus
	// task id must neither error nor touch the DB.
	opsReset()
	observeOps(opsPhaseReconciliation, "req-x", "no-such-task", 0, "", errors.New("boom"))
	if len(opsSnapshot()) != 1 {
		t.Fatal("observe must record without database access")
	}
}

package main

// attention_split_test.go: characterization pin for the P2 attention
// split. The approval-specific escalation path (raise/resolve per
// pending native approval) must behave identically after moving out
// of attention.go into attention_approval.go. Generic persistence
// and revision fencing stay covered by attention_test.go.

import (
	"testing"
)

// No dispatched task: one open missing_approval escalation bound to
// the session; resolution by approval id closes it.
func TestAttentionSplitApprovalNoTask(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if err := raiseApprovalAttention("w-split", "ap-split-1"); err != nil {
		t.Fatal(err)
	}
	// Re-raise is idempotent: same identity reuses the row.
	if err := raiseApprovalAttention("w-split", "ap-split-1"); err != nil {
		t.Fatalf("re-raise must reuse the row: %v", err)
	}
	open, err := listAttention("", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "missing_approval" || open[0].Status != "open" {
		t.Fatalf("one open missing_approval expected, got %+v", open)
	}
	if open[0].Session != "w-split" {
		t.Fatalf("escalation must bind the session, got %+v", open[0])
	}
	resolveApprovalAttention("ap-split-1", "op", "decided:allow")
	still, err := listAttention("", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != 0 {
		t.Fatalf("resolution must close the escalation, got %+v", still)
	}
	hist, err := listAttention("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Status != "resolved" {
		t.Fatalf("closed escalation must remain in history, got %+v", hist)
	}
}

// Resolving an unknown approval id is a silent no-op, never an
// error: the decision stands on its own row.
func TestAttentionSplitResolveUnknownNoop(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	resolveApprovalAttention("ap-no-such", "op", "decided:allow")
	open, err := listAttention("", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("no escalations must exist, got %+v", open)
	}
}

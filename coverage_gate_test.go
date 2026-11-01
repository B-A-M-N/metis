package main

// coverage_gate_test.go: revised evidence re-opens the gate.
//
// An old review must not satisfy a changed revision: satisfying
// the duty owed at digest v1 opens the gate for v1, but once
// revised evidence files digest v2, the gate for v2 is blocked by
// a fresh duty — the satisfied v1 row neither blocks v1 again nor
// unblocks v2. (The sibling invariants — verify never
// auto-satisfies an unrelated duty, and blocked/excepted require
// an explicit reason — are covered by
// TestCoverageVerifiedDoesNotSatisfyObligation and
// TestObligationBlockingSet.)

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoverageRevisedEvidenceReopensGate(t *testing.T) {
	run, work := coverageSetup(t, "reopen")
	tk, err := createTask(run, "w", "touch auth", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	p := filepath.Join(work, "auth.go")
	if err := os.WriteFile(p, []byte("package auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := submitReport(tk.ID, ReportFiling{Report: "v1", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report v1: %v", err)
	}
	tk, err = getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	rev1 := reportDigestOf("v1")
	if got, _ := reportDigestFor(tk.ID, tk.Attempt); got != rev1 {
		t.Fatalf("filed digest = %q, want %q", got, rev1)
	}

	// Fresh evidence is owed a review: the gate is closed.
	if err := coverageGate(tk.ID, tk.Attempt, testReq("reviewer")); err == nil {
		t.Fatal("gate open with an outstanding duty; must be refused")
	} else if !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("refusal must name the obligation: %v", err)
	}

	// An independent review satisfies the v1 duty; the gate opens.
	obs, err := listObligations(run, tk.ID)
	if err != nil || len(obs) != 1 {
		t.Fatalf("setup: obligations = %+v, err %v", obs, err)
	}
	if err := setObligationState(obs[0].ID, ObligationRunning, testReq("reviewer")); err != nil {
		t.Fatalf("pickup: %v", err)
	}
	if err := setObligationState(obs[0].ID, ObligationSatisfied, testReq("reviewer")); err != nil {
		t.Fatalf("satisfy: %v", err)
	}
	if err := coverageGate(tk.ID, tk.Attempt, testReq("reviewer")); err != nil {
		t.Fatalf("gate closed after independent satisfaction: %v", err)
	}

	// Revised evidence files a new revision through the same seam
	// submitReport uses: the old satisfied row stays terminal, and
	// a fresh required duty opens at the new digest.
	rev2 := reportDigestOf("v2")
	if err := generateCoverageObligations(tk.ID, rev2, testReq("test")); err != nil {
		t.Fatalf("generate at rev2: %v", err)
	}
	if got, _ := getObligation(obs[0].ID); got.Status != ObligationSatisfied {
		t.Fatalf("old review must stay satisfied, got %q", got.Status)
	}

	// The old revision stays unblocked...
	if old, err := blockingTaskObligations(tk.ID, rev1); err != nil {
		t.Fatal(err)
	} else if len(old) != 0 {
		t.Fatalf("satisfied v1 review must not re-block v1, got %+v", old)
	}
	// ...while the new revision is blocked until reviewed.
	decision, err := assessCoverageGate(tk.ID, tk.Attempt, rev2)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Blocked() {
		t.Fatal("revised evidence left the gate open; an old review must not satisfy a changed revision")
	}
	if err := decision.asError(); err == nil || !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("blocked decision must refuse naming the obligation, got %v", err)
	}
	// The filed revision's gate is unaffected by the newer duty.
	if err := coverageGate(tk.ID, tk.Attempt, testReq("reviewer")); err != nil {
		t.Fatalf("v1 gate must stay open while v2 is owed: %v", err)
	}
}

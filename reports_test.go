package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArtifactEvidence(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "oa")
	seedOwnedSession(t, "agent9", "oa")
	work := loadRegistry()["agent9"].Workdir
	tk, err := createTask("run9", "agent9", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	// Unknown task and missing file are refused.
	if _, err := attachArtifact("task-nonexistent", "/x"); err == nil {
		t.Fatal("artifact on unknown task should fail")
	}
	if _, err := attachArtifact(tk.ID, filepath.Join(work, "missing.txt")); err == nil {
		t.Fatal("missing file should fail")
	}
	// Files outside the worker's workspace are refused, even when
	// they exist and are readable.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, outside); err == nil {
		t.Fatal("artifact outside the workspace should fail")
	}
	esc := filepath.Join(work, "..", "escape.txt")
	if err := os.WriteFile(esc, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, filepath.Join(work, "..", "escape.txt")); err == nil {
		t.Fatal("path traversal should fail")
	}
	// A symlink inside the workspace pointing out is still an escape.
	link := filepath.Join(work, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, link); err == nil {
		t.Fatal("symlink escape should fail")
	}
	// Round trip: attach, list, digest matches, re-verify OK.
	p := filepath.Join(work, "evidence.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := attachArtifact(tk.ID, p)
	if err != nil {
		t.Fatal(err)
	}
	arts, err := listArtifacts(tk.ID)
	if err != nil || len(arts) != 1 || arts[0].Digest != a.Digest {
		t.Fatalf("list: %v %+v", err, arts)
	}
	ok, err := verifyArtifact(arts[0])
	if err != nil || !ok {
		t.Fatalf("fresh artifact should verify: %v %v", ok, err)
	}
	// Tampering is detected.
	if err := os.WriteFile(p, []byte("forged"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err = verifyArtifact(arts[0])
	if err != nil || ok {
		t.Fatalf("modified artifact should fail verify: %v %v", ok, err)
	}
}

func TestDispatchDeliversInstruction(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "od")
	seedOwnedSession(t, "worker1", "od")
	work := loadRegistry()["worker1"].Workdir
	tk, err := createTask("rund", "worker1", "fix the bug", "tests pass")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(tk.ID, "worker1"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(work, "task-"+tk.ID+".md"))
	if err != nil {
		t.Fatalf("instruction file not delivered: %v", err)
	}
	for _, want := range []string{tk.ID, "fix the bug", "tests pass"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("instruction file missing %q:\n%s", want, body)
		}
	}
	recs, err := listReceipts(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range recs {
		if r.Kind == "submitted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no submitted receipt: %+v", recs)
	}
}

func TestVerifyRequiresEvidence(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "ov")
	seedOwnedSession(t, "worker2", "ov")
	work := loadRegistry()["worker2"].Workdir
	newReported := func() Task {
		tk, err := createTask("runv", "worker2", "obj", "acc")
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatchTask(tk.ID, "worker2"); err != nil {
			t.Fatal(err)
		}
		if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
			t.Fatal(err)
		}
		return tk
	}
	// A bare report is not verifiable.
	tk := newReported()
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("report alone must not verify")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("failed verify changed status to %s", got.Status)
	}
	// Tampered evidence blocks verification.
	p := filepath.Join(work, "ev.txt")
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := attachArtifact(tk.ID, p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("forged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("tampered artifact must not verify")
	}
	// Restored evidence still needs a criterion judgment: hashes
	// alone never verify (TD-100, remediation Phase B).
	if err := os.WriteFile(p, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err == nil {
		t.Fatal("intact digests without a criterion verdict must not verify")
	}
	got, _ := getTask(tk.ID)
	v := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass, Reviewer: "reviewer-01",
		Evidence: []string{p}, Reason: "reproduced independently", Attempt: got.Attempt}
	if err := fileCriterionVerdict(tk.ID, v, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("verifier")); err != nil {
		t.Fatalf("verified evidence with a PASS verdict should pass: %v", err)
	}
	fr, err := getFullReport(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, r := range fr.Receipts {
		kinds[r.Kind] = true
	}
	for _, want := range []string{"submitted", "report_received", "evidence_checked", "verified"} {
		if !kinds[want] {
			t.Fatalf("receipt trail missing %q: %+v", want, fr.Receipts)
		}
	}
	// Evidence-free tasks need a named operator approval.
	tk2 := newReported()
	if err := verifyTask(tk2.ID, true, "op-1", testReq("op-1")); err != nil {
		t.Fatalf("operator approval should verify: %v", err)
	}
	if got, _ := getTask(tk2.ID); got.Status != TaskVerified {
		t.Fatalf("approved task status: %s", got.Status)
	}
	recs, _ := listReceipts(tk2.ID)
	found := false
	for _, r := range recs {
		if r.Kind == "operator_approved" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no operator_approved receipt: %+v", recs)
	}
}

func TestCompareReports(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "or")
	seedOwnedSession(t, "sess-a", "or")
	seedOwnedSession(t, "sess-b", "or")
	a, _ := createTask("runc", "agent-a", "audit", "")
	b, _ := createTask("runc", "agent-b", "audit", "")
	if err := dispatchTask(a.ID, "sess-a"); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTask(b.ID, "sess-b"); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(a.ID, ReportFiling{Report: "a says x", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	// b has no report: excluded from comparison.
	got, err := compareReports("runc")
	if err != nil || len(got) != 1 || got[0].ID != a.ID || got[0].Agent != "sess-a" {
		t.Fatalf("compare: %v %+v", err, got)
	}
	if _, err := compareReports(""); err == nil {
		t.Fatal("compare without run should fail")
	}
	if _, err := compareReports("empty-run"); err != nil {
		t.Fatal(err)
	}
	fr, err := getFullReport(a.ID)
	if err != nil || fr.Task.Report != "a says x" {
		t.Fatalf("full report: %v %+v", err, fr.Task)
	}
}

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0 coverage enforcement, failing-first: policy rules must generate
// obligations from filed evidence, and verify/finalize must refuse
// completion while mandatory duties are outstanding. requireObligation
// alone enforces nothing until these gates exist.

const coverageTestPolicy = `{"workers":[{"id":"w-a","harness":"demo","profile":"","role":"implementation"}],
	"limits":{"max_active_workers":8},
	"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false},
	"coverage":[{"kind":"security_verification","paths":["*auth*"],"independent":true}]}`

// coverageSetup pins a policy with a security rule matching *auth*
// artifacts and returns the run and session workdir.
func coverageSetup(t *testing.T, tag string) (string, string) {
	t.Helper()
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, tag)
	seedHarnessSession(t, "w", tag, "demo")
	project := uniqueTaskKey("proj-cov-" + tag)
	if _, err := setTeamPolicy(project, coverageTestPolicy, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set policy: %v", err)
	}
	run := uniqueTaskKey("run-cov-" + tag)
	if err := pinTeamPolicy(run, project); err != nil {
		t.Fatalf("pin: %v", err)
	}
	return run, loadRegistry()["w"].Workdir
}

func reportDigestOf(report string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(report)))
}

// A filed artifact matching a policy rule must generate its
// obligation at the report digest; a non-matching artifact must
// generate nothing.
func TestCoverageGeneratedFromPolicy(t *testing.T) {
	run, _ := coverageSetup(t, "gen")
	seedHarnessSession(t, "w2", "gen", "demo")
	workOf := func(sess string) string {
		t.Helper()
		reg := loadRegistry()
		e, ok := reg[sess]
		if !ok {
			t.Fatalf("no session %s", sess)
		}
		return e.Workdir
	}
	mk := func(sess, name, objective, body string) string {
		t.Helper()
		tk, err := createTask(run, sess, objective, "checks pass")
		if err != nil {
			t.Fatal(err)
		}
		if err := dispatchTask(tk.ID, sess); err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		if body != "" {
			p := filepath.Join(workOf(sess), name)
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := attachArtifact(tk.ID, p); err != nil {
				t.Fatalf("attach: %v", err)
			}
		}
		rep := "report for " + name
		if err := submitReport(tk.ID, ReportFiling{Report: rep, Source: SourceCLISubmission, Reporter: "test"}); err != nil {
			t.Fatalf("report: %v", err)
		}
		return tk.ID
	}
	authID := mk("w", "auth.go", "touch auth", "package auth")
	plainID := mk("w2", "plain.txt", "touch plain", "hello")
	obs, err := listObligations(run, authID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 || obs[0].Kind != "security_verification" || obs[0].Status != ObligationRequired {
		t.Fatalf("auth task obligations = %+v, want one required security_verification", obs)
	}
	if obs[0].TargetRev != reportDigestOf("report for auth.go") {
		t.Fatalf("target rev = %q, want report digest", obs[0].TargetRev)
	}
	obs, err = listObligations(run, plainID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Fatalf("non-matching artifact generated %+v, want none", obs)
	}
}

// A pass verdict with an outstanding duty is refused; the task stays
// report_ready; an independent satisfaction unblocks verification.
func TestCoverageBlocksVerify(t *testing.T) {
	run, work := coverageSetup(t, "blk")
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
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	obs, err := listObligations(run, tk.ID)
	if err != nil || len(obs) != 1 {
		t.Fatalf("setup: obligations = %+v, err %v", obs, err)
	}
	fileAllPassVerdicts(t, tk.ID, p, "reviewer")
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err == nil {
		t.Fatal("verify passed with outstanding coverage duty; must be refused")
	} else if !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("refusal must name the obligation: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskReportReady {
		t.Fatalf("refused verify changed status to %s", got.Status)
	}
	// The implementing worker cannot discharge its own review duty.
	if err := setObligationState(obs[0].ID, ObligationRunning, testReq("w")); err != nil {
		t.Fatalf("reviewer pickup: %v", err)
	}
	if err := setObligationState(obs[0].ID, ObligationSatisfied, testReq("w")); err == nil {
		t.Fatal("worker satisfied its own review duty; independence required")
	}
	if err := setObligationState(obs[0].ID, ObligationSatisfied, testReq("reviewer")); err != nil {
		t.Fatalf("independent satisfaction refused: %v", err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("reviewer")); err != nil {
		t.Fatalf("verify after independent satisfaction refused: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskVerified {
		t.Fatalf("expected verified, got %s", got.Status)
	}
}

// A run with an unresolved duty cannot finalize even when every
// task is terminal; an explicit exception with reason unblocks it.
func TestCoverageBlocksFinalize(t *testing.T) {
	run, work := coverageSetup(t, "fin")
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
	if err := submitReport(tk.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	if err := verifyTask(tk.ID, false, "", testReq("reviewer")); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := finalizeRun(run, OperationRequest{Actor: "test", RequestID: newRequestID()}); err == nil {
		t.Fatal("finalize passed with an unresolved coverage duty; must be refused")
	} else if !strings.Contains(err.Error(), "obligation") {
		t.Fatalf("refusal must name the obligation: %v", err)
	}
	obs, err := listObligations(run, tk.ID)
	if err != nil || len(obs) != 1 {
		t.Fatalf("setup: obligations = %+v, err %v", obs, err)
	}
	if err := setObligationState(obs[0].ID, ObligationExcepted, testReq("operator"), "auth module removed from scope"); err != nil {
		t.Fatalf("except: %v", err)
	}
	if _, err := finalizeRun(run, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("finalize after exception refused: %v", err)
	}
}

// A new report supersedes outstanding duties pointing at an older
// revision instead of stacking a second required row.
func TestCoverageSupersededOnNewEvidence(t *testing.T) {
	run, work := coverageSetup(t, "sup")
	tk, err := createTask(run, "w", "touch auth", "checks pass")
	if err != nil {
		t.Fatal(err)
	}
	old, err := requireObligation(run, tk.ID, "security_verification", "rev-old",
		"[]", OperationRequest{Actor: "test", RequestID: newRequestID()})
	if err != nil {
		t.Fatalf("seed obligation: %v", err)
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
	if err := submitReport(tk.ID, ReportFiling{Report: "v2", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	seed, err := getObligation(old)
	if err != nil {
		t.Fatal(err)
	}
	if seed.Status != ObligationStale {
		t.Fatalf("old duty status = %q, want stale", seed.Status)
	}
	obs, err := listObligations(run, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	required := 0
	for _, o := range obs {
		if o.Status == ObligationRequired {
			required++
			if o.TargetRev != reportDigestOf("v2") {
				t.Fatalf("required duty targets %q, want current report digest", o.TargetRev)
			}
		}
	}
	if required != 1 {
		t.Fatalf("want exactly one required duty after supersede, got %+v", obs)
	}
}

// Policy validation: unknown kinds and bad globs are refused at
// publish time, never smuggled into enforcement.
func TestCoveragePolicyValidation(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	badKind := `{"workers":[{"id":"w","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":1},"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false},
		"coverage":[{"kind":"vibes_check","paths":[]}]}`
	if _, err := setTeamPolicy(uniqueTaskKey("p"), badKind, OperationRequest{Actor: "test", RequestID: newRequestID()}); err == nil {
		t.Fatal("unknown coverage kind published; must be refused")
	}
	badGlob := `{"workers":[{"id":"w","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":1},"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false},
		"coverage":[{"kind":"security_verification","paths":["[unclosed"]}]}`
	if _, err := setTeamPolicy(uniqueTaskKey("p"), badGlob, OperationRequest{Actor: "test", RequestID: newRequestID()}); err == nil {
		t.Fatal("bad coverage glob published; must be refused")
	}
}

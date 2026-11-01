package main

// Slice 4: one complete correction cycle through the managed loop.
// External orchestrator dispatches; the loop executes; an imperfect
// result draws a FAIL verdict; the orchestrator issues linked
// corrective work; the loop executes again; PASS verdicts verify.
// Deterministic stand-in providers (fixed shell scripts) drive the
// loop — this qualifies the loop/correction machinery, not any
// provider CLI. Real-model qualification stays a manual,
// quota-spending step outside the suite.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scriptAdapter runs one fixed shell script as the provider. The
// script is the stand-in's behavior: v1 ignores the requirement,
// v2 fulfills it. Same argv contract as a real provider CLI.
func scriptAdapter(script string) Adapter {
	return Adapter{
		Harness:   "test-scripted",
		OneShot:   true,
		ManagedArgv: func(workdir, prompt, model string) ([]string, error) {
			return []string{"sh", "-c", script}, nil
		},
	}
}

func TestCorrectionCycleEndToEnd(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "e2e")
	seedOwnedSession(t, "w", "e2e")
	work := loadRegistry()["w"].Workdir

	// 1. Orchestrator creates and dispatches real work.
	tk, err := createTask("run-e2e", "w", "write required.txt containing DONE", "required.txt exists with DONE")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}

	// 2. Managed worker consumes the committed attempt and executes.
	v1 := scriptAdapter("echo 'working on it (but never writing the file)'")
	if ran, err := runWorkerOnce("w", v1, testReq("op")); err != nil || !ran {
		t.Fatalf("loop must execute attempt 1: ran=%v err=%v", ran, err)
	}

	// 3. Imperfect result: the required file does not exist. The
	// reviewer says so, citing the worker's own output log.
	got, _ := getTask(tk.ID)
	if got.Status != TaskReportReady {
		t.Fatalf("attempt 1 must be reported, got %s", got.Status)
	}
	arts, err := currentAttemptArtifacts(tk.ID, 1)
	if err != nil || len(arts) == 0 {
		t.Fatalf("worker output must be attached: %v (%v)", arts, err)
	}
	if _, err := os.Stat(filepath.Join(work, "required.txt")); !os.IsNotExist(err) {
		t.Fatal("stand-in v1 must leave the requirement unmet")
	}
	fail := CriterionVerdict{CriterionID: "C1", Verdict: VerdictFail,
		Reviewer: "reviewer-01", Evidence: []string{arts[0].Path},
		Reason: "required.txt absent after attempt 1; output log shows no write", Attempt: 1}
	if err := fileCriterionVerdict(tk.ID, fail, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(tk.ID, true, "", testReq("reviewer-01")); err == nil {
		t.Fatal("FAIL verdict must block verification")
	}
	// The orchestrator rejects the imperfect result, freeing the
	// workspace occupant record; the correction then takes over.
	if err := verifyTask(tk.ID, false, "", testReq("reviewer-01")); err != nil {
		t.Fatalf("rejection must record the failure: %v", err)
	}

	// 4. Orchestrator submits linked corrective work; the loop
	// executes only the correction — the finished attempt is not
	// relaunched.
	child, err := followupTask(tk.ID, "write required.txt containing DONE (previous attempt never wrote it)", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != tk.ID {
		t.Fatalf("correction must link to the rejected attempt, got parent %q", child.Parent)
	}
	if err := dispatchTaskWithRequest(child.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	v2 := scriptAdapter("echo DONE > required.txt && echo 'wrote required.txt'")
	if ran, err := runWorkerOnce("w", v2, testReq("op")); err != nil || !ran {
		t.Fatalf("loop must execute the correction: ran=%v err=%v", ran, err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("parent attempt must not re-execute: %d receipts", n)
	}

	// 5. Requirement now met: attach the produced file as evidence,
	// judge the criterion PASS, verify.
	cwork := loadRegistry()["w"].Workdir
	req := filepath.Join(cwork, "required.txt")
	body, err := os.ReadFile(req)
	if err != nil || strings.TrimSpace(string(body)) != "DONE" {
		t.Fatalf("correction must produce the requirement, got %q (%v)", body, err)
	}
	if _, err := attachArtifact(child.ID, req); err != nil {
		t.Fatal(err)
	}
	committed, _ := getTask(child.ID)
	pass := CriterionVerdict{CriterionID: "C1", Verdict: VerdictPass,
		Reviewer: "reviewer-01", Evidence: []string{req},
		Reason: "required.txt contains DONE, reproduced from the filed artifact", Attempt: committed.Attempt}
	if err := fileCriterionVerdict(child.ID, pass, testReq("reviewer")); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(child.ID, true, "", testReq("reviewer-01")); err != nil {
		t.Fatalf("correction with PASS verdict must verify: %v", err)
	}

	// 6. Restart stability: nothing dispatched, nothing to run;
	// identities, evidence, judgments, and unresolved work intact.
	if ran, err := runWorkerOnce("w", v2, testReq("op2")); err != nil || ran {
		t.Fatalf("post-verify loop must idle: ran=%v err=%v", ran, err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskFailed {
		t.Fatalf("rejected parent stays failed, got %s", got.Status)
	}
	if got, _ := getTask(child.ID); got.Status != TaskVerified {
		t.Fatalf("correction stays verified, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports`); n != 2 {
		t.Fatalf("both claims preserved, got %d report rows", n)
	}
	r := reconcileOrFail(t, child.ID)
	if !strings.Contains(strings.ToUpper(r.Recommendation+" "+r.Reason), "DONE") {
		t.Fatalf("verified correction reconciles as done, got %q", r.Recommendation)
	}
}

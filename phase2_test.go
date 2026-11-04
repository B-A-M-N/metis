package main

// Phase-2 slice 1: bounded correction/retry budgets (TD-035, TD-036),
// corrective continuity binding (TD-082), and attributed followup
// requests (TD-006, TD-084). Failing-first: followupTask takes no
// request, stores no depth/report binding, and neither followup nor
// retry has any budget.

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// listReceiptsOrFail returns a task's receipts or fails the test.
func listReceiptsOrFail(t *testing.T, taskID string) []Receipt {
	t.Helper()
	rs, err := listReceipts(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Followup without an identifiable request must be refused: every
// state-changing operation carries an authorized request (TD-006) and
// interventions must be attributable (TD-084).
func TestFollowupRequiresRequest(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-p2a", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := followupTask(tk.ID, "try again", OperationRequest{}); err == nil {
		t.Fatal("followup without a request must be refused")
	}
}

// A corrective task must reference the exact preceding state (TD-082):
// parent id, parent report digest snapshot, carried acceptance, depth.
func TestFollowupBindsParentReport(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2b")
	seedOwnedSession(t, "w", "op2b")
	tk, err := createTask("run-p2b", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := markTaskWorking(tk.ID, 1, testReq("test")); err != nil {
		t.Fatalf("working: %v", err)
	}
	report := "finding: nothing wrong"
	if err := submitReport(tk.ID, ReportFiling{Report: report, Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatalf("report: %v", err)
	}
	child, err := followupTask(tk.ID, "fix it", testReq("orch"))
	if err != nil {
		t.Fatalf("followup: %v", err)
	}
	if child.Parent != tk.ID {
		t.Fatalf("child must link parent %s, got %q", tk.ID, child.Parent)
	}
	if child.CorrectionNo != 1 {
		t.Fatalf("first correction depth must be 1, got %d", child.CorrectionNo)
	}
	want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(report)))
	if child.ParentReport != want {
		t.Fatalf("child must snapshot parent report digest %s, got %q", want, child.ParentReport)
	}
	if child.Acceptance != "acc" {
		t.Fatalf("acceptance must carry over, got %q", child.Acceptance)
	}
	if child.Status != TaskPending {
		t.Fatalf("correction starts pending, got %s", child.Status)
	}
}

// Corrective creation must respect an explicit stopping condition
// (TD-036): chaining beyond the budget is refused, and the refusal
// names the budget.
func TestCorrectionBudgetEnforced(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	tk, err := createTask("run-p2c", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	cur := tk
	for i := 0; i < maxCorrectionDepth; i++ {
		next, err := followupTask(cur.ID, fmt.Sprintf("correction %d", i+1), testReq("orch"))
		if err != nil {
			t.Fatalf("correction %d within budget refused: %v", i+1, err)
		}
		if next.CorrectionNo != i+1 {
			t.Fatalf("expected depth %d, got %d", i+1, next.CorrectionNo)
		}
		cur = next
	}
	if _, err := followupTask(cur.ID, "one too many", testReq("orch")); err == nil {
		t.Fatal("correction beyond budget must be refused")
	} else if !strings.Contains(err.Error(), "correction budget") {
		t.Fatalf("refusal must name the budget: %v", err)
	}
}

// Retry follows an explicit policy and preserves the original failure
// (TD-035): attempts are bounded, history is kept, and the refusal
// names the budget instead of silently dropping the retry.
func TestRetryBudgetEnforced(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2d")
	seedOwnedSession(t, "w", "op2d")
	tk, err := createTask("run-p2d", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Each fresh delivery is unconfirmed (submitted, non-live), so
	// reconcile proceeds until the attempt budget is exhausted.
	for i := 0; i < maxDispatchAttempts-1; i++ {
		if err := retryDispatch(tk.ID, "w", testReq("orch")); err != nil {
			t.Fatalf("retry %d within budget refused: %v", i+1, err)
		}
	}
	got, _ := getTask(tk.ID)
	if got.Attempt != maxDispatchAttempts {
		t.Fatalf("expected %d attempts, got %d", maxDispatchAttempts, got.Attempt)
	}
	if err := retryDispatch(tk.ID, "w", testReq("orch")); err == nil {
		t.Fatal("retry beyond budget must be refused")
	} else if !strings.Contains(err.Error(), "retry budget") {
		t.Fatalf("refusal must name the budget: %v", err)
	}
	// Original failure history preserved, never rewritten.
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, tk.ID); n != maxDispatchAttempts {
		t.Fatalf("expected %d attempt rows, got %d", maxDispatchAttempts, n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND state = ?`, tk.ID, AttemptSuperseded); n != maxDispatchAttempts-1 {
		t.Fatalf("expected %d superseded rows, got %d", maxDispatchAttempts-1, n)
	}
}

// --- Slice 2: dependencies (TD-030/031), run finalization (TD-041),
// --- cancellation propagation (TD-037). Failing-first: no task_deps
// --- table, dispatch checks nothing, no finalizeRun, cancelTask takes
// --- no request and orphans active descendants.

// A task with an unsatisfied mandatory dependency must not start
// (TD-030): dispatch is refused and names the blocking dependency.
func TestDependencyBlocksDispatch(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2e")
	seedOwnedSession(t, "w", "op2e")
	dep, err := createTask("run-p2e", "agent", "foundation", "")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-p2e", "agent", "dependent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(tk.ID, dep.ID, testReq("orch")); err != nil {
		t.Fatalf("addTaskDep: %v", err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err == nil {
		t.Fatal("dispatch with unsatisfied dependency must be refused")
	} else if !strings.Contains(err.Error(), dep.ID) {
		t.Fatalf("refusal must name the blocking dependency: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskPending {
		t.Fatalf("refused dispatch changed status to %s", got.Status)
	}
}

// A dependency is satisfied only by verified completion (TD-031):
// reported-but-unverified, failed, and cancelled deps all still block.
func TestDependencyRequiresVerified(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2f")
	seedOwnedSession(t, "w", "op2f")
	dep, err := createTask("run-p2f", "agent", "foundation", "")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-p2f", "agent", "dependent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(tk.ID, dep.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(dep.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dep dispatch: %v", err)
	}
	if err := markTaskWorking(dep.ID, 1, testReq("test")); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(dep.ID, ReportFiling{Report: "done-ish", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err == nil {
		t.Fatal("reported-but-unverified dependency must still block")
	}
	if err := verifyTask(dep.ID, true, "op", testReq("test")); err != nil {
		t.Fatalf("verify dep: %v", err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("verified dependency must unblock dispatch: %v", err)
	}
}

// Failed and cancelled dependencies never unblock a dependent.
func TestDependencyFailedBlocks(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2g")
	seedOwnedSession(t, "w", "op2g")
	dep, err := createTask("run-p2g", "agent", "foundation", "")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-p2g", "agent", "dependent", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(tk.ID, dep.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(dep.ID, "w", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := markTaskWorking(dep.ID, 1, testReq("test")); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(dep.ID, ReportFiling{Report: "bad work", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(dep.ID, false, "", testReq("test")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err == nil {
		t.Fatal("failed dependency must block dispatch")
	}
}

// Dependency edges must be acyclic: A-on-B then B-on-A is refused.
func TestDependencyCycleRejected(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-p2h", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p2h", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(a.ID, b.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := addTaskDep(b.ID, a.ID, testReq("orch")); err == nil {
		t.Fatal("cyclic dependency must be refused")
	} else if !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("refusal must name the cycle: %v", err)
	}
	if err := addTaskDep(a.ID, a.ID, testReq("orch")); err == nil {
		t.Fatal("self-dependency must be refused")
	}
}

// A run must not finalize while required work remains unresolved
// (TD-041): pending/dispatched/working/report_ready tasks block;
// verified/failed/cancelled tasks do not.
func TestFinalizeRunRefusesUnresolved(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	a, err := createTask("run-p2i", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p2i", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeRun("run-p2i", testReq("orch")); err == nil {
		t.Fatal("finalize with pending tasks must be refused")
	} else if !strings.Contains(err.Error(), a.ID) || !strings.Contains(err.Error(), b.ID) {
		t.Fatalf("refusal must name unresolved tasks: %v", err)
	}
	if err := cancelTask(a.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeRun("run-p2i", testReq("orch")); err == nil {
		t.Fatal("finalize with one pending task must still be refused")
	}
	if err := cancelTask(b.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	sum, err := finalizeRun("run-p2i", testReq("orch"))
	if err != nil {
		t.Fatalf("finalize of fully-resolved run: %v", err)
	}
	if sum.Total != 2 || sum.Cancelled != 2 {
		t.Fatalf("bad finalization summary: %+v", sum)
	}
	if _, err := finalizeRun("other-run", testReq("orch")); err == nil {
		t.Fatal("finalize of a run with no tasks must be refused")
	}
}

// Cancelling a parent must account for active descendants (TD-037):
// the whole chain cancels, unrelated run-mates are untouched (TD-040),
// and the cancellation is attributable (TD-006/084).
func TestCancelPropagatesToDescendants(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	root, err := createTask("run-p2j", "agent", "root", "")
	if err != nil {
		t.Fatal(err)
	}
	mate, err := createTask("run-p2j", "agent", "mate", "")
	if err != nil {
		t.Fatal(err)
	}
	child, err := followupTask(root.ID, "fix", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	grand, err := followupTask(child.ID, "fix harder", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cancelTask(root.ID, OperationRequest{}); err == nil {
		t.Fatal("cancel without a request must be refused")
	}
	if err := cancelTask(root.ID, testReq("orch")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	for _, id := range []string{root.ID, child.ID, grand.ID} {
		if got, _ := getTask(id); got.Status != TaskCancelled {
			t.Fatalf("descendant %s is %s, want cancelled", id, got.Status)
		}
	}
	if got, _ := getTask(mate.ID); got.Status != TaskPending {
		t.Fatalf("unrelated run-mate changed to %s", got.Status)
	}
}

// --- Slice 3: execution-root correctness (TD-050), worktree
// --- isolation (TD-051), no implicit provider fallback (TD-052).
// --- Failing-first: tasks record no execution root, dispatch
// --- ignores workspace mismatch, concurrent tasks share a workdir
// --- silently.

// Dispatch immutably binds the task to the session's canonical
// workspace (TD-050): the root is recorded and receipted.
func TestDispatchBindsExecutionRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2k")
	seedOwnedSession(t, "w", "op2k")
	tk, err := createTask("run-p2k", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, _ := getTask(tk.ID)
	want, err := canonicalRoot(loadRegistry()["w"].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutionRoot != want {
		t.Fatalf("task bound to %q, want session root %q", got.ExecutionRoot, want)
	}
}

// A pre-authorized root is enforced (TD-050): dispatch to a session
// rooted elsewhere is refused naming both roots; nothing is claimed.
func TestDispatchEnforcesAuthorizedRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2l")
	seedOwnedSession(t, "wa", "op2l")
	seedOwnedSession(t, "wb", "op2l")
	tk, err := createTask("run-p2l", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	rootA, err := canonicalRoot(loadRegistry()["wa"].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := canonicalRoot(loadRegistry()["wb"].Workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeTaskRoot(tk.ID, rootA, testReq("orch")); err != nil {
		t.Fatalf("authorize root: %v", err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "wb", testReq("orch")); err == nil {
		t.Fatal("dispatch outside the authorized root must be refused")
	} else if !strings.Contains(err.Error(), rootA) || !strings.Contains(err.Error(), rootB) {
		t.Fatalf("refusal must name both roots: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskPending {
		t.Fatalf("refused dispatch changed status to %s", got.Status)
	}
	if err := dispatchTaskWithRequest(tk.ID, "wa", testReq("orch")); err != nil {
		t.Fatalf("dispatch inside the authorized root: %v", err)
	}
}

// Independent tasks must not silently share a workspace (TD-051):
// the second live dispatch to an occupied root is refused until
// shared use is explicitly authorized.
func TestSharedWorkspaceRequiresAuthorization(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2m")
	seedOwnedSession(t, "w", "op2m")
	first, err := createTask("run-p2m", "agent", "one", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createTask("run-p2m", "agent", "two", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(first.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := dispatchTaskWithRequest(second.ID, "w", testReq("orch")); err == nil {
		t.Fatal("second live dispatch to the same root must be refused")
	} else if !strings.Contains(err.Error(), first.ID) {
		t.Fatalf("refusal must name the occupant: %v", err)
	}
	if err := authorizeSharedWorkspace(second.ID, testReq("orch")); err != nil {
		t.Fatalf("authorize shared: %v", err)
	}
	if err := dispatchTaskWithRequest(second.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("authorized shared dispatch: %v", err)
	}
	got, _ := getTask(second.ID)
	if got.SharedWorkspace != 1 || got.ExecutionRoot == "" {
		t.Fatalf("shared dispatch must record authorization and root: %+v", got)
	}
}

// Retries keep the bound root (TD-050): a replacement attempt in a
// different workspace is refused; the same workspace proceeds.
func TestRetryKeepsExecutionRoot(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2n")
	seedOwnedSession(t, "w1", "op2n")
	seedOwnedSession(t, "w2", "op2n")
	tk, err := createTask("run-p2n", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w1", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := retryDispatch(tk.ID, "w2", testReq("orch")); err == nil {
		t.Fatal("retry into a different workspace must be refused")
	}
	got, _ := getTask(tk.ID)
	if got.Attempt != 1 {
		t.Fatalf("refused retry changed attempt count to %d", got.Attempt)
	}
	if err := retryDispatch(tk.ID, "w1", testReq("orch")); err != nil {
		t.Fatalf("retry in the same workspace: %v", err)
	}
}

// No silent provider substitution (TD-052): unknown harnesses fail
// loudly naming the known ones; explicit --cmd is user choice.
func TestNoImplicitProviderFallback(t *testing.T) {
	if _, err := lookupAdapter("no-such-harness"); err == nil {
		t.Fatal("unknown harness must fail, not fall back")
	} else if !strings.Contains(err.Error(), "known:") {
		t.Fatalf("error must list known harnesses: %v", err)
	}
	if _, err := resolveWorkerCommand("no-such-harness", "", t.TempDir(), "do it", ""); err == nil {
		t.Fatal("resolve with unknown harness must fail, not substitute")
	}
	cmd, err := resolveWorkerCommand("no-such-harness", "my-cmd --flag", t.TempDir(), "", "")
	if err != nil || cmd != "my-cmd --flag" {
		t.Fatalf("explicit --cmd must pass through untouched: %q %v", cmd, err)
	}
}

// --- Slice 4: durable guidance (TD-083) + intervention audit (TD-084).
// --- Failing-first: injectGuidance records nothing durable; refused
// --- direct sends vanish except for the returned error.

// Accepted guidance is durable and auditable (TD-083/084): who, when,
// mode, and final disposition are queryable by id and by session.
func TestGuidanceDurableAndAudited(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2o")
	seedOwnedSession(t, "w", "op2o")
	req := testReq("orch")
	gid, err := submitGuidance("w", "do the thing", false, req)
	if err != nil {
		t.Fatalf("submitGuidance: %v", err)
	}
	g, err := guidanceStatus(gid)
	if err != nil {
		t.Fatal(err)
	}
	if g.State != GuidanceFiled || g.Actor != "orch" || g.RequestID != req.RequestID || g.Mode != "file" {
		t.Fatalf("bad guidance record: %+v", g)
	}
	if g.Text != "do the thing" {
		t.Fatalf("guidance text not preserved: %q", g.Text)
	}
	list, err := listGuidance("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != gid {
		t.Fatalf("session listing must return the filing: %+v", list)
	}
}

// An accepted-but-unsettled request survives controller restart and
// is reported as outstanding until its disposition is established
// (TD-083): accept and settle are separate durable steps.
func TestGuidanceCrashRecovery(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2p")
	seedOwnedSession(t, "w", "op2p")
	req := testReq("orch")
	gid, err := recordGuidanceAccepted("w", "half-written", true, req)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate restart: fresh handles, same state dir.
	out, err := outstandingGuidance()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range out {
		if g.ID == gid {
			found = true
		}
	}
	if !found {
		t.Fatal("accepted guidance lost across restart")
	}
	if err := settleGuidance(gid, GuidanceFiled, "recovered after restart"); err != nil {
		t.Fatal(err)
	}
	if out, _ := outstandingGuidance(); len(out) != 0 {
		t.Fatalf("settled guidance still outstanding: %+v", out)
	}
	if g, _ := guidanceStatus(gid); g.State != GuidanceFiled {
		t.Fatalf("bad terminal state: %+v", g)
	}
}

// A refused direct send is recorded, not just returned (TD-084):
// the error reaches the caller AND the audit trail names the reason.
func TestGuidanceRefusalRecorded(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2q")
	seedOwnedSession(t, "w", "op2q")
	reg := loadRegistry()
	e := reg["w"]
	e.Harness = "muse"
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	_, err := submitGuidance("w", "do the thing", true, testReq("orch"))
	if err == nil {
		t.Fatal("direct guidance to a one-shot session must be refused")
	}
	list, err := listGuidance("w")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != GuidanceRefused {
		t.Fatalf("refusal must be recorded: %+v", list)
	}
	if !strings.Contains(list[0].Detail, "one-shot") {
		t.Fatalf("record must name the reason: %q", list[0].Detail)
	}
}

// --- Slice 5: confirmed termination (TD-038) + timeout semantics
// --- (TD-039). Failing-first: cancelTask never touches the bound
// --- worker, so a cancelled task can keep running unrecorded, and
// --- nothing pins the no-marking-on-failed-stop rule.

// Cancelling a live task stops its bound worker and confirms it
// (TD-038): the session ends, the registry records it, and the
// receipt says so.
func TestCancelStopsBoundWorker(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	logPath := installFakeTmux(t, "op2r")
	seedOwnedSession(t, "w", "op2r")
	tk, err := createTask("run-p2r", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskCancelled {
		t.Fatalf("task is %s, want cancelled", got.Status)
	}
	if reg := loadRegistry(); !reg["w"].Stopped {
		t.Fatal("bound worker session must be stopped and recorded")
	}
	logged, _ := os.ReadFile(logPath)
	if !strings.Contains(string(logged), "kill-session") {
		t.Fatal("cancel of a live task must actually stop its worker")
	}
	found := false
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		if r.Kind == "worker_stopped" {
			found = true
		}
	}
	if !found {
		t.Fatal("cancel must receipt confirmed worker stop")
	}
}

// A failed stop is recorded as unconfirmed, never as clean (TD-038):
// the assignment is still cancelled, but nothing claims the worker
// is dead and the session is not marked stopped.
func TestCancelRecordsUnconfirmedStop(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2s")
	t.Setenv("FAKE_TMUX_KILL_RC", "1")
	seedOwnedSession(t, "w", "op2s")
	tk, err := createTask("run-p2s", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := cancelTask(tk.ID, testReq("orch")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if reg := loadRegistry(); reg["w"].Stopped {
		t.Fatal("failed kill must not mark the session stopped")
	}
	var kinds []string
	for _, r := range listReceiptsOrFail(t, tk.ID) {
		kinds = append(kinds, r.Kind)
	}
	for _, k := range kinds {
		if k == "worker_stopped" {
			t.Fatal("unconfirmed stop must never be receipted as stopped")
		}
	}
	found := false
	for _, k := range kinds {
		if k == "worker_stop_unconfirmed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("cancel must receipt the unconfirmed stop, got %v", kinds)
	}
}

// A shared worker outlives one task's cancellation (TD-038/051):
// the session keeps running for the remaining live task and the
// receipt says why.
func TestCancelRetainsSharedWorker(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	logPath := installFakeTmux(t, "op2t")
	seedOwnedSession(t, "w", "op2t")
	first, err := createTask("run-p2t", "agent", "one", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := createTask("run-p2t", "agent", "two", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeSharedWorkspace(second.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []Task{first, second} {
		if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
			t.Fatalf("dispatch %s: %v", tk.ID, err)
		}
	}
	if err := cancelTask(first.ID, testReq("orch")); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got, _ := getTask(second.ID); got.Status != TaskDispatched {
		t.Fatalf("surviving task is %s, want dispatched", got.Status)
	}
	if reg := loadRegistry(); reg["w"].Stopped {
		t.Fatal("session must keep running for the surviving task")
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "kill-session") {
		t.Fatal("shared worker must not be killed while a live task uses it")
	}
	found := false
	for _, r := range listReceiptsOrFail(t, first.ID) {
		if r.Kind == "worker_retained" && strings.Contains(r.Detail, second.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("cancel must receipt why the worker was retained")
	}
}

// A failed termination observation marks nothing (TD-039): kill
// fails, so the session stays unstopped and the task row keeps its
// exact pre-stop status — no failure is inferred.
func TestStopFailureMarksNothing(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2u")
	t.Setenv("FAKE_TMUX_KILL_RC", "1")
	seedOwnedSession(t, "w", "op2u")
	tk, err := createTask("run-p2u", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := stopSession("w"); err == nil {
		t.Fatal("failed kill must return an error")
	}
	if reg := loadRegistry(); reg["w"].Stopped {
		t.Fatal("failed kill must not mark the session stopped")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched {
		t.Fatalf("task is %s, want still dispatched", got.Status)
	}
}

// --- Slice 6: delegation bounds (TD-032/034), crash-safe launch
// --- (TD-116), single scheduling authority (TD-117), independent
// --- concurrency (TD-033). Failing-first: no run-level concurrency
// --- limit exists; nothing pins the crash window or registry race.

// Parallel delegation respects an explicit run limit (TD-032): the
// limit names the run and the live count; resolving a task frees
// a slot. Unset limits mean unbounded.
func TestRunConcurrencyLimitEnforced(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2v")
	seedOwnedSession(t, "w1", "op2v")
	seedOwnedSession(t, "w2", "op2v")
	a, err := createTask("run-p2v", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p2v", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := setRunLimit("run-p2v", 1, testReq("orch")); err != nil {
		t.Fatalf("setRunLimit: %v", err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w1", testReq("orch")); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	if err := dispatchTaskWithRequest(b.ID, "w2", testReq("orch")); err == nil {
		t.Fatal("dispatch over the run limit must be refused")
	} else if !strings.Contains(err.Error(), "run-p2v") || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("refusal must name run and limit: %v", err)
	}
	if got, _ := getTask(b.ID); got.Status != TaskPending {
		t.Fatalf("refused dispatch changed status to %s", got.Status)
	}
	// Resolving a task frees its slot (cancel would also free it,
	// but the fake tmux harness shares one global liveness flag).
	if err := markTaskWorking(a.ID, 1, testReq("test")); err != nil {
		t.Fatal(err)
	}
	if err := submitReport(a.ID, ReportFiling{Report: "done", Source: SourceCLISubmission, Reporter: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := verifyTask(a.ID, true, "op", testReq("test")); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := dispatchTaskWithRequest(b.ID, "w2", testReq("orch")); err != nil {
		t.Fatalf("dispatch after slot freed: %v", err)
	}
}

// Without a limit, parallel delegation is unbounded (TD-032), and
// invalid limits are refused rather than stored.
func TestRunLimitUnsetUnbounded(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2w")
	seedOwnedSession(t, "w1", "op2w")
	seedOwnedSession(t, "w2", "op2w")
	a, err := createTask("run-p2w", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p2w", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(a.ID, "w1", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(b.ID, "w2", testReq("orch")); err != nil {
		t.Fatalf("unlimited run must dispatch in parallel: %v", err)
	}
	if err := setRunLimit("run-p2w", 0, testReq("orch")); err == nil {
		t.Fatal("zero limit must be refused")
	}
	if err := setRunLimit("run-p2w", -2, testReq("orch")); err == nil {
		t.Fatal("negative limit must be refused")
	}
	if err := setRunLimit("run-p2w", 3, testReq("")); err == nil {
		t.Fatal("limit without a request must be refused")
	}
}

// A crash between file delivery and the claim leaves no phantom
// execution (TD-116): the task stays pending with zero attempts,
// and a later dispatch proceeds exactly once.
func TestCrashBetweenDeliverAndClaim(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2x")
	seedOwnedSession(t, "w", "op2x")
	tk, err := createTask("run-p2x", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	tk.Agent = "w"
	if _, err := deliverTask(tk); err != nil {
		t.Fatalf("deliverTask: %v", err)
	}
	// Simulated crash: file on disk, nothing else happened.
	if got, _ := getTask(tk.ID); got.Status != TaskPending {
		t.Fatalf("delivered-but-unclaimed task is %s, want pending", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("expected zero attempts after crash, got %d", n)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch after crash: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Attempt != 1 {
		t.Fatalf("expected exactly one attempt, got %d", got.Attempt)
	}
}

// Concurrent registry writers using the locked pattern never lose
// updates or corrupt the file (TD-117 session side).
func TestRegistryConcurrentSaves(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2y")
	dirs := make([]string, 8)
	for i := range dirs {
		dirs[i] = t.TempDir()
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "rc" + string(rune('a'+i))
			if err := withRegistryLock(func() error {
				reg := loadRegistry()
				reg[name] = Session{Name: name, Workdir: dirs[i], Owner: "op2y"}
				return saveRegistry(reg)
			}); err != nil {
				t.Errorf("locked save %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	reg := loadRegistry()
	for i := 0; i < 8; i++ {
		if _, ok := reg["rc"+string(rune('a'+i))]; !ok {
			t.Fatalf("locked registration lost: %q missing", "rc"+string(rune('a'+i)))
		}
	}
}

// A blocked worker does not serialize unrelated workers (TD-033):
// the live dispatch wins while the dead-session dispatch fails
// fast, with neither outcome affecting the other.
func TestBlockedWorkerDoesNotSerialize(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2z")
	seedOwnedSession(t, "w1", "op2z")
	a, err := createTask("run-p2z", "agent", "a", "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := createTask("run-p2z", "agent", "b", "")
	if err != nil {
		t.Fatal(err)
	}
	var wa, wb sync.WaitGroup
	var erra, errb error
	wa.Add(1)
	go func() {
		defer wa.Done()
		erra = dispatchTaskWithRequest(a.ID, "w1", testReq("orch"))
	}()
	wb.Add(1)
	go func() {
		defer wb.Done()
		errb = dispatchTaskWithRequest(b.ID, "gone", testReq("orch"))
	}()
	wa.Wait()
	wb.Wait()
	if erra != nil {
		t.Fatalf("live dispatch blocked by dead peer: %v", erra)
	}
	if errb == nil {
		t.Fatal("dispatch to a dead session must fail")
	}
	if got, _ := getTask(a.ID); got.Status != TaskDispatched || got.Attempt != 1 {
		t.Fatalf("live task misrecorded: %+v", got)
	}
	if got, _ := getTask(b.ID); got.Status != TaskPending {
		t.Fatalf("failed dispatch changed status to %s", got.Status)
	}
}

// --- Slice 7: adapter translation (TD-054), continuation honesty
// --- (TD-053), restart durability (TD-115), reconcile-before-retry
// --- (TD-118, pins existing behavior). Failing-first: codex gets a
// --- positional model arg; continuation has no explicit predicate.

// Provider flags must match the real CLI (TD-054): `codex exec`
// takes -m/--model, so the model must never be passed positionally.
func TestCodexModelFlag(t *testing.T) {
	a, err := lookupAdapter("codex")
	if err != nil {
		t.Fatal(err)
	}
	cmd := a.BuildCommand(t.TempDir(), "do work", "o3")
	if !strings.Contains(cmd, "--model") {
		t.Fatalf("codex command must name the model flag: %q", cmd)
	}
	if strings.Contains(cmd, `"model" "o3"`) || strings.Contains(cmd, ` model "o3"`) {
		t.Fatalf("model must not be positional: %q", cmd)
	}
	if !strings.HasSuffix(strings.TrimSpace(cmd), shQuote("do work")) {
		t.Fatalf("prompt must remain the trailing argument: %q", cmd)
	}
}

// Continuation is advertised only with a real conversation channel
// (TD-053): Interactive alone is not enough; live input must work.
// Only muse-native has a qualified channel (TestMuseEchoLifecycle
// proves a second turn on the same bound session completes); every
// one-shot adapter must stay silent.
func TestSupportsContinuation(t *testing.T) {
	for name, a := range adapterRegistry() {
		if a.SupportsContinuation() && name != "muse-native" {
			t.Fatalf("harness %q advertises continuation it cannot deliver", name)
		}
	}
	if !adapterRegistry()["muse-native"].SupportsContinuation() {
		t.Fatal("muse-native must advertise its qualified continuation")
	}
	yes := Adapter{Harness: "x", Interactive: true, AcceptsLiveInput: true}
	if !yes.SupportsContinuation() {
		t.Fatal("interactive + live input must count as continuable")
	}
	half := Adapter{Harness: "x", Interactive: true}
	if half.SupportsContinuation() {
		t.Fatal("interactive without live input must not count as continuable")
	}
}

// Task, attempt, guidance, limit, and session identities survive a
// controller restart (TD-115): every store is re-read from disk
// through fresh handles, never from process memory.
func TestRestartPreservesIdentities(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "op2aa")
	seedOwnedSession(t, "w", "op2aa")
	tk, err := createTask("run-p2aa", "agent", "obj", "acc")
	if err != nil {
		t.Fatal(err)
	}
	child, err := followupTask(tk.ID, "fix", testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	gid, err := submitGuidance("w", "keep going", false, testReq("orch"))
	if err != nil {
		t.Fatal(err)
	}
	if err := setRunLimit("run-p2aa", 2, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	// Simulated restart: all handles below are fresh; the state dir
	// is the only continuity.
	if got, err := getTask(child.ID); err != nil || got.Parent != tk.ID || got.Objective != "fix" {
		t.Fatalf("child lost across restart: %+v %v", got, err)
	}
	if g, err := guidanceStatus(gid); err != nil || g.State != GuidanceFiled || g.Text != "keep going" {
		t.Fatalf("guidance lost across restart: %+v %v", g, err)
	}
	if lim, ok, err := getRunLimit("run-p2aa"); err != nil || !ok || lim != 2 {
		t.Fatalf("run limit lost across restart: %d %v %v", lim, ok, err)
	}
	if reg := loadRegistry(); reg["w"].Owner != "op2aa" {
		t.Fatalf("session registry lost across restart: %+v", reg["w"])
	}
	if list, err := listGuidance("w"); err != nil || len(list) != 1 {
		t.Fatalf("guidance listing lost across restart: %+v %v", list, err)
	}
}

// Hermes adapter must match the real CLI (TD-054 class): `hermes
// chat --oneshot` requires -q QUERY; a positional prompt is
// rejected by the CLI, so the adapter must pass -q and the model
// via -m (both verified against `hermes chat --help`).
func TestHermesAdapterMatchesCLI(t *testing.T) {
	a, err := lookupAdapter("hermes")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := a.ManagedArgv(t.TempDir(), "do work; rm -rf /", "m1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"hermes", "chat", "--oneshot", "-q", "do work; rm -rf /"}
	// model rides as -m m1 before the query; accept either order
	// as long as every element is present and prompt is last.
	if argv[len(argv)-1] != "do work; rm -rf /" {
		t.Fatalf("hermes prompt must be the trailing -q value: %v", argv)
	}
	joined := strings.Join(argv, " ")
	for _, frag := range []string{"chat", "--oneshot", "-q"} {
		if !strings.Contains(joined, frag) {
			t.Fatalf("hermes argv missing %q: %v", frag, argv)
		}
	}
	foundModel := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-m" && argv[i+1] == "m1" {
			foundModel = true
		}
	}
	if !foundModel {
		t.Fatalf("hermes argv must pass model via -m: %v", argv)
	}
	_ = want
}

// Muse exec accepts --model (verified against `muse exec --help`);
// the adapter must not silently drop a requested model.
func TestMuseAdapterModelFlag(t *testing.T) {
	a, err := lookupAdapter("muse")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := a.ManagedArgv(t.TempDir(), "do work", "spark-1")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--model" && argv[i+1] == "spark-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("muse argv must carry --model spark-1: %v", argv)
	}
	if argv[len(argv)-1] != "do work" {
		t.Fatalf("muse prompt must stay trailing: %v", argv)
	}
}

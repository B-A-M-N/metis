package main

// consumer_readiness_test.go: F-07 failing-first tests.
//
// Dispatch admission committed a dispatched task with no guarantee a
// worker consumer existed: a live parked session with no loop looked
// identical to a healthy handoff — no heartbeat, no blocked reason,
// no deadline. These tests pin the fix:
//
//   - dispatch to a parked session with no loop commits but projects
//     pickup_pending (blocked, with deadline), and the later pickup
//     still executes exactly once (duplicate pickup and duplicate
//     execution refused);
//   - a killed loop (stale heartbeat) detaches; a restart bumps the
//     generation, re-attaches, and the duplicate pickup stays refused;
//   - the status projection surfaces dispatchable / consumer_attached
//     / pickup_pending / execution_claimed distinctly;
//   - the supervisor path starts and maintains the loop identity.

import (
	"strings"
	"testing"
	"time"
)

// parkSession seeds a live session with no worker loop behind it:
// registered, tmux-live, but never heartbeat-attached.
func parkSession(t *testing.T, name, owner string) {
	t.Helper()
	installFakeTmux(t, owner)
	seedOwnedSession(t, name, owner)
}

func receiptKinds(t *testing.T, taskID string) []string {
	t.Helper()
	recs, err := listReceipts(taskID)
	if err != nil {
		t.Fatalf("list receipts: %v", err)
	}
	var kinds []string
	for _, r := range recs {
		kinds = append(kinds, r.Kind)
	}
	return kinds
}

func hasKind(kinds []string, kind string) bool {
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Dispatch to a live parked session with no loop must commit the
// assignment yet project pickup_pending with a blocked reason and a
// deadline — and the later pickup must execute exactly once: a
// second pickup and a second execution binding are both refused.
func TestDispatchParkedSessionBlockedLaterPickupExactlyOnce(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	parkSession(t, "parked", "f07-park")
	tk, err := createTask("run-f07-park", "agent", "parked objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	if st := projectTaskDispatchState(tk); st.State != DispatchStateDispatchable {
		t.Fatalf("pending task must project %q, got %q", DispatchStateDispatchable, st.State)
	}
	if err := dispatchTaskWithRequest(tk.ID, "parked", testReq("orch")); err != nil {
		t.Fatalf("dispatch to a live parked session must commit: %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched {
		t.Fatalf("expected dispatched, got %s", got.Status)
	}
	ds, err := taskDispatchState(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ds.State != DispatchStatePickupPending {
		t.Fatalf("parked assignment must project %q, got %q", DispatchStatePickupPending, ds.State)
	}
	if ds.Attached {
		t.Fatal("parked session must not project an attached consumer")
	}
	if strings.TrimSpace(ds.BlockedReason) == "" {
		t.Fatal("pickup_pending must carry a blocked reason")
	}
	if !strings.Contains(ds.BlockedReason, "no worker consumer") {
		t.Fatalf("blocked reason must name the missing consumer, got %q", ds.BlockedReason)
	}
	if strings.TrimSpace(ds.PickupDeadline) == "" {
		t.Fatal("pickup_pending must carry a pickup deadline")
	}
	if reason := dispatchBlockedReasonForSession("parked"); strings.TrimSpace(reason) == "" {
		t.Fatal("admission must report a blocked reason for a consumer-less session")
	}
	if !hasKind(receiptKinds(t, tk.ID), "pickup_pending") {
		t.Fatal("dispatch with no consumer must receipt pickup_pending")
	}
	// The later pickup claims exactly once through the real worker
	// claim path (resolve + claim, no execution launched here).
	a, err := lookupAdapter("demo")
	if err != nil {
		t.Fatal(err)
	}
	asn, idle, err := workerResolveAssignment("parked", a, testReq("loop"))
	if err != nil {
		t.Fatalf("resolve after late loop start: %v", err)
	}
	if idle {
		t.Fatal("committed assignment must resolve, not idle")
	}
	if _, err := workerClaimAttempt("parked", asn); err != nil {
		t.Fatalf("late pickup must succeed exactly once: %v", err)
	}
	got, _ = getTask(tk.ID)
	if got.Status != TaskWorking {
		t.Fatalf("expected working after pickup, got %s", got.Status)
	}
	if ds, _ := taskDispatchState(tk.ID); ds.State != DispatchStateExecutionClaimed {
		t.Fatalf("claimed task must project %q, got %q", DispatchStateExecutionClaimed, ds.State)
	}
	if c, ok, _ := getWorkerConsumer("parked"); !ok || c.CurrentAssignment != tk.ID {
		t.Fatalf("claim must bind the consumer assignment, got %+v attached=%v", c, ok)
	}
	if !hasKind(receiptKinds(t, tk.ID), "execution_claimed") {
		t.Fatal("pickup must receipt execution_claimed with its generation")
	}
	// Duplicate pickup of the same attempt is refused.
	if _, err := workerClaimAttempt("parked", asn); err == nil {
		t.Fatal("duplicate pickup must be refused")
	} else if !strings.Contains(err.Error(), "already picked up") {
		t.Fatalf("duplicate pickup must name the conflict, got %v", err)
	}
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("loop-dup")); err == nil {
		t.Fatal("duplicate markTaskWorking must be refused")
	}
	// And the execution binding is single-shot: a second launch for
	// the same attempt never records.
	if err := recordExecutionStart(tk.ID, got.Attempt, "parked", "demo", "[]", 0, "test"); err != nil {
		t.Fatalf("first execution binding must record: %v", err)
	}
	if err := recordExecutionStart(tk.ID, got.Attempt, "parked", "demo", "[]", 0, "test"); err == nil {
		t.Fatal("duplicate execution binding must be refused: no duplicate execution on later pickup")
	}
}

// A killed loop (stale heartbeat) detaches its session; the restart
// bumps the generation and re-attaches, and the pickup that follows
// still wins exactly once — the duplicate stays refused.
func TestConsumerKillRestartDuplicatePickupRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	parkSession(t, "flaky", "f07-flaky")
	a, err := lookupAdapter("demo")
	if err != nil {
		t.Fatal(err)
	}
	gen1, err := startWorkerConsumer("flaky", a)
	if err != nil {
		t.Fatal(err)
	}
	if gen1 != 1 {
		t.Fatalf("first loop start must be generation 1, got %d", gen1)
	}
	if !workerConsumerAttached("flaky") {
		t.Fatal("fresh heartbeat must read attached")
	}
	// Kill the loop: backdate the heartbeat past the stale window.
	// (No global mutation, no sleeping.)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE worker_consumers SET heartbeat_at = ? WHERE session = ?`, old, "flaky"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if workerConsumerAttached("flaky") {
		t.Fatal("killed loop must detach")
	}
	if reason := workerConsumerBlockedReason("flaky"); !strings.Contains(reason, "stale") {
		t.Fatalf("killed loop must report a stale-consumer blocked reason, got %q", reason)
	}
	tk, err := createTask("run-f07-flaky", "agent", "flaky objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "flaky", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if ds, _ := taskDispatchState(tk.ID); ds.State != DispatchStatePickupPending {
		t.Fatalf("assignment with a killed loop must project %q, got %q", DispatchStatePickupPending, ds.State)
	}
	// Restart: new generation, stale assignment cleared, attached.
	gen2, err := startWorkerConsumer("flaky", a)
	if err != nil {
		t.Fatal(err)
	}
	if gen2 != gen1+1 {
		t.Fatalf("restart must bump the generation (%d -> %d)", gen1, gen2)
	}
	ds, _ := taskDispatchState(tk.ID)
	if ds.State != DispatchStateConsumerAttached || ds.Generation != gen2 {
		t.Fatalf("restart must project %q at generation %d, got %+v", DispatchStateConsumerAttached, gen2, ds)
	}
	asn, idle, err := workerResolveAssignment("flaky", a, testReq("loop"))
	if err != nil || idle {
		t.Fatalf("resolve after restart: asn idle=%v err=%v", idle, err)
	}
	if _, err := workerClaimAttempt("flaky", asn); err != nil {
		t.Fatalf("restart pickup must succeed: %v", err)
	}
	foundGen := false
	for _, r := range mustReceipts(t, tk.ID) {
		if r.Kind == "execution_claimed" && strings.Contains(r.Detail, "generation=2") {
			foundGen = true
		}
	}
	if !foundGen {
		t.Fatal("execution_claimed receipt must name the restart generation")
	}
	if _, err := workerClaimAttempt("flaky", asn); err == nil {
		t.Fatal("duplicate pickup after restart must be refused")
	}
}

func mustReceipts(t *testing.T, taskID string) []Receipt {
	t.Helper()
	recs, err := listReceipts(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// The projection must keep the four readiness states distinct across
// the handoff: dispatchable, pickup_pending, consumer_attached,
// execution_claimed.
func TestDispatchReadinessProjectionDistinct(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	parkSession(t, "w-states", "f07-states")
	a, err := lookupAdapter("demo")
	if err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("run-f07-states", "agent", "states objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	if st := projectTaskDispatchState(tk); st.State != DispatchStateDispatchable {
		t.Fatalf("pending must be %q, got %q", DispatchStateDispatchable, st.State)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w-states", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if st := projectTaskDispatchState(got); st.State != DispatchStatePickupPending {
		t.Fatalf("unattached dispatch must be %q, got %q", DispatchStatePickupPending, st.State)
	}
	if _, err := startWorkerConsumer("w-states", a); err != nil {
		t.Fatal(err)
	}
	got, _ = getTask(tk.ID)
	if st := projectTaskDispatchState(got); st.State != DispatchStateConsumerAttached || !st.Attached || st.Generation != 1 {
		t.Fatalf("attached dispatch must be %q gen 1, got %+v", DispatchStateConsumerAttached, st)
	}
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("loop")); err != nil {
		t.Fatal(err)
	}
	got, _ = getTask(tk.ID)
	if st := projectTaskDispatchState(got); st.State != DispatchStateExecutionClaimed {
		t.Fatalf("working must be %q, got %q", DispatchStateExecutionClaimed, st.State)
	}
}

// An overdue assignment — dispatched, never picked up, deadline
// passed — must say so in the projection.
func TestDispatchAssignmentOverdue(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	parkSession(t, "w-late", "f07-late")
	oldDeadline := dispatchPickupDeadline
	dispatchPickupDeadline = -time.Second
	defer func() { dispatchPickupDeadline = oldDeadline }()
	tk, err := createTask("run-f07-late", "agent", "late objective", "done")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w-late", testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if !dispatchAssignmentOverdue(got) {
		t.Fatal("assignment past its pickup deadline must read overdue")
	}
	if ds, _ := taskDispatchState(tk.ID); !ds.Overdue || ds.State != DispatchStatePickupPending {
		t.Fatalf("overdue assignment must project %q + overdue, got %+v", DispatchStatePickupPending, ds)
	}
}

// The supervisor path starts the loop identity for a managed
// session and maintains it across restarts as new generations.
func TestEnsureManagedWorkerLoopStartsAndRestarts(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "f07-sup")
	seedHarnessSession(t, "managed", "f07-sup", "demo")
	sup, gen1, err := ensureManagedWorkerLoop("managed", testReq("sup"))
	if err != nil {
		t.Fatalf("supervisor must start the loop: %v", err)
	}
	if sup.Harness != "demo" {
		t.Fatalf("supervisor must return the bound adapter, got %q", sup.Harness)
	}
	if gen1 != 1 {
		t.Fatalf("first supervision must be generation 1, got %d", gen1)
	}
	if !workerConsumerAttached("managed") {
		t.Fatal("supervised session must read attached")
	}
	if _, gen2, err := ensureManagedWorkerLoop("managed", testReq("sup")); err != nil || gen2 != gen1+1 {
		t.Fatalf("re-supervision must bump the generation, got gen=%d err=%v", gen2, err)
	}
	if _, _, err := ensureManagedWorkerLoop("ghost", testReq("sup")); err == nil {
		t.Fatal("supervising an unknown session must fail")
	}
}

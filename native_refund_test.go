package main

// native_refund_test.go: P0 native refund/receipt consistency.
//
// A proven-not-submitted failure must leave the attempt reusable:
// the open execution reservation, the claim, and the
// delivery/attempt mirrors retire atomically, fenced to the exact
// (task, attempt). An ambiguous submission (bytes may have moved)
// must never refund and must keep the turn fenced for
// reconciliation — never erased, never re-executed.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// seedNativeBoundTask binds session w to a fake native session and
// leaves one claimed (working) task for run "run".
func seedNativeBoundTask(t *testing.T, session, owner, run string) Task {
	t.Helper()
	installFakeTmux(t, owner)
	seedOwnedSession(t, session, owner)
	if err := bindMuseSession(session, "n1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatal(err)
	}
	return seedBoundTask(t, session, run)
}

// countingNativeAdapter counts ManagedTurn invocations and answers
// from a script: each call consumes the next step, or repeats the
// last step when the script is exhausted.
func countingNativeAdapter(calls *int, seen *[]string, steps []func(commandID string) (museTurnOutcome, error)) Adapter {
	return Adapter{
		Harness: "muse-native",
		ManagedTurn: func(ctx context.Context, session, prompt string, timeout time.Duration, commandID string) (museTurnOutcome, error) {
			*calls++
			*seen = append(*seen, commandID)
			if *calls-1 < len(steps) {
				return steps[*calls-1](commandID)
			}
			return steps[len(steps)-1](commandID)
		},
	}
}

func succeedNativeStep(turnID string) func(string) (museTurnOutcome, error) {
	return func(commandID string) (museTurnOutcome, error) {
		if commandID == "" {
			return museTurnOutcome{}, errors.New("fake adapter requires a command identity")
		}
		if err := recordTurnAck(commandID, turnID); err != nil {
			return museTurnOutcome{}, err
		}
		return museTurnOutcome{TurnID: turnID, Terminal: "completed", Text: "ok"}, nil
	}
}

func failNativeStep(err error) func(string) (museTurnOutcome, error) {
	return func(commandID string) (museTurnOutcome, error) {
		return museTurnOutcome{}, err
	}
}

// TestNativePreSubmitRefundIsReusable is the P0 regression: a proven
// pre-submit failure filed the execution reservation before the
// adapter proved nothing was sent. The refund must retire that
// reservation in the same transaction, so the same attempt retries
// cleanly instead of hitting the duplicate-launch guard.
func TestNativePreSubmitRefundIsReusable(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	tk := seedNativeBoundTask(t, "w", "nru", "run-nru")
	att := tk.Attempt
	root := t.TempDir()

	var calls int
	var seen []string
	pre := musePreSubmit(errors.New("resume: no daemon"))
	a := countingNativeAdapter(&calls, &seen, []func(string) (museTurnOutcome, error){failNativeStep(pre)})
	if _, err := runNativeTurnOnce("w", a, tk, "do x", root, testReq("loop")); err == nil {
		t.Fatal("pre-submit failure must error")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched {
		t.Fatalf("proven pre-submit must refund to dispatched, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att); n != 0 {
		t.Fatalf("refund must retire the open reservation, got %d receipt rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND attempt_no = ? AND state = ?`, tk.ID, att, AttemptClaimed); n != 1 {
		t.Fatalf("attempt mirror must return to claimed, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ? AND state = ?`, tk.ID, DeliveryWorking); n != 0 {
		t.Fatalf("no delivery mirror may stay working after refund, got %d", n)
	}
	intent, err := getTurnIntent(tk.ID, att)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "submitted" || intent.TurnID != "" {
		t.Fatalf("unacknowledged intent must be retained for reuse, got %+v", intent)
	}

	// The same attempt retries cleanly: reclaim, run to completion,
	// exactly one receipt, same frozen command identity.
	if err := markTaskWorking(tk.ID, att, testReq("loop2")); err != nil {
		t.Fatalf("reclaim after refund must succeed: %v", err)
	}
	got, _ := getTask(tk.ID)
	if got.Attempt != att {
		t.Fatalf("refund must keep the attempt number, task at %d want %d", got.Attempt, att)
	}
	a2 := countingNativeAdapter(&calls, &seen, []func(string) (museTurnOutcome, error){succeedNativeStep("turn-reuse")})
	ok, err := runNativeTurnOnce("w", a2, got, "do x", root, testReq("loop3"))
	if err != nil || !ok {
		t.Fatalf("retry of a refunded attempt must complete: ok=%v err=%v", ok, err)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att); n != 1 {
		t.Fatalf("retry must leave exactly one receipt, got %d", n)
	}
	var exitCode int
	if err := func() error {
		db, err := openTasksDB()
		if err != nil {
			return err
		}
		defer db.Close()
		return db.QueryRow(`SELECT exit_code FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att).Scan(&exitCode)
	}(); err != nil || exitCode != 0 {
		t.Fatalf("receipt must carry the completed exit, got %d err %v", exitCode, err)
	}
	reused, err := getTurnIntent(tk.ID, att)
	if err != nil {
		t.Fatal(err)
	}
	if reused.CommandID != intent.CommandID || reused.State != "acknowledged" {
		t.Fatalf("retry must reuse the frozen identity to ack, got %+v want cmd %s", reused, intent.CommandID)
	}
}

// TestNativeAmbiguousSubmitNeverRefunds pins the other half: once
// bytes may have moved, nothing auto-refunds, nothing is erased, and
// a retry stays fenced behind the duplicate guard instead of
// submitting a second turn.
func TestNativeAmbiguousSubmitNeverRefunds(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	tk := seedNativeBoundTask(t, "w", "nar", "run-nar")
	att := tk.Attempt
	root := t.TempDir()

	var calls int
	var seen []string
	ambiguous := errors.New("turn/start cmd-1: write: broken pipe (submission uncertain)")
	a := countingNativeAdapter(&calls, &seen, []func(string) (museTurnOutcome, error){failNativeStep(ambiguous)})
	if _, err := runNativeTurnOnce("w", a, tk, "do x", root, testReq("loop")); err == nil {
		t.Fatal("ambiguous submit must fail without success")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("ambiguous submit must keep working, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ? AND exit_code IS NULL`, tk.ID, att); n != 1 {
		t.Fatalf("ambiguous submit must retain the open reservation, got %d rows", n)
	}
	intent, err := getTurnIntent(tk.ID, att)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != "submitted" {
		t.Fatalf("intent must stay submitted for reconcile, got %+v", intent)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_receipts WHERE task_id = ? AND kind = 'claim_refunded'`, tk.ID); n != 0 {
		t.Fatalf("ambiguous submit must never refund, got %d claim_refunded receipts", n)
	}
	open, err := listAttention("run-nar", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "uncertain_outcome" || open[0].TaskID != tk.ID {
		t.Fatalf("ambiguous submit must raise one uncertain attention, got %+v", open)
	}

	// A same-payload retry must not submit a second turn: the
	// duplicate reservation guard keeps it fenced.
	a2 := countingNativeAdapter(&calls, &seen, []func(string) (museTurnOutcome, error){succeedNativeStep("turn-second")})
	got, _ := getTask(tk.ID)
	if _, err := runNativeTurnOnce("w", a2, got, "do x", root, testReq("loop2")); err == nil {
		t.Fatal("retry while the outcome is unknown must be refused")
	} else if !strings.Contains(err.Error(), "already executed") {
		t.Fatalf("refusal must name the duplicate guard, got: %v", err)
	}
	if calls != 1 {
		t.Fatalf("no second turn may be submitted, calls=%d seen=%q", calls, seen)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att); n != 1 {
		t.Fatalf("exactly one reservation must remain, got %d", n)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("fenced retry must keep working, got %s", got.Status)
	}
}

// TestNativeRefundDoesNotRewriteOtherAttempts proves the mirrors and
// the reservation retire fenced to the exact attempt: sibling
// attempts' working rows and open receipts survive a refund.
func TestNativeRefundDoesNotRewriteOtherAttempts(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	tk := seedNativeBoundTask(t, "w", "nro", "run-nro")
	att := tk.Attempt
	other := att - 1
	if other < 0 {
		other = att + 1
	}

	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	otherAttemptID, err := insertAttempt(db, tk.ID, other, AttemptWorking, testReq("setup"))
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := insertDelivery(db, tk.ID, otherAttemptID, DeliveryWorking, testReq("setup"), "sibling working"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO execution_receipts
		(task_id, attempt_no, session, harness, argv, pid, proc_start, reporter, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tk.ID, other, "w", "muse-native", "[]", 0, 0, "test", nowUTC(), nowUTC()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	if err := refundClaim(tk.ID, att, "test fencing"); err != nil {
		t.Fatalf("refund must succeed: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched {
		t.Fatalf("refund must dispatch, got %s", got.Status)
	}
	// The refunded attempt retires fully.
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att); n != 0 {
		t.Fatalf("refunded attempt must lose its reservation, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND attempt_no = ? AND state = ?`, tk.ID, att, AttemptClaimed); n != 1 {
		t.Fatalf("refunded attempt mirror must return to claimed")
	}
	// Sibling attempt rows are untouched.
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND attempt_no = ? AND state = ?`, tk.ID, other, AttemptWorking); n != 1 {
		t.Fatalf("sibling attempt mirror must stay working")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ? AND state = ? AND attempt_id = ?`, tk.ID, DeliveryWorking, otherAttemptID); n != 1 {
		t.Fatalf("sibling delivery mirror must stay working")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, other); n != 1 {
		t.Fatalf("sibling reservation must survive, got %d rows", n)
	}
}

// TestNativePreSubmitCleanupCrashReplay proves a crash anywhere
// around the atomic cleanup creates no second runnable owner: a
// pre-commit crash stays single-fenced (retry refuses the
// duplicate), and a committed cleanup replays safely (re-refund is a
// clean refusal, the attempt reclaims exactly once).
func TestNativePreSubmitCleanupCrashReplay(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	tk := seedNativeBoundTask(t, "w", "ncr", "run-ncr")
	att := tk.Attempt
	root := t.TempDir()

	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := ensureTurnIntent(tk.ID, att, "w", "n1", b.Profile, b.Provider, b.Workspace, "do x", testReq("loop"))
	if err != nil || cmd == "" {
		t.Fatalf("intent must persist before submit: %v %q", err, cmd)
	}
	if err := recordExecutionStartWithProc(tk.ID, att, "w", "muse-native", "[]", 0, 0, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}

	// Crash before cleanup commits: the retry cannot prove nothing
	// was sent, so it stays fenced — one owner, no second turn.
	var calls int
	var seen []string
	a := countingNativeAdapter(&calls, &seen, []func(string) (museTurnOutcome, error){succeedNativeStep("turn-crash")})
	got, _ := getTask(tk.ID)
	if _, err := runNativeTurnOnce("w", a, got, "do x", root, testReq("loop2")); err == nil {
		t.Fatal("post-crash retry with unknown outcome must be refused")
	} else if !strings.Contains(err.Error(), "already executed") {
		t.Fatalf("refusal must name the duplicate guard, got: %v", err)
	}
	if calls != 0 {
		t.Fatalf("fenced replay must not submit a turn, calls=%d", calls)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("pre-commit crash state must stay working, got %s", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`, tk.ID, att); n != 1 {
		t.Fatalf("exactly one fenced reservation must remain, got %d", n)
	}

	// Cleanup commits (proven unsent here: intent unacknowledged),
	// then the cleanup replays: the second call refuses cleanly
	// without creating another runnable owner.
	if err := refundClaim(tk.ID, att, "crash replay cleanup"); err != nil {
		t.Fatalf("cleanup must commit: %v", err)
	}
	if err := refundClaim(tk.ID, att, "crash replay duplicate"); err == nil {
		t.Fatal("replayed cleanup must refuse: nothing left to refund")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched || got.Attempt != att {
		t.Fatalf("replay must leave dispatched attempt %d, got %+v", att, got)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("cleanup must retire the reservation, got %d rows", n)
	}
	// Exactly one reclaim wins: the attempt is runnable once.
	if err := markTaskWorking(tk.ID, att, testReq("loop3")); err != nil {
		t.Fatalf("single reclaim must succeed: %v", err)
	}
	if err := markTaskWorking(tk.ID, att, testReq("loop4")); err == nil {
		t.Fatal("duplicate pickup of the reclaimed attempt must be refused")
	}
}

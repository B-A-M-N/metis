package main

// Phase-level tests for the runWorkerOnce decomposition: each
// worker_* phase is exercised directly for its typed outcome,
// including the idle, refusal, and superseded dispositions the
// coordinator relays without inference.

import (
	"strings"
	"testing"
)

func TestWorkerPhaseResolveIdle(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pPh1")
	seedOwnedSession(t, "w", "pPh1")

	asn, idle, err := workerResolveAssignment("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op"))
	if err != nil {
		t.Fatalf("idle resolve must not error: %v", err)
	}
	if !idle {
		t.Fatalf("no dispatched work must report idle, got %+v", asn)
	}
	if ran, err := runWorkerOnce("w", fakeWorkerAdapter("sh", "-c", "echo hi"), testReq("op")); err != nil || ran {
		t.Fatalf("idle loop must return (false, nil), got ran=%v err=%v", ran, err)
	}
}

func TestWorkerPhaseResolveRefusals(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pPh2")
	seedOwnedSession(t, "w", "pPh2")

	if _, _, err := workerResolveAssignment("missing", fakeWorkerAdapter("sh"), testReq("op")); err == nil {
		t.Fatal("unknown session must be refused")
	}
	a, b := mustPhaseTask(t, "ph2-a"), mustPhaseTask(t, "ph2-b")
	if err := dispatchTask(a.ID, "w"); err != nil {
		t.Fatal(err)
	}
	// A second committed dispatch for the same session cannot
	// happen through dispatchTask (TD-051 refuses it); plant the
	// row directly so resolve must refuse to choose.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE tasks SET agent = 'w', status = 'dispatched' WHERE id = ?`, b.ID); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	_, _, rerr := workerResolveAssignment("w", fakeWorkerAdapter("sh"), testReq("op"))
	err = rerr
	if err == nil || !strings.Contains(err.Error(), "refusing to choose") {
		t.Fatalf("two dispatched tasks must be refused, got %v", err)
	}
}

func TestWorkerPhaseClaimBindsExactAttempt(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pPh3")
	seedOwnedSession(t, "w", "pPh3")
	tk := mustPhaseTask(t, "ph3")
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	ad := fakeWorkerAdapter("sh", "-c", "echo hi")
	asn, idle, err := workerResolveAssignment("w", ad, testReq("op"))
	if err != nil || idle {
		t.Fatalf("resolve must name the assignment: idle=%v err=%v", idle, err)
	}
	if asn.task.ID != tk.ID {
		t.Fatalf("assignment names %s, want %s", asn.task.ID, tk.ID)
	}
	cl, err := workerClaimAttempt("w", asn)
	if err != nil {
		t.Fatalf("claim must succeed: %v", err)
	}
	if cl.fleetOwned {
		t.Fatal("no fleet offer means no fleet ownership")
	}
	got, _ := getTask(tk.ID)
	if got.Status != TaskWorking || got.Attempt != asn.task.Attempt {
		t.Fatalf("claim must bind the exact attempt, got status=%s attempt=%d", got.Status, got.Attempt)
	}
}

// The persist phase records the waited outcome but files no report
// once the task left working: cancellation is honored, facts kept.
func TestWorkerPhasePersistSuperseded(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "pPh4")
	seedOwnedSession(t, "w", "pPh4")
	tk := mustPhaseTask(t, "ph4")
	if err := dispatchTask(tk.ID, "w"); err != nil {
		t.Fatal(err)
	}
	ad := fakeWorkerAdapter("sh", "-c", "echo hi")
	asn, _, err := workerResolveAssignment("w", ad, testReq("op"))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := workerClaimAttempt("w", asn)
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := getTask(tk.ID)
	if err := recordExecutionStart(cur.ID, cur.Attempt, "w", "test-fake", `["sh"]`, 4242, "worker-loop/w"); err != nil {
		t.Fatal(err)
	}
	if err := cancelTask(cur.ID, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	ex := workerExecution{exitCode: 0, outPath: "worker-output-stub.log", outData: []byte("late"), digest: "sha256:stub"}
	done, err := workerPersistOutcome("w", ad, cl, ex)
	if err != nil {
		t.Fatalf("superseded persist must absorb cleanly: %v", err)
	}
	if !done {
		t.Fatal("superseded persist must report the attempt executed")
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_reports WHERE task_id = ?`, cur.ID); n != 0 {
		t.Fatalf("cancelled task must gain no report, got %d rows", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM execution_receipts WHERE task_id = ? AND exit_code = 0`, cur.ID); n != 1 {
		t.Fatalf("waited outcome must still be recorded, got %d rows", n)
	}
}

func mustPhaseTask(t *testing.T, name string) Task {
	t.Helper()
	tk, err := createTask("run-"+name, "w", "objective "+name, "accept "+name)
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

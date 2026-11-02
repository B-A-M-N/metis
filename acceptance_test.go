package main

// Phase-1 exit-gate acceptance tests (CB-01, CB-12, CB-13) plus the
// supervisor control-authority boundary. These exercise production
// paths with negative assertions: concurrent winners, crash-retry
// reconciliation, one-shot guidance refusal, and forged FIFO frames.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testReq(actor string) OperationRequest {
	return OperationRequest{RequestID: newRequestID(), Actor: actor}
}

func countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// CB-01: concurrent dispatch of the same task yields exactly one
// winner, one attempt, and one delivery chain.
func TestCB01ConcurrentDispatchOneWinner(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "ocb1")
	seedOwnedSession(t, "w", "ocb1")
	tk, err := createTask("run-cb1", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if dispatchTaskWithRequest(tk.ID, "w", testReq("orch")) == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one dispatch winner, got %d", wins.Load())
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched || got.Attempt != 1 {
		t.Fatalf("bad post-race task: %+v", got)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("expected 1 attempt row, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ?`, tk.ID); n != 2 {
		t.Fatalf("expected queued+submitted delivery rows, got %d", n)
	}
}

// CB-12: a claimed-but-unconfirmed dispatch retries into a NEW attempt
// with history preserved; a possibly-live attempt refuses replacement.
func TestCB12CrashRetryReconciles(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "ocb2")
	seedOwnedSession(t, "w", "ocb2")
	tk, err := createTask("run-cb2", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	// Crash window: claimed, file written, no acknowledgement.
	if err := retryDispatch(tk.ID, "w", testReq("orch-retry")); err != nil {
		t.Fatalf("retry of uncertain dispatch: %v", err)
	}
	got, _ := getTask(tk.ID)
	if got.Attempt != 2 {
		t.Fatalf("retry must claim attempt 2, got %+v", got)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var states []string
	rows, err := db.Query(`SELECT state FROM task_attempts WHERE task_id = ? ORDER BY attempt_no`, tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		states = append(states, s)
	}
	if len(states) != 2 || states[0] != AttemptSuperseded || states[1] != AttemptClaimed {
		t.Fatalf("attempt history not preserved: %v", states)
	}
	// Now the worker picks up the current attempt: replacement must
	// be refused. (A claim naming superseded attempt 1 is stale and
	// refused; see TestWorkerStaleAttemptRefused.)
	if err := markTaskWorking(tk.ID, 2, testReq("test")); err != nil {
		t.Fatalf("markTaskWorking: %v", err)
	}
	if err := retryDispatch(tk.ID, "w", testReq("orch-retry2")); err == nil {
		t.Fatal("retry over a possibly-live attempt must be refused")
	} else if !strings.Contains(err.Error(), "may still be active") {
		t.Fatalf("wrong refusal: %v", err)
	}
	if got, _ := getTask(tk.ID); got.Attempt != 2 {
		t.Fatalf("refused retry changed attempt count: %+v", got)
	}
}

// CB-13: direct guidance into a one-shot session is refused (file
// kept), never reported delivered; unknown harnesses keep legacy behavior.
func TestCB13OneShotGuidanceRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "ocb3")
	seedOwnedSession(t, "cb13", "ocb3")
	reg := loadRegistry()
	e := reg["cb13"]
	e.Harness = "muse" // one-shot, no live input per adapter registry
	reg["cb13"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	err := injectGuidance("cb13", "do the thing", true)
	if err == nil {
		t.Fatal("direct guidance to a one-shot session must be refused")
	}
	if !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("refusal must name the reason: %v", err)
	}
	guidance, _ := os.ReadFile(filepath.Join(e.Workdir, "guidance.md"))
	if !strings.Contains(string(guidance), "do the thing") {
		t.Fatal("refused guidance must still be saved to the file")
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "send-keys") {
		t.Fatalf("refused guidance still hit the terminal:\n%s", logged)
	}
	// Unknown harness (custom command): legacy attempt path preserved.
	e.Harness = ""
	reg["cb13"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	if err := injectGuidance("cb13", "custom worker note", true); err != nil {
		t.Fatalf("unknown harness should keep legacy direct path: %v", err)
	}
}

// Supervisor boundary: forged frames from a non-lease-holder are
// refused at the supervisor; the PTY is untouched and the worker lives.
func TestSupervisorRejectsForgedFrames(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("vault", work, "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("vault")
	waitForLog(t, "vault", "")
	if err := ptyAcquireLease("vault", "ctl-A"); err != nil {
		t.Fatalf("lease A: %v", err)
	}
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	// Forged input frame naming B while A holds the lease.
	if err := ptyFifoWrite("vault", "IN evil-1 ctl-B 1/1 "+b64("pwned-marker\n")); err != nil {
		t.Fatalf("fifo write: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, detail, err := ptyInputStatus("evil-1")
		if err != nil {
			t.Fatal(err)
		}
		if st == "error" {
			if !strings.Contains(detail, "refused") {
				t.Fatalf("wrong refusal detail: %q", detail)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("forged frame never refused, status=%s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	data, err := ptyLog("vault", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pwned-marker") {
		t.Fatal("forged input reached the PTY")
	}
	// Legacy unqualified frame: refused, never written.
	if err := ptyFifoWrite("vault", "IN "+b64("legacy-poke\n")); err != nil {
		t.Fatalf("fifo write: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		st, _, err := ptyInputStatus("legacy-refused")
		if err != nil {
			t.Fatal(err)
		}
		if st == "error" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("legacy frame never refused, status=%s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Forged stop frame: worker must survive.
	if err := ptyFifoWrite("vault", "ST ctl-B"); err != nil {
		t.Fatalf("fifo write: %v", err)
	}
	time.Sleep(time.Second)
	m, err := ptyGet("vault")
	if err != nil {
		t.Fatal(err)
	}
	if !ptyWorkerAlive(m) || !ptySupervisorAlive(m) {
		t.Fatal("forged ST frame killed a session the sender does not hold")
	}
	// Stop preempts input with proven authority and cleans up.
	if err := ptyStop("vault"); err != nil {
		t.Fatalf("ptyStop: %v", err)
	}
}

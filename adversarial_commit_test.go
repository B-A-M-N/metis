package main

// Adversarial failing-first tests (commit-gate properties).
//
// These tests pin required properties against the CURRENT code and are
// expected to FAIL where the production code lacks the property. A
// failure here is a GOOD outcome: do NOT weaken the assertion to make
// it pass; fix the production code path cited in the failure message.
//
// Helpers reused from the existing suite (same package): testReq,
// countRows, seedOwnedSession, installFakeTmux, ptyAvailable.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// 1a. Dispatch atomicity (success path): after a nil return, the task
// row (dispatched), its task_attempts row, AND its task_deliveries rows
// (queued+submitted) must ALL exist — no torn dispatch.
func TestAdvDispatchAtomicity(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "adv-atom")
	seedOwnedSession(t, "w", "adv-atom")
	tk, err := createTask("adv-atom-run", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskDispatched || got.Attempt != 1 {
		t.Fatalf("torn dispatch: task row = %+v, want dispatched attempt 1", got)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, tk.ID); n != 1 {
		t.Fatalf("torn dispatch: %d attempt rows, want 1", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ?`, tk.ID); n != 2 {
		t.Fatalf("torn dispatch: %d delivery rows, want 2 (queued+submitted)", n)
	}
	var states []string
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT state FROM task_deliveries WHERE task_id = ? ORDER BY rowid`, tk.ID)
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
	if len(states) != 2 || states[0] != DeliveryQueued || states[1] != DeliverySubmitted {
		t.Fatalf("delivery chain wrong: %v, want [queued submitted]", states)
	}
}

// 1b. Concurrent dispatch x8 on the same pending task: exactly ONE
// success, task ends dispatched with exactly one attempt row for that
// attempt number (no duplicates).
func TestAdvDispatchConcurrentOneWinner(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "adv-race")
	seedOwnedSession(t, "w", "adv-race")
	tk, err := createTask("adv-race-run", "agent", "obj", "")
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
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ? AND attempt_no = ?`, tk.ID, 1); n != 1 {
		t.Fatalf("expected exactly one attempt row for attempt 1, got %d", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ?`, tk.ID); n != 2 {
		t.Fatalf("expected queued+submitted delivery rows, got %d", n)
	}
}

// 2. Crash-window (refusal side-effect free): a dispatch whose target
// dir is unusable (workdir is a regular file, so the instruction file
// and sidecar prepares fail) must leave the task pending with NO
// attempt/delivery rows.
func TestAdvDispatchRefusalSideEffectFree(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "adv-crash")
	seedOwnedSession(t, "w", "adv-crash")
	// Point the session workdir at a regular file: WriteFile into it
	// fails deterministically (ENOTDIR) for any uid, unlike chmod
	// bits which root ignores.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := loadRegistry()
	e := reg["w"]
	e.Workdir = blocker
	reg["w"] = e
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
	tk, err := createTask("adv-crash-run", "agent", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, "w", testReq("orch")); err == nil {
		t.Fatal("dispatch into an unusable workdir must fail")
	} else if !strings.Contains(err.Error(), "instruction delivery") && !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("unexpected refusal reason (want file-prepare failure): %v", err)
	}
	got, err := getTask(tk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskPending {
		t.Fatalf("refused dispatch mutated task status to %q, want pending", got.Status)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_attempts WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("refused dispatch left %d attempt rows, want 0", n)
	}
	if n := countRows(t, `SELECT COUNT(*) FROM task_deliveries WHERE task_id = ?`, tk.ID); n != 0 {
		t.Fatalf("refused dispatch left %d delivery rows, want 0", n)
	}
}

// 3. FIFO/input bound: the supervisor must refuse a single reassembled
// command larger than the 256KB client cap. Raw fifo frames bypass
// ptyInput's client-side check (terminal.go:519), and ptySupervise
// tracks reassembly bytes (terminal.go:1062) but never caps them, so
// an unbounded assembly is written to the PTY in full.
// This test FAILS on current code: deficient path is the pending-map
// reassembly loop at terminal.go:1018-1078 (no per-ID byte cap, no
// pending-ID count cap).
func TestAdvFifoReassemblyBounded(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	// Drain worker: succeeds on pty write regardless of echo, so the
	// receipt purely reflects supervisor acceptance.
	if err := ptyStart("advfifo", work, "cat >/dev/null", "", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("advfifo")
	owner := controllerID()
	if err := ptyAcquireLease("advfifo", owner); err != nil {
		t.Fatalf("lease: %v", err)
	}
	const total = 128 // supervisor allows total <= 128 (terminal.go:1046)
	const chunk = 2400
	payload := make([]byte, total*chunk) // 307200 B > 256KB client cap
	for i := range payload {
		payload[i] = byte('a' + i%26)
	}
	id := "advbig-1"
	for i := 0; i < total; i++ {
		lo := i * chunk
		frame := "IN " + id + " " + owner + " " + strconv.Itoa(i+1) + "/" + strconv.Itoa(total) + " " +
			base64.StdEncoding.EncodeToString(payload[lo:lo+chunk])
		if err := ptyFifoWrite("advfifo", frame); err != nil {
			t.Fatalf("frame %d: %v", i+1, err)
		}
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, detail, err := ptyInputStatus(id)
		if err != nil {
			t.Fatalf("receipt: %v", err)
		}
		if st != "queued" {
			// Desired contract: oversize assemblies are refused with
			// an error receipt naming the bound. Current code has no
			// such check, so this FAILS (st is written_to_pty, or a
			// short-write error that names no bound).
			if st != "error" || (!strings.Contains(detail, "oversize") &&
				!strings.Contains(detail, "too large") && !strings.Contains(detail, "bound")) {
				t.Fatalf("UNBOUNDED REASSEMBLY: 307200B raw-fifo command accepted "+
					"(status=%q detail=%q); supervisor enforces no size bound "+
					"(deficient: terminal.go:1018 pending map + terminal.go:1062 bytes, "+
					"client-only cap bypassed at terminal.go:519)", st, detail)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("oversize assembly never dispositioned (still queued)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// advChildPids lists live processes whose parent is ppid.
func advChildPids(ppid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(raw)
		i := strings.LastIndex(s, ")")
		if i < 0 {
			continue
		}
		fields := strings.Fields(s[i+2:])
		if len(fields) < 2 {
			continue
		}
		pp, err := strconv.Atoi(fields[1])
		if err != nil || pp != ppid {
			continue
		}
		out = append(out, pid)
	}
	return out
}

// 4. Process-group termination: the supervisor launches workers via
// `sh -c` (terminal.go:843) with no new process group, and the ST
// path kills only the direct child (terminal.go:1106). A worker that
// spawns a background descendant must have the descendant reaped on
// stop. This test FAILS on current code: the orphaned `sleep` survives
// ptyStop.
//
// NOTE: the naive `sleep 60 & wait` variant PASSES incidentally — the
// orphan dies from SIGHUP when the supervisor exits and the pty master
// closes, not from any kill by the stop path. The `trap '' HUP`
// prefix (inherited across exec, as any daemon/nohup worker would do)
// removes that incidental hangup so the test probes ONLY owned
// process-group termination. Verified: naive orphan dies on stop,
// HUP-immune orphan survives (still alive after ptyStop returns nil).
func TestAdvProcessGroupKillsDescendants(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("advpg", work, "trap '' HUP; sleep 60 & wait", "", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	// Descendant discovery BEFORE stop: the `sleep 60` child of the
	// `sh -c` worker.
	var desc []int
	foundDeadline := time.Now().Add(8 * time.Second)
	for {
		m, _, err := ptyStatus("advpg")
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if m.WorkerPid > 0 {
			desc = advChildPids(m.WorkerPid)
			if len(desc) > 0 {
				break
			}
		}
		if time.Now().After(foundDeadline) {
			t.Fatalf("worker never spawned a background descendant (workerPid=%d)", m.WorkerPid)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := ptyStop("advpg"); err != nil {
		for _, p := range desc {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		t.Fatalf("stop: %v", err)
	}
	goneDeadline := time.Now().Add(5 * time.Second)
	for _, p := range desc {
		for procAlive(p) {
			if time.Now().After(goneDeadline) {
				_ = syscall.Kill(p, syscall.SIGKILL)
				t.Fatalf("ORPHANED DESCENDANT: pid %d survived owned ptyStop; "+
					"stop kills only the sh -c child (deficient: terminal.go:843 "+
					"no Setsid/process-group kill; terminal.go:1106 cmd.Process.Kill)", p)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
)

func ptyAvailable() bool {
	f, _, err := pty.Open()
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func TestPtyLease(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := ptyAcquireLease("s", ""); err == nil {
		t.Fatal("empty owner should fail")
	}
	if err := ptyAcquireLease("s", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := ptyAcquireLease("s", "alice"); err != nil {
		t.Fatal("same owner re-acquire should work")
	}
	if err := ptyAcquireLease("s", "bob"); err == nil {
		t.Fatal("foreign lease holder should be refused")
	}
}

func TestPtyUnknown(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if _, err := ptyLog("nope", 100); err == nil {
		t.Fatal("log of unknown should fail")
	}
	if _, err := ptyInput("nope", "o", "x"); err == nil {
		t.Fatal("input to unknown should fail")
	}
	if err := ptyStop("nope"); err == nil {
		t.Fatal("stop of unknown should fail")
	}
}

func waitForLog(t *testing.T, name, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := ptyLog(name, 8000)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("log never contained %q (err=%v data=%q)", want, err, string(data))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestPtySuperviseLifecycle(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("out", work, "echo pty-hello; sleep 30", "demo", "test-m"); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("out")
	m, state, err := ptyStatus("out")
	if err != nil || state != "running" {
		t.Fatalf("status: %v %s %+v", err, state, m)
	}
	if m.Harness != "demo" || m.Model != "test-m" {
		t.Fatalf("pty labels not persisted: %+v", m)
	}
	if m.WorkerPid <= 0 || !ptyWorkerAlive(m) {
		t.Fatalf("worker not alive: %+v", m)
	}
	waitForLog(t, "out", "pty-hello")
	// Interactive input round trip through a cat worker.
	if err := ptyStart("echoer", work, "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart echoer: %v", err)
	}
	defer ptyStop("echoer")
	waitForLog(t, "echoer", "")
	id, err := ptyInput("echoer", controllerID(), "hello-pty-input\n")
	if err != nil {
		t.Fatalf("ptyInput: %v", err)
	}
	if id == "" {
		t.Fatal("ptyInput must return a command id")
	}
	waitForLog(t, "echoer", "hello-pty-input")
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, _, serr := ptyInputStatus(id)
		if serr != nil {
			t.Fatalf("receipt: %v", serr)
		}
		if st == "written_to_pty" {
			break
		}
		if st == "error" {
			t.Fatalf("receipt reports error for live worker")
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt never left queued: %s", st)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Lease contention is refused without touching the session.
	if err := ptyAcquireLease("echoer", "intruder"); err == nil {
		t.Fatal("intruder should not take a held lease")
	}
	// Resize is accepted by a live supervisor.
	if err := ptyResize("echoer", 30, 100); err != nil {
		t.Fatalf("ptyResize: %v", err)
	}
	// Stop records a clean terminal state.
	if err := ptyStop("echoer"); err != nil {
		t.Fatalf("ptyStop: %v", err)
	}
	_, state, err = ptyStatus("echoer")
	if err != nil || (state != "exited" && state != "gone") {
		t.Fatalf("post-stop state: %v %s", err, state)
	}
	if err := ptyStop("out"); err != nil {
		t.Fatalf("ptyStop out: %v", err)
	}
}

func TestPtyKeyBytes(t *testing.T) {
	cases := map[string]string{
		"enter": "\r", "backspace": "\x7f", "tab": "\t",
		"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C",
		"left": "\x1b[D", "ctrl+c": "\x03",
	}
	for key, want := range cases {
		got, send := ptyKeyBytes(key, nil)
		if !send || got != want {
			t.Fatalf("key %q: got %q,%v want %q,true", key, got, send, want)
		}
	}
	for _, local := range []string{"esc", "pgup", "pgdn"} {
		if _, send := ptyKeyBytes(local, nil); send {
			t.Fatalf("local key %q must not be sent", local)
		}
	}
	if got, send := ptyKeyBytes("a", []rune{'a'}); !send || got != "a" {
		t.Fatalf("rune passthrough: %q %v", got, send)
	}
	if isPtySession("plain") || !isPtySession("pty:x") || ptyBare("pty:x") != "x" {
		t.Fatal("pty prefix helpers broken")
	}
}

func TestCombinedNames(t *testing.T) {
	reg := map[string]Session{"b": {}, "a": {}}
	metas := []ptyMeta{{Name: "z"}, {Name: "a"}}
	got := combinedNames(reg, metas)
	want := []string{"a", "b", "pty:a", "pty:z"}
	if len(got) != len(want) {
		t.Fatalf("combined: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("combined: got %v want %v", got, want)
		}
	}
}

func TestAtomicLeaseRace(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	const racers = 16
	wins := make(chan string, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := ptyAcquireLease("rl", fmt.Sprintf("owner-%d", i)); err == nil {
				wins <- fmt.Sprintf("owner-%d", i)
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	var got []string
	for w := range wins {
		got = append(got, w)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one lease winner, got %d: %v", len(got), got)
	}
}

func TestControllerID(t *testing.T) {
	a, b := controllerID(), controllerID()
	if a == "" || a != b {
		t.Fatalf("controller id must be stable and non-empty: %q %q", a, b)
	}
	if !strings.Contains(a, strconv.Itoa(os.Getpid())) {
		t.Fatalf("controller id must embed pid: %q", a)
	}
}

func TestFramedInputLarge(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("big", work, "cat", "", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("big")
	body := strings.Repeat("0123456789abcdef", 1500) // 24KB, forces many frames
	head, tail := "MARK-HEAD-", "-MARK-TAIL"
	payload := head + body + tail + "\n"
	id, err := ptyInput("big", controllerID(), payload)
	if err != nil {
		t.Fatalf("ptyInput: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, detail, serr := ptyInputStatus(id)
		if serr != nil {
			t.Fatalf("receipt: %v", serr)
		}
		if st == "written_to_pty" {
			break
		}
		if st == "error" {
			t.Fatalf("framed write failed: %s", detail)
		}
		if time.Now().After(deadline) {
			t.Fatal("large framed input never confirmed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitForLog(t, "big", tail)
	data, err := ptyLog("big", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), head+body) || !strings.Contains(string(data), tail) {
		t.Fatal("large payload corrupted or interleaved in transit")
	}
}

func ptyTestDB(t *testing.T) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ptyTable(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// requireSupervisorReaped polls until the recorded supervisor
// process is gone: ptyStop reports the row gone while the
// daemonized supervisor exits asynchronously, and a surviving
// daemon leaks past the test binary (F-17).
func requireSupervisorReaped(t *testing.T, m ptyMeta) {
	t.Helper()
	if m.SupervisorPid <= 0 {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if !procAlive(m.SupervisorPid) {
			return
		}
		if st, err := procStartTime(m.SupervisorPid); err != nil || st != m.SupervisorStart {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("supervisor for %q still alive after stop (pid %d)", m.Name, m.SupervisorPid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSupervisorIdentityFencing(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("fenced", work, "sleep 30", "demo", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("fenced")
	m, err := ptyGet("fenced")
	if err != nil {
		t.Fatal(err)
	}
	if m.SupervisorPid <= 0 || m.SupervisorStart == 0 {
		t.Fatalf("supervisor identity not recorded: %+v", m)
	}
	if st, err := procStartTime(m.SupervisorPid); err != nil || st != m.SupervisorStart {
		t.Fatalf("recorded start %d does not match pid %d (err=%v)", m.SupervisorStart, m.SupervisorPid, err)
	}
	if !ptySupervisorAlive(m) {
		t.Fatal("live supervisor should verify")
	}
	// Simulate pid reuse: the recorded pid is alive, but under a
	// different starttime. Liveness must fail closed.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE pty_sessions SET supervisor_start = ? WHERE name = 'fenced'`,
		m.SupervisorStart+1000000); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt, err := ptyGet("fenced")
	if err != nil {
		t.Fatal(err)
	}
	if ptySupervisorAlive(corrupt) {
		t.Fatal("mismatched supervisor starttime must not verify as alive")
	}
	// Stop must refuse to act on the unverifiable identity: the
	// worker belongs to a session we can no longer authenticate.
	if err := ptyStop("fenced"); err == nil {
		t.Fatal("stop must refuse a session with mismatched supervisor identity")
	}
	if !ptyWorkerAlive(corrupt) {
		t.Fatal("refused stop must not have killed the worker")
	}
	// Hygiene (F-17): restore the corrupted identity and stop
	// normally so the supervisor and worker do not leak past the
	// test binary. The deferred stop stays as a safety net.
	db, err = openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE pty_sessions SET supervisor_start = ? WHERE name = 'fenced'`,
		m.SupervisorStart); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ptyStop("fenced"); err != nil {
		t.Fatalf("restored stop must succeed: %v", err)
	}
	requireSupervisorReaped(t, m)
}

func TestStaleStartingRecovery(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	ptyTestDB(t)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	// A leftover 'starting' row from a dead startup: no live supervisor.
	_, err = db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, logpath, owner, status, heartbeat, generation, harness, model)
		VALUES ('stale', ?, 'sleep 30', ?, 'old-owner', 'starting', '2020-01-01T00:00:00Z', 3, '', '')`,
		work, ptyLogPath("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE pty_sessions SET supervisor_pid = ?, supervisor_start = 1 WHERE name = 'stale'`,
		1<<30); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ptyStart("stale", work, "sleep 30", "demo", ""); err != nil {
		t.Fatalf("stale starting row should be recoverable: %v", err)
	}
	defer ptyStop("stale")
	m, err := ptyGet("stale")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "running" || m.Generation != 4 || m.Owner == "old-owner" {
		t.Fatalf("takeover did not re-authorize the row: %+v", m)
	}
	if !ptySupervisorAlive(m) {
		t.Fatal("replacement supervisor should verify")
	}
}

func TestLiveStartingRefusesTakeover(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("live1", work, "sleep 30", "demo", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("live1")
	// A second starter racing a live startup sees 'starting' but the
	// supervisor is authoritative: it must back off.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE pty_sessions SET status = 'starting' WHERE name = 'live1'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ptyStart("live1", work, "sleep 30", "demo", ""); err == nil {
		t.Fatal("takeover must refuse while the supervisor is alive")
	}
	m, err := ptyGet("live1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Status != "running" && m.Status != "starting" {
		t.Fatalf("refused takeover changed row state: %+v", m)
	}
}

func TestConcurrentTakeoverOneWinner(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	ptyTestDB(t)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO pty_sessions
		(name, workdir, command, logpath, owner, status, heartbeat, generation, harness, model)
		VALUES ('race', ?, 'sleep 30', ?, 'old-owner', 'starting', '2020-01-01T00:00:00Z', 0, '', '')`,
		work, ptyLogPath("race"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ptyStart("race", work, "sleep 30", "demo", "") == nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("expected exactly one takeover winner, got %d", wins.Load())
	}
	defer ptyStop("race")
	m, err := ptyGet("race")
	if err != nil {
		t.Fatal(err)
	}
	if m.Owner == "old-owner" {
		t.Fatalf("winner did not re-authorize the row: %+v", m)
	}
}

func TestBatchedFleetRefresh(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "ob")
	seedOwnedSession(t, "k1", "ob")
	seedOwnedSession(t, "k2", "ob")
	seedOwnedSession(t, "k3", "ob")
	t.Setenv("FAKE_TMUX_LIST", "tui-k1 tui-k2")
	work := t.TempDir()
	if err := ptyStart("bp", work, "sleep 30", "demo", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	defer ptyStop("bp")
	d := collectRefreshData(liveRefreshSource{}, 1, "pty:bp", false)
	if !d.running["k1"] || !d.running["k2"] {
		t.Fatalf("batched tmux state wrong: %+v", d.running)
	}
	if d.running["k3"] {
		t.Fatal("session missing from list-sessions read as running")
	}
	if !d.running["pty:bp"] {
		t.Fatal("live PTY session not classified running")
	}
	if _, ok := d.logGen["pty:bp"]; !ok {
		t.Fatal("refresh missing output generation")
	}
	if d.entry.Name != "pty:bp" {
		t.Fatalf("selected entry wrong: %+v", d.entry)
	}
	logged, _ := os.ReadFile(logPath)
	if strings.Contains(string(logged), "has-session") {
		t.Fatalf("fleet refresh fell back to per-session probes:\n%s", logged)
	}
}

func TestSixWorkersFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (fleet scale); run without -short")
	}
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	var names []string
	for i := 0; i < 6; i++ {
		n := fmt.Sprintf("w%d", i)
		names = append(names, n)
		if err := ptyStart(n, work, "sleep 30", "muse", "code"); err != nil {
			t.Fatalf("ptyStart %s: %v", n, err)
		}
		defer ptyStop(n)
	}
	metas, err := ptyListAll()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ptyMeta{}
	for _, m := range metas {
		byName[m.Name] = m
	}
	seenWorker, seenSup := map[int]bool{}, map[int]bool{}
	for _, n := range names {
		m, ok := byName[n]
		if !ok {
			t.Fatalf("worker %s missing from registry", n)
		}
		if ptyClassify(m) != "running" {
			t.Fatalf("worker %s not running: %+v", n, m)
		}
		if seenWorker[m.WorkerPid] || seenSup[m.SupervisorPid] {
			t.Fatalf("worker %s shares an identity: %+v", n, m)
		}
		seenWorker[m.WorkerPid] = true
		seenSup[m.SupervisorPid] = true
	}
	tabs := combinedNames(loadRegistry(), metas)
	for _, n := range names {
		found := false
		for _, tab := range tabs {
			if tab == ptyDisplay(n) {
				found = true
			}
		}
		if !found {
			t.Fatalf("no tab for %s in %v", n, tabs)
		}
	}
	// Input routes are per-session: a lease on w0 blocks a rival on
	// w0 but never touches w1.
	if err := ptyAcquireLease("w0", "ctl-A"); err != nil {
		t.Fatalf("lease w0: %v", err)
	}
	if err := ptyAcquireLease("w0", "ctl-B"); err == nil {
		t.Fatal("rival took the w0 input lease")
	}
	if err := ptyAcquireLease("w1", "ctl-B"); err != nil {
		t.Fatalf("lease w1 should be independent: %v", err)
	}
}

func TestHighOutputIsolation(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	work := t.TempDir()
	if err := ptyStart("heavy", work, `for i in $(seq 1 30000); do echo heavy-$i; done; sleep 30`, "demo", ""); err != nil {
		t.Fatalf("ptyStart heavy: %v", err)
	}
	defer ptyStop("heavy")
	if err := ptyStart("quiet", work, "cat", "demo", ""); err != nil {
		t.Fatalf("ptyStart quiet: %v", err)
	}
	defer ptyStop("quiet")
	waitForLog(t, "heavy", "heavy-30000")
	id, err := ptyInput("quiet", controllerID(), "quiet-marker-1\n")
	if err != nil {
		t.Fatalf("ptyInput quiet: %v", err)
	}
	if id == "" {
		t.Fatal("ptyInput must return a command id")
	}
	waitForLog(t, "quiet", "quiet-marker-1")
	qdata, err := ptyLog("quiet", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(qdata), "heavy-") {
		t.Fatal("heavy session output leaked into quiet session log")
	}
	d := collectRefreshData(liveRefreshSource{}, 2, "pty:quiet", false)
	if !d.running["pty:heavy"] || !d.running["pty:quiet"] {
		t.Fatalf("fleet state wrong under load: %+v", d.running)
	}
}

func TestPtyWorkerExitRecorded(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := ptyStart("quick", t.TempDir(), "echo bye; exit 3", "", ""); err != nil {
		t.Fatalf("ptyStart: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, err := ptyGet("quick")
		if err != nil {
			t.Fatal(err)
		}
		if m.Status == "exited" {
			if m.ExitCode != 3 {
				t.Fatalf("exit code: got %d want 3", m.ExitCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worker exit never recorded: %+v", m)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

package main

// F-11: terminal input senders never retired. Every test below
// fails on the pre-fix code (bare global map, no close path) and
// passes with the lifecycle-managed service: lanes retire on
// Close, on session stop, and on generation replacement; queued
// work drains as rejected receipts; goroutines and heap return to
// baseline after hundreds of sessions.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubExecConfirm replaces execInputJob with immediate confirmation
// and returns a counter of executed jobs.
func stubExecConfirm(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	execInputJob = func(job inputJob) (string, string) {
		n.Add(1)
		return DispositionConfirmed, "ok"
	}
	t.Cleanup(func() { execInputJob = execInputJobReal })
	return &n
}

// awaitReply waits for one job receipt with a hard deadline so a
// lost reply fails the test instead of hanging the suite.
func awaitReply(t *testing.T, job inputJob) inputResultMsg {
	t.Helper()
	select {
	case res := <-job.reply:
		return res
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for reply to %s", job.opID)
		return inputResultMsg{}
	}
}

// waitFor polls cond until true or the timeout, for goroutine-exit
// assertions that must not be timing-flaky.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// Retirement drains queued work as rejected receipts: only the job
// already executing may settle normally; everything still buffered
// is refused, never delivered.
func TestInputSenderRetireDrainsQueuedWork(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	var executed atomic.Int64
	execInputJob = func(job inputJob) (string, string) {
		if job.opID == "op-1" {
			startOnce.Do(func() { close(started) })
			<-release
		}
		executed.Add(1)
		return DispositionConfirmed, "ok"
	}
	defer func() { execInputJob = execInputJobReal }()

	const name = "life-drain"
	lane := newInputSenderWithCap(name, 2)
	t.Cleanup(func() { retireInputSenders(name) })
	j1, ok := lane.enqueue("op-1", false, "a", nil, "", "owner")
	if !ok {
		t.Fatal("first enqueue must fit")
	}
	// Retire only once op-1 is inside exec: what is buffered after
	// that point is queued work and must drain as rejected.
	<-started
	j2, _ := lane.enqueue("op-2", false, "b", nil, "", "owner")
	j3, _ := lane.enqueue("op-3", false, "c", nil, "", "owner")
	// 1 in flight + 2 buffered = full.
	if _, ok := lane.enqueue("op-4", false, "d", nil, "", "owner"); ok {
		t.Fatal("over-cap enqueue must be refused")
	}

	lane.Retire()
	close(release)

	if res := awaitReply(t, j1); res.disposition != DispositionConfirmed {
		t.Fatalf("in-flight job must settle normally, got %+v", res)
	}
	for i, j := range []inputJob{j2, j3} {
		res := awaitReply(t, j)
		if res.disposition != DispositionRejected {
			t.Fatalf("queued job %d must drain as rejected, got %+v", i+2, res)
		}
	}
	if got := executed.Load(); got != 1 {
		t.Fatalf("exec ran %d jobs, want exactly the in-flight one", got)
	}
	if _, ok := lane.enqueue("op-5", false, "e", nil, "", "owner"); ok {
		t.Fatal("enqueue after retire must be refused")
	}
	waitFor(t, "lane goroutine exit", func() bool {
		select {
		case <-lane.done:
			return true
		default:
			return false
		}
	})
}

// Close is idempotent and also severs the lane from the registry.
func TestInputSenderCloseIsIdempotent(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	stubExecConfirm(t)
	const name = "life-idem"
	lane := newInputSender(name)
	if liveInputSenderCount() < 1 {
		t.Fatal("fresh lane must be registered")
	}
	lane.Close()
	lane.Close()
	lane.Retire()
	if !lane.isRetired() {
		t.Fatal("lane must read retired after Close")
	}
	if _, ok := lane.enqueue("op-x", false, "x", nil, "", "owner"); ok {
		t.Fatal("enqueue after Close must be refused")
	}
	waitFor(t, "lane goroutine exit", func() bool {
		select {
		case <-lane.done:
			return true
		default:
			return false
		}
	})
	if n := retireInputSenders(name); n != 0 {
		t.Fatalf("closed lane must already be deregistered, retired %d", n)
	}
}

// Replacement (new Owner generation) retires the predecessor's
// lane and hands out a fresh one; the registry holds exactly one.
func TestInputSenderReplacementRetiresPredecessor(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	stubExecConfirm(t)
	const name = "life-replace"
	setOwner := func(owner string) {
		t.Helper()
		if err := saveRegistry(map[string]Session{name: {Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: owner}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { retireInputSenders(name) })

	setOwner("gen-1")
	old := senderFor(name)
	j, ok := old.enqueue("op-1", false, "a", nil, "", "owner")
	if !ok {
		t.Fatal("enqueue on live lane must fit")
	}
	if res := awaitReply(t, j); res.disposition != DispositionConfirmed {
		t.Fatalf("live lane must confirm, got %+v", res)
	}

	setOwner("gen-2")
	next := senderFor(name)
	if next == old {
		t.Fatal("replacement must hand out a fresh lane, not the predecessor")
	}
	if !old.isRetired() {
		t.Fatal("predecessor lane must be retired on replacement")
	}
	if _, ok := old.enqueue("op-stale", false, "x", nil, "", "owner"); ok {
		t.Fatal("enqueue on retired predecessor must be refused")
	}
	j2, ok := next.enqueue("op-2", false, "b", nil, "", "owner")
	if !ok {
		t.Fatal("enqueue on successor lane must fit")
	}
	if res := awaitReply(t, j2); res.disposition != DispositionConfirmed {
		t.Fatalf("successor lane must confirm, got %+v", res)
	}
	if got := liveInputSenderCount(); got != 1 {
		t.Fatalf("registry holds %d lanes, want exactly the successor", got)
	}
}

// A job stamped for another generation is stale by definition: the
// lane rejects it without executing, so stale input never reaches
// a successor session even if it is forced onto the live lane.
func TestInputSenderStaleGenerationNeverExecutes(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	executed := stubExecConfirm(t)
	const name = "life-stale"
	lane := newInputSender(name)
	t.Cleanup(func() { retireInputSenders(name) })

	stale := inputJob{session: name, opID: "op-stale", backend: lane.backend,
		generation: lane.generation + "-predecessor", reply: make(chan inputResultMsg, 1)}
	if _, ok := lane.tryEnqueue(stale); !ok {
		t.Fatal("stale job must be accepted into the buffer to prove the lane refuses it")
	}
	if res := awaitReply(t, stale); res.disposition != DispositionRejected {
		t.Fatalf("foreign-generation job must be rejected, got %+v", res)
	}
	if got := executed.Load(); got != 0 {
		t.Fatalf("exec ran %d stale jobs, want 0", got)
	}

	fresh, ok := lane.enqueue("op-fresh", false, "a", nil, "", "owner")
	if !ok {
		t.Fatal("fresh enqueue must fit")
	}
	if res := awaitReply(t, fresh); res.disposition != DispositionConfirmed {
		t.Fatalf("current-generation job must confirm, got %+v", res)
	}
	if got := executed.Load(); got != 1 {
		t.Fatalf("exec ran %d jobs, want exactly the fresh one", got)
	}
}

// Queued stale work drains on replacement: jobs buffered on the
// predecessor are rejected, the successor delivers normally, and
// exec never observes the stale jobs.
func TestInputSenderQueuedStaleDrainsOnReplace(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	release := make(chan struct{})
	started := make(chan struct{})
	var startOnce sync.Once
	var mu sync.Mutex
	var saw []string
	execInputJob = func(job inputJob) (string, string) {
		if job.opID == "op-block" {
			startOnce.Do(func() { close(started) })
			<-release
		}
		mu.Lock()
		saw = append(saw, job.opID)
		mu.Unlock()
		return DispositionConfirmed, "ok"
	}
	defer func() { execInputJob = execInputJobReal }()
	const name = "life-replace-drain"
	setOwner := func(owner string) {
		t.Helper()
		if err := saveRegistry(map[string]Session{name: {Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: owner}}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { retireInputSenders(name) })

	setOwner("gen-1")
	old := senderFor(name)
	blocker, _ := old.enqueue("op-block", false, "a", nil, "", "owner")
	// Replace only once the blocker is inside exec, so op-stale is
	// deterministically still queued when the predecessor retires.
	<-started
	stale, _ := old.enqueue("op-stale", false, "b", nil, "", "owner")

	setOwner("gen-2")
	next := senderFor(name)
	close(release)

	if res := awaitReply(t, blocker); res.disposition != DispositionConfirmed {
		t.Fatalf("in-flight job must settle, got %+v", res)
	}
	if res := awaitReply(t, stale); res.disposition != DispositionRejected {
		t.Fatalf("queued stale job must drain as rejected, got %+v", res)
	}
	fresh, ok := next.enqueue("op-fresh", false, "c", nil, "", "owner")
	if !ok {
		t.Fatal("successor enqueue must fit")
	}
	if res := awaitReply(t, fresh); res.disposition != DispositionConfirmed {
		t.Fatalf("successor job must confirm, got %+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range saw {
		if id == "op-stale" {
			t.Fatalf("exec observed stale job; saw %v", saw)
		}
	}
	if len(saw) != 2 {
		t.Fatalf("exec must see exactly blocker+fresh, saw %v", saw)
	}
}

// Retirement is scoped: retiring one session leaves the other's
// lane live, and retiring unknown/retired names retires zero.
func TestRetireInputSendersIsScopedAndIdempotent(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	stubExecConfirm(t)
	a := newInputSender("life-scope-a")
	b := newInputSender("life-scope-b")
	t.Cleanup(func() { retireAllInputSenders() })

	if n := retireInputSenders("life-scope-a"); n != 1 {
		t.Fatalf("retire must close 1 lane, closed %d", n)
	}
	if !a.isRetired() {
		t.Fatal("retired session lane must read retired")
	}
	if b.isRetired() {
		t.Fatal("other session lane must stay live")
	}
	j, ok := b.enqueue("op-1", false, "a", nil, "", "owner")
	if !ok {
		t.Fatal("live lane must still accept work")
	}
	if res := awaitReply(t, j); res.disposition != DispositionConfirmed {
		t.Fatalf("live lane must confirm, got %+v", res)
	}
	if n := retireInputSenders("life-scope-a"); n != 0 {
		t.Fatalf("second retire must close 0 lanes, closed %d", n)
	}
	if n := retireInputSenders("life-no-such"); n != 0 {
		t.Fatalf("retire of unknown session must close 0 lanes, closed %d", n)
	}
}

// Session stop retires the lane: after stopSession confirms the
// kill, no lane (and no goroutine) remains for the session.
func TestStopSessionRetiresInputLane(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	stubExecConfirm(t)
	const owner = "life-stop-owner"
	installFakeTmux(t, owner)
	const name = "lifestop"
	workdir := t.TempDir()
	if err := saveRegistry(map[string]Session{
		name: {Name: name, Tmux: tmuxName(name), Workdir: workdir, Command: "sleep 60", Owner: owner},
	}); err != nil {
		t.Fatal(err)
	}

	lane := senderFor(name)
	j, ok := enqueueInputJob(name, false, "a", nil, "", "controller")
	if !ok {
		t.Fatal("enqueue on live lane must fit")
	}
	if res := awaitReply(t, j); res.disposition != DispositionConfirmed {
		t.Fatalf("live lane must confirm, got %+v", res)
	}

	if err := stopSession(name); err != nil {
		t.Fatalf("stopSession: %v", err)
	}
	if !lane.isRetired() {
		t.Fatal("stopSession must retire the session's input lane")
	}
	if _, ok := lane.enqueue("op-after", false, "x", nil, "", "owner"); ok {
		t.Fatal("enqueue after stop must be refused")
	}
	if got := liveInputSenderCount(); got != 0 {
		t.Fatalf("registry holds %d lanes after stop, want 0", got)
	}
	waitFor(t, "lane goroutine exit", func() bool {
		select {
		case <-lane.done:
			return true
		default:
			return false
		}
	})
}

// Hundreds of create/use/retire/replace cycles return goroutines
// and heap near baseline: the F-11 leak made both grow per session
// with no path back.
func TestInputSenderLifecycleHundredsReturnToBaseline(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	stubExecConfirm(t)
	t.Cleanup(func() { retireAllInputSenders() })

	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	baseGoroutines := runtime.NumGoroutine()
	var baseMem runtime.MemStats
	runtime.ReadMemStats(&baseMem)

	const unique = 200
	for i := 0; i < unique; i++ {
		name := fmt.Sprintf("life-many-%d", i)
		j, ok := enqueueInputJob(name, false, "x", nil, "", "owner")
		if !ok {
			t.Fatalf("enqueue %d refused", i)
		}
		if res := awaitReply(t, j); res.disposition != DispositionConfirmed {
			t.Fatalf("job %d not confirmed: %+v", i, res)
		}
		if n := retireInputSenders(name); n != 1 {
			t.Fatalf("retire %d closed %d lanes, want 1", i, n)
		}
	}

	const name = "life-churn"
	for i := 0; i < 100; i++ {
		if err := saveRegistry(map[string]Session{
			name: {Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: fmt.Sprintf("churn-%d", i)},
		}); err != nil {
			t.Fatal(err)
		}
		j, ok := enqueueInputJob(name, false, "x", nil, "", "owner")
		if !ok {
			t.Fatalf("churn enqueue %d refused", i)
		}
		if res := awaitReply(t, j); res.disposition != DispositionConfirmed {
			t.Fatalf("churn job %d not confirmed: %+v", i, res)
		}
	}
	retireAllInputSenders()

	if got := liveInputSenderCount(); got != 0 {
		t.Fatalf("registry holds %d lanes after churn, want 0", got)
	}
	waitFor(t, "goroutines near baseline", func() bool {
		return runtime.NumGoroutine() <= baseGoroutines+5
	})
	if got := runtime.NumGoroutine() - baseGoroutines; got > 5 {
		t.Fatalf("goroutine delta=%d after 300 sessions, want <=5", got)
	}
	runtime.GC()
	time.Sleep(50 * time.Millisecond)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if growth := int64(after.HeapAlloc) - int64(baseMem.HeapAlloc); growth > 2<<20 {
		t.Fatalf("heap grew %d bytes after 300 retired sessions, want <2MB", growth)
	}
}

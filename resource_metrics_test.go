package main

// P3 observe-only resource-usage snapshot: failing-first coverage.
// The snapshot must reflect a created-then-retired sender lane and
// an over-cap stream file without altering either: no lane is
// resurrected, no stream file is rotated/truncated, no goroutine
// is spawned, and nothing is persisted.

import (
	"os"
	"runtime"
	"testing"
)

// TestResourceSnapshotReflectsLaneAndOverCapStream is the P3
// failing-first test: it fails until resource_metrics.go owns a
// single observe-only snapshot function.
func TestResourceSnapshotReflectsLaneAndOverCapStream(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	t.Cleanup(func() { retireAllInputSenders() })
	stubExecConfirm(t)

	const name = "resnap-p3"
	if err := saveRegistry(map[string]Session{
		name: {Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: "resnap-owner"},
	}); err != nil {
		t.Fatal(err)
	}

	// Over-cap stream file: strictly above the retention cap.
	over := make([]byte, tmuxStreamCap+1234)
	for i := range over {
		over[i] = 'x'
	}
	if err := os.MkdirAll(stateDir()+"/streams", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath(name), over, 0o600); err != nil {
		t.Fatal(err)
	}
	sizeBefore, err := os.Stat(streamPath(name))
	if err != nil {
		t.Fatal(err)
	}

	// Create a sender lane, then retire it (created-then-retired).
	lane := senderFor(name)
	if got := liveInputSenderCount(); got != 1 {
		t.Fatalf("registry holds %d lanes after create, want 1", got)
	}
	if n := retireInputSenders(name); n != 1 {
		t.Fatalf("retire closed %d lanes, want 1", n)
	}
	if !lane.isRetired() {
		t.Fatal("lane must read retired after retireInputSenders")
	}
	// The retired lane's goroutine exits asynchronously; wait for
	// it so the no-new-goroutine assertion below has a stable
	// baseline.
	waitFor(t, "lane goroutine exit", func() bool {
		select {
		case <-lane.done:
			return true
		default:
			return false
		}
	})

	// The snapshot is observe-only: it must not resurrect the lane,
	// rotate the stream, spawn goroutines, or persist anything.
	goroutinesBefore := runtime.NumGoroutine()
	snap := snapshotResourceUsage()
	if got := runtime.NumGoroutine(); got > goroutinesBefore {
		t.Fatalf("snapshot spawned goroutines: %d -> %d", goroutinesBefore, got)
	}

	if snap.SenderLanes != 0 {
		t.Fatalf("snapshot reports %d sender lanes, want 0 after retire", snap.SenderLanes)
	}
	if snap.Goroutines <= 0 {
		t.Fatalf("snapshot reports %d goroutines, want >0", snap.Goroutines)
	}
	if snap.HeapAlloc == 0 {
		t.Fatal("snapshot reports zero heap alloc")
	}
	if snap.StreamCap != int64(tmuxStreamCap) {
		t.Fatalf("snapshot cap=%d, want %d", snap.StreamCap, tmuxStreamCap)
	}

	var found *streamResourceUsage
	for i := range snap.Streams {
		if snap.Streams[i].Session == name {
			found = &snap.Streams[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("snapshot has no entry for session %q", name)
	}
	if found.Bytes != sizeBefore.Size() {
		t.Fatalf("snapshot bytes=%d, want stream size %d", found.Bytes, sizeBefore.Size())
	}
	if !found.OverCap {
		t.Fatalf("snapshot marks %d-byte stream (cap %d) as within cap", found.Bytes, snap.StreamCap)
	}

	// Post-snapshot: nothing altered.
	if got := liveInputSenderCount(); got != 0 {
		t.Fatalf("snapshot resurrected %d lane(s), want 0", got)
	}
	if lane.isRetired() != true {
		t.Fatal("snapshot un-retired the lane")
	}
	sizeAfter, err := os.Stat(streamPath(name))
	if err != nil {
		t.Fatal(err)
	}
	if sizeAfter.Size() != sizeBefore.Size() {
		t.Fatalf("snapshot altered stream file: %d -> %d bytes", sizeBefore.Size(), sizeAfter.Size())
	}
}

package main

// F-09: tmux stream retention must be autonomous. Before this fix,
// bounding only happened on read (streamTail swept the fleet), so a
// high-output session nobody was watching grew without bound, and a
// missing/corrupt .gen counter read as proof of no loss.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// overCapBurst writes a 2x-cap stream body ending in a sentinel tail.
func writeOverCapBurst(t *testing.T, name string, fill byte) {
	t.Helper()
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = fill
	}
	copy(big[len(big)-6:], "tail\n")
	if err := os.MkdirAll(filepath.Dir(streamPath(name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath(name), big, 0o600); err != nil {
		t.Fatal(err)
	}
}

func streamSizeOrFail(t *testing.T, name string) int64 {
	t.Helper()
	fi, err := os.Stat(streamPath(name))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// waitStreamGeneration polls for an exact rotation count instead of
// asserting it the moment the stream looks bounded. Rotation lands
// the truncation before the generation bump (rotateLocked documents
// the order), so a size poll can observe a mid-rotation file while
// the bump is still in flight — asserting immediately flakes under
// load. The expected values are unchanged; only the wait is
// synchronized.
func waitStreamGeneration(t *testing.T, name string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := streamGeneration(name); got == want {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("stream %q generation must reach %d, got %d", name, want, streamGeneration(name))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A high-output session with NO reader must still stay bounded: the
// supervised sweeper (not a read) owns retention. Repeated over-cap
// bursts with no streamTail/captureOutput/collectRefreshData call in
// between must each be rotated with a visible gap marker and a
// bumped generation.
func TestRetentionAutonomousWithoutReader(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f09a")
	seedOwnedSession(t, "w", "f09a")

	stop := make(chan struct{})
	done := StartRetentionSweeper(10*time.Millisecond, stop)
	// Join the supervisor before TempDir cleanup: an in-flight tick
	// writes into the state dir, racing RemoveAll.
	defer func() { close(stop); <-done }()

	for i := 0; i < 3; i++ {
		writeOverCapBurst(t, "w", 'x')
		// No reader anywhere: only the sweeper may bound this.
		deadline := time.Now().Add(5 * time.Second)
		for {
			if streamSizeOrFail(t, "w") <= tmuxStreamCap {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("burst %d: sweeper did not bound unobserved stream (size %d > cap %d)",
					i, streamSizeOrFail(t, "w"), tmuxStreamCap)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitStreamGeneration(t, "w", 3, 5*time.Second)
	data, err := os.ReadFile(streamPath("w"))
	if err != nil || !strings.Contains(string(data), "older output discarded") {
		t.Fatalf("rotation must leave a visible gap marker: %v", err)
	}
}

// Restart must not lose retention: all rotation state is file-backed,
// so a fresh sweeper after a stop keeps the bound and continues the
// generation instead of resetting history.
func TestRetentionSurvivesRestart(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f09b")
	seedOwnedSession(t, "w", "f09b")

	stop1 := make(chan struct{})
	done1 := StartRetentionSweeper(10*time.Millisecond, stop1)
	writeOverCapBurst(t, "w", 'x')
	deadline := time.Now().Add(5 * time.Second)
	for streamSizeOrFail(t, "w") > tmuxStreamCap && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	close(stop1) // "restart": first supervisor goes away entirely
	<-done1
	waitStreamGeneration(t, "w", 1, 5*time.Second)

	// More output lands while nobody supervises, then a new sweeper
	// starts (fresh process state, same files).
	writeOverCapBurst(t, "w", 'y')
	stop2 := make(chan struct{})
	done2 := StartRetentionSweeper(10*time.Millisecond, stop2)
	defer func() { close(stop2); <-done2 }()
	deadline = time.Now().Add(5 * time.Second)
	for streamSizeOrFail(t, "w") > tmuxStreamCap && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sz := streamSizeOrFail(t, "w"); sz > tmuxStreamCap {
		t.Fatalf("post-restart stream must be bounded at %d, got %d", tmuxStreamCap, sz)
	}
	waitStreamGeneration(t, "w", 2, 5*time.Second)
	data, _ := os.ReadFile(streamPath("w"))
	if !strings.Contains(string(data), "older output discarded") {
		t.Fatal("gap accounting must stay visible across restart")
	}
}

// Missing or corrupt generation metadata is unknown history, never
// proof of no loss. The sweeper must heal it with a fresh epoch and
// a visible gap marker, not claim continuity.
func TestRetentionUnknownGenerationHealsWithGap(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f09c")
	seedOwnedSession(t, "w", "f09c")

	// Missing counter: unknown, not zero-loss.
	if _, known := streamGenerationKnown("w"); known {
		t.Fatal("missing generation metadata must read as unknown, not clean")
	}
	writeOverCapBurst(t, "w", 'x')
	if n := MaintainRetention(); n != 1 {
		t.Fatalf("expected one rotation, got %d", n)
	}
	gen, known := streamGenerationKnown("w")
	if !known || gen != 1 {
		t.Fatalf("rotation must start a fresh known epoch at 1, got gen=%d known=%v", gen, known)
	}

	// Corrupt counter: unknown, not zero-loss either.
	if err := os.WriteFile(streamGenPath("w"), []byte("not-a-number\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, known := streamGenerationKnown("w"); known {
		t.Fatal("corrupt generation metadata must read as unknown, not clean")
	}
	if got := streamGeneration("w"); got != 0 {
		t.Fatalf("compat numeric view of corrupt metadata must stay 0, got %d", got)
	}
	writeOverCapBurst(t, "w", 'y')
	if n := MaintainRetention(); n != 1 {
		t.Fatalf("expected one rotation over corrupt metadata, got %d", n)
	}
	gen, known = streamGenerationKnown("w")
	if !known || gen != 1 {
		t.Fatalf("corrupt metadata must heal to a fresh epoch at 1, got gen=%d known=%v", gen, known)
	}
	data, _ := os.ReadFile(streamPath("w"))
	if !strings.Contains(string(data), "older output discarded") {
		t.Fatal("healed rotation must leave a visible gap marker")
	}
}

// A failed truncation must be observable: the rotation is not
// attempted silently, the generation is not bumped as if history was
// discarded, and the next sweep retries and heals.
func TestRetentionFailureObservable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based failure injection needs a non-root user")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f09d")
	seedOwnedSession(t, "w", "f09d")
	writeOverCapBurst(t, "w", 'x')
	if err := os.Chmod(streamPath("w"), 0o400); err != nil {
		t.Fatal(err)
	}

	if n := MaintainRetention(); n != 0 {
		t.Fatalf("failed rotation must not count as rotated, got %d", n)
	}
	detail, failed := RetentionFailure("w")
	if !failed || detail == "" {
		t.Fatal("failed truncation must be observable via RetentionFailure")
	}
	if _, known := streamGenerationKnown("w"); known {
		t.Fatal("failed rotation must not fabricate a generation")
	}

	// Next sweep retries: restore write access and the bound heals,
	// clearing the failure marker.
	if err := os.Chmod(streamPath("w"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := MaintainRetention(); n != 1 {
		t.Fatalf("retry must rotate once, got %d", n)
	}
	if _, failed := RetentionFailure("w"); failed {
		t.Fatal("successful retry must clear the failure marker")
	}
	if sz := streamSizeOrFail(t, "w"); sz > tmuxStreamCap {
		t.Fatalf("healed stream must be bounded at %d, got %d", tmuxStreamCap, sz)
	}
}

// Concurrent sweeps must serialize rotation + generation updates: N
// observed rotations across goroutines must bump the counter exactly
// N times (no lost read-modify-write bumps) and leave the file bound.
func TestRetentionConcurrentRotationsSerialized(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f09e")
	seedOwnedSession(t, "w", "f09e")

	const workers = 8
	var totalRotated atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writeOverCapBurst(t, "w", 'x')
			totalRotated.Add(int64(MaintainRetention()))
		}()
	}
	wg.Wait()

	// Drain anything still over cap from write-vs-sweep races.
	for i := 0; i < 50 && streamSizeOrFail(t, "w") > tmuxStreamCap; i++ {
		totalRotated.Add(int64(MaintainRetention()))
		time.Sleep(5 * time.Millisecond)
	}
	if sz := streamSizeOrFail(t, "w"); sz > tmuxStreamCap {
		t.Fatalf("stream must end bounded at %d, got %d", tmuxStreamCap, sz)
	}
	if got, want := int64(streamGeneration("w")), totalRotated.Load(); got != want {
		t.Fatalf("serialized rotations must bump gen exactly: gen=%d rotated=%d", got, want)
	}
	if _, known := streamGenerationKnown("w"); totalRotated.Load() > 0 && !known {
		t.Fatal("after successful rotations the generation must be known")
	}
}

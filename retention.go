package main

// retention.go: execution-independent stream retention ownership (P0-C,
// F-09).
//
// The tmux live-stream files (streams/<name>.log) are appended by
// writers the dashboard does not control (`tmux pipe-pane`, the
// headless worker-loop mirror). Bounding them used to be a side
// effect of observation (streamTail swept the fleet on every read),
// so a high-output worker nobody was watching grew its stream file
// without bound.
//
// Retention is owned here instead, autonomously:
//
//   - MaintainRetention bounds every over-cap stream in the fleet and
//     records a persisted per-session rotation generation. It needs
//     no dashboard — worker loops, CLI commands, and tests call it
//     directly.
//   - StartRetentionSweeper runs that sweep on a supervisor-owned
//     ticker, so unobserved sessions stay bounded with no reader at
//     all. Time overshoot is at most one sweep interval plus one
//     sweep pass; byte overshoot for an unobserved session is at
//     most what its writers append in that window, and the read
//     path backstops its own session synchronously (see
//     maintainSessionStream), so an observed session never
//     overshoots past one burst.
//   - Every rotation (detach, truncate, gap marker, generation bump,
//     re-attach) runs under a per-session file lock, so concurrent
//     sweeps, reads, and attaches serialize instead of losing
//     generation bumps or wiping each other's markers.
//
// The generation counter is what lets a (re)opened dashboard report
// discarded history: collectRefreshData surfaces the persisted
// counter through refreshData.logGen, and the model's existing gap
// check turns a jump into an explicit "rotated" status instead of a
// silent discontinuity. Missing or corrupt counter metadata reads as
// UNKNOWN history (streamGenerationKnown), never as proof of no
// loss: a rotation over unknown metadata starts a fresh epoch with
// its gap marker, and a failed rotation bumps nothing while leaving
// an observable failure marker (RetentionFailure).

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// retentionSweepDefaultInterval bounds the time overshoot of
// autonomous retention: an unobserved over-cap stream waits at most
// this long plus one sweep pass before it is rotated.
const retentionSweepDefaultInterval = 30 * time.Second

// streamGenPath persists the rotation count for one session's live
// stream: each retention rotation truncates the file and bumps this
// counter, so readers can tell history was discarded.
func streamGenPath(name string) string {
	return filepath.Join(stateDir(), "streams", name+".gen")
}

// retentionFailurePath marks an unhealed rotation failure for one
// session's live stream: a failed truncation writes its detail here
// so the loss of bounding is observable; the next successful
// rotation removes it.
func retentionFailurePath(name string) string {
	return filepath.Join(stateDir(), "streams", name+".rot_err")
}

// streamLockPath serializes one session's rotation, generation
// updates, and failure-marker updates across processes. flock is
// kernel-managed, so a crashed holder never leaves a stale lock.
func streamLockPath(name string) string {
	return filepath.Join(stateDir(), "streams", name+".lock")
}

// withStreamLock runs fn while holding the session's retention lock.
func withStreamLock(name string, fn func() error) error {
	_ = os.MkdirAll(filepath.Join(stateDir(), "streams"), 0o700)
	lf, err := os.OpenFile(streamLockPath(name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("stream lock for %q: %w", name, err)
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("stream lock for %q: %w", name, err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	return fn()
}

// streamGeneration returns the persisted rotation count for a
// session's stream. This is the best-effort numeric view used by
// read-model assembly: missing or corrupt metadata reads as zero.
// Zero here means "no KNOWN discarded history" — never proof of no
// loss. Code that must distinguish "clean" from "unknown" (rotation
// accounting, gap reporting) must use streamGenerationKnown.
func streamGeneration(name string) int {
	gen, _ := streamGenerationKnown(name)
	return gen
}

// streamGenerationKnown returns the persisted rotation count and
// whether it is trustworthy. Missing or corrupt metadata reports
// known=false: history is UNKNOWN, never proven intact.
func streamGenerationKnown(name string) (gen int, known bool) {
	data, err := os.ReadFile(streamGenPath(name))
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// bumpStreamGenerationLocked advances the rotation counter. Callers
// must hold the session's stream lock. Unknown (missing/corrupt)
// metadata starts a fresh epoch at 1: the rotation's gap marker in
// the stream file is the visible accounting for the unknown past,
// and 1 claims no continuity with it.
func bumpStreamGenerationLocked(name string) {
	gen, known := streamGenerationKnown(name)
	if !known {
		gen = 0
	}
	_ = os.MkdirAll(filepath.Dir(streamGenPath(name)), 0o700)
	_ = os.WriteFile(streamGenPath(name), []byte(strconv.Itoa(gen+1)+"\n"), 0o600)
}

// recordStreamRotation bumps the persisted rotation counter after a
// rotation discards history. Best-effort: retention must never fail
// because accounting did. Serialized under the session's stream
// lock so concurrent rotations cannot lose bumps.
func recordStreamRotation(name string) {
	_ = withStreamLock(name, func() error {
		bumpStreamGenerationLocked(name)
		return nil
	})
}

// noteRetentionFailureLocked records a failed truncation so the
// unhealed over-cap state is observable. Callers must hold the
// session's stream lock.
func noteRetentionFailureLocked(name, detail string) {
	_ = os.MkdirAll(filepath.Dir(retentionFailurePath(name)), 0o700)
	rec := fmt.Sprintf("%s: %s\n", time.Now().UTC().Format(time.RFC3339), detail)
	_ = os.WriteFile(retentionFailurePath(name), []byte(rec), 0o600)
}

// RetentionFailure reports an unhealed rotation failure for a
// session's stream. ok=false means no failure is recorded — not
// that none ever happened (a later success clears the marker).
func RetentionFailure(name string) (detail string, ok bool) {
	data, err := os.ReadFile(retentionFailurePath(name))
	if err != nil {
		return "", false
	}
	if d := strings.TrimSpace(string(data)); d != "" {
		return d, true
	}
	return "", false
}

// clearRetentionFailureLocked removes the failure marker after a
// successful rotation. Callers must hold the session's stream lock.
func clearRetentionFailureLocked(name string) {
	_ = os.Remove(retentionFailurePath(name))
}

// boundSessionStream rotates one owned session's stream when it is
// over cap, re-checking the size under the session's stream lock so
// concurrent sweeps, reads, and attaches cannot double-rotate or
// wipe each other's gap markers. It reports whether it rotated; a
// failure is recorded observably (RetentionFailure) and returned.
func boundSessionStream(name, target string) (bool, error) {
	var rotated bool
	err := withStreamLock(name, func() error {
		fi, err := os.Stat(streamPath(name))
		if err != nil || fi.Size() <= tmuxStreamCap {
			return nil
		}
		if rerr := rotateLocked(name, target); rerr != nil {
			return rerr
		}
		rotated = true
		return nil
	})
	return rotated, err
}

// maintainSessionStream is the read-path backstop: bound one
// session's own stream, best-effort. The autonomous sweeper owns
// the fleet; a read only ever pays for its own session (one stat
// when under cap, ownership resolution plus a locked rotation only
// when over), never a fleet-wide sweep. A failed bound never fails
// the read — the failure marker keeps it observable.
func maintainSessionStream(name string) {
	fi, err := os.Stat(streamPath(name))
	if err != nil || fi.Size() <= tmuxStreamCap {
		return
	}
	_, target, err := resolveOwnedTarget(loadRegistry(), name)
	if err != nil {
		return
	}
	_, _ = boundSessionStream(name, target)
}

// sweepStreamCaps rotates every over-cap stream file in the fleet.
// The common path is one stat per session — no reads, no forks —
// and only a file actually over cap pays for ownership resolution
// and a locked pipe re-attachment. Sessions that fail resolution
// are skipped: a dead or foreign session's file cannot grow, so
// there is nothing to cap. Failures are recorded observably per
// session (RetentionFailure) without failing the sweep.
func sweepStreamCaps(reg map[string]Session) int {
	rotated := 0
	for name := range reg {
		fi, err := os.Stat(streamPath(name))
		if err != nil || fi.Size() <= tmuxStreamCap {
			continue
		}
		_, target, err := resolveOwnedTarget(reg, name)
		if err != nil {
			continue
		}
		if ok, _ := boundSessionStream(name, target); ok {
			rotated++
		}
	}
	return rotated
}

// MaintainRetention is the execution-independent retention owner:
// bound every over-cap session stream in the fleet, with no
// dashboard required. Returns the number of streams rotated.
// Headless execution (worker loops, CLI, tests) calls this
// directly; the autonomous sweeper calls it on a ticker; the
// stream read path backstops its own session on every observation.
func MaintainRetention() int {
	return sweepStreamCaps(loadRegistry())
}

// StartRetentionSweeper supervises autonomous retention: it sweeps
// the fleet every interval until stop closes. It performs no sweep
// at startup, so the first rotation happens within one interval —
// that interval is the documented max time overshoot. Restarts are
// safe: all rotation state (stream file, generation, failure
// marker) is file-backed, so a new sweeper continues the persisted
// generation instead of resetting history. Concurrent sweepers are
// safe: per-session locks serialize rotations. The returned channel
// closes when the supervisor goroutine has exited, so callers can join
// it after closing stop; no sweep runs after done closes.
func StartRetentionSweeper(interval time.Duration, stop <-chan struct{}) (done <-chan struct{}) {
	if interval <= 0 {
		interval = retentionSweepDefaultInterval
	}
	d := make(chan struct{})
	go func() {
		defer close(d)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				MaintainRetention()
			}
		}
	}()
	return d
}

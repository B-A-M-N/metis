package main

// session_stream.go: live stream pipe management and event logging.
//
// Each running session carries an append-only stream file fed by
// `tmux pipe-pane`. This file owns the pipe lifecycle (attach,
// rotation, detach) plus the appendEvent audit trail written by the
// lifecycle and guidance paths. Readers only tail the files; tmux
// owns the write end. Extracted from session.go; behavior unchanged.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tmuxStreamCap bounds each session's live stream file. The pipe
// writer cannot cap itself, so collection rotates over-cap files
// (TD-066/TD-106): one animated worker must not exhaust disk or
// starve the fleet's observation.
const tmuxStreamCap = 1 << 20

// streamPath is the append-only file receiving live pane output via
// `tmux pipe-pane`. Readers only tail it; tmux owns the write end.
func streamPath(name string) string {
	return filepath.Join(stateDir(), "streams", name+".log")
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// ensureStreamPipe attaches a pipe-pane stream for an owned, running
// session. It is idempotent (`-o` only sets when unset) and refuses
// foreign sessions via ownership verification. Best-effort: pipe
// failures never fail session startup; capture falls back to polling.
func ensureStreamPipe(name string) {
	reg := loadRegistry()
	// Bind the pipe to the verified session ID so a replacement session
	// created mid-attach does not capture our stream.
	_, target, err := resolveOwnedTarget(reg, name)
	if err != nil {
		return
	}
	path := streamPath(name)
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	// An over-cap file found at attach pays the locked, re-checked
	// bound path (no double-rotate against a concurrent sweep);
	// when it is already within cap only the pipe attaches.
	if fi, err := os.Stat(path); err == nil && fi.Size() > tmuxStreamCap {
		_, _ = boundSessionStream(name, target)
		return
	}
	_, _ = tmux("pipe-pane", "-o", "-t", target, "cat >> "+shQuote(path))
}

// Stream retention itself is owned by retention.go (MaintainRetention);
// sweepStreamCaps lives there. rotateStream below stays here as the
// shared truncate-and-reattach primitive.

// rotateStream detaches the pipe, truncates the file, stamps a
// rotation marker (gap indication, TD-067), and re-attaches.
// Detach-then-truncate avoids sparse holes from a writer holding an
// old offset; O_APPEND on re-attach resumes at the new end. The
// persisted generation counter is bumped so a (re)opened dashboard
// can report the discarded history instead of showing a silent gap.
//
// The whole rotation runs serialized under the session's stream
// lock (see retention.go) with the failure recorded observably:
// a failed truncation bumps nothing and leaves a RetentionFailure
// marker for the next sweep to retry.
func rotateStream(name, target string) {
	_ = rotateStreamBounded(name, target)
}

// rotateStreamBounded is the locked rotation primitive behind
// rotateStream and boundSessionStream. Callers must hold the
// session's stream lock only when calling rotateLocked directly;
// this wrapper takes the lock itself.
func rotateStreamBounded(name, target string) error {
	return withStreamLock(name, func() error {
		return rotateLocked(name, target)
	})
}

// rotateLocked performs one rotation assuming the session's stream
// lock is held: detach, truncate, gap marker, generation bump, and
// re-attach. Any failure is recorded to the session's failure
// marker and returned; the generation is bumped only after the
// truncation and marker both succeed, so a failed rotation never
// fabricates discarded-history accounting.
func rotateLocked(name, target string) error {
	path := streamPath(name)
	if _, err := tmux("pipe-pane", "-t", target); err != nil {
		noteRetentionFailureLocked(name, "detach pipe: "+err.Error())
		return fmt.Errorf("detach stream pipe for %q: %w", name, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		noteRetentionFailureLocked(name, "open stream: "+err.Error())
		return fmt.Errorf("open stream for %q: %w", name, err)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		noteRetentionFailureLocked(name, "truncate stream: "+err.Error())
		return fmt.Errorf("truncate stream for %q: %w", name, err)
	}
	marker := fmt.Sprintf("--- stream rotated at %s: older output discarded (cap %d bytes) ---\n",
		time.Now().UTC().Format(time.RFC3339), tmuxStreamCap)
	if _, err := f.WriteString(marker); err != nil {
		_ = f.Close()
		noteRetentionFailureLocked(name, "write gap marker: "+err.Error())
		return fmt.Errorf("write gap marker for %q: %w", name, err)
	}
	if err := f.Close(); err != nil {
		noteRetentionFailureLocked(name, "close stream: "+err.Error())
		return fmt.Errorf("close stream for %q: %w", name, err)
	}
	bumpStreamGenerationLocked(name)
	clearRetentionFailureLocked(name)
	if _, err := tmux("pipe-pane", "-o", "-t", target, "cat >> "+shQuote(path)); err != nil {
		noteRetentionFailureLocked(name, "reattach pipe: "+err.Error())
		return fmt.Errorf("reattach stream pipe for %q: %w", name, err)
	}
	return nil
}

func disableStreamPipe(name string) {
	if !sessionRunning(name) {
		return
	}
	_, _ = tmux("pipe-pane", "-t", sessionTmuxTarget(name, ""))
}

// disableStreamPipeTarget detaches the stream pipe from the already-resolved
// session ID, so a replacement session reusing the name is never touched.
func disableStreamPipeTarget(target string) {
	_, _ = tmux("pipe-pane", "-t", target)
}

func appendEvent(workdir, kind, text string) {
	rec, _ := json.Marshal(map[string]string{
		"ts":   time.Now().UTC().Format(time.RFC3339),
		"type": kind,
		"text": text,
	})
	f, err := os.OpenFile(filepath.Join(workdir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(rec, '\n'))
}

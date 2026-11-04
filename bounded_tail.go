package main

// bounded_tail.go: size-bounded read primitives behind the refresh
// path (F-10). The old read path acquired unbounded bytes first and
// sliced later (os.ReadFile of whole logs, full tree walk + sort for
// non-git dirs, naked git spawns with no deadline). Every helper
// here bounds acquisition itself: reverse tail reads seek to the
// last budget bytes, directory sampling stops the walk at a cap
// before any sort, and git probes run under a deadline with
// byte-capped output.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// boundedTailMaxBytes caps one tail acquisition. Guidance and
	// events tails need a few KB; 64 KiB covers thousands of lines
	// while keeping a million-line log out of memory.
	boundedTailMaxBytes int64 = 64 << 10
	// boundedLogLinesMaxBytes caps PTY pane acquisitions on the
	// refresh path (500 display lines of wide output).
	boundedLogLinesMaxBytes int64 = 128 << 10
	// dirSampleCap caps directory enumeration for non-git workdirs.
	// The walk stops at the cap, then the capped sample sorts —
	// never walk-all then sort-all.
	dirSampleCap = 1024
	// gitProbeTimeout bounds repo detection; gitCmdTimeout bounds
	// one status/diff acquisition. A wedged git must degrade a pane,
	// never stall a refresh.
	gitProbeTimeout = 2 * time.Second
	gitCmdTimeout   = 3 * time.Second
	// gitOutputMaxBytes caps one git command's captured output;
	// status --short in a huge checkout must not flood the pane.
	gitOutputMaxBytes int64 = 64 << 10
	// gitDisplayLines caps rendered git pane lines.
	gitDisplayLines = 200
)

// errDirCapReached aborts a filepath.Walk once the sample cap hits.
var errDirCapReached = errors.New("dir sample cap reached")

// readTailLines returns up to the last n lines of path, reading at
// most maxBytes from the file's end. truncated reports head loss
// from either the byte budget or the line count, so callers can
// surface it instead of showing a silent gap. Rotation during the
// read (truncate/replace between stat and read) degrades to a
// short or re-read-from-start result, never an error or an
// unbounded read.
func readTailLines(path string, n int, maxBytes int64) ([]string, bool) {
	if n <= 0 {
		return nil, false
	}
	if maxBytes <= 0 {
		maxBytes = boundedTailMaxBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return nil, false
	}
	size := fi.Size()
	if size == 0 {
		return nil, false
	}
	start := size - maxBytes
	truncated := false
	if start > 0 {
		truncated = true
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			// File shrank (rotation) between stat and seek:
			// re-read bounded from the start.
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return nil, false
			}
			truncated = false
		}
	}
	// Bound the acquisition even if the file grew past maxBytes
	// while we read: the limit covers the segment plus slack for
	// concurrent appends, not the whole growth.
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+8192))
	if err != nil || len(data) == 0 {
		return nil, false
	}
	if truncated {
		// Drop the partial head line created by the mid-line seek.
		// A segment with no newline at all is one huge line whose
		// tail is real content, not a partial head to discard:
		// dropping it would misreport live output as empty.
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil, truncated
	}
	ls := strings.Split(text, "\n")
	if len(ls) > n {
		ls = ls[len(ls)-n:]
		truncated = true
	}
	out := make([]string, len(ls))
	copy(out, ls)
	return out, truncated
}

// boundedPTYLogLines tails a PTY session log without ever reading
// the whole file: at most boundedLogLinesMaxBytes come off disk,
// then at most lines display lines return. The alive/not-running
// sentinels match ptyLogLines so refresh panes keep their contract.
func boundedPTYLogLines(m ptyMeta, lines int) ([]string, bool) {
	ls, truncated := readTailLines(m.LogPath, lines, boundedLogLinesMaxBytes)
	if len(ls) == 0 {
		if ptyWorkerAlive(m) {
			return []string{"(no output yet)"}, false
		}
		return []string{"(session not running)"}, false
	}
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	if len(ls) == 0 {
		return []string{"(no output yet)"}, false
	}
	return ls, truncated
}

// markTruncated prepends an in-band marker so truncation rides the
// existing []string panes through refreshData without a struct or
// protocol change.
func markTruncated(ls []string, what string) []string {
	out := make([]string, 0, len(ls)+1)
	out = append(out, "... ("+what+") ...")
	return append(out, ls...)
}

// sampleDirFiles walks workdir without following symlinks and stops
// at cap entries, before any sorting. truncated reports the cap
// stopped the walk; incomplete reports skipped entries (permission,
// vanished, symlink loops). Both default false for a clean full walk.
func sampleDirFiles(workdir string, maxEntries int) (files []string, truncated bool, incomplete bool) {
	if maxEntries <= 0 {
		maxEntries = dirSampleCap
	}
	err := filepath.Walk(workdir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			incomplete = true
			if fi != nil && fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if fi == nil {
			incomplete = true
			return nil
		}
		// Never follow symlinks: a linked dir counts as one entry
		// and never descends (loop-safe).
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if fi.IsDir() {
			if p != workdir && len(files) >= maxEntries {
				return filepath.SkipDir
			}
			return nil
		}
		if len(files) >= maxEntries {
			return errDirCapReached
		}
		rel, rerr := filepath.Rel(workdir, p)
		if rerr != nil {
			incomplete = true
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if errors.Is(err, errDirCapReached) {
		truncated = true
	} else if err != nil {
		incomplete = true
	}
	return files, truncated, incomplete
}

// isGitRepo reports whether workdir is inside a git work tree, with a
// deadline: a hung git/config must answer false fast, never stall.
func isGitRepo(workdir string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = gitProbeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--git-dir")
	if err := cmd.Run(); err != nil {
		return false
	}
	return ctx.Err() == nil
}

// runGitCapped runs one git command under timeout, capturing at most
// maxBytes of stdout. ok=false means failed or timed out (partial
// output may still return). Streaming through a LimitReader bounds
// acquisition even when the repo would emit megabytes.
func runGitCapped(workdir string, timeout time.Duration, maxBytes int64, args ...string) (out []byte, ok bool) {
	if timeout <= 0 {
		timeout = gitCmdTimeout
	}
	if maxBytes <= 0 {
		maxBytes = gitOutputMaxBytes
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false
	}
	data, rerr := io.ReadAll(io.LimitReader(pipe, maxBytes+1))
	werr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		return data, false
	}
	if rerr != nil || werr != nil {
		return data, false
	}
	return data, true
}

package main

// bounded_reads_test.go: F-10 failing-first bounds. Every test pins
// acquisition bounds (bytes/time), not just final slicing: a
// million-line log, a large non-git tree, symlink/inaccessible dirs,
// rotation during read, and an end-to-end responsive refresh.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeNumberedLog writes count lines "line %d" and returns the path.
func writeNumberedLog(t *testing.T, dir string, count int) string {
	t.Helper()
	p := filepath.Join(dir, "big.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		fmt.Fprintf(f, "line %06d\n", i)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBoundedTailMillionLineLog(t *testing.T) {
	dir := t.TempDir()
	p := writeNumberedLog(t, dir, 1000000)
	start := time.Now()
	ls, truncated := readTailLines(p, 10, boundedTailMaxBytes)
	el := time.Since(start)
	if el > 5*time.Second {
		t.Fatalf("million-line tail took %v, must be bounded (<5s)", el)
	}
	if len(ls) != 10 {
		t.Fatalf("got %d lines, want 10", len(ls))
	}
	if ls[0] != "line 999990" || ls[9] != "line 999999" {
		t.Fatalf("wrong tail: first=%q last=%q", ls[0], ls[9])
	}
	if !truncated {
		t.Fatal("million-line log must report truncated")
	}
	// Compat wrapper agrees on content.
	if got := readTail(p, 10); len(got) != 10 || got[9] != "line 999999" {
		t.Fatalf("readTail compat mismatch: %v", got)
	}
}

func TestBoundedTailRotationDuringRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "rot.log")
	if err := os.WriteFile(p, []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			// Rotate: truncate and rewrite mid-read.
			_ = os.WriteFile(p, []byte(fmt.Sprintf("gen %d\nline a\nline b\nline c\n", i)), 0o600)
		}
	}()
	start := time.Now()
	for i := 0; i < 200; i++ {
		ls, _ := readTailLines(p, 3, 4096)
		if len(ls) > 3 {
			t.Fatalf("read %d: got %d lines, want <=3", i, len(ls))
		}
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("200 racing tails took %v, must stay responsive", el)
	}
}

func TestGitInfoLargeNonGitTree(t *testing.T) {
	dir := t.TempDir()
	// Thousands of files: old code walked+sorted all of them per refresh.
	for i := 0; i < 3000; i++ {
		sub := filepath.Join(dir, fmt.Sprintf("sub%02d", i%20))
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, fmt.Sprintf("f%05d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	out := gitInfo(dir)
	el := time.Since(start)
	if el > 10*time.Second {
		t.Fatalf("large-tree gitInfo took %v, must be bounded", el)
	}
	if len(out) == 0 || out[0] != "(not a git repo)" {
		t.Fatalf("want non-repo header, got %v", out)
	}
	if len(out) > 30 {
		t.Fatalf("pane uncapped: %d lines", len(out))
	}
	if !containsMarker(out) {
		t.Fatalf("large tree must carry a truncation marker, got %v", out)
	}
}

func TestGitInfoSymlinkAndInaccessible(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "ok.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlink loop: link -> parent, plus dangling link.
	_ = os.Symlink(dir, filepath.Join(real, "loop"))
	_ = os.Symlink(filepath.Join(dir, "nope"), filepath.Join(dir, "dangling"))
	// Unreadable dir: must be skipped, not fatal. Best-effort on
	// platforms/users where chmod cannot deny (root): still must not hang.
	denied := filepath.Join(dir, "denied")
	if err := os.MkdirAll(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "s.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(denied, 0o000)
	defer os.Chmod(denied, 0o755)
	done := make(chan []string, 1)
	go func() { done <- gitInfo(dir) }()
	select {
	case out := <-done:
		if len(out) == 0 || out[0] != "(not a git repo)" {
			t.Fatalf("want non-repo header, got %v", out)
		}
		joined := strings.Join(out, "\n")
		if strings.Contains(joined, "loop") {
			t.Fatalf("symlink must not be enumerated as content: %v", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gitInfo hung on symlink/inaccessible tree")
	}
	// Direct sampler contract: loop-safe, bounded, reports status.
	files, _, _ := sampleDirFiles(dir, 16)
	if len(files) > 16 {
		t.Fatalf("sampler exceeded cap: %d", len(files))
	}
	for _, f := range files {
		if strings.Contains(f, "loop") {
			t.Fatalf("sampler followed symlink: %v", files)
		}
	}
}

func TestLiveContentSnapshotBoundedRefresh(t *testing.T) {
	dir := t.TempDir()
	log := writeNumberedLog(t, dir, 200000) // ~2MB of pane output
	evPath := filepath.Join(dir, "events.jsonl")
	ef, err := os.Create(evPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(ef, "{\"n\":%d}\n", i)
	}
	_ = ef.Close()
	// Non-git workdir with filler so gitInfo takes the capped path.
	for i := 0; i < 100; i++ {
		_ = os.WriteFile(filepath.Join(dir, fmt.Sprintf("w%03d.txt", i)), []byte("x"), 0o600)
	}
	mt := ptyMeta{Name: "s", Workdir: dir, LogPath: log}
	start := time.Now()
	snap := liveContentSnapshot(nil, map[string]ptyMeta{"pty:s": mt}, "pty:s", true)
	el := time.Since(start)
	if el > 10*time.Second {
		t.Fatalf("refresh snapshot took %v over huge log+tree, must be bounded", el)
	}
	if !snap.found {
		t.Fatal("snapshot not found")
	}
	if len(snap.live) > 501 {
		t.Fatalf("live pane uncapped: %d lines", len(snap.live))
	}
	if snap.live[0] != "... (truncated: showing last 500 lines) ..." {
		t.Fatalf("live pane must carry truncation head marker, got %q", snap.live[0])
	}
	if len(snap.events) > 9 {
		t.Fatalf("events pane uncapped: %d lines", len(snap.events))
	}
	if snap.events[0] != "... (truncated: showing last 8 lines) ..." {
		t.Fatalf("events pane must carry truncation head marker, got %q", snap.events[0])
	}
	if !snap.hasGit || len(snap.git) == 0 || snap.git[0] != "(not a git repo)" {
		t.Fatalf("git pane must propagate non-repo status, got %v", snap.git)
	}
	// Markers must survive assembly into refreshData.
	rd := assembleRefresh(refreshSnapshots{sessionsOk: true, content: snap}, 1, "pty:s")
	if len(rd.live) == 0 || rd.live[0] != snap.live[0] {
		t.Fatal("refreshData dropped live truncation marker")
	}
	if len(rd.events) == 0 || rd.events[0] != snap.events[0] {
		t.Fatal("refreshData dropped events truncation marker")
	}
	if !rd.hasGit || len(rd.git) == 0 {
		t.Fatal("refreshData dropped git pane")
	}
}

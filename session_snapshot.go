package main

// session_snapshot.go: read-only session inspection. Stream tails,
// bounded byte segments, live output capture, git status, and event
// tail reads used by headless snapshots and TUI projection. No
// writes, no tmux control, no registry mutation. Extracted from
// session.go; behavior unchanged.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// orDash renders an optional display field, keeping snapshot and list
// output stable when a label is unset.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func sortedNames(reg map[string]Session) []string {
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func snapshotText(name string) string {
	reg := loadRegistry()
	var names []string
	if name != "" {
		names = []string{name}
	} else {
		names = sortedNames(reg)
	}
	if len(names) == 0 {
		return "(no sessions in registry)"
	}
	var b strings.Builder
	for _, n := range names {
		entry, ok := reg[n]
		if !ok {
			fmt.Fprintf(&b, "== %s: not in registry ==\n", n)
			continue
		}
		fmt.Fprintf(&b, "== session: %s  tmux=%s  running=%v ==\n", n, entry.Tmux, sessionRunning(n))
		fmt.Fprintf(&b, "workdir: %s\ncommand: %s\n", entry.Workdir, entry.Command)
		b.WriteString("-- live output (tail 40) --\n")
		live := captureOutput(n, 300)
		if len(live) > 40 {
			live = live[len(live)-40:]
		}
		for _, l := range live {
			b.WriteString(l + "\n")
		}
		b.WriteString("-- git --\n")
		for _, l := range gitInfo(entry.Workdir) {
			b.WriteString(l + "\n")
		}
		b.WriteString("-- guidance (tail 10) --\n")
		guid, gcut := readTailTruncated(filepath.Join(entry.Workdir, "guidance.md"), 10)
		if gcut && len(guid) > 0 {
			guid = markTruncated(guid, "truncated: showing last 10 lines")
		}
		for _, l := range guid {
			b.WriteString(l + "\n")
		}
		b.WriteString("-- events (tail 10) --\n")
		ev, ecut := readTailTruncated(filepath.Join(entry.Workdir, "events.jsonl"), 10)
		if ecut && len(ev) > 0 {
			ev = markTruncated(ev, "truncated: showing last 10 lines")
		}
		if len(ev) == 0 {
			b.WriteString("(none)\n")
		} else {
			for _, l := range ev {
				b.WriteString(l + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// streamTail reads the last lines from the live stream file.
// ok=false means no usable stream; caller should fall back. The read
// is bounded to the stream cap from the file's end, so one
// high-output worker cannot force an unbounded read per refresh.
//
// Retention is owned by the autonomous sweeper (retention.go), not
// by the dashboard refresh loop: fleet bounding needs no dashboard
// and no reader. The read path only backstops its own session here
// (maintainSessionStream: one stat when within cap, a locked bound
// only when over), so UI pane reads and headless snapshot reads
// stay bounded alike without sweeping the fleet on every read.
// Best-effort: a failed backstop never fails the read — the failure
// marker (RetentionFailure) keeps it observable.
func streamTail(name string, lines int) ([]string, bool) {
	maintainSessionStream(name)
	data, cut := tailSegment(streamPath(name), tmuxStreamCap)
	if len(data) == 0 {
		return nil, false
	}
	ls := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if cut && len(ls) > 0 {
		ls = ls[1:] // first segment line is a partial head: drop it
	}
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	if len(ls) == 0 {
		return nil, false
	}
	if len(ls) > lines {
		ls = ls[len(ls)-lines:]
	}
	return ls, true
}

// tailSegment returns up to the last maxBytes of path. cut reports
// whether the head was truncated (the first returned line is then
// partial). An empty or unreadable file yields nil.
func tailSegment(path string, maxBytes int64) ([]byte, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return nil, false
	}
	if fi.Size() <= maxBytes {
		data, err := io.ReadAll(f)
		if err != nil || len(data) == 0 {
			return nil, false
		}
		return data, false
	}
	if _, err := f.Seek(fi.Size()-maxBytes, io.SeekStart); err != nil {
		return nil, false
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

func captureOutput(name string, lines int) []string {
	if ls, ok := streamTail(name, lines); ok {
		return ls
	}
	out, err := tmux("capture-pane", "-p", "-t", tmuxName(name), "-S", fmt.Sprintf("-%d", lines))
	if err != nil {
		return []string{"(session not running)"}
	}
	ls := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// Drop trailing blanks.
	for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
		ls = ls[:len(ls)-1]
	}
	if len(ls) == 0 {
		return []string{"(no output yet)"}
	}
	return ls
}

func gitInfo(workdir string) []string {
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		return []string{"(workdir missing: " + workdir + ")"}
	}
	// Repo detection runs under a deadline: a wedged git (or a huge
	// .git/config lookup) answers false fast instead of stalling the
	// refresh. The old .git stat + unbounded walk is gone.
	if !isGitRepo(workdir, gitProbeTimeout) {
		// Not a repo: capped sample instead of a full walk+sort.
		// Enumeration stops at dirSampleCap before sorting, skips
		// symlinks (loop-safe) and unreadable entries, so a huge or
		// hostile tree costs a bounded scan per refresh.
		files, truncated, incomplete := sampleDirFiles(workdir, dirSampleCap)
		sort.Strings(files)
		out := []string{"(not a git repo)", "files:"}
		const shown = 15
		if len(files) > shown {
			files = files[:shown]
			truncated = true
		}
		for _, f := range files {
			out = append(out, "  "+f)
		}
		if len(out) == 2 {
			out = append(out, "  (empty)")
		}
		if truncated {
			out = append(out, "  ... (truncated: capped sample) ...")
		}
		if incomplete {
			out = append(out, "  ... (incomplete: some entries unreadable) ...")
		}
		return out
	}
	var out []string
	r, ok := runGitCapped(workdir, gitCmdTimeout, gitOutputMaxBytes, "status", "--short")
	if ok {
		out = append(out, "status --short:")
		ls := strings.Split(strings.TrimRight(string(r), "\n"), "\n")
		if len(ls) == 1 && ls[0] == "" {
			out = append(out, "  (clean)")
		} else {
			if len(ls) > gitDisplayLines {
				ls = ls[:gitDisplayLines]
				for _, l := range ls {
					out = append(out, "  "+l)
				}
				out = append(out, "  ... (truncated: status capped) ...")
			} else {
				for _, l := range ls {
					out = append(out, "  "+l)
				}
			}
		}
		if int64(len(r)) > gitOutputMaxBytes && !containsMarker(out) {
			out = append(out, "  ... (truncated: status capped) ...")
		}
	} else if len(r) > 0 {
		out = append(out, "status --short:", "  ... (incomplete: git output limited) ...")
	}
	if r, ok := runGitCapped(workdir, gitCmdTimeout, gitOutputMaxBytes, "diff", "--stat"); ok && strings.TrimSpace(string(r)) != "" {
		out = append(out, "diff --stat:")
		ls := strings.Split(strings.TrimRight(string(r), "\n"), "\n")
		if len(ls) > gitDisplayLines {
			ls = ls[:gitDisplayLines]
			for _, l := range ls {
				out = append(out, "  "+l)
			}
			out = append(out, "  ... (truncated: diff capped) ...")
		} else {
			for _, l := range ls {
				out = append(out, "  "+l)
			}
			if int64(len(r)) > gitOutputMaxBytes {
				out = append(out, "  ... (truncated: diff capped) ...")
			}
		}
	}
	if len(out) == 0 {
		return []string{"(git: no output)"}
	}
	return out
}

// containsMarker reports whether a truncation/incomplete marker is
// already present, so byte-cap and line-cap paths never double-mark.
func containsMarker(out []string) bool {
	for _, l := range out {
		if strings.Contains(l, "(truncated:") {
			return true
		}
	}
	return false
}

// readTail returns up to the last n lines of path. Bounded: at most
// boundedTailMaxBytes come off disk (reverse tail seek), so a
// million-line log costs a small fixed read per refresh. The
// truncation flag is dropped for the compat signature; callers that
// surface it use readTailTruncated.
func readTail(path string, n int) []string {
	ls, _ := readTailLines(path, n, boundedTailMaxBytes)
	return ls
}

// readTailTruncated is readTail plus its truncation flag: truncated
// reports head loss from the byte budget or the line count, letting
// refresh panes mark "... (truncated ...)" instead of a silent gap.
func readTailTruncated(path string, n int) ([]string, bool) {
	return readTailLines(path, n, boundedTailMaxBytes)
}

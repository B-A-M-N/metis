package main

// pty_fifo.go: FIFO control-channel framing.
//
// One command per line, each frame written atomically (single
// PIPE_BUF write(2)):
//
//	IN <id> <owner> <seq/total> <base64>  write bytes to the PTY (lease-gated)
//	RS <owner> <rows> <cols>              resize the PTY
//	EX <code>                             worker exited (sent by the supervisor itself)
//	ST <owner>                            stop: terminate worker, mark exited, supervisor exits
//
// Multi-frame IN commands reassemble by ID; partial assemblies are
// bounded in bytes, count, and age — oversize input is refused with
// an error receipt naming the bound, never silently buffered.
// Extracted from terminal.go (ptySupervise); behavior unchanged.

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/creack/pty"
)

// Bounds (P0-E): the client caps text at 256KB, but a raw FIFO
// writer bypasses that check, and partial assemblies previously
// lingered forever. The supervisor enforces the same 256KB byte
// bound per assembly, a cap on concurrent partial assemblies, and
// expiry of stale partials — oversize input is refused with an
// error receipt naming the bound, never silently buffered.
const maxReassemblyBytes = 256 * 1024
const maxPendingReassemblies = 16
const maxReassemblyAge = 60 * time.Second

type reassembly struct {
	total     int
	parts     [][]byte
	got       int
	bytes     int
	firstSeen time.Time
}

// serveFrames serves the FIFO control channel until a terminal frame
// (ST/EX) or EOF. It reports true when the terminal state was already
// recorded by an ST/EX frame, false on unexpected FIFO close (the
// caller then records the worker-decided fate).
func (s *supervisor) serveFrames() bool {
	auth := frameAuth{db: s.db, session: s.name, pid: s.pid, start: s.selfStart}
	pending := map[string]*reassembly{}
	sweepStaleReassembly := func() {
		now := time.Now()
		for id, ra := range pending {
			if now.Sub(ra.firstSeen) > maxReassemblyAge {
				delete(pending, id)
				ptyRecordReceipt(id, s.name, "", 0, "error", "partial command expired without completing")
			}
		}
	}
	sc := bufio.NewScanner(s.fr)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "IN "):
			rest := strings.TrimPrefix(line, "IN ")
			fields := strings.SplitN(rest, " ", 4)
			if len(fields) != 4 {
				// Legacy unqualified frames carry no command ID and
				// no owner: explicitly disabled (TD-020). One rolling
				// receipt records the refusal; nothing reaches the PTY.
				ptyRecordReceipt("legacy-refused", s.name, "", 0, "error", "unqualified legacy input frame refused")
				break
			}
			id, owner, frac, payload := fields[0], fields[1], fields[2], fields[3]
			if reason, ok := auth.check(owner); !ok {
				ptyRecordReceipt(id, s.name, owner, 0, "error", reason)
				break
			}
			slash := strings.Index(frac, "/")
			seq, total := 0, 0
			if slash > 0 {
				seq, _ = strconv.Atoi(frac[:slash])
				total, _ = strconv.Atoi(frac[slash+1:])
			}
			raw, derr := base64.StdEncoding.DecodeString(payload)
			if derr != nil || seq < 1 || total < 1 || total > 128 {
				ptyRecordReceipt(id, s.name, owner, 0, "error", "malformed frame")
				break
			}
			ra, ok := pending[id]
			if !ok {
				sweepStaleReassembly()
				if len(pending) >= maxPendingReassemblies {
					ptyRecordReceipt(id, s.name, owner, 0, "error",
						fmt.Sprintf("too many partial commands (cap %d); refusing", maxPendingReassemblies))
					break
				}
				ra = &reassembly{total: total, parts: make([][]byte, total), firstSeen: time.Now()}
				pending[id] = ra
			}
			if ra.total != total || seq > total || ra.parts[seq-1] != nil {
				ptyRecordReceipt(id, s.name, owner, 0, "error", "inconsistent framing")
				delete(pending, id)
				break
			}
			if ra.bytes+len(raw) > maxReassemblyBytes {
				ptyRecordReceipt(id, s.name, owner, 0, "error",
					fmt.Sprintf("command exceeds %d-byte reassembly bound; refusing oversize input", maxReassemblyBytes))
				delete(pending, id)
				break
			}
			ra.parts[seq-1] = raw
			ra.got++
			ra.bytes += len(raw)
			if ra.got == ra.total {
				delete(pending, id)
				full := make([]byte, 0, ra.bytes)
				for _, p := range ra.parts {
					full = append(full, p...)
				}
				if n, werr := s.pf.Write(full); werr != nil || n != len(full) {
					detail := "short write"
					if werr != nil {
						detail = werr.Error()
					}
					ptyRecordReceipt(id, s.name, owner, n, "error", detail)
				} else {
					ptyRecordReceipt(id, s.name, owner, n, "written_to_pty", "")
				}
			}
		case strings.HasPrefix(line, "RS "):
			parts := strings.Fields(line)
			if len(parts) != 4 {
				break
			}
			if reason, ok := auth.check(parts[1]); !ok {
				_ = reason
				break
			}
			r, rerr := strconv.Atoi(parts[2])
			c, cerr := strconv.Atoi(parts[3])
			if rerr == nil && cerr == nil && r > 0 && c > 0 {
				_ = pty.Setsize(s.pf, &pty.Winsize{Rows: uint16(r), Cols: uint16(c)})
			}
		case strings.HasPrefix(line, "EX "):
			code, _ := strconv.Atoi(strings.TrimPrefix(line, "EX "))
			s.finish("exited", code)
			return true
		case strings.HasPrefix(line, "ST"):
			parts := strings.Fields(line)
			if len(parts) != 2 {
				break
			}
			if reason, ok := auth.check(parts[1]); !ok {
				_ = reason
				break
			}
			// Owned stop: kill the worker's process group, not just
			// the sh -c child, so background descendants die with
			// it. Identity is re-verified inside. A refusal caused
			// by pid reuse must NOT fall back to a direct kill
			// (that would signal a stranger); fall back only while
			// the pid is still ours or already gone.
			if kerr := killWorkerGroup(s.workerPid, s.workerStart); kerr != nil {
				if st, serr := procStartTime(s.workerPid); serr != nil || st == s.workerStart {
					_ = s.cmd.Process.Kill()
				}
			}
			_, _ = s.cmd.Process.Wait()
			s.finish("exited", -1)
			return true
		}
	}
	return false
}

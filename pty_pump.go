package main

// pty_pump.go: supervisor output pumping, heartbeats, and exit
// reporting.
//
// The output pump streams PTY master bytes to the append-only session
// log under a size cap; the heartbeat ticker owns liveness so pump
// writes never contend with CLI writers; the exit watcher translates
// worker termination into an EX control frame. Extracted from
// terminal.go (ptySupervise); behavior unchanged.

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// startOutputPump streams PTY master bytes to the append-only session
// log with a size cap; rotation bumps the generation so readers can
// detect discarded history. pumpDone closes when the pump exits
// (worker EOF or read error).
func (s *supervisor) startOutputPump() {
	s.pumpDone = make(chan struct{})
	go func() {
		defer close(s.pumpDone)
		var written int64
		if fi, err := os.Stat(s.meta.LogPath); err == nil {
			written = fi.Size()
		}
		buf := make([]byte, 32*1024)
		for {
			n, rerr := s.pf.Read(buf)
			if n > 0 {
				if written+int64(n) > ptyLogCap {
					_ = s.logf.Truncate(0)
					_, _ = s.logf.Seek(0, 0)
					written = 0
					s.fenced(`UPDATE pty_sessions SET generation = generation + 1`)
				}
				if _, werr := s.logf.Write(buf[:n]); werr == nil {
					written += int64(n)
				}
				// No heartbeat here: the 2s ticker owns liveness.
				// Heartbeating per chunk contends with CLI writers.
			}
			if rerr != nil {
				return
			}
		}
	}()
}

// startHeartbeat refreshes supervisor liveness for idle workers until
// the supervisor closes.
func (s *supervisor) startHeartbeat() {
	s.hbDone = make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.hbDone:
				return
			case <-t.C:
				ptyHeartbeat(s.db, s.name, s.pid, s.selfStart)
			}
		}
	}()
}

// watchWorkerExit translates worker termination into an EX control
// frame on the supervisor's own FIFO.
func (s *supervisor) watchWorkerExit() {
	go func() {
		err := s.cmd.Wait()
		code := 0
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else {
				code = -1
			}
		}
		f, ferr := os.OpenFile(s.fifo, os.O_WRONLY, 0)
		if ferr != nil {
			return
		}
		defer f.Close()
		fmt.Fprintf(f, "EX %d\n", code)
	}()
}

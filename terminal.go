package main

// terminal.go: managed PTY supervisor.
//
// Each PTY session is owned by a detached supervisor process that
// holds the PTY master, pumps output to a log file, and serves a
// fifo control channel. Closing the TUI never kills workers: a new
// CLI invocation reattaches through the database row plus
// pid+starttime verification (pid reuse can never steer control to
// an unrelated process).
//
// Control only ever touches sessions this tool created (a live
// supervisor row must exist). Arbitrary PIDs are never signaled
// except the verified worker pid recorded at spawn.
//
// ptySupervise below is a short composition over cohesive components:
// process identity (pty_proc.go: spawn), supervision lifecycle
// (pty_supervise.go: claim, control channel, running mark, terminal
// state), output pumping (pty_pump.go), FIFO framing (pty_fifo.go),
// and lease/control authorization (pty_lease.go: frameAuth).
// Supervisor generation fencing and single-writer lease checks are
// preserved exactly inside those components.

import (
	"time"
)

const (
	ptyHeartbeatTTL = 10 * time.Second
	ptyInputTTL     = 30 * time.Second
	ptyLogCap       = 5 << 20 // 5MB per session log; rotation bumps generation
)

// ptySupervise is the detached supervisor body: it owns the PTY
// master, pumps output to the log, and serves the fifo until the
// worker exits or a stop is ordered. It also runs in-process under
// test via goroutine.
func ptySupervise(name string) error {
	s, err := newSupervisor(name)
	if err != nil {
		return err
	}
	defer s.close()
	if err := s.spawn(); err != nil {
		return err
	}
	if err := s.claim(); err != nil {
		return err
	}
	if err := s.openControl(); err != nil {
		return err
	}
	if err := s.markRunning(); err != nil {
		return err
	}
	return s.serve()
}

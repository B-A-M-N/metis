package main

// pty_supervise.go: supervisor lifecycle — setup, claim, control
// channel, running mark, and terminal-state recording.
//
// One supervisor owns one PTY session row through generation fencing:
// every row update matches (supervisor_pid, supervisor_start), so a
// delayed supervisor from an earlier startup matches nothing once a
// replacement takes over. Claim, control-channel setup, and the
// running mark each compensate a lost startup race by killing the
// worker this supervisor spawned, never a replacement's. Extracted
// from terminal.go (ptySupervise); behavior unchanged.

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// supervisor is one detached PTY supervisor: the worker's PTY master,
// its append-only output log, the fenced database row, and the FIFO
// control channel. pid is this process; selfStart is its /proc
// starttime — together the fencing identity.
type supervisor struct {
	name        string
	meta        ptyMeta
	db          *sql.DB
	logf        *os.File
	pf          *os.File
	cmd         *exec.Cmd
	workerPid   int
	workerStart uint64
	pid         int
	selfStart   uint64
	fifo        string
	fr          *os.File
	pumpDone    chan struct{}
	hbDone      chan struct{}
}

// newSupervisor loads the session record and opens the task database
// and the append-only output log. No process is started yet.
func newSupervisor(name string) (*supervisor, error) {
	m, err := ptyGet(name)
	if err != nil {
		return nil, err
	}
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(m.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &supervisor{name: name, meta: m, db: db, logf: logf, pid: os.Getpid()}, nil
}

// close releases supervisor resources in the reverse order the
// original ptySupervise deferred them: heartbeat signal, control
// channel, PTY master, log, database.
func (s *supervisor) close() {
	if s.hbDone != nil {
		close(s.hbDone)
		s.hbDone = nil
	}
	if s.fr != nil {
		_ = s.fr.Close()
		s.fr = nil
	}
	if s.pf != nil {
		_ = s.pf.Close()
		s.pf = nil
	}
	if s.logf != nil {
		_ = s.logf.Close()
		s.logf = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

// fenced executes a supervisor-owned row update: a delayed
// supervisor from an earlier startup matches nothing after a
// replacement takes over, so the new owner is left untouched.
func (s *supervisor) fenced(q string, args ...any) {
	full := append(args, s.name, s.pid, s.selfStart, s.selfStart)
	_, _ = s.db.Exec(q+` WHERE name = ? AND supervisor_pid = ?
		AND (supervisor_start = ? OR supervisor_start = 0)`, full...)
}

// killWorker signals the owned worker's process group. Identity is
// re-verified inside; a refusal (possible pid reuse) kills nothing.
func (s *supervisor) killWorker() {
	_ = killWorkerGroup(s.workerPid, s.workerStart)
}

// abortUnclaimed kills the spawned worker and abandons a row that was
// never claimed, so a replacement startup can recover the name. A
// row already taken by a winner is never abandoned — only the worker
// this supervisor spawned is killed.
func (s *supervisor) abortUnclaimed() {
	s.killWorker()
	ptyAbandon(s.name, s.meta.Owner)
}

// spawn starts the worker under a fresh PTY master. Any failure
// abandons the never-claimed row for a replacement startup.
func (s *supervisor) spawn() error {
	pf, cmd, workerPid, workerStart, selfStart, err := startWorkerPTY(s.meta)
	if err != nil {
		ptyAbandon(s.name, s.meta.Owner)
		return err
	}
	s.pf, s.cmd = pf, cmd
	s.workerPid, s.workerStart = workerPid, workerStart
	s.selfStart = selfStart
	return nil
}

// claim takes ownership of the session row while it is still
// starting: the first supervisor to flip the row owns it. The
// read-back below evicts a loser when two supervisors race the same
// row.
//
// The claim keeps status 'starting': running must mean the
// supervisor is actually serving its control channel, not just
// that a row was claimed. The flip to running happens in
// markRunning after the FIFO opens; ptyStart's wait loop only
// accepts running, so a client can never send input to a supervisor
// whose FIFO does not exist yet.
func (s *supervisor) claim() error {
	res, err := s.db.Exec(`UPDATE pty_sessions SET pts = ?, worker_pid = ?, worker_start = ?,
		supervisor_pid = ?, supervisor_start = ?, heartbeat = ?
		WHERE name = ? AND status = 'starting'`,
		s.pf.Name(), s.workerPid, s.workerStart, s.pid, s.selfStart, nowUTC(), s.name)
	if err != nil {
		s.abortUnclaimed()
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		s.killWorker()
		return fmt.Errorf("supervisor lost the startup race for %q", s.name)
	}
	if cur, gerr := ptyGet(s.name); gerr != nil ||
		cur.SupervisorPid != s.pid || cur.SupervisorStart != s.selfStart {
		s.killWorker()
		return fmt.Errorf("supervisor lost the startup race for %q", s.name)
	}
	return nil
}

// openControl creates and opens the FIFO control channel.
func (s *supervisor) openControl() error {
	s.fifo = ptyFifoPath(s.name)
	_ = os.Remove(s.fifo)
	if err := syscall.Mkfifo(s.fifo, 0o600); err != nil {
		s.abortUnclaimed()
		return err
	}
	fr, err := os.OpenFile(s.fifo, os.O_RDWR, 0)
	if err != nil {
		s.abortUnclaimed()
		return err
	}
	s.fr = fr
	return nil
}

// markRunning reports running only now that the control channel is
// serving. A superseded row (match 0) means a replacement took over
// mid-startup; this supervisor exits rather than serve a session it
// lost.
func (s *supervisor) markRunning() error {
	res, err := s.db.Exec(`UPDATE pty_sessions SET status = 'running', heartbeat = ?
		WHERE name = ? AND supervisor_pid = ? AND supervisor_start = ? AND status = 'starting'`,
		nowUTC(), s.name, s.pid, s.selfStart)
	if err != nil {
		s.abortUnclaimed()
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		s.killWorker()
		return fmt.Errorf("supervisor lost the startup race for %q", s.name)
	}
	return nil
}

// finish records the terminal state, but only while this
// supervisor still owns the row: after a replacement takes over,
// the fenced update matches nothing and the new owner is
// left untouched.
func (s *supervisor) finish(status string, code int) {
	select {
	case <-s.pumpDone:
	case <-time.After(3 * time.Second):
	}
	s.fenced(`UPDATE pty_sessions SET status = ?, exit_code = ?, heartbeat = ?`,
		status, code, nowUTC())
}

// serve starts the supervision loops and serves the control channel
// until a terminal frame (ST/EX) or FIFO close.
func (s *supervisor) serve() error {
	s.startOutputPump()
	s.startHeartbeat()
	s.watchWorkerExit()
	if s.serveFrames() {
		// ST/EX path already recorded the terminal state.
		return nil
	}
	// Fifo closed unexpectedly: worker decides fate. Group kill so
	// descendants do not outlive a supervisor that lost its channel.
	if s.cmd.ProcessState == nil {
		s.killWorker()
	}
	code := -1
	if ws := s.cmd.ProcessState; ws != nil {
		code = ws.ExitCode()
	}
	s.finish("gone", code)
	return nil
}

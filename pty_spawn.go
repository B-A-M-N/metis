package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// pty_spawn.go: extracted from terminal.go; behavior unchanged.

func ptyStart(name, workdir, command, harness, model string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("name must match [A-Za-z0-9_-]{1,64}")
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("workdir does not exist: %s", abs)
	}
	if err := os.MkdirAll(filepath.Join(stateDir(), "pty"), 0o700); err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return err
	}
	owner := fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	var status string
	err = db.QueryRow(`SELECT status FROM pty_sessions WHERE name = ?`, name).Scan(&status)
	switch {
	case err == sql.ErrNoRows:
		// Concurrent initial starts serialize on the primary key:
		// exactly one INSERT wins.
		_, err = db.Exec(`INSERT INTO pty_sessions
			(name, workdir, command, logpath, owner, status, heartbeat, harness, model)
			VALUES (?, ?, ?, ?, ?, 'starting', ?, ?, ?)`,
			name, abs, command, ptyLogPath(name), owner, nowUTC(), harness, model)
		if err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		// A previous startup left this row behind. Recover it only
		// when no supervisor is still authoritative: a live
		// supervisor, or any settled (non-starting) status, refuses,
		// so a delayed original starter can never overwrite a
		// replacement. The generation predicate serializes concurrent
		// recoverers (each sees the latest generation exactly once);
		// the owner check in the wait loop below then reports success
		// to exactly one of them.
		meta, gerr := ptyGet(name)
		if gerr != nil {
			return gerr
		}
		if ptySupervisorAlive(meta) {
			return fmt.Errorf("PTY session %q already recorded (status %s)", name, meta.Status)
		}
		if meta.Status != "starting" {
			return fmt.Errorf("PTY session %q already recorded (status %s)", name, meta.Status)
		}
		res, uerr := db.Exec(`UPDATE pty_sessions SET owner = ?, status = 'starting',
			heartbeat = ?, supervisor_pid = 0, supervisor_start = 0,
			worker_pid = 0, worker_start = 0, generation = generation + 1
			WHERE name = ? AND status = 'starting' AND generation = ?`,
			owner, nowUTC(), name, meta.Generation)
		if uerr != nil {
			return uerr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("PTY session %q is being recovered by another controller", name)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "pty-supervise", "--name", name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	devnull, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("supervisor spawn failed: %w", err)
	}
	// Wait for the first heartbeat: proves the supervisor owns a PTY.
	// A fast worker may already be reaped (terminal status) before we
	// first observe it; that is success, not a startup failure.
	// The owner check settles serial takeovers: each recoverer bumps
	// the generation and spawns, but only the controller whose token
	// still owns the row when it comes up reports success. The rest
	// learn another recovery superseded them instead of silently
	// sharing a session they do not own (TD-029).
	deadline := time.Now().Add(10 * time.Second)
	for {
		m, err := ptyGet(name)
		if err == nil {
			if m.Status == "running" && ptySupervisorAlive(m) {
				if m.Owner != owner {
					return fmt.Errorf("PTY session %q recovered by another controller; lost the startup race", name)
				}
				return nil
			}
			if m.Status == "exited" || m.Status == "gone" {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("supervisor for %q never came up", name)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ptyFifoWrite sends one control line to a live supervisor.
func ptyStop(name string) error {
	m, err := ptyGet(name)
	if err != nil {
		return err
	}
	ptyClearLease(name)
	owner := controllerID()
	if err := ptyAcquireLease(name, owner); err != nil {
		return fmt.Errorf("stop refused: %w", err)
	}
	sameIdentity := func(a, b ptyMeta) bool {
		return a.SupervisorPid == b.SupervisorPid && a.SupervisorStart == b.SupervisorStart
	}
	if ptySupervisorAlive(m) {
		fresh, ferr := ptyGet(name)
		if ferr != nil {
			return ferr
		}
		if !sameIdentity(fresh, m) || !ptySupervisorAlive(fresh) {
			return fmt.Errorf("session %q changed during stop; refusing", name)
		}
		if err := ptyFifoWrite(name, "ST "+owner); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			cur, err := ptyGet(name)
			if err != nil {
				return err
			}
			if !sameIdentity(cur, m) {
				return fmt.Errorf("session %q changed owner during stop; refusing", name)
			}
			if cur.Status == "exited" || cur.Status == "gone" {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("supervisor did not stop %q", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	// No live supervisor: proceed only while the row is still the one
	// we read, then record gone under the same identity. A live
	// process holding the recorded pid under a different starttime
	// is a possible pid reuse: refuse rather than kill a stranger's
	// worker.
	cur, err := ptyGet(name)
	if err != nil {
		return err
	}
	if !sameIdentity(cur, m) {
		return fmt.Errorf("session %q changed owner during stop; refusing", name)
	}
	m = cur
	if m.SupervisorPid > 0 && procAlive(m.SupervisorPid) {
		if st, serr := procStartTime(m.SupervisorPid); serr == nil && st != m.SupervisorStart {
			return fmt.Errorf("session %q supervisor identity changed (possible pid reuse); refusing", name)
		}
	}
	if ptyWorkerAlive(m) {
		// Group kill, not just the recorded pid: the worker may have
		// spawned descendants (sh -c leaves them running past a bare
		// child kill). Identity is re-verified inside; refusal leaves
		// the worker for the post-check below to report surviving.
		_ = killWorkerGroup(m.WorkerPid, m.WorkerStart)
		time.Sleep(500 * time.Millisecond)
	}
	if err := ptyMarkGoneFenced(m); err != nil {
		return err
	}
	if m, err = ptyGet(name); err != nil {
		return err
	}
	if ptyWorkerAlive(m) {
		return fmt.Errorf("worker %d survived; refusing further signals", m.WorkerPid)
	}
	return nil
}

// ptyMarkGoneFenced records gone only while the row still carries the
// given supervisor identity, so stopping a dead session never
// clobbers a replacement that took the name.
func ptyMarkGoneFenced(m ptyMeta) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE pty_sessions SET status = 'gone', exit_code = -1, heartbeat = ?
		WHERE name = ? AND supervisor_pid = ? AND supervisor_start = ?`,
		nowUTC(), m.Name, m.SupervisorPid, m.SupervisorStart)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("session %q changed owner during stop; refusing", m.Name)
	}
	return nil
}

// ptySupervise is the detached supervisor body: it owns the PTY
// master, pumps output to the log, and serves the fifo until the
// worker exits or a stop is ordered. It also runs in-process under
// test via goroutine.

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// pty_proc.go: extracted from terminal.go; behavior unchanged.

// startWorkerPTY spawns meta.Command under a fresh PTY master and
// reads back the (pid, starttime) identities fencing every later
// signal and classification. On identity failure the direct child is
// killed — pid reuse is not a live concern milliseconds after spawn —
// and the error names which identity was unreadable; row abandonment
// stays with the caller.
func startWorkerPTY(meta ptyMeta) (pf *os.File, cmd *exec.Cmd, workerPid int, workerStart, selfStart uint64, err error) {
	cmd = exec.Command("sh", "-c", meta.Command)
	cmd.Dir = meta.Workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	pf, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, nil, 0, 0, 0, err
	}
	workerPid = cmd.Process.Pid
	if workerStart, err = procStartTime(workerPid); err != nil {
		// Direct child kill, not a group kill: without a verified
		// start time there is no safe pgid to signal. The pid was
		// spawned milliseconds ago by this supervisor, so reuse is
		// not a live concern on this path.
		_ = cmd.Process.Kill()
		_ = pf.Close()
		return nil, nil, 0, 0, 0, fmt.Errorf("worker identity unreadable: %w", err)
	}
	if selfStart, err = procStartTime(os.Getpid()); err != nil {
		// Same as above: no verified worker start yet on this path.
		_ = cmd.Process.Kill()
		_ = pf.Close()
		return nil, nil, 0, 0, 0, fmt.Errorf("supervisor identity unreadable: %w", err)
	}
	return pf, cmd, workerPid, workerStart, selfStart, nil
}

func procStartTime(pid int) (uint64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return 0, fmt.Errorf("bad stat for pid %d", pid)
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("short stat for pid %d", pid)
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func procAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// Signal 0 succeeds for zombies: an exited-but-unreaped child
	// would otherwise be misclassified as running. Daemon supervisors
	// are children of short-lived CLIs, so a zombie here means the
	// supervisor already finished stopping.
	st, err := procState(pid)
	return err == nil && st != "Z"
}

// procState returns the /proc single-letter process state.
func procState(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	if i < 0 {
		return "", fmt.Errorf("bad stat for pid %d", pid)
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) == 0 {
		return "", fmt.Errorf("short stat for pid %d", pid)
	}
	return fields[0], nil
}

func killWorkerGroup(workerPid int, workerStart uint64) error {
	if workerPid <= 0 {
		return fmt.Errorf("no worker pid recorded")
	}
	st, err := procStartTime(workerPid)
	if err != nil {
		return nil // pid unreadable: already gone, nothing to kill
	}
	if st != workerStart {
		return fmt.Errorf("worker pid %d is a different process (possible pid reuse); refusing group kill", workerPid)
	}
	if err := syscall.Kill(-workerPid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("group kill pgid %d: %w", workerPid, err)
	}
	return nil
}

// ptyWorkerAlive verifies the recorded worker is still the same process.
func ptyWorkerAlive(m ptyMeta) bool {
	if m.WorkerPid <= 0 || !procAlive(m.WorkerPid) {
		return false
	}
	st, err := procStartTime(m.WorkerPid)
	return err == nil && st == m.WorkerStart
}

// ptySupervisorAlive checks the heartbeat is fresh and the supervisor
// still matches its recorded (pid, starttime) identity. The starttime
// check closes the pid-reuse hole: a dead supervisor whose pid was
// recycled by an unrelated process must not read as alive. Rows that
// predate the supervisor_start column (zero) fail closed until the
// running supervisor heals them with its next fenced heartbeat.
func ptySupervisorAlive(m ptyMeta) bool {
	if m.SupervisorPid <= 0 || !procAlive(m.SupervisorPid) {
		return false
	}
	if m.SupervisorStart == 0 {
		return false
	}
	st, err := procStartTime(m.SupervisorPid)
	if err != nil || st != m.SupervisorStart {
		return false
	}
	if m.Heartbeat == "" {
		return false
	}
	hb, err := time.Parse(time.RFC3339, m.Heartbeat)
	if err != nil {
		return false
	}
	return time.Since(hb) < ptyHeartbeatTTL
}

// ptyHeartbeat refreshes liveness only when the writer still holds the
// recorded supervisor identity. A delayed supervisor from an earlier
// startup matches no rows after a replacement takes over, so its writes
// are silently dropped instead of resurrecting stale state. The
// supervisor_start=0 arm heals pre-migration rows on the next tick.

package main

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// pty_lease.go: input leases, FIFO control frames, and input
// receipts for managed PTY sessions. Extracted from terminal.go;
// behavior unchanged.

var (
	// controllerIDValue identifies this OS process as an input owner.
	// A constant owner string would let two TUI processes hold the
	// same lease identity and defeat exclusivity; the pid+start
	// component makes each process distinct.
	controllerIDValue = fmt.Sprintf("metis-%d-%d", os.Getpid(), time.Now().UnixNano())
	// fifoMu serializes multi-frame commands from this process so one
	// command's frames cannot interleave with another's. Cross-process
	// atomicity comes from keeping every frame under PIPE_BUF with a
	// single write(2) each (see ptyFifoWrite).
	fifoMu   = map[string]*sync.Mutex{}
	fifoMuMu sync.Mutex
)

func controllerID() string { return controllerIDValue }

func fifoLock(name string) func() {
	fifoMuMu.Lock()
	mu, ok := fifoMu[name]
	if !ok {
		mu = &sync.Mutex{}
		fifoMu[name] = mu
	}
	fifoMuMu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// frameAuth verifies control authority at the supervisor boundary
// (TD-020): every IN/RS/ST frame must name an owner that currently
// holds the live input lease, and this supervisor must still own its
// row. A delayed supervisor from an earlier startup matches nothing;
// a forged or stale frame is refused without touching the PTY.
// Extracted from terminal.go (ptySupervise's checkFrame); the
// generation fencing and single-writer lease checks are preserved
// exactly.
type frameAuth struct {
	db      *sql.DB
	session string
	pid     int
	start   uint64
}

// check verifies owner against the live input lease and the
// supervisor's current row ownership. ok=false names the refusal.
func (a frameAuth) check(owner string) (reason string, ok bool) {
	if strings.TrimSpace(owner) == "" {
		return "empty control owner refused", false
	}
	var holder string
	var exp int64
	if err := a.db.QueryRow(`SELECT owner, expires FROM input_leases WHERE channel = ?`, "pty:"+a.session).Scan(&holder, &exp); err != nil {
		return "no input lease held; control refused", false
	}
	if holder != owner || exp <= time.Now().Unix() {
		return "stale or foreign control grant refused", false
	}
	var spid int
	var sstart uint64
	if err := a.db.QueryRow(`SELECT supervisor_pid, supervisor_start FROM pty_sessions WHERE name = ?`, a.session).Scan(&spid, &sstart); err != nil || spid != a.pid || sstart != a.start {
		return "supervisor identity changed; control refused", false
	}
	return "", true
}

// ptyFifoWrite sends one control line with a single write(2) call so
// frames at or under PIPE_BUF stay atomic across concurrent writers.
func ptyFifoWrite(name, line string) error {
	f, err := os.OpenFile(ptyFifoPath(name), os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("supervisor not accepting input: %w", err)
	}
	defer f.Close()
	_ = f.SetWriteDeadline(time.Now().Add(5 * time.Second))
	buf := append([]byte(line), '\n')
	n, err := f.Write(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return fmt.Errorf("short fifo write (%d/%d)", n, len(buf))
	}
	return nil
}

// ptyAcquireLease grants exclusive input to owner for ptyInputTTL.
// It is the PTY channel of the single input-writer authority
// (TD-061); see acquireInputLease.
func ptyAcquireLease(name, owner string) error {
	return acquireInputLease("pty:"+name, owner)
}

// tmuxAcquireLease grants exclusive input to owner for ptyInputTTL
// on the tmux channel (TD-061): direct keystrokes into an owned tmux
// session arbitrate through the same single-writer authority as PTY
// input, so two controllers cannot interleave on one terminal.
func tmuxAcquireLease(name, owner string) error {
	return acquireInputLease("tmux:"+name, owner)
}

// acquireInputLease grants exclusive input on channel to owner for
// ptyInputTTL. A live foreign lease is refused; expiry frees the
// channel (TD-062). The acquire is a single conditional statement:
// exactly one competing acquirer wins, the rest get a meaningful
// conflict error instead of silently sharing the channel.
func acquireInputLease(channel, owner string) error {
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("input requires an owner")
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return err
	}
	now := time.Now().Unix()
	res, err := db.Exec(`INSERT INTO input_leases (channel, owner, expires) VALUES (?, ?, ?)
		ON CONFLICT(channel) DO UPDATE SET owner = excluded.owner, expires = excluded.expires
		WHERE input_leases.expires <= ? OR input_leases.owner = excluded.owner`,
		channel, owner, now+int64(ptyInputTTL/time.Second), now)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var cur string
		var exp int64
		if qerr := db.QueryRow(`SELECT owner, expires FROM input_leases WHERE channel = ?`, channel).Scan(&cur, &exp); qerr == nil {
			left := exp - now
			if left < 0 {
				left = 0
			}
			return fmt.Errorf("input held by %q for %ds more", cur, left)
		}
		return fmt.Errorf("input held by another controller")
	}
	return nil
}

// Frame sizing: each fifo line must fit in one atomic write(2).
// Linux guarantees atomicity for writes at or under PIPE_BUF (4096).
// ptyFrameRaw caps one IN frame's payload so every frame stays a
// single write(2) under PIPE_BUF (see ptyFifoWrite).
const ptyFrameRaw = 2850

// ptyInput queues text for the named session under the caller's
// lease and returns the input receipt id for status polling.
func ptyInput(name, owner, text string) (string, error) {
	if len(text) > 256*1024 {
		return "", fmt.Errorf("input too large (%d bytes, cap 256KB)", len(text))
	}
	m, err := ptyGet(name)
	if err != nil {
		return "", err
	}
	if !ptySupervisorAlive(m) {
		return "", fmt.Errorf("supervisor for %q is not running", name)
	}
	if err := ptyAcquireLease(name, owner); err != nil {
		return "", err
	}
	id := fmt.Sprintf("in-%d-%d", time.Now().UnixNano(), os.Getpid())
	raw := []byte(text)
	total := (len(raw) + ptyFrameRaw - 1) / ptyFrameRaw
	if total < 1 {
		total = 1
	}
	unlock := fifoLock(name)
	defer unlock()
	for i := 0; i < total; i++ {
		lo := i * ptyFrameRaw
		hi := lo + ptyFrameRaw
		if hi > len(raw) {
			hi = len(raw)
		}
		if err := ptyFifoWrite(name, "IN "+id+" "+owner+" "+strconv.Itoa(i+1)+"/"+strconv.Itoa(total)+" "+
			base64.StdEncoding.EncodeToString(raw[lo:hi])); err != nil {
			return "", err
		}
	}
	return id, nil
}

// ptyInputStatus reports the supervisor's disposition of a queued
// command: queued (accepted, not yet processed), written_to_pty
// (bytes handed to the PTY master), or error (with detail).
// written_to_pty is transport delivery, not worker comprehension.
func ptyInputStatus(id string) (status, detail string, err error) {
	db, err := openTasksDB()
	if err != nil {
		return "", "", err
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return "", "", err
	}
	err = db.QueryRow(`SELECT status, detail FROM pty_input_log WHERE id = ?`, id).Scan(&status, &detail)
	if err == sql.ErrNoRows {
		return "queued", "", nil
	}
	return status, detail, err
}

func ptyRecordReceipt(id, session, owner string, nbytes int, status, detail string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`INSERT INTO pty_input_log (id, session, owner, bytes, status, detail, created)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET status = excluded.status, detail = excluded.detail`,
		id, session, owner, nbytes, status, detail, nowUTC())
	_, _ = db.Exec(`DELETE FROM pty_input_log WHERE created < datetime('now', '-1 hour')`)
}

// ptyResize propagates terminal dimensions to the worker. Resize is
// a control operation: it requires the input lease, so a rival
// controller cannot resize a session it does not hold.
func ptyResize(name string, rows, cols int) error {
	return ptyResizeAsOwner(name, controllerID(), rows, cols)
}

// ptyResizeAsOwner is the owner-explicit resize channel: the MCP
// terminal tools resize under the caller's scoped lease identity,
// never the process controller identity, so a remote writer's
// authority is fenced to the lease it actually holds.
func ptyResizeAsOwner(name, owner string, rows, cols int) error {
	m, err := ptyGet(name)
	if err != nil {
		return err
	}
	if !ptySupervisorAlive(m) {
		return fmt.Errorf("supervisor for %q is not running", name)
	}
	if rows < 1 || cols < 1 {
		return fmt.Errorf("bad size %dx%d", cols, rows)
	}
	if err := ptyAcquireLease(name, owner); err != nil {
		return err
	}
	return ptyFifoWrite(name, fmt.Sprintf("RS %s %d %d", owner, rows, cols))
}

// ptyCheckGeneration fences a control request against the session's
// generation counter: a caller naming a stale generation (its view
// predates a restart/recovery that bumped the counter) is refused
// before any lease is touched, so a delayed request can never act
// on the replacement session. hasWant false skips the fence.
func ptyCheckGeneration(name string, hasWant bool, want int) (ptyMeta, error) {
	m, err := ptyGet(name)
	if err != nil {
		return m, err
	}
	if hasWant && m.Generation != want {
		return m, fmt.Errorf("session %q generation changed (have %d, want %d); refusing stale request", name, m.Generation, want)
	}
	return m, nil
}

// clearTerminalLeases drops every input lease for a session on both
// transports. Only session stop uses it: terminating the session
// ends all input, so a stale or rival lease must not veto a stop
// and must not linger past it.
func clearTerminalLeases(name string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return
	}
	_, _ = db.Exec(`DELETE FROM input_leases WHERE channel = ? OR channel = ?`, "pty:"+name, "tmux:"+name)
}

// ptyLog tails the session output log.
func ptyAbandon(name, owner string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`UPDATE pty_sessions SET status = 'gone', exit_code = -1, heartbeat = ?
		WHERE name = ? AND owner = ? AND status = 'starting'`, nowUTC(), name, owner)
}

func ptyMark(name, status string, code int) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	_, _ = db.Exec(`UPDATE pty_sessions SET status = ?, exit_code = ?, heartbeat = ? WHERE name = ?`,
		status, code, nowUTC(), name)
}

// ptyClearLease drops any input lease for a session. Only stop uses
// it: terminating the session ends all input, so a stale or rival
// lease must not veto a stop. The supervisor still verifies the ST
// frame owner against the freshly acquired lease, so forged stop
// frames from processes that never established authority are
// refused at the boundary.
func ptyClearLease(name string) {
	db, err := openTasksDB()
	if err != nil {
		return
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return
	}
	_, _ = db.Exec(`DELETE FROM input_leases WHERE channel = ?`, "pty:"+name)
}

// ptyStop terminates an owned session: supervisor first (clean exit
// record), falling back to the verified worker pid when the
// supervisor is already gone. Stop preempts input: it clears any
// lease, then establishes its own, so the ST frame carries proven
// authority. Every step re-verifies the (supervisor pid, starttime)
// identity, so stopping a session that was replaced mid-stop refuses
// instead of signalling or overwriting the new owner's row.

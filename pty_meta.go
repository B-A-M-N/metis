package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// pty_meta.go: extracted from terminal.go; behavior unchanged.

type ptyMeta struct {
	Name          string `json:"name"`
	Workdir       string `json:"workdir"`
	Command       string `json:"command"`
	Pts           string `json:"pts"`
	LogPath       string `json:"log_path"`
	WorkerPid     int    `json:"worker_pid"`
	WorkerStart   uint64 `json:"worker_start"`
	SupervisorPid int    `json:"supervisor_pid"`
	// SupervisorStart is the /proc starttime of the supervisor process.
	// Together with SupervisorPid it is the fencing identity: control
	// writes must match both, so a delayed supervisor from an earlier
	// startup cannot overwrite a replacement session after pid reuse.
	SupervisorStart uint64 `json:"supervisor_start"`
	Owner           string `json:"owner"`
	Status          string `json:"status"` // starting | running | exited | gone
	ExitCode        int    `json:"exit_code"`
	Heartbeat       string `json:"heartbeat"`
	Generation      int    `json:"generation"`
	Harness         string `json:"harness"`
	Model           string `json:"model"`
}

// ptyPrefix namespaces PTY-backed sessions in the unified display
// list. Tmux registry names cannot contain ':' (nameRe), so the
// prefix is unambiguous and reversible.
const ptyPrefix = "pty:"
func isPtySession(display string) bool { return strings.HasPrefix(display, ptyPrefix) }
func ptyBare(display string) string    { return strings.TrimPrefix(display, ptyPrefix) }
func ptyDisplay(name string) string    { return ptyPrefix + name }

// combinedNames merges tmux registry names and PTY display names into
// one stable selection list.
func combinedNames(reg map[string]Session, metas []ptyMeta) []string {
	out := make([]string, 0, len(reg)+len(metas))
	for n := range reg {
		out = append(out, n)
	}
	for _, m := range metas {
		out = append(out, ptyDisplay(m.Name))
	}
	sort.Strings(out)
	return out
}

func ptyTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS pty_sessions (
		name TEXT PRIMARY KEY,
		workdir TEXT NOT NULL,
		command TEXT NOT NULL,
		pts TEXT NOT NULL DEFAULT '',
		logpath TEXT NOT NULL DEFAULT '',
		worker_pid INTEGER NOT NULL DEFAULT 0,
		worker_start INTEGER NOT NULL DEFAULT 0,
		supervisor_pid INTEGER NOT NULL DEFAULT 0,
		supervisor_start INTEGER NOT NULL DEFAULT 0,
		owner TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'starting',
		exit_code INTEGER NOT NULL DEFAULT -1,
		heartbeat TEXT NOT NULL DEFAULT '',
		generation INTEGER NOT NULL DEFAULT 0
	)`)
	if err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS pty_leases (
		session TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		expires INTEGER NOT NULL
	)`)
	if err != nil {
		return err
	}
	// input_leases is the single input-writer authority for every
	// transport (TD-061). Channels are transport-qualified —
	// "pty:<name>" vs "tmux:<name>" — so a PTY session and a tmux
	// session never arbitrate each other's input. pty_leases is
	// superseded and no longer consulted.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS input_leases (
		channel TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		expires INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS pty_input_log (
		id TEXT PRIMARY KEY,
		session TEXT NOT NULL,
		owner TEXT NOT NULL,
		bytes INTEGER NOT NULL,
		status TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '',
		created TEXT NOT NULL
	)`)
	if err != nil {
		return err
	}
	// Additive migration for pre-label databases.
	for _, col := range []string{
		`harness TEXT NOT NULL DEFAULT ''`,
		`model TEXT NOT NULL DEFAULT ''`,
		`supervisor_start INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(`ALTER TABLE pty_sessions ADD COLUMN ` + col); err != nil {
			if !strings.Contains(err.Error(), "duplicate column") {
				return err
			}
		}
	}
	return nil
}

func ptyLogPath(name string) string  { return filepath.Join(stateDir(), "pty", name+".log") }
func ptyFifoPath(name string) string { return filepath.Join(stateDir(), "pty", name+".fifo") }

// procStartTime returns Linux /proc pid starttime (field 22), the
// identity that distinguishes a live process from pid reuse.
func ptyGet(name string) (ptyMeta, error) {
	var m ptyMeta
	db, err := openTasksDB()
	if err != nil {
		return m, err
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return m, err
	}
	err = db.QueryRow(`SELECT name, workdir, command, pts, logpath, worker_pid, worker_start,
		supervisor_pid, supervisor_start, owner, status, exit_code, heartbeat, generation, harness, model
		FROM pty_sessions WHERE name = ?`, name).Scan(
		&m.Name, &m.Workdir, &m.Command, &m.Pts, &m.LogPath, &m.WorkerPid, &m.WorkerStart,
		&m.SupervisorPid, &m.SupervisorStart, &m.Owner, &m.Status, &m.ExitCode, &m.Heartbeat, &m.Generation,
		&m.Harness, &m.Model)
	if err == sql.ErrNoRows {
		return m, fmt.Errorf("no such PTY session: %s", name)
	}
	return m, err
}

// killWorkerGroup SIGKILLs the owned worker's entire process group.
// The pty library starts workers as group leaders (Setsid), so the
// recorded worker pid is the pgid. Identity is re-verified against
// the recorded start time first: group-killing a recycled pid would
// hit strangers' processes. An unreadable pid reads back as already
// gone, which is success for a stop path.
func ptyHeartbeat(db *sql.DB, name string, pid int, start uint64) {
	_, _ = db.Exec(`UPDATE pty_sessions SET heartbeat = ?, supervisor_start = ?
		WHERE name = ? AND supervisor_pid = ?
		AND (supervisor_start = ? OR supervisor_start = 0)`,
		nowUTC(), start, name, pid, start)
}

// ptyStart records the session and spawns a detached supervisor.
func ptyListAll() ([]ptyMeta, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ptyTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT name, workdir, command, pts, logpath, worker_pid, worker_start,
		supervisor_pid, supervisor_start, owner, status, exit_code, heartbeat, generation, harness, model
		FROM pty_sessions ORDER BY heartbeat DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ptyMeta
	for rows.Next() {
		var m ptyMeta
		if err := rows.Scan(&m.Name, &m.Workdir, &m.Command, &m.Pts, &m.LogPath, &m.WorkerPid,
			&m.WorkerStart, &m.SupervisorPid, &m.SupervisorStart, &m.Owner, &m.Status, &m.ExitCode, &m.Heartbeat,
			&m.Generation, &m.Harness, &m.Model); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// publicPtyMeta is the display/JSON projection of a ptyMeta record.
// Owner is a control credential and must never cross the
// machine-readable boundary; ptyView carries this, never ptyMeta.
type publicPtyMeta struct {
	Name       string `json:"name"`
	Workdir    string `json:"workdir"`
	Command    string `json:"command"`
	Pts        string `json:"pts"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Heartbeat  string `json:"heartbeat"`
	Generation int    `json:"generation"`
	Harness    string `json:"harness"`
	Model      string `json:"model"`
}

// publicPty strips control credentials for external display.
func (m ptyMeta) publicPty() publicPtyMeta {
	return publicPtyMeta{
		Name: m.Name, Workdir: m.Workdir, Command: m.Command,
		Pts: m.Pts, Status: m.Status, ExitCode: m.ExitCode,
		Heartbeat: m.Heartbeat, Generation: m.Generation,
		Harness: m.Harness, Model: m.Model,
	}
}

// ptyView couples a record with its live classification for display.
type ptyView struct {
	Session publicPtyMeta `json:"session"`
	State   string        `json:"state"`
}

func ptyViewFor(name string) (ptyView, error) {
	m, state, err := ptyStatus(name)
	if err != nil {
		return ptyView{}, err
	}
	return ptyView{Session: m.publicPty(), State: state}, nil
}

func ptyViewAll() ([]ptyView, error) {
	metas, err := ptyListAll()
	if err != nil {
		return nil, err
	}
	out := make([]ptyView, 0, len(metas))
	for _, m := range metas {
		_, state, _ := ptyStatus(m.Name)
		out = append(out, ptyView{Session: m.publicPty(), State: state})
	}
	return out, nil
}

// ptyClassify derives the display/control state from an already-fetched
// record, so fleet refresh can classify every session from one query
// instead of reopening the database per session.
//
// A recorded terminal disposition is the supervisor's own final word
// and takes precedence over transient liveness: after ST/EX handling
// the supervisor writes exited/gone before its process finishes
// tearing down, and that lingering teardown must not reclassify the
// session as "worker-gone". A new generation always resets the row
// to starting, so a terminal status can only belong to the finished
// generation.
func ptyClassify(m ptyMeta) string {
	if m.Status == "exited" || m.Status == "gone" {
		return m.Status
	}
	switch {
	case ptySupervisorAlive(m) && ptyWorkerAlive(m):
		return "running"
	case ptySupervisorAlive(m):
		return "worker-gone"
	case ptyWorkerAlive(m):
		return "orphaned"
	default:
		return m.Status
	}
}

// ptyStatus classifies a session for display and control decisions.
func ptyStatus(name string) (ptyMeta, string, error) {
	m, err := ptyGet(name)
	if err != nil {
		return m, "", err
	}
	return m, ptyClassify(m), nil
}

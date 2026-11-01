package main

// doctor.go: genuinely read-only diagnostics (P0-A).
//
// The critical property: doctor never migrates schema, never
// writes receipts, never creates state files, never starts
// processes for mutation. It opens the task database with
// query_only=ON and probes sqlite_master before touching any
// table, so a bare or legacy state dir yields UNKNOWN/BLOCKED
// conditions instead of silent migration.
//
// Every condition carries its evidence source. Statuses:
// OK, DEGRADED, BLOCKED, UNKNOWN.

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	ConditionOK       = "OK"
	ConditionDegraded = "DEGRADED"
	ConditionBlocked  = "BLOCKED"
	ConditionUnknown  = "UNKNOWN"
)

// DoctorCondition is one structured health fact with its evidence.
type DoctorCondition struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Source string `json:"source"`
}

// DoctorReport is the full diagnostic projection.
type DoctorReport struct {
	Conditions []DoctorCondition `json:"conditions"`
}

// openReadOnlyDB opens the task database without any migration or
// write path. query_only=ON makes any accidental write fail
// loudly instead of mutating state.
func openReadOnlyDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", tasksDBPath())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA query_only = ON`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func tablePresent(db *sql.DB, name string) bool {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

func doctorReport() (DoctorReport, error) {
	var rep DoctorReport
	add := func(name, status, detail, source string) {
		rep.Conditions = append(rep.Conditions, DoctorCondition{Name: name, Status: status, Detail: detail, Source: source})
	}

	// Startup security posture first: it is env-only, so it
	// reports even when no state database exists.
	doctorMCPIngress(add)

	// Database file itself.
	if fi, err := os.Stat(tasksDBPath()); err != nil {
		add("database", ConditionBlocked, fmt.Sprintf("task database unreadable: %v", err), "stat tasks.db")
		add("schema", ConditionUnknown, "no database: table inventory impossible without migrating", "stat tasks.db")
		doctorSessions(add, nil)
		return rep, nil
	} else if fi.Size() == 0 {
		add("database", ConditionBlocked, "task database is empty", "stat tasks.db")
		doctorSessions(add, nil)
		return rep, nil
	} else {
		add("database", ConditionOK, fmt.Sprintf("task database present (%d bytes)", fi.Size()), "stat tasks.db")
	}

	db, err := openReadOnlyDB()
	if err != nil {
		add("schema", ConditionBlocked, fmt.Sprintf("cannot open database read-only: %v", err), "PRAGMA query_only")
		doctorSessions(add, nil)
		return rep, nil
	}
	defer db.Close()

	// Schema inventory: the table set is the version signal.
	// Missing core tables mean a legacy or bare database; doctor
	// reports that, never migrates into it. Optional tables only
	// appear once their feature is first used (leases on first
	// input grant, artifacts on first attach), so their absence
	// is noted, not alarmed.
	core := []string{"tasks", "task_attempts", "task_deliveries", "task_reports", "task_receipts", "execution_receipts"}
	optional := []string{"task_verdicts", "verification_evidence", "artifacts", "input_leases", "guidance"}
	var missingCore, missingOpt []string
	for _, t := range core {
		if !tablePresent(db, t) {
			missingCore = append(missingCore, t)
		}
	}
	for _, t := range optional {
		if !tablePresent(db, t) {
			missingOpt = append(missingOpt, t)
		}
	}
	if len(missingCore) > 0 {
		add("schema", ConditionUnknown, fmt.Sprintf("core tables absent (legacy or bare state; not migrated by doctor): %s", strings.Join(missingCore, ", ")), "sqlite_master")
	} else if len(missingOpt) > 0 {
		add("schema", ConditionOK, fmt.Sprintf("core tables present; unused features not yet materialized: %s", strings.Join(missingOpt, ", ")), "sqlite_master")
	} else {
		add("schema", ConditionOK, "all expected tables present", "sqlite_master")
	}

	// Helpers append through add (closure over rep); their return
	// values are intentionally ignored — reassigning rep here
	// would discard appended conditions via a stale slice header.
	doctorTasks(add, db)
	doctorLeases(add, db)
	doctorSessions(add, db)
	doctorStreams(add)
	doctorAdapters(add)
	return rep, nil
}

// doctorMCPIngress is the operator-only startup security
// diagnostic for MCP ingress (F-02): the resolved ingress mode,
// the detected transport, credential presence (never the secret
// itself), and the capabilities granted to remote callers. Doctor
// runs as the local operator, so this is the surface that names
// the resolved mode — the world-readable metis_status projection
// never carries it. Fail-closed postures report BLOCKED: every
// mutation through the MCP boundary refuses until the operator
// declares remote ingress with a credential.
func doctorMCPIngress(add func(string, string, string, string)) {
	const src = "env METIS_MCP_INGRESS/METIS_MCP_REMOTE_INGRESS/METIS_MCP_HOSTED/METIS_MCP_WRITE_TOKEN/METIS_MCP_REMOTE_CAPABILITIES"
	mode, merr := mcpResolveIngressMode()
	hosted := mcpHostedTransport()
	tokenSet := os.Getenv("METIS_MCP_WRITE_TOKEN") != ""
	transport := "local"
	if hosted {
		transport = "hosted"
	}
	resolved := string(mode)
	if resolved == "" {
		resolved = "unspecified"
	}
	grants := mcpGrantedRemoteCapabilities()
	credential := "unset"
	if tokenSet {
		credential = "configured"
	}
	detail := fmt.Sprintf("mode=%s transport=%s token=%s remote_caps=[%s]",
		resolved, transport, credential, strings.Join(grants, ","))
	switch {
	case merr != nil:
		add("mcp-ingress", ConditionBlocked, "invalid ingress declaration: "+merr.Error()+"; mutations fail closed", src)
	case hosted && mode != mcpIngressRemote:
		add("mcp-ingress", ConditionBlocked, detail+"; hosted transport without remote-ingress declaration refuses local-operator trust (declare METIS_MCP_INGRESS=remote or METIS_MCP_REMOTE_INGRESS=1 and set METIS_MCP_WRITE_TOKEN)", src)
	case mode == mcpIngressRemote && !tokenSet:
		add("mcp-ingress", ConditionBlocked, detail+"; remote ingress declared without a credential refuses local-operator trust (set METIS_MCP_WRITE_TOKEN)", src)
	case mode == mcpIngressUnspecified && !tokenSet:
		add("mcp-ingress", ConditionOK, detail+"; implicit local trust on a local transport (pin with METIS_MCP_INGRESS=local)", src)
	default:
		add("mcp-ingress", ConditionOK, detail, src)
	}
	return
}

// doctorTasks covers outstanding attempts, uncertain executions,
// and missing report/evidence rows.
func doctorTasks(add func(string, string, string, string), db *sql.DB) {
	if !tablePresent(db, "tasks") {
		add("tasks", ConditionUnknown, "tasks table absent", "sqlite_master")
		return
	}
	statuses := []string{TaskPending, TaskDispatched, TaskWorking, TaskReportReady}
	var outstanding []string
	for _, st := range statuses {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status = ?`, st).Scan(&n); err == nil && n > 0 {
			outstanding = append(outstanding, fmt.Sprintf("%s=%d", st, n))
		} else if err != nil {
			add("tasks", ConditionUnknown, fmt.Sprintf("status scan failed: %v", err), "SELECT tasks")
			return
		}
	}
	if len(outstanding) == 0 {
		add("tasks", ConditionOK, "no outstanding attempts (nothing pending/dispatched/working/report_ready)", "SELECT tasks GROUP BY status")
	} else {
		add("tasks", ConditionDegraded, "outstanding: "+strings.Join(outstanding, " "), "SELECT tasks GROUP BY status")
	}

	// Uncertain executions: working tasks whose receipt is open
	// or missing entirely, plus open receipts on non-working tasks.
	if tablePresent(db, "execution_receipts") {
		rows, err := db.Query(`SELECT t.id, t.attempt FROM tasks t LEFT JOIN execution_receipts e
			ON e.task_id = t.id AND e.attempt_no = t.attempt
			WHERE t.status = 'working' AND (e.task_id IS NULL OR e.exit_code IS NULL)`)
		if err != nil {
			add("executions", ConditionUnknown, fmt.Sprintf("uncertainty scan failed: %v", err), "LEFT JOIN execution_receipts")
			return
		}
		var uncertain []string
		for rows.Next() {
			var id string
			var att int
			if err := rows.Scan(&id, &att); err != nil {
				rows.Close()
				add("executions", ConditionUnknown, fmt.Sprintf("row scan failed: %v", err), "LEFT JOIN execution_receipts")
				return
			}
			uncertain = append(uncertain, fmt.Sprintf("%s@%d", id, att))
		}
		rows.Close()
		if len(uncertain) > 0 {
			add("executions", ConditionDegraded, fmt.Sprintf("uncertain outcomes (open or missing receipt): %s", strings.Join(uncertain, ", ")), "LEFT JOIN execution_receipts")
		} else {
			add("executions", ConditionOK, "no working task lacks a finished execution receipt", "LEFT JOIN execution_receipts")
		}
	} else {
		add("executions", ConditionUnknown, "execution_receipts absent", "sqlite_master")
	}

	// Report/evidence gaps behind terminal claims.
	if tablePresent(db, "task_reports") {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE status = 'report_ready' AND id NOT IN (SELECT task_id FROM task_reports)`).Scan(&n); err == nil && n > 0 {
			add("reports", ConditionDegraded, fmt.Sprintf("%d report_ready task(s) with no report row", n), "tasks LEFT JOIN task_reports")
		} else if err != nil {
			add("reports", ConditionUnknown, fmt.Sprintf("report gap scan failed: %v", err), "tasks LEFT JOIN task_reports")
		} else {
			add("reports", ConditionOK, "every report_ready task has a report row", "tasks LEFT JOIN task_reports")
		}
	}
	return
}

// doctorLeases lists active input leases with expiry.
func doctorLeases(add func(string, string, string, string), db *sql.DB) {
	if !tablePresent(db, "input_leases") {
		add("leases", ConditionUnknown, "input_leases absent", "sqlite_master")
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	rows, err := db.Query(`SELECT channel, owner, expires FROM input_leases WHERE expires > ?`, now)
	if err != nil {
		add("leases", ConditionUnknown, fmt.Sprintf("lease scan failed: %v", err), "SELECT input_leases")
		return
	}
	defer rows.Close()
	var active []string
	for rows.Next() {
		var ch, owner, exp string
		if err := rows.Scan(&ch, &owner, &exp); err != nil {
			add("leases", ConditionUnknown, fmt.Sprintf("row scan failed: %v", err), "SELECT input_leases")
			return
		}
		active = append(active, fmt.Sprintf("%s held by %s until %s", ch, owner, exp))
	}
	if len(active) > 0 {
		add("leases", ConditionDegraded, "active input leases: "+strings.Join(active, "; "), "SELECT input_leases WHERE expires > now")
	} else {
		add("leases", ConditionOK, "no active input leases", "SELECT input_leases WHERE expires > now")
	}
	return
}

// doctorSessions reports registry entries with transport,
// ownership, and liveness. db may be nil (no database at all).
func doctorSessions(add func(string, string, string, string), db *sql.DB) {
	reg := loadRegistry()
	if len(reg) == 0 {
		add("sessions", ConditionOK, "no registered sessions", "sessions.json")
		return
	}
	for name, e := range reg {
		transport := "tmux"
		alive := sessionRunning(name)
		detail := fmt.Sprintf("session=%s transport=%s harness=%s model=%s alive=%v owner=%s root=%s",
			name, transport, e.Harness, e.Model, alive, e.Owner, e.Workdir)
		status := ConditionOK
		if !alive {
			status = ConditionDegraded
			detail += " (registered but not running)"
		}
		add("session:"+name, status, detail, "sessions.json + tmux ls")
	}
	// PTY sessions live in the database, not the registry.
	if db != nil && tablePresent(db, "pty_sessions") {
		rows, err := db.Query(`SELECT name, status, worker_pid FROM pty_sessions`)
		if err != nil {
			add("pty-sessions", ConditionUnknown, fmt.Sprintf("pty scan failed: %v", err), "SELECT pty_sessions")
			return
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var name, status string
			var pid int
			if err := rows.Scan(&name, &status, &pid); err != nil {
				continue
			}
			n++
			st := ConditionOK
			if status != "running" {
				st = ConditionDegraded
			}
			add("pty:"+name, st, fmt.Sprintf("status=%s worker_pid=%d alive=%v", status, pid, procAlive(pid)), "pty_sessions + /proc")
		}
		if n == 0 {
			add("pty-sessions", ConditionOK, "no PTY sessions recorded", "SELECT pty_sessions")
		}
	}
	return
}

// doctorStreams checks stream file size, freshness, and rotation
// markers without triggering maintenance.
func doctorStreams(add func(string, string, string, string)) {
	reg := loadRegistry()
	if len(reg) == 0 {
		return
	}
	for name := range reg {
		path := streamPath(name)
		fi, err := os.Stat(path)
		if err != nil {
			add("stream:"+name, ConditionUnknown, "no stream file (capture-pane fallback in effect)", "stat streams/<session>.log")
			continue
		}
		age := time.Since(fi.ModTime()).Round(time.Second)
		detail := fmt.Sprintf("size=%d age=%s cap=%d", fi.Size(), age, tmuxStreamCap)
		status := ConditionOK
		if fi.Size() > tmuxStreamCap {
			status = ConditionDegraded
			detail += " (over cap: refresh-time rotation pending)"
		}
		// Rotation marker at head means history was discarded.
		f, err := os.Open(path)
		if err == nil {
			buf := make([]byte, 64)
			if n, rerr := f.Read(buf); rerr == nil && n > 0 && strings.Contains(string(buf[:n]), "stream rotated at") {
				status = ConditionDegraded
				detail += " (head truncated by rotation)"
			}
			_ = f.Close()
		}
		add("stream:"+name, status, detail, "stat streams/<session>.log")
	}
	return
}

// doctorAdapters projects static harness capabilities, including
// explicitly unsupported operations.
func doctorAdapters(add func(string, string, string, string)) {
	for _, h := range []string{"demo", "codex", "claude", "muse", "hermes"} {
		a, err := lookupAdapter(h)
		if err != nil {
			add("adapter:"+h, ConditionUnknown, fmt.Sprintf("lookup failed: %v", err), "adapterRegistry")
			continue
		}
		detail := fmt.Sprintf("one_shot=%v interactive=%v live_input=%v managed_argv=%v",
			a.OneShot, a.Interactive, a.AcceptsLiveInput, a.ManagedArgv != nil)
		status := ConditionOK
		if !a.Interactive {
			detail += " (interactive NOT qualified)"
		}
		add("adapter:"+h, status, detail, "adapterRegistry")
	}
	return
}

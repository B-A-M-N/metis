package main

import (
	"database/sql"
	"time"
)

// muse_presence.go: authenticated orchestrator presence leases.
//
// museActivity previously derived "activity" only from task rows and
// orchestration events, which conflates execution workload with
// actual orchestrator presence: an old approval.required event could
// hold Attention after the approval was decided, and no state
// distinguished a live orchestrator from a busy worker fleet.
//
// A presence lease records that an authenticated principal (an MCP
// caller admitted by mcpAuthCaller) was recently active, with its
// source and an expiry.
//
// Presence is five separate typed facts — connection,
// orchestrator_lease, current_operation, last_progress,
// blocking_reason — each with an explicit Unknown for "could not
// determine". The Muse-tab indicator derives from these facts
// (deriveMuseState), never from raw event-age heuristics: traffic
// recency alone is a labeled estimate (MuseActiveEstimated), never
// verified activity. A verified Active needs BOTH a live lease and
// committed open work; either fact alone degrades honestly
// (Connected for a live lease with no work, Delegated for work with
// no live lease).

// musePresenceLeaseTTL bounds how long one authenticated call keeps
// the orchestrator "present". Request-based web clients re-record on
// every call, so expiry correctly returns the tab to inactive.
const musePresenceLeaseTTL = 10 * time.Minute

type musePresence struct {
	Principal string
	Source    string
	LastSeen  int64
	ExpiresAt int64
}

func ensurePresenceTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS muse_presence_leases (
		principal  TEXT PRIMARY KEY,
		source     TEXT NOT NULL DEFAULT '',
		last_seen_ns INTEGER NOT NULL,
		expires_ns INTEGER NOT NULL
	)`)
	return err
}

// recordMusePresence upserts the principal's lease to now+ttl. A
// non-positive ttl expires immediately, which tests use to simulate
// a lapsed lease without sleeping.
func recordMusePresence(principal, source string, ttl time.Duration) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensurePresenceTable(db); err != nil {
		return err
	}
	now := time.Now().UnixNano()
	_, err = db.Exec(`INSERT INTO muse_presence_leases (principal, source, last_seen_ns, expires_ns)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (principal) DO UPDATE SET source = excluded.source,
			last_seen_ns = excluded.last_seen_ns, expires_ns = excluded.expires_ns`,
		principal, source, now, now+int64(ttl))
	return err
}

// liveMuseLeaseCount prunes expired leases on db and counts the
// survivors. It shares one handle with the caller so fact collection
// can distinguish "table unreadable" (error → Unknown) from "no
// live lease" (zero → Absent).
func liveMuseLeaseCount(db *sql.DB) (int, error) {
	if err := ensurePresenceTable(db); err != nil {
		return 0, err
	}
	now := time.Now().UnixNano()
	if _, err := db.Exec(`DELETE FROM muse_presence_leases WHERE expires_ns <= ?`, now); err != nil {
		return 0, err
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM muse_presence_leases`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// activeMusePresence returns unexpired leases, pruning the expired
// ones so stale principals cannot linger.
func activeMusePresence() []musePresence {
	db, err := openTasksDB()
	if err != nil {
		return nil
	}
	defer db.Close()
	if _, err := liveMuseLeaseCount(db); err != nil {
		return nil
	}
	rows, err := db.Query(`SELECT principal, source, last_seen_ns, expires_ns
		FROM muse_presence_leases ORDER BY last_seen_ns DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []musePresence
	for rows.Next() {
		var p musePresence
		if err := rows.Scan(&p.Principal, &p.Source, &p.LastSeen, &p.ExpiresAt); err != nil {
			break
		}
		out = append(out, p)
	}
	return out
}

// MuseConnection is the verified reachability fact: is the
// orchestrator channel known up, known down, or undetermined?
type MuseConnection int

const (
	// MuseConnUnknown means the state database itself was
	// unreadable — reachability cannot be determined.
	MuseConnUnknown MuseConnection = iota
	// MuseConnDown means a recent transport.disconnect stands
	// unrecovered by any newer traffic.
	MuseConnDown
	// MuseConnUp means no disconnect signal stands; it asserts
	// only the absence of a known-down signal, not liveness.
	MuseConnUp
)

// MuseLease is the authenticated orchestrator lease fact: was an
// admitted principal recently active?
type MuseLease int

const (
	// MuseLeaseUnknown means the presence table was unreadable.
	MuseLeaseUnknown MuseLease = iota
	// MuseLeaseAbsent means no unexpired lease exists.
	MuseLeaseAbsent
	// MuseLeaseLive means an admitted principal holds an
	// unexpired lease — verified presence, not traffic.
	MuseLeaseLive
)

// MuseOperation is the verified execution-workload fact, derived
// only from committed task rows: is orchestrated work in flight?
type MuseOperation int

const (
	// MuseOpUnknown means the task ledger was unreadable.
	MuseOpUnknown MuseOperation = iota
	// MuseOpNone means no dispatched, working, or
	// report-ready task rows exist.
	MuseOpNone
	// MuseOpInFlight means committed open work exists.
	MuseOpInFlight
)

// MuseProgress is the last-progress fact: when did the durable
// communication feed last move? Fresh means recent traffic — an
// estimate of liveness, never verified execution.
type MuseProgress int

const (
	// MuseProgressUnknown means the event feed was unreadable.
	MuseProgressUnknown MuseProgress = iota
	// MuseProgressNone means the feed holds no events.
	MuseProgressNone
	// MuseProgressStale means the newest event is older than
	// museActiveWindow.
	MuseProgressStale
	// MuseProgressFresh means the newest event is within
	// museActiveWindow. Traffic only — see MuseActiveEstimated.
	MuseProgressFresh
)

// MuseBlocking is the blocking-reason fact: what verified signal,
// if any, says the orchestrator is stuck?
type MuseBlocking int

const (
	// MuseBlockingUnknown means pending-approval state could not
	// be read.
	MuseBlockingUnknown MuseBlocking = iota
	// MuseBlockingNone means no blocking signal stands.
	MuseBlockingNone
	// MuseBlockingApproval means approval rows are still pending.
	MuseBlockingApproval
	// MuseBlockingRefused means a message carries an explicit
	// refused/failed disposition.
	MuseBlockingRefused
	// MuseBlockingTransport means a recent transport.disconnect
	// stands unrecovered.
	MuseBlockingTransport
)

// MuseFacts is the separated presence model: connection,
// orchestrator lease, current operation, last progress, and
// blocking reason as independent typed facts plus the outstanding
// workload counts. Unknown is explicit on every axis.
type MuseFacts struct {
	Connection MuseConnection
	Lease      MuseLease
	Operation  MuseOperation
	Progress   MuseProgress
	Blocking   MuseBlocking
	Open       int
	Blocked    int
}

// collectMuseFacts reads the five presence facts from durable
// state. Every axis degrades to its Unknown when its source is
// unreadable; a fully unreadable database yields all-Unknown, which
// derives to Disconnected.
func collectMuseFacts() MuseFacts {
	f := MuseFacts{
		Connection: MuseConnUnknown,
		Lease:      MuseLeaseUnknown,
		Operation:  MuseOpUnknown,
		Progress:   MuseProgressUnknown,
		Blocking:   MuseBlockingUnknown,
	}
	db, err := openTasksDB()
	if err != nil {
		return f
	}
	defer db.Close()
	if err := ensureOrchEventsTable(db); err != nil {
		return f
	}
	f.Connection = MuseConnUp
	f.Operation = MuseOpNone
	f.Progress = MuseProgressNone
	f.Blocking = MuseBlockingNone

	// Current operation: committed open task rows are the
	// verified execution signal. A read failure leaves
	// Operation Unknown rather than projecting None.
	if rows, qerr := db.Query(`SELECT status, COUNT(*) FROM tasks GROUP BY status`); qerr != nil {
		f.Operation = MuseOpUnknown
	} else {
		func() {
			defer rows.Close()
			for rows.Next() {
				var st string
				var n int
				if err := rows.Scan(&st, &n); err != nil {
					break
				}
				switch st {
				case TaskDispatched, TaskWorking, TaskReportReady:
					f.Open += n
				case TaskFailed:
					f.Blocked += n
				}
			}
		}()
		if f.Open > 0 {
			f.Operation = MuseOpInFlight
		}
	}

	// Orchestrator lease: a live lease is verified presence.
	if n, lerr := liveMuseLeaseCount(db); lerr != nil {
		f.Lease = MuseLeaseUnknown
	} else if n > 0 {
		f.Lease = MuseLeaseLive
	} else {
		f.Lease = MuseLeaseAbsent
	}

	scanMuseEventFacts(db, &f)
	return f
}

// scanMuseEventFacts fills the connection, progress, and blocking
// facts from the durable event feed. A disconnect counts only when
// no newer traffic recovered it, and pending-approval Attention
// derives from current pending rows, never from a resolved
// historical event.
func scanMuseEventFacts(db *sql.DB, f *MuseFacts) {
	rows, err := db.Query(`SELECT event_type, disposition, created
		FROM orchestration_events ORDER BY seq DESC LIMIT 50`)
	if err != nil {
		f.Progress = MuseProgressUnknown
		return
	}
	defer rows.Close()
	type evt struct {
		typ, disp, created string
	}
	var newest *evt
	var recent []evt
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.typ, &e.disp, &e.created); err != nil {
			break
		}
		recent = append(recent, e)
	}
	if len(recent) == 0 {
		f.Progress = MuseProgressNone
	} else {
		// Rows arrived newest-first (ORDER BY seq DESC), so the
		// first element is the newest event on record.
		newest = &recent[0]
		if ts, terr := parseOrchTime(newest.created); terr == nil &&
			time.Now().UnixNano()-ts < museActiveWindow {
			f.Progress = MuseProgressFresh
		} else {
			f.Progress = MuseProgressStale
		}
	}
	// Walk newest-first so newer traffic supersedes older
	// signals: sawTraffic means traffic NEWER than the event under
	// examination, so a disconnect counts only when nothing newer
	// recovered it.
	sawTraffic := false
	for i := 0; i < len(recent); i++ {
		e := recent[i]
		switch e.typ {
		case OrchTransportDisconnected:
			if sawTraffic {
				continue
			}
			if ts, terr := parseOrchTime(e.created); terr == nil &&
				time.Now().UnixNano()-ts < museActiveWindow {
				f.Connection = MuseConnDown
				f.Blocking = MuseBlockingTransport
			}
		case OrchMessageSent:
			sawTraffic = true
			if e.disp == string(GuidanceRefused) || e.disp == string(GuidanceFailed) {
				f.Blocking = MuseBlockingRefused
			}
		case OrchMessageDelivered:
			sawTraffic = true
		}
	}
	// Attention derives from current pending state, not history: a
	// required approval that has since been decided leaves only a
	// historical event behind. Pending rows are the verified
	// signal, so they hold Attention with or without a recent
	// approval.required event.
	if n, perr := countPendingMuseApprovals(); perr == nil && n > 0 {
		// A transport outage explains itself first.
		if f.Blocking != MuseBlockingTransport {
			f.Blocking = MuseBlockingApproval
		}
	}
}

// deriveMuseState projects the Muse-tab indicator from the typed
// presence facts. Verified Active requires BOTH a live lease and
// committed open work; recent traffic alone yields the labeled
// estimate MuseActiveEstimated, never bare Active.
func deriveMuseState(f MuseFacts) string {
	switch f.Connection {
	case MuseConnDown:
		return MuseError
	case MuseConnUp:
	default:
		return MuseDisconnected
	}
	if f.Blocking == MuseBlockingTransport {
		return MuseError
	}
	if f.Blocking == MuseBlockingApproval || f.Blocking == MuseBlockingRefused {
		return MuseAttention
	}
	if f.Operation == MuseOpInFlight {
		if f.Lease == MuseLeaseLive {
			// Live lease plus committed open work: the only
			// verified Active. Needs no recent traffic.
			return MuseActive
		}
		// Open work with no live orchestrator: abandoned,
		// not active.
		return MuseDelegated
	}
	if f.Lease == MuseLeaseLive {
		return MuseConnected
	}
	if f.Progress == MuseProgressFresh {
		// Recent traffic with no lease and no open work: kept
		// as a labeled estimate, never verified activity.
		return MuseActiveEstimated
	}
	return MuseIdle
}

// String renders one presence fact for the CONTROLLER pane: the
// separated axes are shown verbatim so connection, lease, and
// operation are never conflated into the derived state word.
func (c MuseConnection) String() string {
	switch c {
	case MuseConnDown:
		return "down"
	case MuseConnUp:
		return "up"
	default:
		return "unknown"
	}
}

func (l MuseLease) String() string {
	switch l {
	case MuseLeaseLive:
		return "live"
	case MuseLeaseAbsent:
		return "absent"
	default:
		return "unknown"
	}
}

func (o MuseOperation) String() string {
	switch o {
	case MuseOpInFlight:
		return "in-flight"
	case MuseOpNone:
		return "none"
	default:
		return "unknown"
	}
}

func (p MuseProgress) String() string {
	switch p {
	case MuseProgressFresh:
		return "fresh"
	case MuseProgressStale:
		return "stale"
	case MuseProgressNone:
		return "none"
	default:
		return "unknown"
	}
}

func (b MuseBlocking) String() string {
	switch b {
	case MuseBlockingTransport:
		return "transport"
	case MuseBlockingApproval:
		return "approval"
	case MuseBlockingRefused:
		return "refused"
	case MuseBlockingNone:
		return "none"
	default:
		return "unknown"
	}
}

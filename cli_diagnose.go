package main

// cli_diagnose.go: one-command bounded investigation bundle.
//
// `tui-dash diagnose --session NAME [--json]` combines the existing
// diagnostic sources into a single bounded report: session identity
// and backend, latest task (status + waiting reasons), recent phase
// transitions for the session, active input leases, and resource
// pressure. It answers "which component owns this failure" without
// manually correlating doctor, ops, registry, and lease state.
//
// Read-only by construction: repository state comes through the
// same read seams as the other diagnostics (registry, pty/logical
// listings, task readmodel, ops sink, read-only lease query); it
// opens no write handle, files no receipts, starts nothing, and
// never dials a daemon — native aliveness is recorded state.
// Redaction is mandatory: error text passes through redactOpsDetail,
// and raw transcripts are never read. The ops-sink section needs
// the TUI_DASH_OPS_DIAG=1 opt-in that produced it; without sink
// rows the section says so instead of inventing history.

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// diagnoseOpsLimit bounds the transition section; diagnoseWaitingCap
// bounds blocking reasons. The bundle never grows with fleet size:
// leases and streams filter to this session only.
const diagnoseOpsLimit = 10
const diagnoseWaitingCap = 5

type diagnoseIdentity struct {
	Name    string `json:"name"`
	Backend string `json:"backend"`
	Alive   bool   `json:"alive"`
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
	Owner   string `json:"owner,omitempty"`
	Workdir string `json:"workdir,omitempty"`
	Created string `json:"created,omitempty"`
}

type diagnoseTask struct {
	ID      string   `json:"id"`
	Run     string   `json:"run"`
	Status  string   `json:"status"`
	Attempt int      `json:"attempt"`
	Waiting []string `json:"waiting,omitempty"`
}

type diagnoseLease struct {
	Channel string `json:"channel"`
	Owner   string `json:"owner"`
	Expires string `json:"expires"`
}

type diagnoseResources struct {
	Goroutines    int    `json:"goroutines"`
	HeapAlloc     uint64 `json:"heap_alloc_bytes"`
	SenderLanes   int    `json:"sender_lanes"`
	StreamCap     int64  `json:"stream_cap_bytes"`
	StreamBytes   int64  `json:"stream_bytes"`
	StreamOverCap bool   `json:"stream_over_cap"`
}

type diagnoseBundle struct {
	Session     diagnoseIdentity `json:"session"`
	Task        *diagnoseTask    `json:"task"`
	Transitions []opsEventJSON   `json:"transitions"`
	Leases      []diagnoseLease  `json:"leases"`
	Resources   diagnoseResources `json:"resources"`
	Notes       []string         `json:"notes,omitempty"`
}

// resolveDiagnoseSession maps a display name (bare, pty:<n>,
// native:<n>) to its backend and bare name. Registry wins ties:
// a tmux name and a PTY row can share the spelling, and the
// registry is the operator-addressable session.
func resolveDiagnoseSession(display string) (bare, backend string, ok bool) {
	if _, found := loadRegistry()[display]; found {
		return display, TargetTMUX, true
	}
	name, kind := display, ""
	if i := strings.Index(display, ":"); i >= 0 {
		name, kind = display[i+1:], display[:i]
	}
	switch kind {
	case "pty":
		if metas, err := ptyListAll(); err == nil {
			for _, m := range metas {
				if m.Name == name {
					return name, TargetPTY, true
				}
			}
		}
	case "native":
		if rows, err := listNativeSessions(); err == nil {
			for _, ls := range rows {
				if ls.Name == name {
					return name, TargetMuseNative, true
				}
			}
		}
	case "":
		if metas, err := ptyListAll(); err == nil {
			for _, m := range metas {
				if m.Name == name {
					return name, TargetPTY, true
				}
			}
		}
		if rows, err := listNativeSessions(); err == nil {
			for _, ls := range rows {
				if ls.Name == name {
					return name, TargetMuseNative, true
				}
			}
		}
	}
	return "", "", false
}

// diagnoseIdentityFor builds the identity section from the
// authoritative store of the resolved backend.
func diagnoseIdentityFor(display, bare, backend string) diagnoseIdentity {
	id := diagnoseIdentity{Name: display, Backend: backend}
	switch backend {
	case TargetTMUX:
		e := loadRegistry()[bare]
		id.Harness, id.Model, id.Owner = e.Harness, e.Model, e.Owner
		id.Workdir, id.Created = e.Workdir, e.Created
		id.Alive = sessionRunning(bare)
	case TargetPTY:
		if metas, err := ptyListAll(); err == nil {
			for _, m := range metas {
				if m.Name == bare {
					id.Harness, id.Model = m.Harness, m.Model
					id.Owner = m.Owner
					id.Workdir = m.Workdir
					id.Alive = workerSessionAlive("pty:" + bare)
				}
			}
		}
	case TargetMuseNative:
		if rows, err := listNativeSessions(); err == nil {
			for _, ls := range rows {
				if ls.Name == bare {
					id.Harness, id.Model = ls.Harness, ls.Model
					id.Workdir = ls.Workspace
					id.Created = ls.Created
					id.Alive = ls.State == "active"
				}
			}
		}
	}
	return id
}

// diagnoseTaskFor finds the newest task bound to any agent spelling
// of the session and projects its status plus waiting reasons.
// A missing task row is an empty task section, not an error:
// sessions outlive and predate tasks.
func diagnoseTaskFor(bare, display string) *diagnoseTask {
	id, err := queryLatestRun(`SELECT id FROM tasks WHERE agent IN (?, ?, ?, ?)
		ORDER BY created DESC LIMIT 1`, bare, display, "pty:"+bare, "native:"+bare)
	if err != nil || id == "" {
		return nil
	}
	tk, waiting, _, _, err := taskStatusDetail(id)
	if err != nil {
		return &diagnoseTask{ID: id}
	}
	if len(waiting) > diagnoseWaitingCap {
		waiting = waiting[:diagnoseWaitingCap]
	}
	return &diagnoseTask{ID: tk.ID, Run: tk.Run, Status: tk.Status, Attempt: tk.Attempt, Waiting: waiting}
}

// diagnoseLeasesFor lists active input leases on this session's
// channels through the read-only handle. Lease channels are
// backend-qualified ("tmux:<n>", "pty:<n>"); native turns hold no
// terminal lease, so a native session reports none.
func diagnoseLeasesFor(bare string) ([]diagnoseLease, string) {
	db, err := openReadOnlyDB()
	if err != nil {
		return nil, "leases unreadable: " + err.Error()
	}
	defer db.Close()
	rows, err := db.Query(`SELECT channel, owner, expires FROM input_leases
		WHERE channel IN (?, ?) AND expires > ?`,
		"tmux:"+bare, "pty:"+bare, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, "lease scan failed: " + err.Error()
	}
	defer rows.Close()
	var out []diagnoseLease
	for rows.Next() {
		var l diagnoseLease
		if err := rows.Scan(&l.Channel, &l.Owner, &l.Expires); err != nil {
			continue
		}
		out = append(out, l)
	}
	return out, ""
}

// diagnoseResourcesFor snapshots process pressure plus this
// session's stream usage against the rotation cap.
func diagnoseResourcesFor(bare string) diagnoseResources {
	snap := snapshotResourceUsage()
	r := diagnoseResources{
		Goroutines: snap.Goroutines, HeapAlloc: snap.HeapAlloc,
		SenderLanes: snap.SenderLanes, StreamCap: snap.StreamCap,
		StreamBytes: -1,
	}
	for _, u := range snap.Streams {
		if u.Session == bare {
			r.StreamBytes, r.StreamOverCap = u.Bytes, u.OverCap
		}
	}
	return r
}

// cliDiagnose implements `tui-dash diagnose --session NAME [--json]`.
func cliDiagnose(args []string) int {
	fs := flag.NewFlagSet("diagnose", flag.ContinueOnError)
	session := fs.String("session", "", "session display name (bare, pty:<n>, or native:<n>)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	limit := fs.Int("limit", diagnoseOpsLimit, "max transition events to include")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tui-dash diagnose --session NAME [--json] [--limit N]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Bounded read-only bundle: identity, latest task, recent")
		fmt.Fprintln(os.Stderr, "transitions, input leases, resource pressure. The")
		fmt.Fprintln(os.Stderr, "transitions section needs the TUI_DASH_OPS_DIAG=1")
		fmt.Fprintln(os.Stderr, "opt-in that produced the sink rows.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*session) == "" {
		fmt.Fprintln(os.Stderr, "diagnose needs --session NAME")
		fs.Usage()
		return 2
	}
	display := strings.TrimSpace(*session)
	bare, backend, ok := resolveDiagnoseSession(display)
	if !ok {
		fmt.Fprintf(os.Stderr, "no such session: %s\n", display)
		return 1
	}
	bundle := diagnoseBundle{Session: diagnoseIdentityFor(display, bare, backend)}
	bundle.Task = diagnoseTaskFor(bare, display)
	bundle.Transitions = []opsEventJSON{}
	if evs, err := readOpsSink(*limit); err == nil {
		kept := filterOpsEvents(evs, opsFilter{session: bare, attempt: -1}, time.Now().UTC())
		if display != bare {
			kept = append(kept, filterOpsEvents(evs, opsFilter{session: display, attempt: -1}, time.Now().UTC())...)
		}
		for _, ev := range kept {
			ev.Detail = redactOpsDetail(ev.Detail)
			bundle.Transitions = append(bundle.Transitions, opsEventToJSON(ev))
		}
		if len(bundle.Transitions) == 0 {
			bundle.Notes = append(bundle.Notes, "no ops transitions recorded for this session (sink empty or TUI_DASH_OPS_DIAG=1 was off)")
		}
	} else if os.IsNotExist(err) {
		bundle.Notes = append(bundle.Notes, "no ops sink yet ("+opsSinkPath()+"; set TUI_DASH_OPS_DIAG=1 to opt in)")
	} else {
		bundle.Notes = append(bundle.Notes, "transitions unreadable: "+err.Error())
	}
	if leases, note := diagnoseLeasesFor(bare); note != "" {
		bundle.Notes = append(bundle.Notes, note)
	} else {
		bundle.Leases = leases
		if bundle.Leases == nil {
			bundle.Leases = []diagnoseLease{}
		}
	}
	bundle.Resources = diagnoseResourcesFor(bare)
	if *asJSON {
		out, err := json.MarshalIndent(bundle, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(string(out))
		return 0
	}
	fmt.Printf("session: %s backend=%s alive=%v", bundle.Session.Name, bundle.Session.Backend, bundle.Session.Alive)
	if bundle.Session.Workdir != "" {
		fmt.Printf(" root=%s", bundle.Session.Workdir)
	}
	fmt.Println()
	if bundle.Task != nil && bundle.Task.ID != "" {
		fmt.Printf("task: %s run=%s status=%s attempt=%d\n", bundle.Task.ID, bundle.Task.Run, bundle.Task.Status, bundle.Task.Attempt)
		for _, w := range bundle.Task.Waiting {
			fmt.Printf("  blocked: %s\n", w)
		}
	} else {
		fmt.Println("task: none recorded for this session")
	}
	if len(bundle.Transitions) == 0 {
		fmt.Println("transitions: none")
	}
	for _, tr := range bundle.Transitions {
		detail := tr.Detail
		if len(detail) > opsHumanCap {
			detail = detail[:opsHumanCap] + "…"
		}
		row := fmt.Sprintf("  %s %-14s req=%s %s", tr.At, tr.Phase, tr.RequestID, tr.Disposition)
		if tr.ErrorClass != "" {
			row += " class=" + tr.ErrorClass
		}
		fmt.Printf("%s %s\n", row, detail)
	}
	if len(bundle.Leases) == 0 {
		fmt.Println("leases: none active")
	}
	for _, l := range bundle.Leases {
		fmt.Printf("  lease: %s held by %s until %s\n", l.Channel, l.Owner, l.Expires)
	}
	fmt.Printf("resources: goroutines=%d heap=%d lanes=%d stream=%d/%d overcap=%v\n",
		bundle.Resources.Goroutines, bundle.Resources.HeapAlloc, bundle.Resources.SenderLanes,
		bundle.Resources.StreamBytes, bundle.Resources.StreamCap, bundle.Resources.StreamOverCap)
	for _, n := range bundle.Notes {
		fmt.Printf("note: %s\n", n)
	}
	return 0
}

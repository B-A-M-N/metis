package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// tui_source.go: the read-model behind one dashboard refresh.
//
// collectRefreshData (tui_projection.go) used to own both the
// controller queries (registry, PTY store, tmux, tasks DB,
// orchestration feed, graph store) and the assembly of refreshData.
// The queries now live here behind the refreshSource interface; the
// projection only assembles snapshots into refreshData and formats
// nothing. Tests inject a fake refreshSource to exercise assembly
// with no tmux, filesystem, or database.
//
// One snapshots() call performs one query round (one registry load,
// one PTY list) so a refresh costs the same I/O it always did; the
// per-component snapshot structs mirror the view-state components
// (sessions, terminal content, muse feed, panels).

// sessionSnapshot is one row of the session list read-model.
type sessionSnapshot struct {
	name     string
	running  bool
	entry    Session
	identity sessionIdentity
	// logGen is the session's output generation; hasGen is false
	// when the store reports none (tmux sessions report theirs
	// through the content snapshot instead).
	logGen int
	hasGen bool
	detail tabDetail
}

// sessionIdentity keeps the display key used by navigation separate
// from the canonical backend session name used by execution paths.
type sessionIdentity struct {
	Canonical   string
	Backend     string
	Lifecycle   string
	// UnifiedLifecycle is the cross-backend running/ready/busy/
	// stopped/failed/unknown projection (session_lifecycle_unified).
	// Lifecycle keeps the backend's recorded state for compatibility.
	UnifiedLifecycle string
	TaskOccupancy    string
	Observation      string
	Provider         string
	Profile          string
}

// contentSnapshot is the selected session's terminal content.
type contentSnapshot struct {
	found  bool
	entry  Session
	live   []string
	git    []string
	hasGit bool
	events []string
	// streamGen carries the persisted stream rotation counter for
	// tmux sessions; hasStreamGen is false for PTY sessions (their
	// generation already rides the session snapshot) and for
	// unknown selections.
	streamGen    int
	hasStreamGen bool
}

// feedSnapshot is the global orchestration projection.
type feedSnapshot struct {
	feed          []OrchEvent
	ok            bool
	state         string
	open, blocked int
	// controllers lists unexpired controller presence leases
	// (verified transport identities, newest first). Recently
	// seen only — never proof of a live connection.
	controllers []musePresence
}

// panelSnapshot is the MD path-traversal banner projection.
type panelSnapshot struct {
	run   string
	lines []string
}

// refreshSnapshots groups one refresh's read-model by view-state
// component. It is pure data: no controllers, no formatting.
type refreshSnapshots struct {
	sessions []sessionSnapshot
	// sessionsOk says whether the session list is trustworthy: the
	// registry read cleanly AND the PTY query succeeded. Either
	// failure means the list may be incomplete, so the installer
	// keeps last-known sessions instead of wiping the panes.
	sessionsOk bool
	content    contentSnapshot
	identities map[string]sessionIdentity
	feed       feedSnapshot
	panel      panelSnapshot
}

// refreshSource serves the data behind one dashboard refresh. The
// production implementation queries the controllers; refresh
// assembly and rendering consume only this interface.
type refreshSource interface {
	snapshots(ctx context.Context, sel string, everyGit bool) refreshSnapshots
}

// liveRefreshSource is the production refreshSource: it queries the
// session registry, PTY store, tmux, tasks DB, orchestration feed,
// and graph store. It runs off the Bubble Tea update path (inside
// refreshCmd), never inside Update or View.
type liveRefreshSource struct{}

func (liveRefreshSource) snapshots(ctx context.Context, sel string, everyGit bool) refreshSnapshots {
	var out refreshSnapshots
	if ctx.Err() != nil {
		return out
	}
	reg, metas, metaByDisplay, batchLive, listOK := sessionsRoundContext(ctx)
	out.sessionsOk = listOK
	busyByAgent, busyKnown := sessionBusyAll()
	out.sessions = liveSessionsSnapshotContextBusy(ctx, reg, metas, metaByDisplay, batchLive, busyByAgent, busyKnown)
	nativeRows, nativeErr := listNativeSessions()
	if nativeErr != nil {
		out.sessionsOk = false
	} else {
		for _, ls := range nativeRows {
			key := nativePrefix + ls.Name
			occ := sessionOccupancyFor(key, busyByAgent, busyKnown)
			unified, alive := lifecycleForNative(ls.State, ls.NativeID != "" && harnessInteractive(ls.Harness))
			out.sessions = append(out.sessions, sessionSnapshot{
				name: key, running: alive,
				entry: Session{Name: ls.Name, Workdir: ls.Workspace, Harness: ls.Harness, Model: ls.Model},
				identity: sessionIdentity{Canonical: ls.Name, Backend: TargetMuseNative,
					Lifecycle: ls.State, UnifiedLifecycle: unified, TaskOccupancy: occ, Observation: "durable_local", Provider: ls.Provider, Profile: ls.Profile},
				detail: tabDetail{Harness: ls.Harness, Provider: ls.Provider, Profile: ls.Profile,
					Model: ls.Model, Workdir: ls.Workspace, Title: ls.Objective},
			})
		}
	}
	if ctx.Err() != nil {
		return refreshSnapshots{sessionsOk: false}
	}
	sort.Slice(out.sessions, func(i, j int) bool { return out.sessions[i].name < out.sessions[j].name })
	assignProjectTabLabels(out.sessions)
	out.content = liveContentSnapshotContext(ctx, reg, metaByDisplay, sel, everyGit)
	if strings.HasPrefix(sel, nativePrefix) {
		out.content = liveNativeContentSnapshot(nativeRows, strings.TrimPrefix(sel, nativePrefix))
	}
	out.feed = liveFeedSnapshot()
	// The path projection is a pure graph snapshot collected with
	// every refresh; the installer keeps last-known on miss. The
	// toggle state and its status text read it; the primary worker
	// view carries no banner (drawer contract).
	out.panel = livePanelSnapshot(strings.HasPrefix(sel, nativePrefix), sel)
	if ctx.Err() != nil {
		return refreshSnapshots{sessionsOk: false}
	}
	return out
}

// sessionsRound performs one shared query round for the sessions
// pane: one registry load, one PTY list, one batched tmux
// liveness classification. Pane builders below share these maps
// instead of reopening stores per session.
func sessionsRound() (reg map[string]Session, metas []ptyMeta, metaByDisplay map[string]ptyMeta, batchLive map[string]bool, listOK bool) {
	return sessionsRoundContext(context.Background())
}

func sessionsRoundContext(ctx context.Context) (reg map[string]Session, metas []ptyMeta, metaByDisplay map[string]ptyMeta, batchLive map[string]bool, listOK bool) {
	var regOK bool
	reg, regOK = loadRegistryKnown()
	var ptyErr error
	metas, ptyErr = ptyListAll()
	listOK = regOK && ptyErr == nil
	metaByDisplay = make(map[string]ptyMeta, len(metas))
	for _, mt := range metas {
		metaByDisplay[ptyDisplay(mt.Name)] = mt
	}
	// One list-sessions call classifies the whole tmux fleet; a nil
	// map means the batch failed and rows fall back to per-session
	// checks.
	if lv, err := tmuxLiveSessionsContext(ctx); err == nil {
		batchLive = lv
	}
	return reg, metas, metaByDisplay, batchLive, listOK
}

// liveSessionsSnapshot builds the session-list pane from one shared
// query round. A nil batchLive falls back to per-session liveness
// checks; unknown names resolve to zero rows. One indexed titles
// query names every tab; task.Agent is the session name for tmux
// sessions and either display or bare name for PTY ones.
func liveSessionsSnapshot(reg map[string]Session, metas []ptyMeta, metaByDisplay map[string]ptyMeta, batchLive map[string]bool) []sessionSnapshot {
	return liveSessionsSnapshotContext(context.Background(), reg, metas, metaByDisplay, batchLive)
}

func liveSessionsSnapshotContext(ctx context.Context, reg map[string]Session, metas []ptyMeta, metaByDisplay map[string]ptyMeta, batchLive map[string]bool) []sessionSnapshot {
	return liveSessionsSnapshotContextBusy(ctx, reg, metas, metaByDisplay, batchLive, nil, false)
}

func liveSessionsSnapshotContextBusy(ctx context.Context, reg map[string]Session, metas []ptyMeta, metaByDisplay map[string]ptyMeta, batchLive map[string]bool, busyByAgent map[string]bool, busyKnown bool) []sessionSnapshot {
	var out []sessionSnapshot
	titles := latestTitleByAgent()
	for _, n := range combinedNames(reg, metas) {
		row := sessionSnapshot{name: n}
		row.identity = sessionIdentity{Canonical: n, Backend: TargetTMUX, Observation: "terminal_stream"}
		if mt, ok := metaByDisplay[n]; ok {
			row.identity = sessionIdentity{Canonical: mt.Name, Backend: TargetPTY, Observation: "pty_log"}
			// ptyListAll fetched every record in one query;
			// classify in memory instead of reopening the
			// database per session.
			occ := sessionOccupancyFor(n, busyByAgent, busyKnown)
			unified, alive := lifecycleForPty(ptyClassify(mt), mt.ExitCode, harnessInteractive(mt.Harness))
			row.identity.UnifiedLifecycle = unified
			row.identity.TaskOccupancy = occ
			row.running = alive
			row.entry = Session{Name: n, Workdir: mt.Workdir, Command: mt.Command,
				Harness: mt.Harness, Model: mt.Model}
			row.logGen, row.hasGen = mt.Generation, true
			title := titles[n]
			if title == "" {
				title = titles[mt.Name]
			}
			row.detail = tabDetail{Harness: mt.Harness, Model: mt.Model,
				Workdir: mt.Workdir, Title: title}
		} else if e, ok := reg[n]; ok {
			var probe bool
			if batchLive != nil {
				target := e.Tmux
				if target == "" {
					target = sessionTmuxTarget(n, "")
				}
				probe = batchLive[target]
			} else {
				probe = sessionRunningContext(ctx, n)
			}
			occ := sessionOccupancyFor(n, busyByAgent, busyKnown)
			unified, alive := lifecycleForTmux(e.Stopped, probe, harnessInteractive(e.Harness))
			row.identity.UnifiedLifecycle = unified
			row.identity.TaskOccupancy = occ
			row.running = alive
			row.entry = e
			row.detail = tabDetail{Harness: e.Harness, Provider: e.Provider, Profile: e.Profile,
				Model: e.Model, Workdir: e.Workdir, Title: titles[n]}
		}
		out = append(out, row)
	}
	return out
}

// assignProjectTabLabels gives sessions in one directory readable instance
// ordinals while keeping the canonical name in the identity map and all
// execution paths. Creation time breaks ties; the backend name is stable.
func assignProjectTabLabels(rows []sessionSnapshot) {
	type indexed struct {
		idx int
		key string
	}
	groups := map[string][]indexed{}
	for i := range rows {
		work := rows[i].detail.Workdir
		if work == "" {
			work = rows[i].entry.Workdir
		}
		project := filepath.Base(filepath.Clean(work))
		if project == "." || project == string(filepath.Separator) || project == "" {
			project = rows[i].name
		}
		rows[i].detail.Label = project
		if work != "" {
			created := rows[i].entry.Created
			groups[filepath.Clean(work)] = append(groups[filepath.Clean(work)], indexed{idx: i, key: created + "\x00" + rows[i].name})
		}
	}
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return group[i].key < group[j].key })
		for n, item := range group {
			project := filepath.Base(filepath.Clean(rows[item.idx].detail.Workdir))
			if project == "." || project == string(filepath.Separator) || project == "" {
				project = rows[item.idx].name
			}
			rows[item.idx].detail.Label = fmt.Sprintf("%s · %02d", project, n+1)
		}
	}
}

// liveNativeContentSnapshot projects durable local facts only. It
// never starts an MSP host during the periodic refresh loop.
func liveNativeContentSnapshot(rows []logicalSession, name string) contentSnapshot {
	for _, ls := range rows {
		if ls.Name != name {
			continue
		}
		lines := []string{
			"Backend: muse-native",
			"Lifecycle: " + ls.State + " (recorded)",
			"Binding: " + map[bool]string{true: "bound", false: "not bound"}[ls.NativeID != ""],
			"Native session: " + orDash(ls.NativeID),
			"Provider: " + orDash(ls.Provider),
			"Profile: " + orDash(ls.Profile),
			"Workspace: " + orDash(ls.Workspace),
			"Updated: " + orDash(ls.Updated),
			"Current task: " + nativeTaskSummary(ls.Name, ls.Workspace),
			"Turn record: " + nativeTurnSummary(ls.Name),
			"Direct message: " + nativeMessageSummary(ls.Name),
		}
		return contentSnapshot{found: true, entry: Session{Name: ls.Name, Workdir: ls.Workspace,
			Harness: ls.Harness, Model: ls.Model}, live: lines}
	}
	return contentSnapshot{}
}

func nativeTaskSummary(name, workspace string) string {
	id, err := queryLatestRun(`SELECT id FROM tasks WHERE agent = ? AND execution_root = ?
		ORDER BY created DESC LIMIT 1`, name, workspace)
	if err != nil || id == "" {
		return "none recorded"
	}
	tk, _, _, _, err := taskStatusDetail(id)
	if err != nil {
		return id + " (status unavailable)"
	}
	return id + " (" + tk.Status + ")"
}

// nativeTurnSummary reads an already persisted admission record only;
// it does not contact Muse and does not claim terminal completion.
func nativeTurnSummary(name string) string {
	db, err := openReadOnlyDB()
	if err != nil {
		return "unavailable"
	}
	defer db.Close()
	var state, turn string
	err = db.QueryRow(`SELECT state, turn_id FROM muse_turn_intents
		WHERE session = ? ORDER BY updated DESC LIMIT 1`, name).Scan(&state, &turn)
	if err != nil {
		return "no durable terminal observation"
	}
	if turn == "" {
		return "submission " + state + "; terminal outcome not recorded here"
	}
	return "turn " + turn + " submission " + state + "; terminal outcome not recorded here"
}

// nativeMessageSummary surfaces the latest direct-message outcome
// (metis_muse_message) for one session: request ID, turn ID, and
// the durable terminal — or the unresolved state with the recovery
// pointer. Read-only and bounded (one row) against the persisted
// intents; it never contacts Muse, and the refresh path already
// polls on its own cadence, so this adds no new polling. Storage
// failure reads as unavailable, never as an invented state: only
// sql.ErrNoRows means none recorded. Completed and failed resolve
// through the same terminal=value shape so the disposition stays
// exact.
func nativeMessageSummary(name string) string {
	// A store that was never created holds nothing: "none
	// recorded" is the accurate observation, not a failure.
	if _, serr := os.Stat(tasksDBPath()); serr != nil {
		return "none recorded"
	}
	// The row read goes through the read-model seam (which owns
	// the driver import); only the found/failed distinction
	// crosses into the projection.
	req, turn, terminal, found, err := latestMuseMessageRow(name)
	if err != nil {
		return "unavailable"
	}
	if !found {
		return "none recorded"
	}
	if terminal != "" {
		return "req " + req + " turn " + turn + " terminal=" + terminal
	}
	if turn != "" {
		return "req " + req + " turn " + turn + " admitted; terminal unresolved (poll metis_muse_message_status)"
	}
	return "req " + req + " submitted; not yet admitted"
}

// liveContentSnapshot reads the selected session's terminal content.
// Read-only: a missing stream is observed, never healed here.
// Re-attachment is the session owner's job (attach path,
// registration); capture falls back to polling.
//
// Every pane read here is acquisition-bounded (F-10): PTY logs come
// through the size-bounded reverse tail reader (never a whole-file
// ReadFile), events tails carry the default byte budget, and git
// runs under probe/command deadlines with capped output. Truncation
// propagates in-band: a "... (truncated ...)" head marker rides the
// existing live/git/events slices through refreshData, so a capped
// pane never presents as complete.
func liveContentSnapshot(reg map[string]Session, metaByDisplay map[string]ptyMeta, sel string, everyGit bool) contentSnapshot {
	return liveContentSnapshotContext(context.Background(), reg, metaByDisplay, sel, everyGit)
}

func liveContentSnapshotContext(ctx context.Context, reg map[string]Session, metaByDisplay map[string]ptyMeta, sel string, everyGit bool) contentSnapshot {
	if sel == "" {
		return contentSnapshot{}
	}
	if mt, ok := metaByDisplay[sel]; ok {
		out := contentSnapshot{found: true,
			entry: Session{Name: sel, Workdir: mt.Workdir, Command: mt.Command,
				Harness: mt.Harness, Model: mt.Model}}
		// PTY output is a byte stream: render it through the
		// screen model so CR/erase overwrites paint as final
		// text, not stacked fragments (pty_screen.go). Styles
		// and alt-screen apps are not emulated (pane title).
		live, cut := boundedPTYScreenLines(mt, 500)
		if cut {
			live = markTruncated(live, "truncated: showing last 500 lines")
		}
		out.live = live
		if everyGit {
			out.git = gitInfo(mt.Workdir)
			out.hasGit = true
		}
		ev, ecut := readTailTruncated(filepath.Join(mt.Workdir, "events.jsonl"), 8)
		if ecut && len(ev) > 0 {
			ev = markTruncated(ev, "truncated: showing last 8 lines")
		}
		out.events = ev
		return out
	}
	if e, ok := reg[sel]; ok {
		out := contentSnapshot{found: true, entry: e}
		out.live = capturePaneSnapshot(ctx, sel, sessionTmuxTarget(sel, e.Tmux), 500)
		// Read-path retention backstop (TD-066/TD-106, CB-10): one
		// stat when within cap, a locked bound only when over, so a
		// high-output worker is capped on every refresh even when
		// the pane preview comes from the screen buffer.
		maintainSessionStream(sel)
		// A blank screen buffer must not hide retained history: when
		// capture-pane is empty but the stream file survived (notably
		// the rotation gap marker, TD-067), show the stream tail so
		// reopening after a headless rotation reports the discarded
		// history instead of a silent discontinuity.
		if len(out.live) == 1 && out.live[0] == "(no output yet)" {
			if ls, ok := streamTail(sel, 500); ok {
				out.live = ls
			}
		}
		if everyGit {
			out.git = gitInfo(e.Workdir)
			out.hasGit = true
		}
		// Surface the persisted stream rotation counter so the
		// model reports discarded history (including rotations
		// that happened while the dashboard was closed) instead
		// of showing a silent discontinuity.
		out.streamGen, out.hasStreamGen = streamGeneration(sel), true
		ev, ecut := readTailTruncated(filepath.Join(e.Workdir, "events.jsonl"), 8)
		if ecut && len(ev) > 0 {
			ev = markTruncated(ev, "truncated: showing last 8 lines")
		}
		out.events = ev
		return out
	}
	return contentSnapshot{}
}

// liveFeedSnapshot reads the global orchestration projection.
// Best-effort: ok=false leaves the last-known projection, flagged,
// instead of painting a miss as an empty feed.
func liveFeedSnapshot() feedSnapshot {
	var out feedSnapshot
	if feed, ferr := listLatestOrchEvents(maxOrchPage); ferr == nil {
		out.feed = feed
		out.ok = true
	}
	out.controllers = activeMusePresence()
	out.state, out.open, out.blocked = museActivity()
	if out.state == "" {
		out.state = MuseDisconnected
	}
	return out
}

// livePanelSnapshot reads the path banner projection: the graph
// lineage for the selected session's run (or the newest run when
// nothing resolves). Best-effort like the feed — a miss leaves the
// last-known banner, never a fabricated lineage.
func livePanelSnapshot(muse bool, sel string) panelSnapshot {
	run := resolvePanelRun(muse, sel)
	return panelSnapshot{run: run, lines: graphMDLines(run, panelCollectCap)}
}

// latestTitleByAgent returns the newest task objective per worker
// session. It delegates to the indexed store projection
// (latestObjectiveByAgent): one grouped query over (agent,
// created), never a full task-history scan on the refresh path. A
// missing entry means no task yet; a store error keeps last-known
// titles rather than blanking the rail.
func latestTitleByAgent() map[string]string {
	titles, err := latestObjectiveByAgent()
	if err != nil {
		return map[string]string{}
	}
	return titles
}

// assembleRefresh builds the immutable refreshData projection from
// read-model snapshots. Pure: no controllers, no formatting, no I/O.
func assembleRefresh(s refreshSnapshots, gen int, sel string) refreshData {
	out := refreshData{gen: gen, sel: sel, logGen: map[string]int{}, tabInfo: map[string]tabDetail{}, identities: map[string]sessionIdentity{}}
	out.sessionsOk = s.sessionsOk
	out.names = make([]string, 0, len(s.sessions))
	out.running = make(map[string]bool, len(s.sessions))
	for _, r := range s.sessions {
		out.names = append(out.names, r.name)
		out.running[r.name] = r.running
		if r.hasGen {
			out.logGen[r.name] = r.logGen
		}
		out.tabInfo[r.name] = r.detail
		out.identities[r.name] = r.identity
	}
	// The feed is global and gen-gated like everything else in the
	// projection: a refused generation never paints a stale feed as
	// live. A failed feed read keeps last-known data; only a
	// successful read replaces it — a miss must not masquerade as
	// empty.
	if s.feed.ok {
		out.feed = s.feed.feed
		out.controllers = s.feed.controllers
	}
	out.feedOk = s.feed.ok
	out.museState = s.feed.state
	out.museOpen = s.feed.open
	out.museBlocked = s.feed.blocked
	// The path banner is a pure graph projection: same lineage in,
	// same markdown out.
	out.panelRun = s.panel.run
	out.panel = s.panel.lines
	if s.content.found {
		out.entry = s.content.entry
		out.live = s.content.live
		out.events = s.content.events
		if s.content.hasGit {
			out.git = s.content.git
			out.hasGit = true
		}
		if s.content.hasStreamGen {
			out.logGen[sel] = s.content.streamGen
		}
	}
	return out
}

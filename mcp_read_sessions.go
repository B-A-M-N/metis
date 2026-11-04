package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// mcp_read_sessions.go: read-only session/fleet MCP tools (no
// execution side effects). Split from mcp_read_tools.go; behavior
// unchanged.

// nativePrefix namespaces muse-native sessions in the unified
// display ("native:<name>"), mirroring ptyPrefix. Transport-owned:
// listing only, never an attachment capability.
const nativePrefix = "native:"

// mcpWriteAuth binds mutations to operator identity when the server
// runs with METIS_MCP_WRITE_TOKEN set. The token travels in the
// `auth` argument; Ocliv operators inject the secret with
// `--env METIS_MCP_WRITE_TOKEN=...` so tunneled visitors never see
// it, and the child never inherits ambient credentials. Unset means
// local-stdio trust (behavior unchanged). Reads never require it.
// mcpToolStatus: read-only status projection.
func mcpToolStatus(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolStatusScope(raw, nil)
}

// mcpToolStatusScope: unscoped counts state/activity as before.
// A scoped remote reader gets counts over ITS OWN tasks/events
// only — fleet-wide aggregates require the fleet grant.
func mcpToolStatusScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	if scope == nil || scope.FleetWide {
		state, open, blocked := museActivity()
		latest, _ := listLatestOrchEvents(1)
		var seq int64
		if len(latest) > 0 {
			seq = latest[0].Seq
		}
		return mcpTextResult(map[string]any{
			"muse_state": state, "open": open, "blocked": blocked,
			"latest_event_seq": seq, "server": mcpServerName,
		})
	}
	all, err := listTasks("")
	if err != nil {
		return mcpToolError("status: %v", err)
	}
	all = filterScopedTasks(scope, all)
	open, blocked := 0, 0
	for _, tk := range all {
		switch tk.Status {
		case TaskDispatched, TaskWorking, TaskReportReady:
			open++
		case TaskFailed:
			blocked++
		}
	}
	// Bounded backward scan for the NEWEST visible event:
	// pages newest-first by seq, at most eventsScanBudgetPages,
	// so a latest authorized event outside the newest window
	// still names the progress indicator correctly.
	var seq int64
	before := int64(0)
	for page := 0; page < eventsScanBudgetPages && seq == 0; page++ {
		fetched, err := listOrchEventsBefore(before, maxOrchPage)
		if err != nil {
			return mcpToolError("status: %v", err)
		}
		if len(fetched) == 0 {
			break
		}
		for _, e := range fetched {
			if scopeAllowsEvent(scope, e) {
				seq = e.Seq
				break
			}
		}
		before = fetched[len(fetched)-1].Seq
		if len(fetched) < maxOrchPage {
			break
		}
	}
	return mcpTextResult(map[string]any{
		"open": open, "blocked": blocked,
		"latest_event_seq": seq, "server": mcpServerName,
	})
}

// mcpToolAgents: deterministic paged agent listing.
func mcpToolAgents(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolAgentsScope(raw, nil)
}

func mcpToolAgentsScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	// Deterministic order with cursor pagination: map
	// iteration order would show a different fleet per
	// call. `after` is an exclusive name cursor; `limit`
	// caps one page (default 200, hard cap 200).
	limit := 200
	after := ""
	if len(strings.TrimSpace(string(raw))) > 0 {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolResult{}, &mcpParamError{"arguments must be an object"}
		}
		if n := int(mcpNum(args, "limit")); n > 0 {
			limit = n
		}
		if limit > 200 {
			limit = 200
		}
		after = mcpStr(args, "after")
	}
	// Unified session view: the tmux registry, managed PTY
	// supervisors, and muse-native logical sessions. Display names
	// are backend-qualified ("pty:<name>", "native:<name>"); tmux
	// registry names cannot contain ':' (nameRe), so the three
	// namespaces never collide. A worker invisible here is
	// invisible to the model: metis_agents is the authoritative
	// listing, not a tmux-only projection. Listing is observation
	// only: a native entry is not a terminal attachment capability
	// (a native session is not a fake PTY).
	reg := loadRegistry()
	metas, err := ptyListAll()
	if err != nil {
		return mcpToolError("agents: %v", err)
	}
	byPty := make(map[string]ptyMeta, len(metas))
	for _, m := range metas {
		byPty[ptyDisplay(m.Name)] = m
	}
	nativeRows, err := listNativeSessions()
	if err != nil {
		return mcpToolError("agents: native sessions: %v", err)
	}
	byNative := make(map[string]logicalSession, len(nativeRows))
	for _, ls := range nativeRows {
		byNative[nativePrefix+ls.Name] = ls
	}
	names := make([]string, 0, len(reg)+len(byPty)+len(byNative))
	for name := range reg {
		names = append(names, name)
	}
	for display := range byPty {
		names = append(names, display)
	}
	for display := range byNative {
		names = append(names, display)
	}
	sort.Strings(names)
	// Scope is enforced over the full name list BEFORE
	// pagination: a scoped page never spends its slots on
	// other controllers' sessions.
	if scope != nil && !scope.FleetWide {
		filtered := names[:0]
		for _, n := range names {
			if scopeAllowsSession(scope, n) {
				filtered = append(filtered, n)
			}
		}
		names = filtered
	}
	type agentView struct {
		Name              string `json:"name"`
		Backend           string `json:"backend"`
		Harness           string `json:"harness"`
		Model             string `json:"model"`
		Provider          string `json:"provider,omitempty"`
		Profile           string `json:"profile,omitempty"`
		Alive             bool   `json:"alive"`
		Workdir           string `json:"workdir"`
		Created           string `json:"created"`
		Lifecycle         string `json:"lifecycle,omitempty"`
		Observation       string `json:"observation,omitempty"`
		UnifiedLifecycle  string `json:"unified_lifecycle"`
		TaskOccupancy     string `json:"task_occupancy"`
	}
	busyByAgent, busyKnown := sessionBusyAll()
	out := []agentView{}
	truncated := false
	for _, name := range names {
		if name <= after {
			continue
		}
		if len(out) >= limit {
			truncated = true
			break
		}
		if m, ok := byPty[name]; ok {
			occ := sessionOccupancyFor(name, busyByAgent, busyKnown)
			unified, alive := lifecycleForPty(ptyClassify(m), m.ExitCode, harnessInteractive(m.Harness))
			out = append(out, agentView{
				Name: name, Backend: TargetPTY,
				Harness: m.Harness, Model: m.Model,
				Alive:   alive,
				Workdir: m.Workdir,
				UnifiedLifecycle: unified, TaskOccupancy: occ,
			})
			continue
		}
		if ls, ok := byNative[name]; ok {
			// Recorded state only: no daemon dial, so Alive
			// reflects the stored lifecycle, not a live check.
			occ := sessionOccupancyFor(name, busyByAgent, busyKnown)
			unified, alive := lifecycleForNative(ls.State, ls.NativeID != "" && harnessInteractive(ls.Harness))
			out = append(out, agentView{
				Name: name, Backend: TargetMuseNative,
				Harness: ls.Harness, Model: ls.Model, Provider: ls.Provider, Profile: ls.Profile,
				Alive:   alive,
				Workdir: ls.Workspace, Created: ls.Created,
				Lifecycle: ls.State, Observation: "not_probed",
				UnifiedLifecycle: unified, TaskOccupancy: occ,
			})
			continue
		}
		s := reg[name]
		occ := sessionOccupancyFor(name, busyByAgent, busyKnown)
		unified, alive := lifecycleForTmux(s.Stopped, workerSessionAlive(name), harnessInteractive(s.Harness))
		out = append(out, agentView{
			Name: name, Backend: TargetTMUX,
			Harness: s.Harness, Model: s.Model, Provider: s.Provider, Profile: s.Profile,
			Alive:   alive,
			Workdir: s.Workdir, Created: s.Created,
			UnifiedLifecycle: unified, TaskOccupancy: occ,
		})
	}
	return mcpTextResult(map[string]any{"agents": out, "total": len(names), "truncated": truncated})
}

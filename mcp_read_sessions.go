package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// mcp_read_sessions.go: read-only session/fleet MCP tools (no
// execution side effects). Split from mcp_read_tools.go; behavior
// unchanged.

// mcpWriteAuth binds mutations to operator identity when the server
// runs with METIS_MCP_WRITE_TOKEN set. The token travels in the
// `auth` argument; Ocliv operators inject the secret with
// `--env METIS_MCP_WRITE_TOKEN=...` so tunneled visitors never see
// it, and the child never inherits ambient credentials. Unset means
// local-stdio trust (behavior unchanged). Reads never require it.
// mcpToolStatus: read-only status projection.
func mcpToolStatus(raw json.RawMessage) (mcpToolResult, error) {
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

// mcpToolAgents: deterministic paged agent listing.
func mcpToolAgents(raw json.RawMessage) (mcpToolResult, error) {
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
	// Unified session view: the tmux registry plus managed PTY
	// supervisors. Display names are backend-qualified
	// ("pty:<name>"); tmux registry names cannot contain ':'
	// (nameRe), so the two namespaces never collide. A worker
	// invisible here is invisible to the model: metis_agents is
	// the authoritative listing, not a tmux-only projection.
	reg := loadRegistry()
	metas, err := ptyListAll()
	if err != nil {
		return mcpToolError("agents: %v", err)
	}
	byPty := make(map[string]ptyMeta, len(metas))
	for _, m := range metas {
		byPty[ptyDisplay(m.Name)] = m
	}
	names := make([]string, 0, len(reg)+len(byPty))
	for name := range reg {
		names = append(names, name)
	}
	for display := range byPty {
		names = append(names, display)
	}
	sort.Strings(names)
	type agentView struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Harness string `json:"harness"`
		Model   string `json:"model"`
		Alive   bool   `json:"alive"`
		Workdir string `json:"workdir"`
		Created string `json:"created"`
	}
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
			out = append(out, agentView{
				Name: name, Backend: TargetPTY,
				Harness: m.Harness, Model: m.Model,
				Alive:   workerSessionAlive(name),
				Workdir: m.Workdir,
			})
			continue
		}
		s := reg[name]
		out = append(out, agentView{
			Name: name, Backend: TargetTMUX,
			Harness: s.Harness, Model: s.Model,
			Alive:   workerSessionAlive(name),
			Workdir: s.Workdir, Created: s.Created,
		})
	}
	return mcpTextResult(map[string]any{"agents": out, "total": len(names), "truncated": truncated})
}

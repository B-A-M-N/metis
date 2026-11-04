package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// mcp_read_projects.go: read-only project MCP tools (no execution
// side effects). Split from mcp_read_tools.go; behavior unchanged.

// mcpProjectInspect details one workdir: its sessions plus the
// tasks filed through those sessions, oldest first. Unknown
// workdirs return an empty inspect, never an error that could be
// mistaken for a missing project authority.
func mcpProjectInspect(workdir string) (mcpToolResult, error) {
	// Unified projection: tmux registry sessions plus managed PTY
	// supervisors in the same workdir, display-qualified.
	reg := loadRegistry()
	var names []string
	for name, s := range reg {
		if s.Workdir == workdir {
			names = append(names, name)
		}
	}
	if metas, err := ptyListAll(); err != nil {
		return mcpToolError("projects: %v", err)
	} else {
		for _, m := range metas {
			if m.Workdir == workdir {
				names = append(names, ptyDisplay(m.Name))
			}
		}
	}
	sort.Strings(names)
	inSet := map[string]bool{}
	for _, n := range names {
		inSet[n] = true
	}
	all, err := listTasks("")
	if err != nil {
		return mcpToolResult{}, err
	}
	rows := []map[string]any{}
	runs := []string{}
	seenRun := map[string]bool{}
	for _, tk := range all {
		if !inSet[tk.Agent] {
			continue
		}
		rows = append(rows, mcpTaskRow(tk))
		if !seenRun[tk.Run] {
			seenRun[tk.Run] = true
			runs = append(runs, tk.Run)
		}
	}
	sort.Strings(runs)
	return mcpTextResult(map[string]any{
		"workdir": workdir, "sessions": names, "tasks": rows, "runs": runs,
	})
}

// mcpToolProjects: project inspection.
func mcpToolProjects(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolProjectsScope(raw, nil)
}

func mcpToolProjectsScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	// Projects are a read-only projection, not a registry:
	// distinct session workdirs, sorted. With `workdir`,
	// inspect one: its sessions plus the tasks filed through
	// those sessions. No workdir is ever created or claimed.
	if len(strings.TrimSpace(string(raw))) > 0 {
		var args map[string]json.RawMessage
		if err := json.Unmarshal(raw, &args); err != nil {
			return mcpToolResult{}, &mcpParamError{"arguments must be an object"}
		}
		if wd := mcpStr(args, "workdir"); wd != "" {
			res, err := mcpProjectInspect(wd)
			if err != nil || scope == nil || scope.FleetWide {
				return res, err
			}
			// Scoped inspect: sessions/tasks limited to the
			// reader's own; a workdir with none of its own
			// sessions reads as an unknown workdir.
			var payload map[string]any
			if jerr := json.Unmarshal([]byte(res.Content[0].Text), &payload); jerr != nil {
				return res, err
			}
			sessions, _ := payload["sessions"].([]any)
			keep := []any{}
			for _, sn := range sessions {
				name, _ := sn.(string)
				if scopeAllowsSession(scope, name) {
					keep = append(keep, name)
				}
			}
			if len(keep) == 0 {
				return mcpTextResult(map[string]any{"workdir": wd, "sessions": []any{}, "tasks": []any{}, "runs": []any{}})
			}
			payload["sessions"] = keep
			return mcpTextResult(payload)
		}
	}
	reg := loadRegistry()
	byDir := map[string][]string{}
	for name, s := range reg {
		if s.Workdir == "" {
			continue
		}
		byDir[s.Workdir] = append(byDir[s.Workdir], name)
	}
	if metas, err := ptyListAll(); err != nil {
		return mcpToolError("projects: %v", err)
	} else {
		for _, m := range metas {
			if m.Workdir == "" {
				continue
			}
			byDir[m.Workdir] = append(byDir[m.Workdir], ptyDisplay(m.Name))
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	projs := []map[string]any{}
	for _, d := range dirs {
		names := byDir[d]
		sort.Strings(names)
		if scope != nil && !scope.FleetWide {
			keep := names[:0]
			for _, n := range names {
				if scopeAllowsSession(scope, n) {
					keep = append(keep, n)
				}
			}
			names = keep
			if len(names) == 0 {
				continue
			}
		}
		projs = append(projs, map[string]any{"workdir": d, "sessions": names})
	}
	return mcpTextResult(map[string]any{"projects": projs, "total": len(projs)})
}

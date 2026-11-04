package main

import (
	"encoding/json"
	"strings"
)

// mcp_read_tasks.go: read-only task MCP tools (no execution side
// effects). Split from mcp_read_tools.go; behavior unchanged.

func mcpTaskRow(tk Task) map[string]any {
	return map[string]any{
		"id": tk.ID, "run": tk.Run, "agent": tk.Agent,
		"status": tk.Status, "attempt": tk.Attempt, "parent": tk.Parent,
		"correction_no": tk.CorrectionNo, "objective": firstLine(tk.Objective),
		"created": tk.Created, "updated": tk.Updated,
	}
}

// mcpToolTasks: paged task listing.
func mcpToolTasks(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolTasksScope(raw, nil)
}

func mcpToolTasksScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	// Paginated task list, oldest first. `run` narrows to one
	// run; `after` is an exclusive task-ID cursor in the same
	// order; `limit` caps the page (default 50, hard cap 200).
	// Rows carry the objective head line; the full text lives
	// behind metis_task_status / metis_report.
	limit := 50
	after, run := "", ""
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
		run = mcpStr(args, "run")
	}
	all, err := listTasks(run)
	if err != nil {
		return mcpToolError("tasks: %v", err)
	}
	all = filterScopedTasks(scope, all)
	rows := []map[string]any{}
	truncated := false
	// Positional cursor: task IDs carry random suffixes, so
	// lexicographic comparison cannot page created order.
	// An unknown cursor yields an empty page (re-list from
	// scratch) rather than a fabricated position.
	seen := after == ""
	for _, tk := range all {
		if !seen {
			if tk.ID == after {
				seen = true
			}
			continue
		}
		if len(rows) >= limit {
			truncated = true
			break
		}
		rows = append(rows, mcpTaskRow(tk))
	}
	return mcpTextResult(map[string]any{"tasks": rows, "total": len(all), "truncated": truncated})
}

// mcpToolTaskStatus: single task status.
func mcpToolTaskStatus(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolTaskStatusScope(raw, nil)
}

func mcpToolTaskStatusScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	// Single-task detail: lifecycle state, attempt, gate
	// (unsatisfied deps), criterion verdicts for the current
	// attempt, artifact count, report presence. Read-only.
	args, err := mcpNeedArgs(raw, "task_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	tk, waiting, verdictRows, artifactCount, err := taskStatusDetail(mcpStr(args, "task_id"))
	if err != nil {
		return mcpToolError("task_status: %v", err)
	}
	if !scopeAllowsTask(scope, tk) {
		// Inaccessible ids read exactly like missing ones: no
		// existence leak to an unscoped remote reader.
		return mcpToolError("task_status: task %s not found", mcpStr(args, "task_id"))
	}
	verdicts := map[string]any{}
	for cid, v := range verdictRows {
		verdicts[cid] = map[string]any{"verdict": v.Verdict, "reviewer": v.Reviewer, "reason": v.Reason}
	}
	row := mcpTaskRow(tk)
	row["objective"] = tk.Objective
	row["acceptance"] = tk.Acceptance
	row["waiting_on"] = waiting
	row["verdicts"] = verdicts
	row["artifact_count"] = artifactCount
	row["has_report"] = strings.TrimSpace(tk.Report) != ""
	row["verified"] = tk.Status == TaskVerified
	if assurance, aerr := taskAssurance(tk.ID); aerr == nil {
		row["disposition"] = assurance.Disposition
		row["assurance"] = assurance.Assurance
		row["evidence_rev"] = assurance.EvidenceRev
	}
	return mcpTextResult(row)
}

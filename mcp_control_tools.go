package main

// mcp_control_tools.go: the three authenticated MCP control tools.
//
// metis_depend is a thin wrapper over addTaskDep: it records that a
// pending task must not start until another task in the same run is
// verified. Idempotent by request_id with exact-payload semantics:
// an identical replay converges, a reused key with a different edge
// is a conflict. metis_cancel is a thin
// wrapper over cancelTask: cancellation goes through the real path
// (native cancellation where bound), and refusing or terminal-state
// tasks are isError refusals, never silent success. metis_correct
// is a thin wrapper over followupTask: it creates a lineage-linked
// correction task (not a fresh unrelated task). Idempotent by
// request_id with exact-payload semantics: an identical (source,
// objective) replay converges, a reused key with a different
// correction is a conflict.

import (
	"encoding/json"
	"errors"
)

// mcpToolDepend: record a task dependency (idempotent by request_id).
func mcpToolDepend(raw json.RawMessage, sess *stdioSess) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "task_id", "dep_id", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args, sess)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_depend", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	taskID := mcpStr(args, "task_id")
	depID := mcpStr(args, "dep_id")
	// Key ownership lives in the domain operation: the key claim
	// and the edge mutation commit in one transaction, so a
	// concurrent same-key request converges or conflicts without
	// ever mutating domain state first.
	out, derr := addTaskDepIdempotent(taskID, depID, req, req.RequestID)
	if derr != nil {
		var kc *dependKeyConflict
		if errors.As(derr, &kc) {
			return mcpToolError("depend conflict: request_id %q already recorded edge %s -> %s; a reused key with different arguments is rejected, never converged",
				kc.RequestKey, kc.WinnerTask, kc.WinnerDep)
		}
		return mcpToolError("depend: %v", derr)
	}
	return mcpTextResult(map[string]any{"task_id": out.TaskID, "dep_id": out.DepID, "replay": out.Replay})
}

// mcpToolCancel: cancel a task through the real cancellation path.
func mcpToolCancel(raw json.RawMessage, sess *stdioSess) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "task_id", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args, sess)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_cancel", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	id := mcpStr(args, "task_id")
	if err := cancelTask(id, req); err != nil {
		return mcpToolError("cancel: %v", err)
	}
	cur, err := getTask(id)
	if err != nil {
		return mcpToolError("cancel: %v", err)
	}
	return mcpTextResult(map[string]any{"task_id": id, "status": cur.Status})
}

// mcpToolCorrect: create a lineage-linked correction task
// (idempotent by request_id).
func mcpToolCorrect(raw json.RawMessage, sess *stdioSess) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "task_id", "objective", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args, sess)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_correct", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	// Key ownership lives in the domain operation: the key claim
	// and the correction fork commit in one transaction, so a
	// concurrent same-key request converges or conflicts without
	// ever forking first.
	out, cerr := followupTaskIdempotent(mcpStr(args, "task_id"), mcpStr(args, "objective"), req, req.RequestID)
	if cerr != nil {
		var kc *correctKeyConflict
		if errors.As(cerr, &kc) {
			return mcpToolError("correct conflict: request_id %q already forked correction %s of %s; a reused key with different arguments is rejected, never converged",
				kc.RequestKey, kc.WinnerTask, kc.WinnerParent)
		}
		return mcpToolError("correct: %v", cerr)
	}
	if out.Bare {
		return mcpTextResult(map[string]any{"task_id": out.TaskID, "replay": true})
	}
	return mcpTextResult(map[string]any{"task_id": out.TaskID, "parent": out.Parent, "run": out.Run, "replay": out.Replay})
}

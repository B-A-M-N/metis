package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// mcp_attention_tools.go: operator attention over MCP.
//
// metis_attention lists escalations; metis_attention_open files
// one; metis_attention_settle resolves or dismisses one under the
// row's revision guard. Settlement is operator authority: the write
// gate plus remote-write opt-in apply, and the actor never gains
// approval power by name alone — resolve/dismiss record a decision,
// never an approval.

func mcpToolAttentionList(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "run")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	openOnly := true
	if v, ok := args["open_only"]; ok {
		var b bool
		if jerr := json.Unmarshal(v, &b); jerr == nil {
			openOnly = b
		}
	}
	attns, err := listAttention(mcpStr(args, "run"), openOnly)
	if err != nil {
		return mcpToolResult{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d attention row(s) (open_only=%v):\n", len(attns), openOnly)
	for _, a := range attns {
		fmt.Fprintf(&b, "- id=%s rev=%d status=%s kind=%s task=%s session=%s reason=%s\n",
			a.ID, a.Revision, a.Status, a.Kind, a.TaskID, a.Session, firstLine(a.Reason))
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: b.String()}}}, nil
}

func mcpToolAttentionSettle(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "id", "revision", "action", "outcome", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_attention_settle", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	by := req.Actor + " (" + principal + ")"
	rev := int(mcpNum(args, "revision"))
	id := mcpStr(args, "id")
	switch action := mcpStr(args, "action"); action {
	case "resolve":
		if err := resolveAttention(id, rev, by, mcpStr(args, "outcome")); err != nil {
			return mcpToolResult{}, err
		}
	case "dismiss":
		if err := dismissAttention(id, rev, by, mcpStr(args, "outcome")); err != nil {
			return mcpToolResult{}, err
		}
	default:
		return mcpToolResult{}, fmt.Errorf("metis_attention_settle: action must be resolve or dismiss, got %q", action)
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text",
		Text: fmt.Sprintf("attention %s settled (%s) at rev %d by %s", id, mcpStr(args, "action"), rev, by)}}}, nil
}

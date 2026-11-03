package main

// mcp_muse_message.go: authenticated metis_muse_message and
// metis_muse_message_status tools.
//
// Thin transport wrappers over sendMuseMessage and
// queryMuseMessageStatus: authenticate the caller, authorize the
// message capability, then delegate. No side effect precedes auth;
// the domain owns persistence, scope checks, and the MSP submit.
//
// The submit op returns promptly with a durable admission receipt
// (accepted + turn ID) and NEVER waits for the terminal: the MCP
// exchange budget cannot cover an unbounded turn. Terminal
// observation is a separate status poll with its own bounded wait.
// The request_id is the idempotency key: an identical retry
// reconciles the stored command instead of submitting a second
// turn, and a completed request replays its durable terminal.

import (
	"encoding/json"
	"strconv"
	"time"
)

func museMessagePayload(res MuseMessageResult) map[string]any {
	payload := map[string]any{
		"session":    res.SessionID,
		"request_id": res.RequestID,
		"state":      res.State,
		"detail":     res.Detail,
	}
	if res.TurnID != "" {
		payload["turn_id"] = res.TurnID
	}
	return payload
}

func mcpToolMuseMessage(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpAuthorize("metis_muse_message", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	msg := MuseMessageRequest{
		SessionID: mcpStr(args, "session"),
		Text:      mcpStr(args, "text"),
		RequestID: mcpStr(args, "request_id"),
		Actor:     mcpStr(args, "actor"),
	}
	res, err := sendMuseMessage(msg, req)
	if err != nil {
		return mcpToolError("muse message: %v", err)
	}
	return mcpTextResult(museMessagePayload(res))
}

// mcpMessageStatusWait parses the optional timeout_ms poll budget
// and clamps it to the domain cap. Garbage means the default.
func mcpMessageStatusWait(args map[string]json.RawMessage) time.Duration {
	if v, ok := args["timeout_ms"]; ok {
		var ms int
		if err := json.Unmarshal(v, &ms); err == nil {
			return time.Duration(ms) * time.Millisecond
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			if ms, err := strconv.Atoi(s); err == nil {
				return time.Duration(ms) * time.Millisecond
			}
		}
	}
	return 0
}

func mcpToolMuseMessageStatus(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpAuthorize("metis_muse_message_status", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	res, err := queryMuseMessageStatus(mcpStr(args, "request_id"), req, mcpMessageStatusWait(args))
	if err != nil {
		return mcpToolError("muse message status: %v", err)
	}
	return mcpTextResult(museMessagePayload(res))
}

package main

import (
	"context"
	"encoding/json"
	"time"
)

// mcp_read_approvals.go: read-only approvals MCP tool (no execution
// side effects). Split from mcp_read_tools.go; behavior unchanged.

// mcpToolApprovals: pending approvals listing.
func mcpToolApprovals(raw json.RawMessage) (mcpToolResult, error) {
	// Pending native approvals for one bound Metis session:
	// durable rows plus the server's current fold view.
	// Read-only: no lease, no decision, no receipt.
	args, err := mcpNeedArgs(raw, "session", "provider")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	session := mcpStr(args, "session")
	b, err := getMuseBinding(session)
	if err != nil {
		return mcpToolError("approvals: %v", err)
	}
	profile := mcpStr(args, "profile")
	provider := mcpStr(args, "provider")
	if err := checkMuseBindingScope(b, provider, profile); err != nil {
		return mcpToolError("approvals: %v", err)
	}
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if err := museRequireDaemonFor(c); err != nil {
		return mcpToolError("approvals: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return mcpToolError("approvals: %v", err)
	}
	defer h.Close()
	fold, err := museListPendingApprovals(ctx, h, b.NativeID)
	if err != nil {
		return mcpToolError("approvals: %v", err)
	}
	rows, err := listMuseApprovals(session, true)
	if err != nil {
		return mcpToolError("approvals: %v", err)
	}
	known := map[string]string{}
	for _, r := range rows {
		known[r.ApprovalID] = r.Status
	}
	items := []map[string]any{}
	for _, a := range fold.Approvals {
		id, _ := a["approvalId"].(string)
		items = append(items, map[string]any{
			"approval_id": id, "durable": known[id],
			"item": a["itemId"], "choices": a["availableChoices"],
		})
	}
	return mcpTextResult(map[string]any{
		"session": session, "pending": items,
		"user_inputs": fold.UserInputs,
	})
}

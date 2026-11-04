package main

import (
	"encoding/json"
)

// mcp_read_graph.go: read-only graph and orchestration-event MCP
// tools (no execution side effects). Split from mcp_read_tools.go;
// behavior unchanged.

// mcpToolEvents: orchestration event page.
func mcpToolEvents(raw json.RawMessage) (mcpToolResult, error) {
	return mcpToolEventsScope(raw, nil)
}

// mcpToolEventsScope is mcpToolEvents with a remote read scope:
// scope is enforced over a full window BEFORE the caller's limit
// is applied, so counts/cursors never leak other controllers.
func mcpToolEventsScope(raw json.RawMessage, scope *ReadScope) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw)
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	limit := int(mcpNum(args, "limit"))
	if scope != nil && !scope.FleetWide {
		// Bounded forward scanning: pages of maxOrchPage are
		// scanned (never the whole history per request) and
		// the cursor advances past every scanned row —
		// accessible or not — so a window full of foreign
		// events cannot stall an authorized reader at 0.
		// The visible-event limit is preserved.
		cursor := int64(mcpNum(args, "after_seq"))
		var visible []OrchEvent
		for {
			page, err := listOrchEvents(mcpStr(args, "run"), cursor, maxOrchPage)
			if err != nil {
				return mcpToolError("events: %v", err)
			}
			if len(page) == 0 {
				break
			}
			cursor = page[len(page)-1].Seq
			for _, e := range page {
				if scopeAllowsEvent(scope, e) {
					visible = append(visible, e)
				}
			}
			if limit > 0 && len(visible) >= limit {
				visible = visible[:limit]
				break
			}
			if len(page) < maxOrchPage {
				break
			}
		}
		return mcpTextResult(map[string]any{"events": visible, "cursor": cursor})
	}
	events, err := listOrchEvents(mcpStr(args, "run"), int64(mcpNum(args, "after_seq")), limit)
	if err != nil {
		return mcpToolError("events: %v", err)
	}
	var cursor int64
	if len(events) > 0 {
		cursor = events[len(events)-1].Seq
	}
	return mcpTextResult(map[string]any{"events": events, "cursor": cursor})
}

// mcpToolGraph: graph traversal projection.
func mcpToolGraph(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "run")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	op := mcpStr(args, "op")
	if op == "" {
		op = "walk"
	}
	var (
		v    any
		gerr error
	)
	switch op {
	case "walk":
		v, gerr = buildRunGraph(mcpStr(args, "run"))
	case "blockers":
		v, gerr = graphBlockers(mcpStr(args, "run"))
	case "ready":
		v, gerr = graphReady(mcpStr(args, "run"))
	case "coverage":
		v, gerr = graphCoverage(mcpStr(args, "run"))
	case "project":
		v, gerr = buildCoverageGraph(mcpStr(args, "run"))
	default:
		return mcpToolResult{}, &mcpParamError{"unknown graph op: walk|blockers|ready|coverage|project"}
	}
	if gerr != nil {
		return mcpTextResult(map[string]any{"ok": false, "error": gerr.Error()})
	}
	return mcpTextResult(v)
}

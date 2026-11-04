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
	// Bounded responses, normalized once for every reader
	// (scoped, fleet, and local stdio alike): omitted/zero
	// defaults to 100; above 500 clamps to 500.
	limit := int(mcpNum(args, "limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > maxOrchPage {
		limit = maxOrchPage
	}
	if scope != nil && !scope.FleetWide {
		// Bounded forward scanning with a finite page budget:
		// at most eventsScanBudgetPages database pages per
		// request (never the whole history). The cursor names
		// the last RETURNED event when the visible limit is
		// reached — authorized events beyond it in the same
		// fetched page stay reachable — and advances past
		// every scanned row when no visible event is returned
		// or the budget is exhausted, so foreign windows can
		// neither stall nor skip authorized readers.
		cursor := int64(mcpNum(args, "after_seq"))
		var visible []OrchEvent
		for page := 0; page < eventsScanBudgetPages; page++ {
			fetched, err := listOrchEvents(mcpStr(args, "run"), cursor, maxOrchPage)
			if err != nil {
				return mcpToolError("events: %v", err)
			}
			if len(fetched) == 0 {
				break
			}
			limitReached := false
			for _, e := range fetched {
				if !scopeAllowsEvent(scope, e) {
					continue
				}
				if limit > 0 && len(visible) >= limit {
					limitReached = true
					break
				}
				visible = append(visible, e)
			}
			if limitReached {
				// Cursor names the last RETURNED event;
				// remaining authorized rows in this page
				// are reachable from there.
				return mcpTextResult(map[string]any{"events": visible, "cursor": visible[len(visible)-1].Seq})
			}
			cursor = fetched[len(fetched)-1].Seq
			if len(fetched) < maxOrchPage {
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

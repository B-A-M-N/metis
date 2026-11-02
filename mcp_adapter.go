package main

import (
	"encoding/json"
	"fmt"
)

// mcp_adapter.go: thin model-facing stdio adapter over the Metis
// controller. Newline-delimited JSON-RPC 2.0 (the framing Ocliv's
// `mcp expose` stdio adapter speaks: one request line, one reply
// line, 30s exchange timeout). No HTTP listener, no auth stack, no
// second state authority — every tool calls the same controller
// functions as the CLI and TUI.
//
// tools/list advertises ONLY tools with real backends. Graph
// traversal (metis_graph_walk/coverage) is omitted until the graph
// module lands; advertising it earlier would be a lie.
//
// Safety properties:
//   - Reads never launch workers or consume inference tokens.
//   - Mutations require actor + request_id; core requireRequest and
//     idempotency keys (task key, guidance request_id, delegation
//     replay) make retried calls safe. Duplicate requests cannot
//     duplicate dispatches or messages. When METIS_MCP_WRITE_TOKEN is
//     set, mutations additionally require the matching auth argument.
//   - Verification/approval stay behind separately authorized tools
//     and are NOT exposed here: a bare reviewer string never
//     satisfies independence (TD-094).
//   - Agent views never expose session Owner tokens or raw commands.
//   - Results are bounded; long work returns durable IDs for
//     follow-up calls.

// JSON-RPC error codes.
const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
	mcpInternalError  = -32603
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "metis"
	mcpServerVersion   = "0.1.0"
)

// maxToolResult bounds one tool result for the 30s Ocliv exchange.
const maxToolResult = 256 << 10

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError,omitempty"`
}

func mcpTextResult(v any) (mcpToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcpToolResult{}, err
	}
	if len(data) > maxToolResult {
		// Never ship a byte-cut payload: a truncated JSON
		// document stops being parseable, and the caller
		// cannot tell data from cut mark. Return an explicit
		// bounded-result error naming the size and the
		// narrowing knobs instead; paginated tools already
		// accept after/limit.
		return mcpToolResult{}, &mcpTooLargeError{size: len(data), limit: maxToolResult}
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(data)}}}, nil
}

// mcpTooLargeError marks a result that exceeds the per-tool
// budget for the Ocliv exchange. Callers narrow the query
// (limit/run/task_id) and retry; nothing partial is returned.
type mcpTooLargeError struct{ size, limit int }

func (e *mcpTooLargeError) Error() string {
	return fmt.Sprintf("tool result %d bytes exceeds %d byte limit; narrow the query (limit, run, or task_id) and retry", e.size, e.limit)
}

func mcpToolError(format string, args ...any) (mcpToolResult, error) {
	return mcpToolResult{
		Content: []mcpContent{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}, nil
}

func mcpSchema(required []string, props map[string]string) any {
	p := map[string]any{}
	for k, t := range props {
		p[k] = map[string]string{"type": t}
	}
	return map[string]any{"type": "object", "properties": p, "required": required}
}

func mcpTools() []mcpTool {
	return []mcpTool{
		{"metis_status", "Controller health: Muse activity state, outstanding and blocked work, latest event sequence. Read-only.",
			mcpSchema(nil, map[string]string{})},
		{"metis_agents", "Managed sessions with backend, harness, model, and liveness, unified across the tmux registry and managed PTY supervisors (PTY names are pty:-qualified), sorted by name with cursor pagination (after/limit). Owner tokens and commands are never exposed. Read-only.",
			mcpSchema(nil, map[string]string{"after": "string", "limit": "number"})},
		{"metis_events", "Durable orchestration events after a cursor, oldest first. Read-only.",
			mcpSchema(nil, map[string]string{"run": "string", "after_seq": "number", "limit": "number"})},
		{"metis_delegate", "Create (idempotent by key) and dispatch one task to a session. Returns the durable task ID; observe via metis_report/metis_events. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"run", "session", "objective", "actor", "request_id"},
				map[string]string{"run": "string", "session": "string", "objective": "string", "acceptance": "string", "key": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_send", "Send guidance to a session (filed or live-delivered). Idempotent by request_id: replays return the original row. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "text", "actor", "request_id"},
				map[string]string{"session": "string", "text": "string", "direct": "boolean", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_report", "Full task report: task row, artifacts, receipts. Read-only.",
			mcpSchema([]string{"task_id"}, map[string]string{"task_id": "string"})},
		{"metis_reconcile", "Read-only recovery recommendation for a task: no launch, no relaunch, no state change.",
			mcpSchema([]string{"task_id"}, map[string]string{"task_id": "string"})},
		{"metis_graph", "Read-only deterministic graph projection over a run (walk, blockers, ready, coverage). No execution, no state change.",
			mcpSchema([]string{"run"}, map[string]string{"run": "string", "op": "string"})},
		{"metis_approvals", "Pending native approvals for one bound session: server fold plus durable row status. Read-only: no lease, no decision, no receipt.",
			mcpSchema([]string{"session", "provider"}, map[string]string{"session": "string", "provider": "string", "profile": "string"})},
		{"metis_approve", "Decide one pending approval the durable row already knows. Token-gated; the choice must have been offered and the stored requirement guard rides along. A name never confers approval authority by itself.",
			mcpSchema([]string{"session", "provider", "approval_id", "choice", "actor", "request_id"},
				map[string]string{"session": "string", "provider": "string", "profile": "string", "approval_id": "string", "choice": "string", "feedback": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_tasks", "Paginated task list, oldest first, optional run filter and task-ID cursor. Read-only.",
			mcpSchema(nil, map[string]string{"run": "string", "after": "string", "limit": "number"})},
		{"metis_task_status", "Single-task detail: lifecycle state, attempt, unsatisfied deps, current-attempt verdicts, artifact count. Read-only.",
			mcpSchema([]string{"task_id"}, map[string]string{"task_id": "string"})},
		{"metis_projects", "Distinct session workdirs, sorted; with workdir, its sessions plus their tasks. Read-only projection, never creates or claims.",
			mcpSchema(nil, map[string]string{"workdir": "string"})},
		{"metis_verify", "Approve (pass=true) or reject (pass=false) a reported task. pass must be explicit. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required; worker self-approval is refused.",
			mcpSchema([]string{"task_id", "pass", "actor", "request_id"},
				map[string]string{"task_id": "string", "pass": "boolean", "approve": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_provision", "Admit a worker session for a run. Use harness=muse-code for one interactive Muse Code process in tmux; pass an explicit workdir plus provider, model, and optional permission profile. objective is optional for this interactive session. The workdir must sit under a locally registered project root (project-root-register CLI). Other curated and parked sessions remain supported. Idempotent by request_id: identical replays return the original session identity; a reused key with different session, root, backend, harness, or command is a conflict. backend muse-native and native-only harnesses are refused — bind those with metis_bind. Returns session/run/backend/workdir; owner tokens and commands are never exposed. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "actor", "request_id"},
				map[string]string{"run": "string", "session": "string", "objective": "string", "workdir": "string", "backend": "string", "command": "string", "harness": "string", "provider": "string", "profile": "string", "model": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_bind", "Bind a session to a native Muse session under a commander profile. Refuses when the muse binary is missing or no session daemon serves the store; never fabricates a binding. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "profile", "actor", "request_id"},
				map[string]string{"session": "string", "profile": "string", "provider": "string", "title": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_terminal", "Read-only terminal observation for one session across backends (tmux, managed PTY): status reports backend, liveness, workdir, and native binding; output returns a bounded tail labelled with its source (stream-tail, capture-pane, or pty-log-tail) — a snapshot, never a complete event stream. Unknown or dead sessions are refused. Read-only.",
			mcpSchema([]string{"session"},
				map[string]string{"session": "string", "op": "string", "lines": "number"})},
		{"metis_terminal_input", "Authorized terminal input to one running session (tmux, managed PTY). Authenticates the caller; remote callers need the scoped admin capability or the call fails closed. Delivers through the single-writer input-lease authority under a scoped mcp:<principal>:<actor> lease identity (rival writers conflict, never interleave) on the ownership-bound target (tmux immutable session ID, PTY supervisor identity); an optional generation fences stale PTY requests. Idempotent by request_id: identical replays converge on the recorded typed outcome (queued/confirmed/blocked/rejected), a reused key with different text conflicts. Harnesses without a live-input channel report blocked, never silent success. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "text", "actor", "request_id"},
				map[string]string{"session": "string", "text": "string", "generation": "number", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_terminal_resize", "Authorized terminal resize for one running session (tmux, managed PTY). Authenticates the caller; remote callers need the scoped admin capability or the call fails closed. PTY resizes travel the lease-fenced control frame, tmux resizes address the bound session ID, both under a scoped mcp:<principal>:<actor> lease identity; an optional generation fences stale PTY requests. Idempotent by request_id: identical replays converge, a reused key with different dimensions conflicts. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "rows", "cols", "actor", "request_id"},
				map[string]string{"session": "string", "rows": "number", "cols": "number", "generation": "number", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_session_stop", "Authorized session termination (tmux, managed PTY). Authenticates the caller; remote callers need the scoped admin capability or the call fails closed. PTY stops travel the generation-fenced stop path (a session replaced mid-stop refuses instead of signalling the new owner's processes); tmux stops kill the bound session ID and mark the registry only after a confirmed kill. Stop preempts input: session leases are cleared. Idempotent by request_id: identical replays converge, a reused key with a different target conflicts. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"session", "actor", "request_id"},
				map[string]string{"session": "string", "generation": "number", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_depend", "Record that a pending task must not start until another task in the same run is verified. Idempotent by request_id: identical replays return the original edge; a reused key with a different edge is a conflict. Self-dependencies and unknown task IDs are refused. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"task_id", "dep_id", "actor", "request_id"},
				map[string]string{"task_id": "string", "dep_id": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_cancel", "Cancel a task through the real cancellation path (native cancellation where bound). Refusing or terminal-state tasks are refused, never silently succeeded. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"task_id", "actor", "request_id"},
				map[string]string{"task_id": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_correct", "Create a lineage-linked correction task for a prior task, carrying its parent binding. Idempotent by request_id: identical replays return the original correction; a reused key with a different correction is a conflict. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"task_id", "objective", "actor", "request_id"},
				map[string]string{"task_id": "string", "objective": "string", "actor": "string", "request_id": "string", "auth": "string"})},
		{"metis_attention", "List operator escalations for a run, newest posture with revision guards. Read-only.",
			mcpSchema([]string{"run"}, map[string]string{"run": "string", "open_only": "boolean"})},
		{"metis_attention_settle", "Resolve or dismiss one escalation under its revision guard. Operator authority: token-gated, records a decision, never an approval. When the server sets METIS_MCP_WRITE_TOKEN, a matching auth argument is required.",
			mcpSchema([]string{"id", "revision", "action", "outcome", "actor", "request_id"},
				map[string]string{"id": "string", "revision": "number", "action": "string", "outcome": "string", "actor": "string", "request_id": "string", "auth": "string"})},
	}
}

package main

// mcp_tools.go: JSON-RPC protocol, arg helpers, and the thin tool
// dispatcher. Read-only tools live in mcp_read_tools.go,
// authenticated mutations in mcp_write_tools.go. Nothing here owns
// state; every tool calls the existing controller.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// mcpServeWorkers bounds how many tool calls run at once on one
// connection. A stalled host (slow daemon, wedged startup) may
// occupy a worker up to its RPC budget, but the remaining workers
// keep unrelated control operations moving. Fixed pool, no
// per-request goroutine growth: the semaphore — not the client —
// decides how many run. Responses carry their request IDs, so a
// fast reply may lawfully precede a slow one on the wire.
const mcpServeWorkers = 4

// mcpServeLoop speaks newline-delimited JSON-RPC on r/w: one request
// line in, one reply line out. EOF ends the loop. It never launches
// workers on its own; tools that delegate return durable IDs.
func mcpServeLoop(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	out := bufio.NewWriter(w)
	defer out.Flush()
	var mu sync.Mutex
	slots := make(chan struct{}, mcpServeWorkers)
	var wg sync.WaitGroup
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			reply := mcpHandle(line)
			if reply == nil {
				return // notification: no reply
			}
			mu.Lock()
			out.Write(reply)
			out.Write([]byte("\n"))
			out.Flush()
			mu.Unlock()
		}()
	}
	wg.Wait()
}

// mcpHandle routes one raw request line. It returns nil for
// notifications (no id) and the reply line otherwise.
func mcpHandle(line []byte) []byte {
	var req mcpRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpParseError, Message: "parse error"}})
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: req.ID, Error: &mcpError{Code: mcpInvalidRequest, Message: "invalid request"}})
	}
	id, hasID := mcpID(req.ID)
	switch req.Method {
	case "initialize":
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"serverInfo":      map[string]string{"name": mcpServerName, "version": mcpServerVersion},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}})
	case "ping":
		out := mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{}})
		if !hasID {
			return nil
		}
		return out
	case "tools/list":
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"tools": mcpTools()}})
	case "tools/call":
		// Notifications (absent id) never execute tools: with no
		// requester to receive the result, running the call would
		// be a fire-and-forget mutation (P0-D). Other id-less
		// branches (ping, unknown methods) already stay silent.
		if !hasID {
			return nil
		}
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
			return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: mcpInvalidParams, Message: "tools/call requires name and arguments"}})
		}
		res, cerr := mcpCallTool(p.Name, p.Arguments)
		if cerr != nil {
			code := mcpInternalError
			var pe *mcpParamError
			var me *mcpMethodError
			switch {
			case errors.As(cerr, &pe):
				code = mcpInvalidParams
			case errors.As(cerr, &me):
				code = mcpMethodNotFound
			}
			return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: cerr.Error()}})
		}
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: res})
	default:
		// Unknown methods (including notifications like
		// notifications/initialized, which carry no id) are never
		// answered with fabricated capability.
		if !hasID {
			return nil
		}
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: mcpMethodNotFound, Message: "method not found: " + req.Method}})
	}
}

// mcpID normalizes the request id; hasID is false only for
// notifications (absent id). Numeric zero is a valid id and must
// round-trip: through the bridge a missing reply hangs to the
// exchange timeout and retires the adapter.
func mcpID(id any) (any, bool) {
	if id == nil {
		return nil, false
	}
	return id, true
}

func mcpMarshal(v mcpResponse) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpInternalError, Message: "marshal failed"}})
	}
	return data
}

func mcpNeedArgs(raw json.RawMessage, required ...string) (map[string]json.RawMessage, error) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("arguments must be an object")
	}
	for _, k := range required {
		v, ok := args[k]
		if !ok || strings.TrimSpace(string(v)) == "" || string(v) == "null" {
			return nil, fmt.Errorf("missing required argument %q", k)
		}
	}
	return args, nil
}

func mcpStr(args map[string]json.RawMessage, k string) string {
	var s string
	if v, ok := args[k]; ok {
		_ = json.Unmarshal(v, &s)
	}
	return s
}

func mcpBool(args map[string]json.RawMessage, k string) bool {
	var b bool
	if v, ok := args[k]; ok {
		_ = json.Unmarshal(v, &b)
	}
	return b
}

func mcpNum(args map[string]json.RawMessage, k string) float64 {
	var n float64
	if v, ok := args[k]; ok {
		_ = json.Unmarshal(v, &n)
	}
	return n
}

// mcpTaskRow is the task list projection: identity, lifecycle
// state, and the objective head line. Full text lives behind
// metis_task_status / metis_report so pages stay bounded.
func mcpCallTool(name string, raw json.RawMessage) (mcpToolResult, error) {
	switch name {
	case "metis_status":
		return mcpToolStatus(raw)
	case "metis_agents":
		return mcpToolAgents(raw)
	case "metis_events":
		return mcpToolEvents(raw)
	case "metis_delegate":
		return mcpToolDelegate(raw)
	case "metis_send":
		return mcpToolSend(raw)
	case "metis_graph":
		return mcpToolGraph(raw)
	case "metis_tasks":
		return mcpToolTasks(raw)
	case "metis_task_status":
		return mcpToolTaskStatus(raw)
	case "metis_projects":
		return mcpToolProjects(raw)
	case "metis_approvals":
		return mcpToolApprovals(raw)
	case "metis_approve":
		return mcpToolApprove(raw)
	case "metis_verify":
		return mcpToolVerify(raw)
	case "metis_provision":
		return mcpToolProvision(raw)
	case "metis_bind":
		return mcpToolBind(raw)
	case "metis_terminal":
		return mcpToolTerminal(raw)
	case "metis_terminal_input":
		return mcpToolTerminalInput(raw)
	case "metis_terminal_resize":
		return mcpToolTerminalResize(raw)
	case "metis_session_stop":
		return mcpToolSessionStop(raw)
	case "metis_depend":
		return mcpToolDepend(raw)
	case "metis_cancel":
		return mcpToolCancel(raw)
	case "metis_correct":
		return mcpToolCorrect(raw)
	case "metis_muse_message":
		return mcpToolMuseMessage(raw)
	case "metis_muse_message_status":
		return mcpToolMuseMessageStatus(raw)
	case "metis_report":
		return mcpToolReport(raw)
	case "metis_reconcile":
		return mcpToolReconcile(raw)
	case "metis_attention":
		return mcpToolAttentionList(raw)
	case "metis_attention_settle":
		return mcpToolAttentionSettle(raw)
	default:
		return mcpToolResult{}, &mcpMethodError{name}
	}
}

// mcpParamError marks invalid tool arguments (JSON-RPC -32602).
type mcpParamError struct{ msg string }

func (e *mcpParamError) Error() string { return e.msg }

// mcpMethodError marks unknown tools (JSON-RPC -32601).
type mcpMethodError struct{ name string }

func (e *mcpMethodError) Error() string { return "unknown tool: " + e.name }

// mcpServeStdio runs the adapter on stdin/stdout for `mcp serve`.
// It is the only MCP transport in Metis: remote exposure belongs to
// Ocliv, which hosts this stdio interface.
func mcpServeStdio() {
	mcpServeLoop(os.Stdin, os.Stdout)
	// The stdio transport ended: if an authenticated controller
	// used this process, its disconnect is a durable ledger fact
	// (presence would otherwise only expire silently).
	emitTransportDisconnected("mcp-local")
}

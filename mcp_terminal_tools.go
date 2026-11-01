package main

// mcp_terminal_tools.go: model-facing terminal read surface.
//
// metis_terminal exposes status and bounded output for one session
// across backends (tmux, managed PTY). It delegates to the existing
// observation implementations — streamTail/capture-pane for tmux,
// PTY log tails for supervisors — and names the output source, so a
// periodic snapshot is never mistaken for a complete event stream
// (TD-059). Observation is strictly non-destructive (TD-060): no
// input, resize, or termination lives here.

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// terminalMaxLines caps one output read: a high-output worker must
// not force an unbounded read per call.
const terminalMaxLines = 200

func mcpToolTerminal(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	session := mcpStr(args, "session")
	op := mcpStr(args, "op")
	if op == "" {
		op = "status"
	}
	lines := 50
	if n := int(mcpNum(args, "lines")); n > 0 {
		lines = n
	}
	if lines > terminalMaxLines {
		lines = terminalMaxLines
	}
	switch op {
	case "status":
		return mcpTerminalStatus(session)
	case "output":
		return mcpTerminalOutput(session, lines)
	default:
		return mcpToolResult{}, &mcpParamError{"unknown terminal op (want status|output)"}
	}
}

// terminalIdentity is one session resolved to its observation
// backend without claiming execution readiness: status reports
// what is, it never dispatches.
type terminalIdentity struct {
	backend     string
	workdir     string
	harness     string
	model       string
	alive       bool
	nativeBound bool
	nativeID    string
}

func describeTerminalSession(session string) (terminalIdentity, error) {
	if isPtySession(session) {
		m, err := ptyGet(ptyBare(session))
		if err != nil {
			return terminalIdentity{}, fmt.Errorf("terminal: %v", err)
		}
		return terminalIdentity{
			backend: TargetPTY, workdir: m.Workdir,
			harness: m.Harness, model: m.Model,
			alive: ptyClassify(m) == "running",
		}, nil
	}
	if e, ok := loadRegistry()[session]; ok {
		id := terminalIdentity{
			backend: TargetTMUX, workdir: e.Workdir,
			harness: e.Harness, model: e.Model,
			alive: !e.Stopped && sessionRunning(session),
		}
		if a, err := lookupAdapter(e.Harness); err == nil && a.ManagedTurn != nil {
			if b, err := getMuseBinding(session); err == nil && b.NativeID != "" {
				id.nativeBound = true
				id.nativeID = b.NativeID
			}
		}
		return id, nil
	}
	if b, err := getMuseBinding(session); err == nil && b.NativeID != "" {
		return terminalIdentity{
			backend: TargetMuseNative, nativeBound: true, nativeID: b.NativeID,
		}, nil
	}
	return terminalIdentity{}, fmt.Errorf("terminal: no such session: %s", session)
}

// F-08: authorized terminal lifecycle. metis_terminal stays
// read-only; input, resize, and session stop arrive here as three
// authenticated tools that delegate to the existing terminal/session
// domain services — the single-writer input-lease authority, the
// generation-fenced PTY transitions, and the ownership-bound tmux
// targets — instead of exposing unrestricted remote send-keys.
//
// Every mutation carries the same contract:
//   - F-02 ingress/capability boundary: the caller authenticates via
//     mcpAuthCaller and a remote principal additionally needs the
//     scoped admin capability; unqualified callers fail closed.
//   - Scoped lease ownership: transport acts under
//     "mcp:<principal>:<actor>", so rival writers arbitrate through
//     the lease authority and never interleave.
//   - Target identity + generation fencing: tmux acts on the bound
//     immutable session ID; PTY callers may name a generation and a
//     stale one is refused before any lease is touched.
//   - Request idempotency: request_id claims exactly one recorded
//     outcome; identical replays converge, reused keys with
//     different payloads conflict, and transient transport failures
//     leave the key resumable instead of replaying a failure.
//   - Typed outcomes: pipeline verdicts (queued/confirmed/blocked/
//     rejected/resized/stopped) return as result payloads; refusals
//     (unknown session, stale generation, held lease, conflict) are
//     isError results and never act.

// terminalInputMaxBytes caps one MCP input delivery, matching the
// PTY frame path so neither backend takes an unbounded write.
const terminalInputMaxBytes = 256 * 1024

// terminalSizeMax caps one resize dimension: a bounded sanity fence,
// far above any real terminal, that keeps a malformed request from
// reaching the transport.
const terminalSizeMax = 1024

// mcpTerminalOwner derives the caller's lease identity: transport
// principal plus asserted actor, so distinct callers hold distinct
// leases and rival writers conflict instead of interleaving.
func mcpTerminalOwner(req OperationRequest) string {
	return "mcp:" + req.Principal + ":" + req.Actor
}

// mcpTerminalGate binds the terminal lifecycle tools to the admin
// capability. The local operator holds it; a remote-token principal
// holds only granted subsets, so an unqualified remote caller fails
// closed here before any session is touched.
func mcpTerminalGate(tool string, req OperationRequest) error {
	if !req.Remote && req.Principal == mcpPrincipalLocalOperator {
		return nil
	}
	if req.Remote && mcpRemoteCapabilities()[mcpCapAdmin] {
		return nil
	}
	return fmt.Errorf("tool %s: principal %q lacks %q capability; remote callers need a scoped METIS_MCP_REMOTE_CAPABILITIES grant",
		tool, req.Principal, mcpCapAdmin)
}

// mcpTerminalConflict reports a request_id reuse with a different
// terminal payload. It is never silently folded into the recorded
// outcome.
type mcpTerminalConflict struct {
	RequestID string
	Tool      string
}

func (e *mcpTerminalConflict) Error() string {
	return fmt.Sprintf("terminal conflict: request_id %q already recorded a different %s payload; a reused key with different arguments is rejected, never converged",
		e.RequestID, e.Tool)
}

// mcpTerminalClaim claims request_id for one terminal op through
// the terminal domain's idempotency store (terminal_input_queue.go):
// the MCP adapter never opens persistence itself (boundary_test).
func mcpTerminalClaim(requestID, tool, session, payload string) (stored string, replay bool, err error) {
	return claimTerminalOp(requestID, tool, session, payload)
}

// mcpTerminalRecord fills the claimed key with the settled typed
// outcome through the terminal domain's store.
func mcpTerminalRecord(requestID, result string) {
	recordTerminalOp(requestID, result)
}

// mcpTerminalRelease drops a claimed-but-unsettled key after a
// transient transport failure through the terminal domain's store,
// so an identical retry starts fresh.
func mcpTerminalRelease(requestID string) {
	releaseTerminalOp(requestID)
}

// mcpTerminalReplayResult converges an identical replay on the
// recorded outcome, marked replay=true so a retried call is never
// mistaken for fresh work.
func mcpTerminalReplayResult(stored string) (mcpToolResult, error) {
	var v map[string]any
	if err := json.Unmarshal([]byte(stored), &v); err != nil {
		return mcpToolError("terminal: recorded outcome unreadable: %v", err)
	}
	v["replay"] = true
	return mcpTextResult(v)
}

// mcpTerminalGeneration reads the optional fencing generation: an
// absent or null generation skips the fence, a present one must be
// a non-negative integer. A non-numeric generation is a protocol
// error, never silently zero.
func mcpTerminalGeneration(args map[string]json.RawMessage) (bool, int, error) {
	raw, ok := args["generation"]
	if !ok || strings.TrimSpace(string(raw)) == "" || string(raw) == "null" {
		return false, 0, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return false, 0, fmt.Errorf("generation must be a number")
	}
	if f < 0 || f != float64(int(f)) {
		return false, 0, fmt.Errorf("generation must be a non-negative integer")
	}
	return true, int(f), nil
}

// mcpTerminalOpID mints one operation identity for a tmux delivery,
// matching the receipt shape of the PTY path.
func mcpTerminalOpID() string {
	return fmt.Sprintf("in-%d-%d", time.Now().UnixNano(), os.Getpid())
}

// terminalInputPayload is the exact-payload identity of one input
// op: the text travels as its hash (bounded rows, no content
// retention) plus its length and the fencing generation when given.
func terminalInputPayload(session, text string, hasGen bool, gen int) string {
	sum := sha256.Sum256([]byte(text))
	if hasGen {
		return fmt.Sprintf("input|%s|%x|%d|gen=%d", session, sum, len(text), gen)
	}
	return fmt.Sprintf("input|%s|%x|%d", session, sum, len(text))
}

// mcpTerminalResolve maps a session to its control backend: managed
// PTY (bare name) or owned tmux. Native-bound sessions have no
// terminal control channel and fail closed here.
func mcpTerminalResolve(session string) (backend, bare string, alive bool, err error) {
	id, derr := describeTerminalSession(session)
	if derr != nil {
		return "", "", false, derr
	}
	switch id.backend {
	case TargetPTY:
		return TargetPTY, ptyBare(session), id.alive, nil
	case TargetTMUX:
		return TargetTMUX, session, id.alive, nil
	default:
		return "", "", false, fmt.Errorf("terminal control: session %q (backend %s) has no terminal control channel", session, id.backend)
	}
}

// mcpTerminalPtyInput queues text through the PTY lease authority
// and observes the supervisor's verdict: confirmed (bytes on the
// PTY master), rejected (the supervisor refused the frame), or
// queued (accepted, not yet settled). Transport delivery is
// reported honestly — never worker comprehension.
func mcpTerminalPtyInput(name, owner, text string) (opID, disposition, detail string, rerr error) {
	id, err := ptyInput(name, owner, text)
	if err != nil {
		return "", "", "", err
	}
	for i := 0; i < 10; i++ {
		status, det, serr := ptyInputStatus(id)
		if serr != nil {
			return id, DispositionQueued, fmt.Sprintf("op %s accepted; status unreadable: %v", id, serr), nil
		}
		switch status {
		case "written_to_pty":
			return id, DispositionConfirmed, fmt.Sprintf("op %s on PTY master", id), nil
		case "error":
			return id, DispositionRejected, fmt.Sprintf("op %s refused: %s", id, det), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return id, DispositionQueued, fmt.Sprintf("op %s accepted; supervisor has not settled it yet", id), nil
}

// mcpToolTerminalInput: authorized terminal input (idempotent by
// request_id). Authenticates, gates the admin capability, fences
// the generation, claims the key, then delivers through the
// lease-authority transports — never unrestricted send-keys.
func mcpToolTerminalInput(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpTerminalGate("metis_terminal_input", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	session := mcpStr(args, "session")
	text := mcpStr(args, "text")
	hasGen, gen, gerr := mcpTerminalGeneration(args)
	if gerr != nil {
		return mcpToolResult{}, &mcpParamError{gerr.Error()}
	}
	if len(text) > terminalInputMaxBytes {
		return mcpToolError("terminal input: text too large (%d bytes, cap 256KB)", len(text))
	}
	backend, bare, alive, rerr := mcpTerminalResolve(session)
	if rerr != nil {
		return mcpToolError("%v", rerr)
	}
	if !alive {
		return mcpToolError("terminal input: session %q is not running; refusing stale input", session)
	}
	var meta ptyMeta
	if backend == TargetPTY {
		if meta, err = ptyCheckGeneration(bare, hasGen, gen); err != nil {
			return mcpToolError("%v", err)
		}
	}
	payload := terminalInputPayload(session, text, hasGen, gen)
	stored, replay, cerr := mcpTerminalClaim(req.RequestID, "metis_terminal_input", session, payload)
	if cerr != nil {
		return mcpToolError("%v", cerr)
	}
	if replay {
		return mcpTerminalReplayResult(stored)
	}
	owner := mcpTerminalOwner(req)
	var opID, disp, detail string
	if backend == TargetPTY {
		var terr error
		if opID, disp, detail, terr = mcpTerminalPtyInput(bare, owner, text); terr != nil {
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("terminal input: %v", terr)
		}
	} else {
		if terr := sendTerminalInputAsOwner(bare, owner, text); terr != nil {
			if isTerminalBlocked(terr) {
				return mcpTerminalRecorded(req.RequestID, map[string]any{
					"session": session, "backend": backend, "op": "input",
					"op_id": mcpTerminalOpID(), "disposition": DispositionBlocked,
					"detail": terr.Error(), "replay": false,
				})
			}
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("terminal input: %v", terr)
		}
		opID, disp = mcpTerminalOpID(), DispositionConfirmed
		detail = "bytes handed to tmux (worker consumption not tracked)"
	}
	out := map[string]any{
		"session": session, "backend": backend, "op": "input",
		"op_id": opID, "disposition": disp, "detail": detail, "replay": false,
	}
	if backend == TargetPTY {
		out["generation"] = meta.Generation
	}
	return mcpTerminalRecorded(req.RequestID, out)
}

// mcpTerminalRecorded records the settled outcome under the claimed
// key and returns it as the typed result.
func mcpTerminalRecorded(requestID string, out map[string]any) (mcpToolResult, error) {
	data, err := json.Marshal(out)
	if err != nil {
		return mcpToolResult{}, err
	}
	mcpTerminalRecord(requestID, string(data))
	return mcpTextResult(out)
}

// mcpTerminalSize parses one required resize dimension: present,
// numeric, an integer in 1..terminalSizeMax.
func mcpTerminalSize(args map[string]json.RawMessage, key string) (int, error) {
	raw, ok := args[key]
	if !ok || strings.TrimSpace(string(raw)) == "" || string(raw) == "null" {
		return 0, fmt.Errorf("missing required argument %q", key)
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, fmt.Errorf("%s must be a number", key)
	}
	if f < 1 || f > terminalSizeMax || f != float64(int(f)) {
		return 0, fmt.Errorf("%s must be an integer in 1..%d", key, terminalSizeMax)
	}
	return int(f), nil
}

// mcpToolTerminalResize: authorized terminal resize (idempotent by
// request_id). PTY resizes travel the lease-fenced RS frame;
// tmux resizes address the bound session ID under the same lease.
func mcpToolTerminalResize(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpTerminalGate("metis_terminal_resize", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	rows, rerr := mcpTerminalSize(args, "rows")
	if rerr != nil {
		return mcpToolResult{}, &mcpParamError{rerr.Error()}
	}
	cols, cerr := mcpTerminalSize(args, "cols")
	if cerr != nil {
		return mcpToolResult{}, &mcpParamError{cerr.Error()}
	}
	hasGen, gen, gerr := mcpTerminalGeneration(args)
	if gerr != nil {
		return mcpToolResult{}, &mcpParamError{gerr.Error()}
	}
	session := mcpStr(args, "session")
	backend, bare, alive, terr := mcpTerminalResolve(session)
	if terr != nil {
		return mcpToolError("%v", terr)
	}
	if !alive {
		return mcpToolError("terminal resize: session %q is not running; refusing stale resize", session)
	}
	var meta ptyMeta
	if backend == TargetPTY {
		if meta, err = ptyCheckGeneration(bare, hasGen, gen); err != nil {
			return mcpToolError("%v", err)
		}
	}
	var payload string
	if hasGen {
		payload = fmt.Sprintf("resize|%s|%dx%d|gen=%d", session, rows, cols, gen)
	} else {
		payload = fmt.Sprintf("resize|%s|%dx%d", session, rows, cols)
	}
	stored, replay, kerr := mcpTerminalClaim(req.RequestID, "metis_terminal_resize", session, payload)
	if kerr != nil {
		return mcpToolError("%v", kerr)
	}
	if replay {
		return mcpTerminalReplayResult(stored)
	}
	owner := mcpTerminalOwner(req)
	if backend == TargetPTY {
		if err := ptyResizeAsOwner(bare, owner, rows, cols); err != nil {
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("terminal resize: %v", err)
		}
	} else {
		if err := tmuxResizePaneAsOwner(bare, owner, rows, cols); err != nil {
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("terminal resize: %v", err)
		}
	}
	out := map[string]any{
		"session": session, "backend": backend, "op": "resize",
		"rows": rows, "cols": cols, "state": "resized", "replay": false,
	}
	if backend == TargetPTY {
		out["generation"] = meta.Generation
	}
	return mcpTerminalRecorded(req.RequestID, out)
}

// mcpToolSessionStop: authorized session termination (idempotent by
// request_id). PTY stops travel the generation-fenced ptyStop path
// (a replaced session refuses instead of signalling the new
// owner's processes); tmux stops kill the bound session ID and mark
// the registry only after a confirmed kill. Stop preempts input:
// leases are cleared so no stale or rival lease survives it.
func mcpToolSessionStop(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpTerminalGate("metis_session_stop", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	session := mcpStr(args, "session")
	hasGen, gen, gerr := mcpTerminalGeneration(args)
	if gerr != nil {
		return mcpToolResult{}, &mcpParamError{gerr.Error()}
	}
	backend, bare, _, terr := mcpTerminalResolve(session)
	if terr != nil {
		return mcpToolError("%v", terr)
	}
	var meta ptyMeta
	if backend == TargetPTY {
		if meta, err = ptyCheckGeneration(bare, hasGen, gen); err != nil {
			return mcpToolError("%v", err)
		}
	}
	var payload string
	if hasGen {
		payload = fmt.Sprintf("stop|%s|gen=%d", session, gen)
	} else {
		payload = fmt.Sprintf("stop|%s", session)
	}
	stored, replay, kerr := mcpTerminalClaim(req.RequestID, "metis_session_stop", session, payload)
	if kerr != nil {
		return mcpToolError("%v", kerr)
	}
	if replay {
		return mcpTerminalReplayResult(stored)
	}
	if backend == TargetPTY {
		if err := ptyStop(bare); err != nil {
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("session stop: %v", err)
		}
	} else {
		if err := stopSession(bare); err != nil {
			mcpTerminalRelease(req.RequestID)
			return mcpToolError("session stop: %v", err)
		}
	}
	clearTerminalLeases(bare)
	out := map[string]any{
		"session": session, "backend": backend, "op": "stop",
		"state": "stopped", "replay": false,
	}
	if backend == TargetPTY {
		out["generation"] = meta.Generation
	}
	return mcpTerminalRecorded(req.RequestID, out)
}

func mcpTerminalStatus(session string) (mcpToolResult, error) {
	id, err := describeTerminalSession(session)
	if err != nil {
		return mcpToolError("%v", err)
	}
	return mcpTextResult(map[string]any{
		"session": session, "backend": id.backend, "alive": id.alive,
		"workdir": id.workdir, "harness": id.harness, "model": id.model,
		"native_bound": id.nativeBound, "native_id": id.nativeID,
	})
}

func mcpTerminalOutput(session string, lines int) (mcpToolResult, error) {
	id, err := describeTerminalSession(session)
	if err != nil {
		return mcpToolError("%v", err)
	}
	if !id.alive {
		return mcpToolError("terminal: session %q is not running; refusing stale output", session)
	}
	switch id.backend {
	case TargetPTY:
		got := readTail(ptyLogPath(ptyBare(session)), lines)
		return mcpTextResult(map[string]any{
			"session": session, "backend": TargetPTY,
			"lines": got, "source": "pty-log-tail", "empty": len(got) == 0,
		})
	default:
		if ls, ok := streamTail(session, lines); ok {
			return mcpTextResult(map[string]any{
				"session": session, "backend": TargetTMUX,
				"lines": ls, "source": "stream-tail",
			})
		}
		// No stream file: fall back to a live capture-pane
		// snapshot, labelled as such — never presented as a
		// complete stream.
		out, err := tmux("capture-pane", "-p", "-t", tmuxName(session), "-S", fmt.Sprintf("-%d", lines))
		if err != nil {
			return mcpToolError("terminal: capture failed for %q: %v", session, err)
		}
		ls := strings.Split(strings.TrimRight(out, "\n"), "\n")
		for len(ls) > 0 && strings.TrimSpace(ls[len(ls)-1]) == "" {
			ls = ls[:len(ls)-1]
		}
		if len(ls) == 0 || (len(ls) == 1 && ls[0] == "") {
			ls = []string{}
		}
		return mcpTextResult(map[string]any{
			"session": session, "backend": TargetTMUX,
			"lines": ls, "source": "capture-pane", "empty": len(ls) == 0,
		})
	}
}

package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// mcp_write_tools.go: authenticated mutating MCP tools.
// Extracted from mcp_tools.go; behavior unchanged.

func mcpWriteAuth(args map[string]json.RawMessage) error {
	_, _, err := mcpAuthCaller(args)
	return err
}

// Authenticated MCP principals. The principal is transport
// identity only — it never adopts the caller-supplied actor
// string, which remains an asserted label.
const (
	mcpPrincipalLocalOperator = "local-operator"
	mcpPrincipalRemoteToken   = "remote-token"
)

// Scoped MCP capabilities. Task management, native approval,
// verification, rejection, and host-level administration are
// distinct: holding one never implies another. The remote-token
// principal only ever holds granted subsets of
// tasks/admin/reject/message;
// approve and verify are local-operator-only (their domain gates
// deny remote principals regardless, and the boundary check below
// refuses first). Rejection (pass=false) is severable from
// verification (pass=true): retracting a claim confers no approval,
// but it is still an authoritative state transition, so it needs
// its own explicit grant (P0-C) rather than riding on bare token
// auth or the legacy blanket write opt-in.
const (
	mcpCapTasks   = "tasks"
	mcpCapApprove = "approve"
	mcpCapVerify  = "verify"
	mcpCapReject  = "reject"
	mcpCapAdmin   = "admin"
	// mcpCapMessage is the message-only capability for direct
	// muse messaging (metis_muse_message + status). It is
	// narrower than tasks: a remote caller granted message can
	// reach bound, in-scope sessions but cannot mutate tasks,
	// and a tasks grant confers no messaging authority.
	mcpCapMessage = "message"
)

// mcpIngressMode is the declared MCP transport posture:
// LocalTrusted (this stdio serves the local operator) or
// RemoteAuthenticated (this stdio is bridged to remote callers,
// so every mutation must carry the credential).
type mcpIngressMode string

const (
	mcpIngressUnspecified mcpIngressMode = ""
	mcpIngressLocal       mcpIngressMode = "local"
	mcpIngressRemote      mcpIngressMode = "remote"
)

// mcpResolveIngressMode reads the explicit ingress declaration.
// METIS_MCP_INGRESS=local|remote wins; the legacy
// METIS_MCP_REMOTE_INGRESS=1 maps to remote. Anything else fails
// closed: an ingress declaration the operator cannot have meant
// must never silently resolve to local trust.
func mcpResolveIngressMode() (mcpIngressMode, error) {
	if v := strings.TrimSpace(os.Getenv("METIS_MCP_INGRESS")); v != "" {
		switch strings.ToLower(v) {
		case "local":
			return mcpIngressLocal, nil
		case "remote":
			return mcpIngressRemote, nil
		default:
			return "", fmt.Errorf("invalid METIS_MCP_INGRESS=%q: declare \"local\" or \"remote\"", v)
		}
	}
	if os.Getenv("METIS_MCP_REMOTE_INGRESS") == "1" {
		return mcpIngressRemote, nil
	}
	return mcpIngressUnspecified, nil
}

// mcpHostedTransport reports whether this stdio is served to remote
// callers through a bridge/host rather than to the local operator
// directly. METIS_MCP_HOSTED=1 is the generic launcher marker; the
// legacy METIS_MCP_REMOTE_INGRESS=1 exists precisely for bridged
// (Ocliv `mcp expose`) transports, so it signals hosted too.
func mcpHostedTransport() bool {
	if os.Getenv("METIS_MCP_HOSTED") == "1" {
		return true
	}
	return os.Getenv("METIS_MCP_REMOTE_INGRESS") == "1"
}

// mcpToolCapability binds each mutating tool to the capability it
// needs. Reads and unmapped names need none.
func mcpToolCapability(tool string) string {
	switch tool {
	case "metis_delegate", "metis_send",
		"metis_depend", "metis_cancel", "metis_correct",
		"metis_attention_settle":
		return mcpCapTasks
	case "metis_muse_message", "metis_muse_message_status":
		return mcpCapMessage
	case "metis_approve":
		return mcpCapApprove
	case "metis_verify":
		return mcpCapVerify
	case "metis_provision", "metis_bind", "metis_terminal":
		return mcpCapAdmin
	default:
		return ""
	}
}

// mcpRemoteCapabilities returns the capabilities granted to the
// remote-token principal. The legacy METIS_MCP_ALLOW_REMOTE_WRITE=1
// blanket opt-in grants task management and host administration
// (its historical scope, preserved — it never confers rejection);
// the scoped METIS_MCP_REMOTE_CAPABILITIES list grants exactly what
// it names, including "reject". approve and verify are never
// grantable to remote callers: approval and pass-verdict authority
// require the local operator.
func mcpRemoteCapabilities() map[string]bool {
	caps := map[string]bool{}
	if os.Getenv("METIS_MCP_ALLOW_REMOTE_WRITE") == "1" {
		caps[mcpCapTasks] = true
		caps[mcpCapAdmin] = true
	}
	for _, c := range strings.Split(os.Getenv("METIS_MCP_REMOTE_CAPABILITIES"), ",") {
		switch c = strings.ToLower(strings.TrimSpace(c)); c {
		case mcpCapTasks, mcpCapAdmin, mcpCapReject, mcpCapMessage:
			caps[c] = true
		}
	}
	return caps
}

// mcpGrantedRemoteCapabilities is the sorted grant list for
// diagnostics. It never includes credential material.
func mcpGrantedRemoteCapabilities() []string {
	var out []string
	for c := range mcpRemoteCapabilities() {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// mcpAuthCaller authenticates the transport and reports who the
// caller is: a local operator when no shared token is configured
// on a local transport, or a remote token principal when a valid
// token is presented. The principal is transport identity only —
// it never adopts the caller-supplied actor string, which remains
// an asserted label.
//
// Fail-closed ingress (F-02): local-operator trust requires a
// local transport. A hosted transport must declare remote ingress
// AND a credential — an undeclared mode, a local declaration on a
// hosted transport, or a missing credential refuses instead of
// silently promoting a remote caller to local-operator. A
// configured credential is always required when set: declaring
// local ingress never bypasses it.
func mcpAuthCaller(args map[string]json.RawMessage) (principal string, remote bool, err error) {
	mode, merr := mcpResolveIngressMode()
	if merr != nil {
		return "", false, merr
	}
	hosted := mcpHostedTransport()
	if hosted && mode != mcpIngressRemote {
		return "", false, fmt.Errorf("hosted MCP transport without remote-ingress declaration; refusing local-operator trust (declare METIS_MCP_INGRESS=remote or METIS_MCP_REMOTE_INGRESS=1 and set METIS_MCP_WRITE_TOKEN)")
	}
	want := os.Getenv("METIS_MCP_WRITE_TOKEN")
	if want == "" {
		if mode == mcpIngressRemote {
			return "", false, fmt.Errorf("remote ingress declared but no write token configured; refusing local-operator trust (set METIS_MCP_WRITE_TOKEN)")
		}
		// Advisory only: a record failure must never fail auth.
		_ = recordMusePresence(mcpPrincipalLocalOperator, "mcp-local", musePresenceLeaseTTL)
		return mcpPrincipalLocalOperator, false, nil
	}
	if subtle.ConstantTimeCompare([]byte(mcpStr(args, "auth")), []byte(want)) != 1 {
		return "", false, fmt.Errorf("write operations require the configured auth token")
	}
	// Advisory only: a record failure must never fail auth.
	_ = recordMusePresence(mcpPrincipalRemoteToken, "mcp-remote", musePresenceLeaseTTL)
	return mcpPrincipalRemoteToken, true, nil
}

// mcpRefuseRemoteWrite keeps remote writes disabled unless the
// remote-token principal holds the tool's capability (legacy
// blanket opt-in or scoped grant). Local-operator calls always
// pass. Approval and verification authority is never granted
// here: approve and verify-pass have their own gates.
func mcpRefuseRemoteWrite(tool string, req OperationRequest) error {
	if !req.Remote {
		return nil
	}
	need := mcpToolCapability(tool)
	if need == "" {
		need = mcpCapTasks
	}
	if mcpRemoteCapabilities()[need] {
		return nil
	}
	if need == mcpCapTasks {
		return fmt.Errorf("tool %s: remote callers cannot mutate by default (principal %q); set METIS_MCP_ALLOW_REMOTE_WRITE=1 to opt in or grant %q via METIS_MCP_REMOTE_CAPABILITIES",
			tool, req.Principal, need)
	}
	return fmt.Errorf("tool %s: remote principal %q lacks %q capability; grant it via METIS_MCP_REMOTE_CAPABILITIES",
		tool, req.Principal, need)
}

// mcpAuthorize binds a tool to the authenticated principal's scoped
// capabilities. The local operator holds every capability; the
// remote-token principal holds only granted tasks/admin subsets,
// and never approve or verify — those require the local operator
// even when every grant is listed.
func mcpAuthorize(tool string, req OperationRequest) error {
	need := mcpToolCapability(tool)
	if need == "" {
		return nil
	}
	if !req.Remote && req.Principal == mcpPrincipalLocalOperator {
		return nil
	}
	// Any explicitly granted capability except approve/verify
	// authorizes a remote caller; approve and verify stay
	// local-operator-only below.
	if req.Remote && need != mcpCapApprove && need != mcpCapVerify && mcpRemoteCapabilities()[need] {
		return nil
	}
	if req.Remote && (need == mcpCapApprove || need == mcpCapVerify) {
		return fmt.Errorf("tool %s: %s authority requires the local operator (remote principal %q holds no %s capability)",
			tool, need, req.Principal, need)
	}
	return fmt.Errorf("tool %s: principal %q lacks %s capability; remote callers need a scoped METIS_MCP_REMOTE_CAPABILITIES grant",
		tool, req.Principal, need)
}

// mcpCallTool executes one named tool. Unknown names are a protocol
// error; domain failures (conflicts, missing rows) are isError
// results so the client knows which tool failed.
// mcpToolDelegate: admit and dispatch a task.
func mcpToolDelegate(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "run", "session", "objective", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_delegate", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	key := mcpStr(args, "key")
	if key == "" {
		key = "mcp:" + mcpStr(args, "request_id")
	}
	tk, err := createTaskIdem(mcpStr(args, "run"), mcpStr(args, "session"),
		mcpStr(args, "objective"), mcpStr(args, "acceptance"), key)
	if err != nil {
		return mcpToolError("delegate create: %v", err)
	}
	if tk.Status != TaskPending {
		// Idempotent replay: the task already left pending.
		return mcpTextResult(map[string]any{"task_id": tk.ID, "status": tk.Status, "replay": true})
	}
	if err := dispatchTaskWithRequest(tk.ID, mcpStr(args, "session"), req); err != nil {
		// A lost dispatch race surfaces here; report current
		// status instead of duplicating the dispatch.
		if cur, gerr := getTask(tk.ID); gerr == nil && cur.Status != TaskPending {
			return mcpTextResult(map[string]any{"task_id": tk.ID, "status": cur.Status, "replay": true})
		}
		return mcpToolError("delegate dispatch: %v", err)
	}
	return mcpTextResult(map[string]any{"task_id": tk.ID, "status": TaskDispatched})
}

// mcpToolSend: send guidance to a worker.
func mcpToolSend(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_send", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	return mcpSendOp(req, args)
}

// mcpSendOp files or live-delivers one guidance message, idempotent
// by request_id. Transport-neutral: local stdio keeps its
// refuse-remote gate above; the HTTP transport substitutes its
// scope plus capability check before funneling here.
func mcpSendOp(req OperationRequest, args map[string]json.RawMessage) (mcpToolResult, error) {
	id, err := submitGuidance(mcpStr(args, "session"), mcpStr(args, "text"), mcpBool(args, "direct"), req)
	if err != nil {
		return mcpToolError("send: %v", err)
	}
	st, _ := guidanceStatus(id)
	return mcpTextResult(map[string]any{"guidance_id": id, "state": st.State})
}

// mcpToolApprove: decide a pending approval.
func mcpToolApprove(raw json.RawMessage) (mcpToolResult, error) {
	// Decide one pending approval the durable row already
	// knows. Token-gated; the choice must have been offered
	// (refused locally otherwise) and the stored requirement
	// guard rides along. A reviewer name never confers
	// authority by itself: only offered choices on known
	// pending rows can be sent, and the decision is
	// receipted with actor identity.
	args, err := mcpNeedArgs(raw, "session", "provider", "approval_id", "choice", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	session := mcpStr(args, "session")
	req0 := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpAuthorize("metis_approve", req0); err != nil {
		// Authorization denial, not a protocol error: like
		// every domain refusal, it arrives as an isError
		// result. Only authentication failures (mcpAuthCaller
		// above) are RPC errors.
		return mcpToolError("%s", err.Error())
	}
	approvalID := mcpStr(args, "approval_id")
	choice := mcpStr(args, "choice")
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := museApproveViaBinding(session, mcpStr(args, "provider"),
		mcpStr(args, "profile"), approvalID, choice, mcpStr(args, "feedback"), req); err != nil {
		return mcpToolError("approve: %v", err)
	}
	rows, err := listMuseApprovals(session, false)
	if err != nil {
		return mcpToolError("approve: %v", err)
	}
	for _, r := range rows {
		if r.ApprovalID == approvalID {
			return mcpTextResult(map[string]any{
				"session": session, "approval_id": approvalID,
				"decision": r.Decision, "status": r.Status,
			})
		}
	}
	return mcpToolError("approve: decision sent but row %q vanished", approvalID)
}

// mcpToolVerify: verify a reported task.
func mcpToolVerify(raw json.RawMessage) (mcpToolResult, error) {
	// Approval-gated verdict on a reported task. Token-gated
	// like every mutation; TD-094 independence is enforced
	// inside verifyTask, so naming a reviewer never confers
	// approval by itself. `pass` must be explicit: an omitted
	// pass is refused, never coerced to rejection.
	args, err := mcpNeedArgs(raw, "task_id", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	rawPass, ok := args["pass"]
	if !ok {
		return mcpToolResult{}, &mcpParamError{"missing required argument \"pass\": explicit true (verify) or false (reject)"}
	}
	var pass bool
	if err := json.Unmarshal(rawPass, &pass); err != nil {
		return mcpToolResult{}, &mcpParamError{"pass must be a boolean"}
	}
	id := mcpStr(args, "task_id")
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if pass {
		// A pass verdict (or override) is verify authority:
		// the local operator only. Denial is an isError result,
		// matching domain refusals; only authentication
		// failures are RPC errors.
		if err := mcpAuthorize("metis_verify", req); err != nil {
			return mcpToolError("%s", err.Error())
		}
	} else if req.Remote {
		// Retraction (pass=false) confers no approval, but it is
		// still an authoritative state transition: the
		// remote-token principal needs the explicit "reject"
		// grant (P0-C). Bare token auth proves transport, not
		// rejection authority, and the legacy blanket write
		// opt-in does not confer it.
		if !mcpRemoteCapabilities()[mcpCapReject] {
			return mcpToolError("verify: remote principal %q lacks %q capability; grant it via METIS_MCP_REMOTE_CAPABILITIES", req.Principal, mcpCapReject)
		}
	}
	if err := verifyTask(id, pass, mcpStr(args, "approve"), req); err != nil {
		return mcpToolError("verify: %v", err)
	}
	cur, err := getTask(id)
	if err != nil {
		return mcpToolError("verify: %v", err)
	}
	return mcpTextResult(map[string]any{"task_id": id, "status": cur.Status})
}

// mcpToolReport: file a worker report.
func mcpToolReport(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "task_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	rep, err := getFullReport(mcpStr(args, "task_id"))
	if err != nil {
		return mcpToolError("report: %v", err)
	}
	return mcpTextResult(rep)
}

// mcpToolReconcile: reconcile run state.
func mcpToolReconcile(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "task_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	rec, err := reconcileTask(mcpStr(args, "task_id"))
	if err != nil {
		return mcpToolError("reconcile: %v", err)
	}
	return mcpTextResult(rec)
}

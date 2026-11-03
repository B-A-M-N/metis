package main

// mcp_provision_tools.go: the two authenticated MCP control tools.
//
// metis_provision is a thin wrapper over the ProvisionService: it
// parses, authenticates the caller, refuses remote writes, and only
// then delegates — so no side effect ever precedes auth. The domain
// outcome (ProvisionResult/ProvisionError) is translated here into
// the MCP response shape; the service itself never touches
// transport types. The request_id is the idempotency key.
// metis_agents enumerates the unified session view (tmux registry
// plus managed PTY supervisors), so provisioned sessions are
// observable regardless of backend.
//
// metis_bind is a thin wrapper over the museCmdBind path. It runs
// the same preflight the CLI runs — known commander profile,
// binary on PATH, daemon serving the store — and refuses instead
// of binding when any of them is missing, so a scrubbed MCP-exposed
// PATH (LEDGER) can never yield a fabricated binding row.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Provision lifecycle states: intent is persisted (reserved) before
// any process starts, the launch window is marked (starting), and
// the confirmed process identity is recorded (running). A crash
// between steps leaves a non-running row the next identical replay
// reconciles instead of duplicating; a failed launch is terminal
// for that attempt and an identical retry may try again. See
// provision_service.go for the full machine (plus the unknown
// disposition) and the generation-fenced transitions.
const (
	provisionReserved = "reserved"
	provisionStarting = "starting"
	provisionRunning  = "running"
	provisionFailed   = "failed"
)

// Provision backends: the execution runtime a provisioned session
// runs on. tmux and pty provisions start shell sessions; a
// muse-native provision establishes the logical session record
// (authorized root plus provider/model/profile scope) with no shell
// process — the Muse native session itself is created later by
// metis_bind, which owns its own idempotent session/start path.
const (
	provisionBackendTMUX   = "tmux"
	provisionBackendPTY    = "pty"
	provisionBackendNative = "muse-native"
)

// mcpProvisionRecord is the durable provision intent + outcome for
// one request_id. Generation is the fencing counter: every state
// change bumps it, and a write naming a stale generation never
// commits. It is not part of the payload identity.
//
// Provider and Profile ride the intent for muse-native provisions
// but are NOT persisted in mcp_provision (that table's migration is
// frozen): their durable home is the logical session record the
// native launch claims. samePayload deliberately ignores them — a
// DB-loaded prior always reads them empty, so comparing them would
// conflict every native replay. Provider pinning stays enforced at
// bind time, where a provider switch is refused.
type mcpProvisionRecord struct {
	Session    string
	Run        string
	Objective  string
	Workdir    string
	Backend    string
	Harness    string
	Command    string
	Model      string
	Provider   string
	Profile    string
	State      string
	Generation int64
}

// samePayload reports exact-payload equality: a reused request_id
// with any differing field — session, root, backend, harness, or
// command — is an idempotency conflict, never a silent
// convergence.
func (r mcpProvisionRecord) samePayload(o mcpProvisionRecord) bool {
	return r.Session == o.Session && r.Run == o.Run &&
		r.Objective == o.Objective && r.Workdir == o.Workdir &&
		r.Backend == o.Backend && r.Harness == o.Harness &&
		r.Command == o.Command && r.Model == o.Model
}

// ensureMcpProvisionTable is the compatibility entry point for the
// provision intent store. The table itself is owned by the
// provisioning repository (ProvisionRepository.Ensure); this wrapper
// keeps existing callers compiling while the domain owns the
// migration.
func ensureMcpProvisionTable(db *sql.DB) error {
	return NewProvisionRepository(db).Ensure()
}

func mcpProvisionLoad(db *sql.DB, requestID string) (mcpProvisionRecord, bool) {
	rec, _, ok := NewProvisionRepository(db).Load(requestID)
	return rec, ok
}

// mcpProvisionSetState moves request_id to a new state only when the
// row still carries the expected state at the expected generation,
// bumping the generation on success. It reports (false, nil) when a
// stale expectation or an illegal lifecycle edge refuses the write,
// so a delayed holder can never clobber the winner's disposition.
func mcpProvisionSetState(db *sql.DB, requestID, expectedState string, expectedGeneration int64, newState string) (bool, error) {
	return NewProvisionRepository(db).CompareAndSet(requestID, expectedState, expectedGeneration, newState)
}

// mcpProvisionSessionBackend is the concrete session backend behind
// the service's narrow interfaces: tmux via startSession, PTY via
// ptyStart (which carries its own generation-fenced crash recovery),
// muse-native via the logical session claim (a durable record, no
// shell process). Admission keeps incoherent payloads out — a shell
// command or shell harness on a native backend never reaches here.
type mcpProvisionSessionBackend struct{}

// LaunchProvisionSession starts one session on the requested
// backend. It implements ProvisionLauncher.
func (mcpProvisionSessionBackend) LaunchProvisionSession(want mcpProvisionRecord) error {
	if want.Backend == provisionBackendPTY {
		return ptyStart(want.Session, want.Workdir, want.Command, want.Harness, want.Model)
	}
	if want.Backend == provisionBackendNative {
		return claimNativeProvision(want)
	}
	label := want.Harness
	if label == "" {
		label = "custom"
	}
	return startSessionConfigured(want.Session, want.Workdir, want.Command, label, want.Model, want.Provider, want.Profile)
}

// provisionWorkspaceMatches reports whether a live backend record
// still names the workspace a provision intent launched: exact
// string equality first, canonical-root equality across spellings
// (symlinks, relative segments) second. Anything else — a re-homed
// entry, a deleted workdir, a different spelling that still
// canonicalizes apart — is stale, never the intent's session.
func provisionWorkspaceMatches(recordWorkdir, wantWorkdir string) bool {
	if recordWorkdir == wantWorkdir {
		return true
	}
	recordRoot, rerr := canonicalRoot(recordWorkdir)
	wantRoot, werr := canonicalRoot(wantWorkdir)
	if rerr != nil || werr != nil {
		return false
	}
	return recordRoot == wantRoot
}

// provisionTmuxEstablished reports whether the wanted tmux session is
// an established outcome of this intent: a registry entry that is
// not stopped, still bound to its immutable tmux session ID under
// ownership verification (a killed-and-recreated or foreign session
// fails ownership and is never adopted), and still naming the
// intent's workspace. Registry presence alone is NOT liveness — a
// stopped, replaced, foreign, or re-homed entry is history, and the
// replay must fail explicitly instead of adopting it as live.
func provisionTmuxEstablished(want mcpProvisionRecord) bool {
	reg := loadRegistry()
	entry, ok := reg[want.Session]
	if !ok || entry.Stopped {
		return false
	}
	if entry.Owner == "" {
		return false
	}
	if want.Harness == "muse-code" &&
		(entry.Harness != want.Harness || entry.Model != want.Model ||
			entry.Provider != want.Provider || entry.Profile != want.Profile ||
			entry.Command != want.Command) {
		return false
	}
	_, target, err := resolveOwnedTarget(reg, want.Session)
	if err != nil {
		return false
	}
	if want.Harness == "muse-code" {
		command, err := tmux("display-message", "-p", "-t", target, "#{pane_current_command}")
		if err != nil || !(strings.HasPrefix(command, "muse") || strings.HasPrefix(command, "muse-")) {
			return false
		}
	}
	return provisionWorkspaceMatches(entry.Workdir, want.Workdir)
}

// provisionTmuxLaunchEstablished waits briefly for the tmux shell to exec
// the curated Muse Code process. `new-session` returns as soon as the shell
// exists, before exec.LookPath's target becomes pane_current_command; without
// this bounded settle window a healthy launch can be marked indeterminate.
func provisionTmuxLaunchEstablished(want mcpProvisionRecord) bool {
	if want.Harness != "muse-code" {
		return provisionTmuxEstablished(want)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		if provisionTmuxEstablished(want) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// provisionPtyLive reports whether the wanted PTY session is live
// AND dispatchable right now: the supervisor and worker are both
// provably alive under their fencing identities, and the row still
// names the intent's workspace. Classification alone is not enough:
// a row that merely SAYS running with no live process behind it (a
// corpse after a reboot, a stale registration) classifies running
// through the display fallback, but nothing dispatchable exists.
// A corpse is never adopted as live.
func provisionPtyLive(want mcpProvisionRecord) bool {
	m, err := ptyGet(want.Session)
	if err != nil {
		return false
	}
	if ptyClassify(m) != "running" {
		return false
	}
	if !ptySupervisorAlive(m) || !ptyWorkerAlive(m) {
		return false
	}
	if !provisionPtyCommandMatches(m, want) {
		return false
	}
	return provisionWorkspaceMatches(m.Workdir, want.Workdir)
}

// provisionPtyLaunched reports the historical counterpart: the
// supervisor took the session, whether or not it is still
// dispatchable. Any status past starting — running, worker-gone, or
// the supervisor's own terminal word (exited/gone) — proves the
// launch landed; starting with no owner never counts. A stale
// workspace still refuses: a row for another root is not this
// intent's launch.
func provisionPtyLaunched(want mcpProvisionRecord) bool {
	m, err := ptyGet(want.Session)
	if err != nil {
		return false
	}
	if m.Status == "starting" {
		return false
	}
	if !provisionPtyCommandMatches(m, want) {
		return false
	}
	return provisionWorkspaceMatches(m.Workdir, want.Workdir)
}

// provisionPtyCommandMatches keeps muse-code replays exact: the
// curated launcher embeds provider, model, and permission profile,
// so a row recorded for another configuration is never adopted as
// this intent's launch. Other harnesses keep existing behavior —
// their rows carry no comparable curated identity.
func provisionPtyCommandMatches(m ptyMeta, want mcpProvisionRecord) bool {
	if want.Harness != "muse-code" {
		return true
	}
	return m.Command == want.Command
}

// provisionNativeEstablished reports whether the wanted native
// session is established: an active logical record with the typed
// backend and the identity this intent names (workspace,
// provider/profile scope). Establishment and verification coincide
// for a record-backed backend — the claim IS the launch — while
// dispatchability (binding plus a live daemon) is proven per use by
// resolveExecTarget, never by provisioning. A stopped or
// differently-scoped record is never adopted.
func provisionNativeEstablished(want mcpProvisionRecord) bool {
	cur, found, err := loadLogicalSession(want.Session)
	if err != nil || !found {
		return false
	}
	if cur.State != logicalActive {
		return false
	}
	return nativeIdentityMatches(cur, want)
}

// claimNativeProvision establishes the logical session behind a
// native provision: one active record per name carrying the
// authorized root and the provider/model/profile scope. No tmux
// session starts, no PTY supervisor spawns — a native session needs
// neither. An established record with the same identity converges
// (no duplicate); one with a different identity is a conflict, and
// its wording says so.
func claimNativeProvision(want mcpProvisionRecord) error {
	workspace, err := canonicalRoot(want.Workdir)
	if err != nil {
		return fmt.Errorf("provision: native workspace %q unresolvable: %v", want.Workdir, err)
	}
	claimed, err := claimLogicalSession(logicalSession{
		Name: want.Session, Backend: provisionBackendNative,
		Workspace: workspace, Harness: want.Harness, Model: want.Model,
		Provider: want.Provider, Profile: want.Profile,
		Run: want.Run, Objective: want.Objective,
	})
	if err != nil {
		return fmt.Errorf("provision: %v", err)
	}
	if claimed {
		return nil
	}
	cur, found, lerr := loadLogicalSession(want.Session)
	if lerr != nil {
		return fmt.Errorf("provision: logical session %q: %v", want.Session, lerr)
	}
	if !found {
		return fmt.Errorf("provision: logical session %q lost a concurrent claim; retry with the same request_id", want.Session)
	}
	// A stopped record is terminal: never re-adopt it, never
	// silently un-stop it. Re-provision under a new name.
	if cur.State == logicalStopped {
		return fmt.Errorf("provision: logical session %q is stopped; refusing to adopt it (provision a new session instead)", want.Session)
	}
	if nativeIdentityMatches(cur, want) {
		return nil
	}
	return fmt.Errorf("provision conflict: logical session %q already established with a different identity (backend %q workspace %q provider %q profile %q); a reused name with different arguments is rejected, never converged",
		want.Session, cur.Backend, cur.Workspace, cur.Provider, cur.Profile)
}

// ProvisionSessionLive reports whether the wanted session is already
// established on its backend in a form the replay may adopt instead
// of launching a duplicate. Backend-aware, so a tmux row never
// converges on a PTY session or vice versa; each backend proves its
// own identity (ownership + stopped-state + workspace for tmux,
// supervisor aliveness + workspace for PTY, logical record + scope
// for native). It implements ProvisionLiveness.
func (mcpProvisionSessionBackend) ProvisionSessionLive(want mcpProvisionRecord) bool {
	switch want.Backend {
	case provisionBackendPTY:
		return provisionPtyLive(want)
	case provisionBackendNative:
		return provisionNativeEstablished(want)
	default:
		return provisionTmuxEstablished(want)
	}
}

// ProvisionLaunchVerified confirms the identity behind a nil launch
// before running is recorded: the tmux ownership binding, the PTY
// supervisor's ownership (any status past starting), or the native
// logical record — each with the intent's workspace. Historically
// launched and live/dispatchable stay distinct: an exited PTY
// supervisor verifies the launch happened but is not live, and a
// native establishment verifies without proving a bound daemon. An
// unobservable session after a nil launch is indeterminate, never
// running. It implements ProvisionLiveness.
func (mcpProvisionSessionBackend) ProvisionLaunchVerified(want mcpProvisionRecord) bool {
	switch want.Backend {
	case provisionBackendPTY:
		return provisionPtyLaunched(want)
	case provisionBackendNative:
		return provisionNativeEstablished(want)
	default:
		return provisionTmuxLaunchEstablished(want)
	}
}

// launchProvisionSession starts one session on the requested
// backend. Compatibility entry point: the implementation lives on
// the session backend behind the ProvisionLauncher interface.
func launchProvisionSession(want mcpProvisionRecord) error {
	return mcpProvisionSessionBackend{}.LaunchProvisionSession(want)
}

// provisionSessionLive reports whether the wanted session already
// exists on its backend. Compatibility entry point: the
// implementation lives on the session backend behind the
// ProvisionLiveness interface.
func provisionSessionLive(want mcpProvisionRecord) bool {
	return mcpProvisionSessionBackend{}.ProvisionSessionLive(want)
}

// provisionLaunchVerified confirms the process identity behind a nil
// launch. Compatibility entry point over an explicit liveness probe
// (kept for callers that supply their own probe); the service uses
// the ProvisionLiveness interface instead.
func provisionLaunchVerified(want mcpProvisionRecord, live func(mcpProvisionRecord) bool) bool {
	if want.Backend == provisionBackendPTY {
		m, err := ptyGet(want.Session)
		if err != nil {
			return false
		}
		return m.Status != "starting"
	}
	return live(want)
}

// mcpToolProvision: admit a worker session (idempotent by request_id).
// Authentication completes before any side effect: this wrapper
// parses, authenticates, and refuses remote writes, then hands the
// authorized request to the ProvisionService, which owns the durable
// intent, the workdir, and the launch. The domain result is
// translated here into the MCP response shape.
func mcpToolProvision(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_provision", req); err != nil {
		emitProvisionOutcome(req, ProvisionResult{}, err)
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	command := mcpStr(args, "command")
	if command == "" {
		command = mcpStr(args, "cmd")
	}
	explicitCommand := strings.TrimSpace(command) != ""
	result, perr := DefaultProvisionService().Provision(ProvisionRequest{
		Actor: req.Actor, RequestID: req.RequestID, Principal: req.Principal, Remote: req.Remote,
		Run: mcpStr(args, "run"), Session: mcpStr(args, "session"),
		Objective: mcpStr(args, "objective"), Workdir: mcpStr(args, "workdir"),
		Backend: mcpStr(args, "backend"), Harness: mcpStr(args, "harness"),
		Command: command, Model: mcpStr(args, "model"),
		Provider: mcpStr(args, "provider"), Profile: mcpStr(args, "profile"),
		ExplicitCommand: explicitCommand,
	})
	emitProvisionOutcome(req, result, perr)
	if perr != nil {
		return mcpToolError("%s", perr.Error())
	}
	return provisionResultToMCP(result)
}

// emitProvisionOutcome records one controller-originated provision
// call as a provision.requested feed event with its ACTUAL terminal
// disposition (admitted, replayed, reconciled, conflict, refused).
// Origin is the server-established principal, never the asserted
// actor label — the timeline distinguishes controllers by verified
// transport identity. Best-effort like other feed emitters: a
// feed-write failure is recorded as a gap receipt and never fails
// the provision itself.
func emitProvisionOutcome(req OperationRequest, res ProvisionResult, perr error) {
	e := OrchEvent{
		OriginPrincipal: req.Principal,
		TargetAgent:     res.SessionID,
		EventType:       OrchProvisionRequested,
		RequestID:       req.RequestID,
	}
	if perr != nil {
		e.Disposition = "refused"
		var pe *ProvisionError
		if errors.As(perr, &pe) && pe.Conflict {
			e.Disposition = "conflict"
		}
		e.Payload = perr.Error()
	} else {
		e.Disposition = string(res.Disposition)
		if res.Reconciled {
			e.Disposition = "reconciled"
		} else if res.Replay {
			e.Disposition = "replayed"
		} else if res.Disposition == "" {
			e.Disposition = "admitted"
		}
		e.Payload = "session=" + res.SessionID + " backend=" + res.Backend + " run=" + res.Run
	}
	if _, err := recordOrchEvent(e); err != nil {
		recordReceipt("", "orch_gap", "provision feed write failed: "+err.Error())
	}
}

// provisionResultToMCP translates the transport-independent domain
// outcome into the MCP response shape. Fresh launches report their
// workdir with replay=false; converging replays omit the already
// reported workdir; reconciled adoptions add reconciled=true.
func provisionResultToMCP(result ProvisionResult) (mcpToolResult, error) {
	payload := map[string]any{
		"session": result.SessionID,
		"run":     result.Run,
		"backend": result.Backend,
		"replay":  result.Replay,
		"state":   string(result.Disposition),
	}
	if result.Workdir != "" {
		payload["workdir"] = result.Workdir
	}
	if result.Reconciled {
		payload["reconciled"] = true
	}
	return mcpTextResult(payload)
}

// mcpToolBind: bind a session to a native Muse session. The daemon
// and binary preflight runs before anything is recorded: when
// either is missing the tool refuses with the blocker instead of
// fabricating a binding.
func mcpToolBind(raw json.RawMessage) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	// Profile selects the commander store; the key must be
	// present, with "" meaning the default commander.
	profileRaw, ok := args["profile"]
	if !ok {
		return mcpToolResult{}, &mcpParamError{`missing required argument "profile"`}
	}
	var profile string
	_ = json.Unmarshal(profileRaw, &profile)
	principal, remote, aerr := mcpAuthCaller(args)
	if aerr != nil {
		return mcpToolResult{}, &mcpParamError{aerr.Error()}
	}
	req := OperationRequest{Actor: mcpStr(args, "actor"), RequestID: mcpStr(args, "request_id"), Principal: principal, Remote: remote}
	if err := mcpRefuseRemoteWrite("metis_bind", req); err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	session := mcpStr(args, "session")
	provider := mcpStr(args, "provider")
	title := mcpStr(args, "title")
	if title == "" {
		title = "metis:" + session
	}
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return mcpToolError("bind: %v", err)
	}
	if _, err := exec.LookPath(c.Binary); err != nil {
		return mcpToolError("bind: muse binary %q not on PATH: %v", c.Binary, err)
	}
	if err := museRequireDaemonFor(c); err != nil {
		return mcpToolError("bind: %v", err)
	}
	// Typed protocol operation: it returns values and prints
	// nothing, so the JSON-RPC stream stays protocol-clean. The
	// CLI formats its own message in museCmdBind. The MCP
	// request_id is the retry identity: the bind claims one
	// durable session/start command per key before Muse is ever
	// called, so an identical retry replays the already-created
	// session instead of minting a second one; a reused key with
	// a different payload is refused as a conflict.
	nativeID, cursor, err := bindMuseNativeSession(session, provider, profile, title, req)
	if err != nil {
		return mcpToolError("bind: %v", err)
	}
	return mcpTextResult(map[string]any{"session": session, "provider": provider, "profile": profile, "bound": true, "native_id": nativeID, "cursor": cursor})
}

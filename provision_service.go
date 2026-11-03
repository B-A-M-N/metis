package main

// provision_service.go: durable provisioning ownership (P0-C).
//
// Transport-independent domain: this file knows nothing of
// transport result shapes. It admits an
// already-authorized ProvisionRequest, records the durable intent
// first, and only then touches the filesystem or starts a process.
// The MCP handler (mcp_provision_tools.go) authenticates the caller
// first and translates the domain outcome, so no side effect ever
// precedes auth.
//
// Lifecycle (explicit dispositions, never inferred success):
//
//	reserved -> starting -> running   (verified launch)
//	reserved -> running               (verified adoption of a live session)
//	starting -> failed                (explicit launch error only)
//	starting -> unknown               (launch returned but the session
//	                                   is not observable: indeterminate)
//	failed/unknown -> starting        (identical retry may try again)
//	running                           (terminal; same payload replays
//	                                   converge, differing payloads conflict)
//
// Concurrency is fenced by (state, generation): every transition is
// a compare-and-set on the expected state+generation and bumps the
// generation, so a stale holder can neither launch twice nor
// complete (or fail) another attempt's claim. A crash between steps
// leaves a non-running row the next identical replay reconciles:
// when the named session already lives on its backend the replay
// adopts it as the confirmed outcome instead of launching a second
// one, and when the workdir is missing it is recreated from the
// recorded intent.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// provisionUnknown records an indeterminate launch outcome: the
// backend neither confirmed the process identity nor reported an
// explicit error. It is retryable like failed, and it is never
// reported as success.
const provisionUnknown = "unknown"

// ProvisionDisposition is the transport-independent outcome of a
// provision: a lifecycle state for successes, or conflict when a
// reused request_id carries a different payload.
type ProvisionDisposition string

// Provision dispositions. running/reserved/unknown/failed mirror
// the durable lifecycle states; conflict marks a reused request_id
// with differing arguments (nothing was created).
const (
	ProvisionDispositionRunning  ProvisionDisposition = "running"
	ProvisionDispositionReserved ProvisionDisposition = "reserved"
	ProvisionDispositionUnknown  ProvisionDisposition = "unknown"
	ProvisionDispositionFailed   ProvisionDisposition = "failed"
	ProvisionDispositionConflict ProvisionDisposition = "conflict"
)

// ProvisionResult is the transport-independent outcome of one
// admitted request. SessionID/Run/Backend identify the confirmed
// session; Workdir is set only on a fresh launch (replays already
// reported it); Replay marks convergence on an existing row;
// Reconciled marks adoption or repair of a crashed attempt.
type ProvisionResult struct {
	SessionID   string
	Run         string
	Backend     string
	Workdir     string
	Disposition ProvisionDisposition
	Replay      bool
	Reconciled  bool
}

// ProvisionError is the transport-independent failure of a
// provision. Conflict reports a reused request_id with a different
// payload; anything else is operational (workdir, launch, fence).
// Disposition carries the recorded terminal state (failed/unknown)
// when the failure booked one, and is empty otherwise.
type ProvisionError struct {
	Message     string
	Conflict    bool
	Disposition ProvisionDisposition
}

func (e *ProvisionError) Error() string { return e.Message }

// OpsErrorCode reports the stable diagnostic class for observeOps.
// Conflicts are typed; every other provision failure keeps the
// substring fallback (empty code is not a known class).
func (e *ProvisionError) OpsErrorCode() string {
	if e != nil && e.Conflict {
		return "conflict"
	}
	return ""
}

func provisionFailure(format string, args ...any) *ProvisionError {
	return &ProvisionError{Message: fmt.Sprintf(format, args...)}
}

func provisionFailureWithState(state ProvisionDisposition, format string, args ...any) *ProvisionError {
	return &ProvisionError{Message: fmt.Sprintf(format, args...), Disposition: state}
}

func provisionConflict(format string, args ...any) *ProvisionError {
	return &ProvisionError{Message: fmt.Sprintf(format, args...), Conflict: true, Disposition: ProvisionDispositionConflict}
}

// provisionTransitionAllowed is the lifecycle gate. running is
// terminal; every other edge needs a verified session (for running)
// or a fresh claim (for starting) enforced by the caller.
func provisionTransitionAllowed(from, to string) bool {
	switch from {
	case provisionReserved:
		return to == provisionStarting || to == provisionRunning
	case provisionStarting:
		return to == provisionStarting || to == provisionRunning ||
			to == provisionFailed || to == provisionUnknown
	case provisionFailed, provisionUnknown:
		return to == provisionStarting || to == provisionRunning
	default:
		return false
	}
}

// ProvisionRequest is an already-authorized provision. The MCP
// handler authenticates the caller and refuses remote writes before
// building this; the service performs no auth and starts no work the
// row does not record.
type ProvisionRequest struct {
	Actor     string
	RequestID string
	Principal string
	Remote    bool

	Run       string
	Session   string
	Objective string
	Workdir   string
	Backend   string
	Harness   string
	Command   string
	Model     string

	// Provider and Profile scope a muse-native provision: which
	// native store the later bind must create the session in. They
	// ride the intent into the logical session record (their durable
	// home) and are ignored by every shell backend.
	Provider string
	Profile  string

	// ExplicitCommand reports the caller supplied its own command
	// string (command/capability gate), as opposed to a parked or
	// harness-built one.
	ExplicitCommand bool
}

// ProvisionLauncher starts one session on the requested backend.
// Exactly one fenced holder launches; the backend itself performs no
// fencing beyond its own start path.
type ProvisionLauncher interface {
	LaunchProvisionSession(want mcpProvisionRecord) error
}

// ProvisionLiveness observes the session backend: whether the
// wanted session already exists there, and whether a nil launch
// actually confirmed the process identity (never inferred success).
type ProvisionLiveness interface {
	ProvisionSessionLive(want mcpProvisionRecord) bool
	ProvisionLaunchVerified(want mcpProvisionRecord) bool
}

// ProvisionService owns provisioning: durable intent first, then
// the workdir, then at most one fenced launch per claim. It speaks
// only domain types (ProvisionResult/ProvisionError) and never
// touches transport result shapes.
type ProvisionService struct {
	OpenDB   func() (*sql.DB, error)
	Launcher ProvisionLauncher
	Liveness ProvisionLiveness
	MkdirAll func(string, os.FileMode) error
	// BindOwner persists the pre-launch owner binding on the
	// given store; nil uses the claim. On the fresh path the
	// store is the reservation transaction (binding commits with
	// the intent or rolls back with it); on the replay path it
	// is the repository handle (single-statement claim). Tests
	// inject persistence failures here to prove a failed binding
	// never reports success. Production never overrides it.
	BindOwner func(store OwnerStore, session, principal string) (bool, error)
}

// DefaultProvisionService wires the service to the real backends.
func DefaultProvisionService() *ProvisionService {
	backend := mcpProvisionSessionBackend{}
	return &ProvisionService{
		OpenDB:   openTasksDB,
		Launcher: backend,
		Liveness: backend,
		MkdirAll: os.MkdirAll,
	}
}

// Provision admits one authorized request and converges it:
// idempotent replay for the same payload, conflict for a reused key
// with different arguments, at most one verified launch.
func (s *ProvisionService) Provision(req ProvisionRequest) (ProvisionResult, error) {
	want, run, err := s.admit(req)
	if err != nil {
		return ProvisionResult{}, err
	}
	db, err := s.OpenDB()
	if err != nil {
		return ProvisionResult{}, provisionFailure("provision: %v", err)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	if err := repo.Ensure(); err != nil {
		return ProvisionResult{}, provisionFailure("provision: %v", err)
	}
	if prior, gen, found := repo.Load(req.RequestID); found {
		return s.replay(repo, req, run, want, prior, gen)
	}
	// Authorization before persistence: a session owned by
	// another controller refuses here, and a live-but-unowned
	// session refuses remote adoption. Both checks are read-only;
	// the binding claim below serializes in-protocol races
	// first-writer-wins.
	if err := s.checkSessionOwnership(repo, req, want); err != nil {
		return ProvisionResult{}, err
	}
	// Atomic reserve-and-bind: the intent insert and the owner
	// claim share one transaction. Both commit together or roll
	// back together; no worker starts until Commit succeeds, so
	// a crash mid-unit replays from zero, never from a
	// half-written reservation.
	resv, rerr := repo.BeginReserve()
	if rerr != nil {
		return ProvisionResult{}, provisionFailure("provision: reservation failed: %v", rerr)
	}
	if ierr := resv.InsertReserved(req.RequestID, want); ierr != nil {
		// A concurrent replay won the key: abandon our unit
		// and converge on the committed winner when it
		// carries the same payload, conflict when it does not.
		_ = resv.Rollback()
		if prior, gen, found := repo.Load(req.RequestID); found {
			return s.replay(repo, req, run, want, prior, gen)
		}
		return ProvisionResult{}, provisionFailure("provision: %v", ierr)
	}
	// Durable owner binding BEFORE worker launch, inside the
	// reservation transaction. A binding failure rolls the whole
	// unit back and fails loudly: no worker starts, no false
	// success, no partial row. A lost claim race refuses without
	// rebinding the winner's worker.
	if other, berr := s.bindOwner(resv, want.Session, req.Principal); berr != nil {
		_ = resv.Rollback()
		return ProvisionResult{}, provisionFailure("provision: owner binding failed: %v", berr)
	} else if other {
		_ = resv.Rollback()
		return ProvisionResult{}, provisionConflict("provision: session %q is owned by another controller; a conflicting admission cannot rebind it", want.Session)
	}
	if cerr := resv.Commit(); cerr != nil {
		return ProvisionResult{}, provisionFailure("provision: reservation commit failed: %v", cerr)
	}
	prior := want
	prior.State = provisionReserved
	return s.launch(repo, req.RequestID, run, want, prior, 0)
}

// checkSessionOwnership refuses adoptions the caller must never
// make: sessions owned by another principal, and live-but-unowned
// sessions for remote callers (pre-policy or operator-owned: only
// the machine operator may adopt those, locally). Empty-principal
// callers skip the policy exactly as before.
func (s *ProvisionService) checkSessionOwnership(repo *ProvisionRepository, req ProvisionRequest, want mcpProvisionRecord) error {
	principal := strings.TrimSpace(req.Principal)
	if principal == "" {
		return nil
	}
	owner, found, err := repo.OwnerOf(want.Session)
	if err != nil {
		return provisionFailure("provision: owner lookup failed: %v", err)
	}
	if found && owner != principal {
		return provisionConflict("provision: session %q is owned by another controller; a conflicting admission cannot rebind it", want.Session)
	}
	if !found && req.Remote && s.Liveness.ProvisionSessionLive(want) {
		return provisionConflict("provision: session %q is live without a recorded owner; its operator must stop it before a remote controller may claim the name", want.Session)
	}
	return nil
}

// bindOwner persists the pre-launch binding on the given store.
// Production always uses the claim; the seam exists so tests can
// fail the binding and prove the provision fails with it.
func (s *ProvisionService) bindOwner(store OwnerStore, session, principal string) (bool, error) {
	if s.BindOwner != nil {
		return s.BindOwner(store, session, principal)
	}
	return claimProvisionOwner(store, session, principal)
}

// admit validates without side effects: unknown backends,
// incoherent native payloads (a shell command or shell harness on a
// native backend), unresolvable commands, and unauthorized targets
// are all refused before any directory is created, any session
// starts, or any intent row exists. A coherent native payload
// admits to the logical-session path (admitNativeProvision).
func (s *ProvisionService) admit(req ProvisionRequest) (want mcpProvisionRecord, run string, err error) {
	backend := req.Backend
	if backend == "" {
		backend = provisionBackendTMUX
	}
	if backend != provisionBackendTMUX && backend != provisionBackendPTY && backend != provisionBackendNative {
		return mcpProvisionRecord{}, "", provisionFailure("unknown backend (want tmux|pty|muse-native)")
	}
	if backend == provisionBackendNative {
		return admitNativeProvision(req)
	}
	command := req.Command
	if req.Harness == "muse-code" {
		if !nameRe.MatchString(req.Session) {
			return mcpProvisionRecord{}, "", provisionFailure("provision: name must match [A-Za-z0-9_-]{1,64}")
		}
		// New interactive Muse Code workers default to the managed
		// PTY backend: the supervisor owns the process identity and
		// the TUI renders a bounded view, never a tmux
		// capture-pane. An explicit tmux backend stays supported
		// for existing sessions; anything else is refused.
		if strings.TrimSpace(req.Backend) == "" {
			backend = provisionBackendPTY
		}
		if backend != provisionBackendTMUX && backend != provisionBackendPTY {
			return mcpProvisionRecord{}, "", provisionFailure("provision: muse-code runs on the pty or tmux backend (want pty|tmux)")
		}
		if req.ExplicitCommand || strings.TrimSpace(req.Command) != "" {
			return mcpProvisionRecord{}, "", provisionFailure("provision: muse-code uses its curated interactive launcher; custom commands are refused")
		}
		if strings.TrimSpace(req.Workdir) == "" {
			return mcpProvisionRecord{}, "", provisionFailure("provision: muse-code requires an explicit workdir")
		}
		absoluteWorkdir, absErr := filepath.Abs(req.Workdir)
		if absErr != nil {
			return mcpProvisionRecord{}, "", provisionFailure("provision: Muse Code workdir: %v", absErr)
		}
		req.Workdir = absoluteWorkdir
		if req.Provider == "" {
			req.Provider = "meta"
		}
		command, err = museCodeCommand(req.Workdir, req.Provider, req.Model, req.Profile)
		if err != nil {
			return mcpProvisionRecord{}, "", provisionFailure("provision Muse Code: %v", err)
		}
	}
	if a, aerr := lookupAdapter(req.Harness); aerr == nil && a.ManagedTurn != nil && a.BuildCommand == nil {
		return mcpProvisionRecord{}, "", provisionFailure("provision: harness %q has no shell launch path; bind a native session with metis_bind instead of provisioning a shell session", req.Harness)
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = filepath.Join(stateDir(), "work", req.Run, req.Session)
	}
	if command == "" && req.Harness != "" && req.Harness != "muse-code" {
		c, cerr := resolveWorkerCommand(req.Harness, "", workdir, req.Objective, req.Model)
		if cerr != nil {
			// Explicit capability refusal (e.g. muse-native has
			// no shell command): never a panic, never a row.
			return mcpProvisionRecord{}, "", provisionFailure("provision worker: %v", cerr)
		}
		command = c
	}
	if command == "" {
		// Parked session: admitted for later delegation and
		// guidance, running nothing until then.
		command = "sleep 86400"
	}
	abs, _ := filepath.Abs(workdir)
	// P1-2 target policy: registered root first, custom-command
	// capability when the caller supplies its own command. Refused
	// before any directory is created, any session starts, or any
	// intent row exists.
	if terr := authorizeProvisionTarget(abs, req.ExplicitCommand); terr != nil {
		return mcpProvisionRecord{}, "", provisionFailure("provision: %v", terr)
	}
	want = mcpProvisionRecord{
		Session: req.Session, Run: req.Run, Objective: req.Objective,
		Workdir: abs, Backend: backend, Harness: req.Harness, Command: command, Model: req.Model,
		Provider: req.Provider, Profile: req.Profile,
	}
	return want, req.Run, nil
}

// admitNativeProvision validates a muse-native provision without
// side effects. A native logical session executes via MSP turns,
// never a shell command, so an explicit command or a shell harness
// is incoherent and refused — pointing at metis_bind, never a
// panic, never a row, never a shell session mislabeled native.
// Anything else records the authorized root plus the
// provider/model/profile scope the later bind must honor. No tmux
// session starts, no PTY supervisor spawns, and no daemon is needed
// yet: the Muse native session itself is created at bind time,
// which owns its own idempotent session/start path.
func admitNativeProvision(req ProvisionRequest) (mcpProvisionRecord, string, error) {
	if !nameRe.MatchString(req.Session) {
		return mcpProvisionRecord{}, "", provisionFailure("provision: name must match [A-Za-z0-9_-]{1,64}")
	}
	if strings.TrimSpace(req.Command) != "" {
		return mcpProvisionRecord{}, "", provisionFailure("provision: backend %q runs native turns, never a shell command; omit command (bind it with metis_bind instead of provisioning a shell session)", provisionBackendNative)
	}
	if h := strings.TrimSpace(req.Harness); h != "" && h != "muse-native" {
		if _, lerr := lookupAdapter(h); lerr != nil {
			return mcpProvisionRecord{}, "", provisionFailure("provision: %v", lerr)
		}
		return mcpProvisionRecord{}, "", provisionFailure("provision: harness %q is a shell harness; backend %q runs native turns (harness %q)", req.Harness, provisionBackendNative, "muse-native")
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = filepath.Join(stateDir(), "work", req.Run, req.Session)
	}
	abs, _ := filepath.Abs(workdir)
	// Nothing executes at provision time, so no custom-command
	// capability applies — but the root still needs authorizing:
	// the later bind pins exactly this root and turns refuse any
	// other. Refused before any directory exists or any row exists.
	if terr := authorizeProvisionTarget(abs, false); terr != nil {
		return mcpProvisionRecord{}, "", provisionFailure("provision: %v", terr)
	}
	want := mcpProvisionRecord{
		Session: req.Session, Run: req.Run, Objective: req.Objective,
		Workdir: abs, Backend: provisionBackendNative, Harness: "muse-native",
		Model: req.Model, Provider: req.Provider, Profile: req.Profile,
	}
	return want, req.Run, nil
}

// replay converges a repeated request_id. An identical payload
// returns the original (reconciling a crashed launch when the
// session exists); any differing field — session, root, backend,
// harness, or command — is an idempotency conflict and creates
// nothing: no directory, no session, no row change.
func (s *ProvisionService) replay(repo *ProvisionRepository, req ProvisionRequest, run string, want, prior mcpProvisionRecord, generation int64) (ProvisionResult, error) {
	if !prior.samePayload(want) {
		return ProvisionResult{}, provisionConflict("provision conflict: request_id %q already provisioned session %q via backend %q (run %q); a reused key with different arguments is rejected, never converged",
			req.RequestID, prior.Session, prior.Backend, prior.Run)
	}
	// Owner convergence on replay: the recorded binding stands. A
	// different principal replaying the key cannot claim the
	// worker; a remote replay of an unowned intent cannot adopt
	// it; a local replay binds a missing row to its own principal
	// (grandfathering pre-policy intents). Empty-principal
	// callers skip the policy exactly as before.
	if principal := strings.TrimSpace(req.Principal); principal != "" {
		owner, found, oerr := repo.OwnerOf(prior.Session)
		if oerr != nil {
			return ProvisionResult{}, provisionFailure("provision: owner lookup failed: %v", oerr)
		}
		if found && owner != principal {
			return ProvisionResult{}, provisionConflict("provision: request_id %q is owned by another controller; replaying it cannot claim session %q",
				req.RequestID, prior.Session)
		}
		if !found {
			if req.Remote {
				return ProvisionResult{}, provisionConflict("provision: session %q has no recorded owner; a remote replay cannot adopt it", prior.Session)
			}
			if other, berr := s.bindOwner(repo.Store(), prior.Session, principal); berr != nil {
				return ProvisionResult{}, provisionFailure("provision: owner binding failed: %v", berr)
			} else if other {
				return ProvisionResult{}, provisionConflict("provision: session %q is owned by another controller; a conflicting admission cannot rebind it", prior.Session)
			}
		}
	}
	if prior.State == provisionRunning {
		return ProvisionResult{SessionID: prior.Session, Run: run, Backend: prior.Backend, Disposition: ProvisionDispositionRunning, Replay: true}, nil
	}
	return s.launch(repo, req.RequestID, run, want, prior, generation)
}

// launch performs at most one verified launch for the recorded
// intent. A live session is adopted (fenced) instead of duplicated;
// the workdir is reconciled from the intent; the launch window is
// claimed under the fence so concurrent replays cannot both start a
// session; running is recorded only after the backend confirms the
// process identity.
func (s *ProvisionService) launch(repo *ProvisionRepository, requestID, run string, want, prior mcpProvisionRecord, generation int64) (ProvisionResult, error) {
	// Crash safety first: a live session means a previous attempt
	// already started it. Adopt the confirmed outcome under the
	// fence instead of launching a second one.
	if s.Liveness.ProvisionSessionLive(want) {
		if committed, err := repo.CompareAndSet(requestID, prior.State, generation, provisionRunning); err != nil {
			return ProvisionResult{}, provisionFailure("provision: %v", err)
		} else if committed {
			return ProvisionResult{SessionID: want.Session, Run: run, Backend: want.Backend, Disposition: ProvisionDispositionRunning, Replay: true, Reconciled: true}, nil
		}
		return s.converge(repo, requestID, run, want)
	}
	// Files reconcile from intent: the workdir is created only now
	// that the durable row exists. A crash between the commit and
	// this point replays here instead of failing the launch.
	if err := s.MkdirAll(want.Workdir, 0o755); err != nil {
		_, _ = repo.CompareAndSet(requestID, prior.State, generation, provisionFailed)
		return ProvisionResult{}, provisionFailureWithState(ProvisionDispositionFailed, "provision workdir: %v", err)
	}
	// Claim the launch window under the fence: exactly one holder
	// launches. Reclaiming a stale starting claim bumps the
	// generation so the previous holder's late completion can no
	// longer commit (starting->starting is a legal reclaim).
	committed, err := repo.CompareAndSet(requestID, prior.State, generation, provisionStarting)
	if err != nil {
		return ProvisionResult{}, provisionFailure("provision: %v", err)
	}
	if !committed {
		return s.converge(repo, requestID, run, want)
	}
	claimGen := generation + 1
	if err := s.Launcher.LaunchProvisionSession(want); err != nil {
		// A lost race already confirmed the original: converge
		// on it rather than reporting a duplicate.
		if cur, _, found := repo.Load(requestID); found &&
			cur.State == provisionRunning && cur.samePayload(want) {
			return ProvisionResult{SessionID: cur.Session, Run: run, Backend: cur.Backend, Disposition: ProvisionDispositionRunning, Replay: true}, nil
		}
		_ = recordFenced(repo, requestID, provisionStarting, claimGen, provisionFailed)
		return ProvisionResult{}, provisionFailureWithState(ProvisionDispositionFailed, "provision: %v (state=%s)", err, provisionFailed)
	}
	// Never inferred success: record running only after the backend
	// confirms the process identity. An unobservable session after
	// a nil launch is indeterminate, never running.
	if !s.Liveness.ProvisionLaunchVerified(want) {
		_ = recordFenced(repo, requestID, provisionStarting, claimGen, provisionUnknown)
		return ProvisionResult{}, provisionFailureWithState(ProvisionDispositionUnknown, "provision: launch completed but session %q is not observable on backend %q (state=%s); retry with the same request_id",
			want.Session, want.Backend, provisionUnknown)
	}
	if committed, err := repo.CompareAndSet(requestID, provisionStarting, claimGen, provisionRunning); err != nil {
		return ProvisionResult{}, provisionFailure("provision: %v", err)
	} else if !committed {
		return s.converge(repo, requestID, run, want)
	}
	// running means the backend confirmed the process identity. It
	// does not mean the agent is ready for interaction: use
	// metis_terminal status (alive) and metis_bind (native) for
	// readiness.
	return ProvisionResult{SessionID: want.Session, Run: run, Backend: want.Backend, Workdir: want.Workdir, Disposition: ProvisionDispositionRunning}, nil
}

// converge reports the current durable truth after losing a fence:
// a confirmed same-payload running row converges, anything else is
// an explicit concurrent-update error — never an inferred outcome.
func (s *ProvisionService) converge(repo *ProvisionRepository, requestID, run string, want mcpProvisionRecord) (ProvisionResult, error) {
	cur, _, found := repo.Load(requestID)
	if found && cur.State == provisionRunning && cur.samePayload(want) {
		return ProvisionResult{SessionID: cur.Session, Run: run, Backend: cur.Backend, Disposition: ProvisionDispositionRunning, Replay: true}, nil
	}
	if found {
		return ProvisionResult{}, provisionFailure("provision: concurrent provision update for request_id %q left state %q; retry with the same request_id", requestID, cur.State)
	}
	return ProvisionResult{}, provisionFailure("provision: concurrent provision update for request_id %q; retry with the same request_id", requestID)
}

// recordFenced is best-effort terminal bookkeeping: the fence
// decides, and a lost fence means a concurrent update already owns
// the row.
func recordFenced(repo *ProvisionRepository, requestID, expected string, generation int64, next string) error {
	_, err := repo.CompareAndSet(requestID, expected, generation, next)
	return err
}

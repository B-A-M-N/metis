package main

// muse_cmds.go: `metis muse bind|turn|status` — the operator and
// orchestrator surface for native Muse sessions.
//
// Every invocation spawns its own short-lived host and resumes the
// durable native session, so no daemon is required for correctness:
// durability lives in muse's session store plus Metis's binding row
// (native ID + replay cursor). A host that dies mid-turn leaves the
// outcome unknown — the wait reports the timeout and the last
// observed cursor, never a fabricated terminal value.
//
// Exit codes: 0 means the outcome is known (turn terminal observed,
// or status read); 1 means unknown or refused.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// museCmdDaemon runs one session daemon in the foreground for the
// given provider under the given commander profile. The operator
// backgrounds it (tmux, systemd, setsid); every bind/turn/status
// joins it through that commander's well-known socket and refuses
// to run without it. One provider per store: the daemon owns the
// store for its lifetime.
func museCmdDaemon(provider, profile string) error {
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return err
	}
	path, err := exec.LookPath(c.Binary)
	if err != nil {
		return fmt.Errorf("muse binary %q not on PATH: %w", c.Binary, err)
	}
	cmd := exec.Command(path, "serve", "--provider", provider, "--listen")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), museDaemonEnv(c)...)
	return cmd.Run()
}

// museDaemonEnv returns the environment override that pins the
// daemon child to its commander's store. The serve child resolves
// its store from XDG_DATA_HOME; without the override a profiled
// daemon (musefi) inherits the caller's ambient store and serves
// the wrong socket (found live: `muse daemon --profile musefi`
// listened on ~/.local/share instead of MUSEFI_DATA_HOME).
func museDaemonEnv(c museCommander) []string {
	if c.DataHome == "" {
		return nil
	}
	return []string{"XDG_DATA_HOME=" + c.DataHome}
}

// museRequireDaemon refuses to proceed when no session daemon
// serves this muse home. Without the daemon a host would serve
// in-process and its sessions would die with the call while the
// binding row pretends they are durable.
func museRequireDaemon() error {
	if !museDaemonReady() {
		return fmt.Errorf("no muse session daemon serves %s; start one first: muse serve --provider <name> --listen (or: metis muse daemon --provider <name>)", museDaemonSocket())
	}
	return nil
}

// museCmdBind creates a native session under the given commander
// profile and records the binding (native ID + profile). The
// profile selects the store; resuming under another profile is
// refused at turn/status time.
// bindMuseNativeSession performs the native bind and returns the
// bound identity (native session ID + event cursor) without printing
// anything: protocol operations return typed results; only the CLI
// layer owns presentation, so machine-protocol streams stay clean.
//
// Retry identity comes from the caller's request key (the MCP
// request_id at the metis_bind edge): the bind command ID is
// claimed-or-recovered before Muse is ever called, so an identical
// retry reuses the same session/start command and the lower-level
// dedup replays the already-created session instead of minting a
// second one. A reused key with a different payload is a conflict;
// an acknowledged-but-unbound session is adopted by reconcile.
func bindMuseNativeSession(session, provider, profile, title string, req OperationRequest) (nativeID, cursor string, err error) {
	if err := requireRequest(req); err != nil {
		return "", "", err
	}
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return "", "", err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return "", "", err
	}
	// The native session is created under the Metis session's
	// authorized root: the binding below pins this exact root,
	// and turns later refuse a re-homed session. The scope is
	// resolved BEFORE the intent claim so the frozen fingerprint
	// pins the same root the binding will pin. The root resolves
	// decoupled from shell backing — a native-only logical session
	// binds with no tmux session or PTY supervisor behind it.
	workdir, err := resolveSessionWorkspace(session)
	if err != nil {
		return "", "", err
	}
	workspaceRoot, err := canonicalRoot(workdir)
	if err != nil {
		return "", "", err
	}
	model, err := resolveSessionModel(session)
	if err != nil {
		return "", "", fmt.Errorf("muse bind refused for %s: %v", session, err)
	}
	// Reconcile first: an acknowledged intent whose binding row
	// never committed (crash between the ack and the bind) is
	// adopted as-is — no new native session, whichever request
	// key the recovery arrives under.
	if intent, found, rerr := findAckedUnboundBindIntent(session, provider, profile, workspaceRoot, model); rerr != nil {
		return "", "", rerr
	} else if found {
		if err := bindMuseSession(session, intent.NativeID, intent.ViewCursor, provider, profile, req); err != nil {
			return "", "", err
		}
		return intent.NativeID, intent.ViewCursor, nil
	}
	// Claim-or-recover the durable command for this request key,
	// then run the single start-and-bind below through it.
	cmdID, err := claimBindIntent(req, session, provider, profile, title, workspaceRoot, model)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return "", "", err
	}
	defer h.Close()
	sid, cursor, err := bindMuseNativeSessionOnHost(ctx, h, session, provider, profile, title, workspaceRoot, model, cmdID, req)
	if err != nil {
		return "", "", err
	}
	return sid, cursor, nil
}

// bindMuseNativeSessionOnHost runs one claimed bind command against
// an already-joined host: idempotent start under the claimed
// command identity, then the durable binding row. The lower-level
// start replays an acknowledged intent without retransmitting, and
// re-sends the SAME command for a merely submitted one (server
// dedup), so retries through this function create at most one
// native session. Split out so tests drive the exact production
// path against a stub host.
func bindMuseNativeSessionOnHost(ctx context.Context, h *museHost, session, provider, profile, title, workspaceRoot, model, commandID string, req OperationRequest) (nativeID, cursor string, err error) {
	// The authorized routing is explicit: host provider plus the
	// session's registered model (empty selects the server
	// default). session/start must echo the same effective
	// config back, or the session is refused before any binding.
	sel := museSessionSelection{ProviderID: provider, ModelID: model}
	sid, cursor, err := museStartSessionWithID(ctx, h, title, workspaceRoot, sel, commandID)
	if err != nil {
		return "", "", err
	}
	if err := bindMuseSession(session, sid, cursor, provider, profile, req); err != nil {
		return "", "", err
	}
	return sid, cursor, nil
}

func museCmdBind(session, provider, profile, title, actor string) error {
	sid, cursor, err := bindMuseNativeSession(session, provider, profile, title, OperationRequest{Actor: actor, RequestID: newRequestID()})
	if err != nil {
		return err
	}
	fmt.Printf("bound %s -> native %s (provider %s, profile %q, cursor %q)\n", session, sid, provider, profile, cursor)
	return nil
}

// museCmdTurn submits one turn to the bound native session and waits
// for its terminal notification. Provider and commander profile must
// match the bind: a silent switch of either is refused (TD-052,
// cross-profile resume).
func museCmdTurn(session, provider, profile, text string, timeout time.Duration) error {
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return err
	}
	b, err := getMuseBinding(session)
	if err != nil {
		return err
	}
	if err := checkMuseBindingScope(b, provider, profile); err != nil {
		return err
	}
	if err := checkMuseExecutionScope(session, b); err != nil {
		return err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return err
	}
	defer h.Close()
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		return fmt.Errorf("resume: %w (outcome of any in-flight turn unknown; cursor %q retained)", err, b.Cursor)
	}
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		return verr
	}
	ack, err := museSubmitTurn(ctx, h, b.NativeID, text, "")
	if err != nil {
		return err
	}
	wctx, wcancel := context.WithTimeout(ctx, timeout)
	defer wcancel()
	terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, ack.TurnID,
		chainTurnObserver(ctx, h, session, b.NativeID, nil))
	if err != nil {
		return err
	}
	if err := updateMuseCursor(session, cursor, ack.TurnID); err != nil {
		return err
	}
	fmt.Printf("turn %s terminal=%s cursor=%q\n", ack.TurnID, terminal, cursor)
	if terminal != "completed" {
		return fmt.Errorf("turn ended %s (outcome known; not a Metis failure)", terminal)
	}
	return nil
}

// museCmdStatus reads the bound native session without attaching:
// no lease, no load, no subscription.
func museCmdStatus(session, provider, profile string) error {
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return err
	}
	b, err := getMuseBinding(session)
	if err != nil {
		return err
	}
	if err := checkMuseBindingScope(b, provider, profile); err != nil {
		return err
	}
	if err := checkMuseExecutionScope(session, b); err != nil {
		return err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return err
	}
	defer h.Close()
	// Read-only: the cursor is the event-ingestion path's property.
	// A point-in-time read must never advance it, or unprocessed
	// events between the stored cursor and head would be skipped.
	status, cursor, err := museReadSession(ctx, h, b.NativeID)
	if err != nil {
		return err
	}
	fmt.Printf("native %s status=%s cursor=%q\n", b.NativeID, status, cursor)
	return nil
}

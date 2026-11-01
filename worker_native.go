package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// worker_native.go: native Muse turn execution. Extracted from worker_run.go; behavior unchanged.

func museTurnTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("METIS_MUSE_TURN_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Minute
}

// runNativeTurnOnce executes one claimed attempt as a native MSP
// turn and files the same downstream records as the exec path:
// execution receipt (bound 1:1 to the attempt), output artifact,
// runner-observation report, completion receipt. Terminal values
// come only from the server's turn/completed; the exit projection
// (completed=0, anything else=1) is labeled in the receipt, never
// silent. Unknown outcomes file nothing and keep the task working
// for explicit reconciliation — and never refund, since a turn may
// still be running server-side.
// nativeTurnIdentity resolves the binding and persists the turn
// execution identity before anything is sent: a crash between
// accept and ack must still name this command on retry, and the
// retry must reuse it so the server dedups instead of executing
// twice. A non-nil error is always pre-submission (binding or
// persistence failed before any byte moved).
func nativeTurnIdentity(session string, t Task, prompt string, loopReq OperationRequest) (error, string, museBinding) {
	b, berr := getMuseBinding(session)
	if berr != nil {
		return musePreSubmit(fmt.Errorf("no muse binding for session %s: %v", session, berr)), "", museBinding{}
	}
	cmdID, err := ensureTurnIntent(t.ID, t.Attempt, session, b.NativeID, b.Profile, b.Provider, b.Workspace, prompt, loopReq)
	if err != nil {
		// A frozen-identity conflict may already be running
		// server-side: pass it through unrefunded so the attempt
		// stays fenced for reconciliation.
		if isTurnIntentConflict(err) {
			return err, "", museBinding{}
		}
		return musePreSubmit(fmt.Errorf("turn intent unpersisted for task %s attempt %d: %v", t.ID, t.Attempt, err)), "", museBinding{}
	}
	return nil, cmdID, b
}

func runNativeTurnOnce(session string, a Adapter, t Task, prompt, root string, loopReq OperationRequest) (bool, error) {
	loopID := "worker-loop/" + session
	preErr, cmdID, _ := nativeTurnIdentity(session, t, prompt, loopReq)
	if preErr != nil {
		// A frozen identity may already be executing server-side:
		// keep the attempt fenced for reconciliation, never refund.
		if isTurnIntentConflict(preErr) {
			return false, preErr
		}
		// Nothing was submitted: refund like a failed provider
		// launch so the attempt is not consumed.
		if rerr := refundClaim(t.ID, t.Attempt, fmt.Sprintf("native turn never submitted: %v", preErr)); rerr != nil {
			return false, rerr
		}
		return false, preErr
	}
	// Durable native execution binding BEFORE submission: the
	// receipt proves an MSP execution owns this attempt from the
	// moment bytes may move. pid 0 is the native sentinel — no OS
	// process exists to signal, so cancellation must route through
	// turn/cancel, never PID kill. A duplicate row means something
	// may already be running: keep the attempt working for
	// reconciliation and never refund a possibly-live execution.
	argvJSON, _ := marshalRedactedArgv([]string{"msp", "turn/start", cmdID})
	if serr := recordExecutionStartWithProc(t.ID, t.Attempt, session, a.Harness, string(argvJSON), 0, 0, loopID); serr != nil {
		return false, fmt.Errorf("native execution already bound for task %s attempt %d: %v (outcome unknown; task stays %s)",
			t.ID, t.Attempt, serr, TaskWorking)
	}
	outcome, err := a.ManagedTurn(context.Background(), session, prompt, museTurnTimeout(), cmdID)
	if err != nil {
		var pre *musePreSubmitError
		if errors.As(err, &pre) {
			// Nothing was submitted: refund like a failed
			// provider launch so the attempt is not consumed.
			if rerr := refundClaim(t.ID, t.Attempt, fmt.Sprintf("native turn never submitted: %v", err)); rerr != nil {
				return false, rerr
			}
			return false, err
		}
		// The attempt may be running server-side: keep it working
		// and escalate one bounded attention for a human or lead
		// to reconcile. Attention failure never masks the error.
		if _, aerr := raiseAttention(t.Run, t.ID, t.Attempt, session, "uncertain_outcome",
			"native turn submission uncertain; outcome unknown",
			fmt.Sprintf("command=%s", cmdID)); aerr != nil {
			recordReceipt(t.ID, "attention_error", fmt.Sprintf("attempt=%d %v", t.Attempt, aerr))
		}
		return false, fmt.Errorf("native turn for task %s attempt %d: %v (outcome unknown; task stays %s)", t.ID, t.Attempt, err, TaskWorking)
	}
	exitCode := 1
	if outcome.Terminal == "completed" {
		exitCode = 0
	}
	outPath := filepath.Join(root, fmt.Sprintf("worker-output-%s-attempt%d.log", t.ID, t.Attempt))
	outFile, err := openWorkerOutput(outPath)
	if err != nil {
		return false, fmt.Errorf("worker output file failed for task %s: %v", t.ID, err)
	}
	body := fmt.Sprintf("terminal=%s turn=%s\n--- final ---\n%s\n", outcome.Terminal, outcome.TurnID, outcome.Text)
	fileCap := &cappedWriter{w: outFile, limit: workerOutputCap}
	if _, werr := io.WriteString(fileCap, body); werr != nil {
		_ = outFile.Close()
		return false, fmt.Errorf("worker output unwritable for task %s: %v", t.ID, werr)
	}
	_ = outFile.Close()
	if fileCap.discarded > 0 {
		recordReceipt(t.ID, "output_truncated", fmt.Sprintf("attempt=%d stored=%d discarded=%d cap=%d",
			t.Attempt, fileCap.stored, fileCap.discarded, workerOutputCap))
	}
	outData, rerr := readConfinedFile(root, outPath)
	if rerr != nil {
		return false, fmt.Errorf("worker output unreadable for task %s: %v (execution recorded, report missing: task stays %s)", t.ID, rerr, TaskWorking)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(outData))
	// The execution receipt was filed pre-submit; only the result
	// is recorded here. The acknowledged turn id lives in the
	// turn intent row, bound to the same command id.
	if err := recordExecutionResult(t.ID, t.Attempt, exitCode, digest); err != nil {
		return false, fmt.Errorf("execution result unfiled for task %s: %v (task stays %s, orchestrator must reconcile)", t.ID, err, TaskWorking)
	}
	if _, aerr := attachArtifact(t.ID, outPath); aerr != nil {
		recordReceipt(t.ID, "evidence_attach_failed", fmt.Sprintf("attempt=%d %v", t.Attempt, aerr))
	}
	tail := outData
	truncated := ""
	if len(tail) > workerReportTail {
		tail = tail[len(tail)-workerReportTail:]
		truncated = " (output truncated to tail)"
	}
	report := fmt.Sprintf("worker-loop/%s executed task %s attempt %d (%s): terminal=%s turn=%s exit=%d output=%s %s%s\n--- output tail ---\n%s",
		session, t.ID, t.Attempt, a.Harness, outcome.Terminal, outcome.TurnID, exitCode, filepath.Base(outPath), digest, truncated, strings.TrimRight(string(tail), "\n"))
	if err := submitReport(t.ID, ReportFiling{Report: report, Source: SourceRunnerObservation, Reporter: loopID}); err != nil {
		return false, fmt.Errorf("result unfiled for task %s: %v (execution recorded, task stays %s)", t.ID, err, TaskWorking)
	}
	recordReceipt(t.ID, "execution_completed", fmt.Sprintf("attempt=%d terminal=%s turn=%s exit=%d %s", t.Attempt, outcome.Terminal, outcome.TurnID, exitCode, digest))
	return true, nil
}

// refundClaim returns a claimed-but-never-executed task to
// dispatched so the orchestrator can retry or cancel. No execution
// happened, so no attempt was consumed; the receipt says exactly
// that. Mirrors are moved back alongside the task in one
// transaction.

// The managed-turn coordinator below owns task-attempt
// orchestration for native execution: cancellation routing,
// observer chaining, and the ManagedTurn implementation the
// muse-native adapter dispatches. Pure MSP wire operations stay
// in muse_sessions.go. Extracted without behavior change.

// stopNativeTurn cancels the MSP turn bound to one attempt. Every
// refusal along the way fails closed to uncertain: without the
// binding, the daemon, the acknowledged turn id, or an observed
// terminal outcome, Metis cannot prove the turn stopped.
func stopNativeTurn(taskID string, attempt int, session string, _ OperationRequest) (string, error) {
	b, err := getMuseBinding(session)
	if err != nil {
		return ExecutionCancelUncertain, nil
	}
	c, err := resolveMuseCommander(b.Profile)
	if err != nil {
		return ExecutionCancelUncertain, nil
	}
	if err := museRequireDaemonFor(c); err != nil {
		return ExecutionCancelUncertain, nil
	}
	intent, err := getTurnIntent(taskID, attempt)
	if err != nil {
		return ExecutionCancelUncertain, nil
	}
	if intent.TurnID == "" {
		// Submitted but never acknowledged: it may still be
		// running server-side, and there is no turn to cancel.
		return ExecutionCancelUncertain, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, b.Provider, nil, "--join")
	if err != nil {
		return ExecutionCancelUncertain, nil
	}
	defer h.Close()
	return stopNativeTurnOnHost(ctx, h, b.NativeID, intent.TurnID, nativeCancelWait())
}

// stopNativeTurnOnHost admits turn/cancel for one exact turn and
// then requires the authoritative terminal outcome. Any terminal
// value (completed, cancelled, failed) proves no execution
// remains; a failed admission, a lost terminal, or an expired
// wait leaves the outcome unknown.
func stopNativeTurnOnHost(ctx context.Context, h *museHost, nativeID, turnID string, wait time.Duration) (string, error) {
	if err := museCancelTurn(ctx, h, nativeID, turnID); err != nil {
		return ExecutionCancelUncertain, nil
	}
	wctx, wcancel := context.WithTimeout(ctx, wait)
	defer wcancel()
	if _, _, err := museWaitTurn(wctx, h, nativeID, turnID, nil); err != nil {
		return ExecutionCancelUncertain, nil
	}
	return ExecutionCancelConfirmed, nil
}

// musePreSubmitError marks failures before the turn reached the
// server (no binding, no daemon, resume/submit refused). Nothing
// was submitted, so the worker loop may refund the claim: the
// attempt was never consumed. Anything later is post-submit and
// must stay working for reconciliation.
type musePreSubmitError struct{ err error }

func (e *musePreSubmitError) Error() string { return e.err.Error() }
func (e *musePreSubmitError) Unwrap() error { return e.err }

func musePreSubmit(err error) error {
	if err == nil {
		return nil
	}
	return &musePreSubmitError{err: err}
}

// chainTurnObserver runs the approval observer and the terminal
// text extractor on every notification, in that order. Approvals
// are recorded durably (with must-answer receipts) on the same
// event path that resolves the turn — never on a side channel the
// wait does not consume. onText may be nil when the caller only
// needs the approval side.
func chainTurnObserver(ctx context.Context, h *museHost, session, nativeID string, onText func(string)) func(museNotification) {
	approve := museApprovalObserver(ctx, h, session, nativeID)
	return func(n museNotification) {
		approve(n)
		if n.Method != "item/completed" {
			return
		}
		var p struct {
			Item struct {
				Kind         string `json:"kind"`
				Status       string `json:"status"`
				Text         string `json:"text"`
				MessagePhase string `json:"messagePhase"`
			} `json:"item"`
		}
		if err := json.Unmarshal(n.Params, &p); err != nil {
			return
		}
		if onText != nil && p.Item.Kind == "agentMessage" && p.Item.Status == "completed" && p.Item.Text != "" {
			onText(p.Item.Text)
		}
	}
}

// museManagedTurn implements Adapter.ManagedTurn: one prompt becomes
// one native turn on the session's bound native session, and the
// wait returns only what the server terminally reported. Unknown
// outcomes (timeout, host death) return an error with no outcome;
// the caller keeps the task unresolved. On timeout it best-effort
// cancels the turn first so a retry starts clean instead of racing
// a still-running turn — the cancel is bounded and its own failure
// never masks the unknown outcome.
func museManagedTurn(ctx context.Context, session, prompt string, timeout time.Duration, commandID string) (museTurnOutcome, error) {
	var out museTurnOutcome
	b, err := getMuseBinding(session)
	if err != nil {
		return out, musePreSubmit(fmt.Errorf("no muse binding for session %s: bind it first (tui-dash muse bind); refusing silent fallback", session))
	}
	if err := checkMuseExecutionScope(session, b); err != nil {
		return out, musePreSubmit(err)
	}
	c, err := resolveMuseCommander(b.Profile)
	if err != nil {
		return out, musePreSubmit(err)
	}
	if err := museRequireDaemonFor(c); err != nil {
		return out, musePreSubmit(err)
	}
	h, err := startMuseHost(ctx, c.Binary, b.Provider, nil, "--join")
	if err != nil {
		return out, musePreSubmit(err)
	}
	defer h.Close()
	if _, _, err := museResumeSession(ctx, h, b.NativeID, b.Cursor); err != nil {
		return out, musePreSubmit(fmt.Errorf("resume %s: %w (cursor %q retained)", b.NativeID, err, b.Cursor))
	}
	// Revalidate after recovery: the server-reported effective
	// config must still match the binding. A drifted provider,
	// model, or workspace refuses before anything is submitted.
	if _, _, _, verr := museVerifySessionConfig(ctx, h, b.NativeID, b.Provider, b.Model, b.Workspace); verr != nil {
		return out, musePreSubmit(verr)
	}
	if commandID == "" {
		// No durable caller identity (ad-hoc use): mint one so
		// the wire always carries an idempotency key.
		if commandID, err = newMuseCommandID(); err != nil {
			return out, musePreSubmit(err)
		}
	}
	ack, err := museSubmitTurnWithID(ctx, h, b.NativeID, prompt, "", commandID)
	if err != nil {
		// The intent row (persisted by the durable caller)
		// stays submitted: the request may have reached Muse,
		// so this is unknown, never proof nothing ran. Only a
		// failure before transmission refunds an attempt.
		return out, fmt.Errorf("turn/start %s: %v (submission uncertain; intent %s retained, no refund)", commandID, err, commandID)
	}
	if err := recordTurnAck(commandID, ack.TurnID); err != nil {
		return out, fmt.Errorf("turn %s acknowledged but intent unrecorded: %v (outcome unknown; reconcile by command %s)", ack.TurnID, err, commandID)
	}
	out.TurnID = ack.TurnID
	var finalText string
	wctx, wcancel := context.WithTimeout(ctx, timeout)
	defer wcancel()
	observe := chainTurnObserver(ctx, h, session, b.NativeID, func(s string) { finalText = s })
	terminal, cursor, err := museWaitTurn(wctx, h, b.NativeID, ack.TurnID, observe)
	if err != nil {
		ccancel, stop := context.WithTimeout(context.Background(), 15*time.Second)
		cerr := museCancelTurn(ccancel, h, b.NativeID, ack.TurnID)
		stop()
		if cerr != nil {
			return out, fmt.Errorf("turn %s outcome unknown (%v); best-effort cancel also failed: %v (cursor %q retained)", ack.TurnID, err, cerr, b.Cursor)
		}
		return out, fmt.Errorf("turn %s outcome unknown (%v); cancel requested, re-resume to reconcile (cursor %q retained)", ack.TurnID, err, b.Cursor)
	}
	out.Terminal = terminal
	out.Text = finalText
	if cursor != "" {
		_ = updateMuseCursor(session, cursor, ack.TurnID)
	}
	return out, nil
}

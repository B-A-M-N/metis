package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// worker_phases.go: typed phases behind runWorkerOnce.
//
// The coordinator in worker_subprocess.go stays short: resolve the
// assigned task, claim it, execute it, persist the outcome, settle
// ownership. Each phase below takes an explicit typed input and
// returns an explicit typed outcome. Nothing here schedules or
// orchestrates — there is exactly one worker loop, and these are
// its sequential steps for one attempt.
//
// The coordinator must never infer success from launch or output:
// terminal values come only from the waited child (exit code) or
// the server-reported turn outcome (native path). Cancellation,
// uncertainty, recovery, and fleet settlement stay bound to the
// exact attempt the claim holds.

// workerAssignment is phase 1's outcome: the one ledger-committed
// task bound to this session, with its validated attempt identity.
type workerAssignment struct {
	task    Task
	prompt  string
	root    string
	model   string
	loopID  string
	loopReq OperationRequest
}

// workerResolveAssignment finds the task the ledger committed to
// session and validates its attempt. It executes nothing. It
// reports idle=true when the ledger holds no work for the session.
// workerSessionBase resolves the execution base for assignment
// pickup: workdir, model, and bound harness. Shell-backed sessions
// use the live registry entry (today's behavior, errors included);
// a name with no shell backing resolves through its active
// native-only logical record. Unknown, stopped, or non-native
// rows without shell backing are refused exactly as before, so a
// vanished shell session is never masked by a stale record.
func workerSessionBase(session string) (workdir, model, harness string, err error) {
	if entry, ok := loadRegistry()[session]; ok {
		return entry.Workdir, entry.Model, entry.Harness, nil
	}
	ls, found, lerr := loadLogicalSession(session)
	if lerr != nil || !found {
		return "", "", "", fmt.Errorf("no such session: %s", session)
	}
	if ls.State == logicalStopped {
		return "", "", "", fmt.Errorf("worker refused: logical session %s is stopped", session)
	}
	if ls.Backend != TargetMuseNative {
		return "", "", "", fmt.Errorf("no such session: %s", session)
	}
	return ls.Workspace, ls.Model, ls.Harness, nil
}

func workerResolveAssignment(session string, a Adapter, req OperationRequest) (asn workerAssignment, idle bool, err error) {
	defer func() { observeOps(opsPhaseClaim, req.RequestID, "", 0, session, err) }()
	workdir, model, harness, berr := workerSessionBase(session)
	if berr != nil {
		return asn, false, berr
	}
	if harness != "" && a.Harness != "" && harness != a.Harness {
		return asn, false, fmt.Errorf("session %s is bound to harness %q, not %q", session, harness, a.Harness)
	}
	root := workdir
	if fi, serr := os.Stat(root); serr != nil || !fi.IsDir() {
		return asn, false, fmt.Errorf("session %s workdir unusable: %v", session, serr)
	}
	ids, derr := dispatchedTasksForSession(session)
	if derr != nil {
		return asn, false, derr
	}
	if len(ids) == 0 {
		return asn, true, nil
	}
	if len(ids) > 1 {
		return asn, false, fmt.Errorf("session %s has %d dispatched tasks (%s); orchestrator must leave exactly one: refusing to choose",
			session, len(ids), strings.Join(ids, ", "))
	}
	loopID := "worker-loop/" + session
	loopReq := OperationRequest{Actor: loopID, RequestID: req.RequestID}
	t, gerr := getTask(ids[0])
	if gerr != nil {
		return asn, false, gerr
	}
	promptRaw, rerr := os.ReadFile(filepath.Join(root, "task-"+t.ID+".md"))
	if rerr != nil {
		return asn, false, fmt.Errorf("task %s has no instruction file in %s: %v", t.ID, root, rerr)
	}
	if cerr := checkAttemptSidecar(root, t); cerr != nil {
		return asn, false, cerr
	}
	env := TaskEnvelope{
		SchemaVersion: WorkerProtocolVersion,
		TaskID:        t.ID,
		Attempt:       t.Attempt,
		RunID:         t.Run,
		SessionID:     session,
		ExecutionRoot: t.ExecutionRoot,
		Objective:     t.Objective,
		Acceptance:    criteriaFromAcceptance(t.Acceptance),
	}
	if verr := env.Validate(); verr != nil {
		return asn, false, fmt.Errorf("task %s cannot drive execution: %v", t.ID, verr)
	}
	return workerAssignment{
		task:    t,
		prompt:  string(promptRaw),
		root:    root,
		model:   model,
		loopID:  loopID,
		loopReq: loopReq,
	}, false, nil
}

// workerClaim is phase 2's outcome: the ledger claim plus the fleet
// ownership bound to the same exact attempt.
type workerClaim struct {
	asn        workerAssignment
	fleetNode  string
	fleetOwned bool
}

// workerClaimAttempt claims the assigned attempt on the ledger and
// drains the fleet offer for the same exact (task, attempt,
// session). A lost fleet claim refunds the pickup and refuses
// loudly — the attempt never executes unowned.
func workerClaimAttempt(session string, asn workerAssignment) (claim workerClaim, opErr error) {
	defer func() {
		observeOps(opsPhaseClaim, asn.loopReq.RequestID, asn.task.ID, asn.task.Attempt, session, opErr)
	}()
	t := asn.task
	if err := markTaskWorking(t.ID, t.Attempt, asn.loopReq); err != nil {
		return workerClaim{}, fmt.Errorf("claim failed for task %s attempt %d: %w", t.ID, t.Attempt, err)
	}
	// P0-E: fleet-placed attempts execute under the node's claim.
	// A pending offer for this exact (task, attempt, session) is
	// claimed now; without one the loop runs the legacy path.
	if node, owned, derr := drainFleetOffer(t.ID, t.Attempt, session); derr != nil {
		_ = refundClaim(t.ID, t.Attempt, "fleet claim refused: "+derr.Error())
		return workerClaim{}, derr
	} else if owned {
		bindClaimConsumer(session, t)
		// P3: claim-side pickup stamp. Best-effort observation only:
		// the ledger claim already won, so a metrics failure must
		// never overturn it.
		_ = observePickupClaim(t.ID, t.Attempt, time.Now())
		return workerClaim{asn: asn, fleetNode: node, fleetOwned: true}, nil
	}
	bindClaimConsumer(session, t)
	_ = observePickupClaim(t.ID, t.Attempt, time.Now())
	return workerClaim{asn: asn}, nil
}

// bindClaimConsumer records the consumer-side half of a won ledger
// claim: the session's current assignment becomes the exact task the
// claim holds, and the execution_claimed receipt names the
// generation the pickup executed under — so a pickup from a killed
// loop stays distinguishable from the restart's. Best-effort: the
// ledger claim already won, and tracking must never overturn it.
func bindClaimConsumer(session string, t Task) {
	gen, herr := claimWorkerAssignment(session, t.ID)
	if herr != nil {
		recordReceipt(t.ID, "consumer_track_failed",
			fmt.Sprintf("attempt=%d session=%s assignment untracked: %v (ledger claim stands)", t.Attempt, session, herr))
		return
	}
	recordReceipt(t.ID, "execution_claimed",
		fmt.Sprintf("attempt=%d session=%s generation=%d picked up task", t.Attempt, session, gen))
}

// workerSettleFleetOwnership is phase 5's fleet half: it files the
// fleet execution outcome for the exact attempt the claim bound.
// A failed worker turn fails the fleet execution; any other exit
// completes it with the recorded-outcome note. Unowned claims are
// a no-op reporting a skipped disposition.
//
// Settlement touches only fleet tables and the advisory receipt
// trail — never the task ledger. A settlement failure therefore
// cannot rewrite an established report as a failed execution: the
// task keeps its outcome, the fleet row keeps its durable claimed
// state with its capacity reservation, and the returned result
// carries the discrepancy (reconcile with
// reconcileFleetSettlement).
func workerSettleFleetOwnership(cl workerClaim, turnErr error) fleetSettleResult {
	t := cl.asn.task
	res := fleetSettleResult{
		TaskID:      t.ID,
		Attempt:     t.Attempt,
		AttemptID:   fleetAttemptID(t.Attempt),
		NodeID:      cl.fleetNode,
		Disposition: fleetSettleSkipped,
	}
	if !cl.fleetOwned {
		return res
	}
	if turnErr != nil {
		res.Disposition = fleetExecFailed
		if serr := failFleetExecution(cl.fleetNode, t.ID, res.AttemptID, turnErr.Error()); serr != nil {
			recordReceipt(t.ID, "fleet_settle_failed",
				fmt.Sprintf("attempt=%d node=%s turn failed and settlement refused: %v (claimed state preserved; reconcile)", t.Attempt, cl.fleetNode, serr))
			res.Disposition = fleetSettleUnsettled
			res.Err = fmt.Errorf("fleet: turn for task %s attempt %d failed (%v) and settlement failed: %v (claimed state preserved; reconcile fleet settlement)",
				t.ID, t.Attempt, turnErr, serr)
			return res
		}
		res.Settled = true
		return res
	}
	res.Disposition = fleetExecCompleted
	if serr := completeFleetExecution(cl.fleetNode, t.ID, res.AttemptID, "worker loop outcome recorded"); serr != nil {
		recordReceipt(t.ID, "fleet_settle_failed",
			fmt.Sprintf("attempt=%d node=%s outcome recorded but settlement refused: %v (claimed state preserved; reconcile)", t.Attempt, cl.fleetNode, serr))
		res.Disposition = fleetSettleUnsettled
		res.Err = fmt.Errorf("fleet: outcome for task %s attempt %d recorded, but settlement failed: %v (claimed state preserved; reconcile fleet settlement)",
			t.ID, t.Attempt, serr)
		return res
	}
	res.Settled = true
	return res
}

// workerExecution is phase 3's outcome for the subprocess path:
// the waited exit code and the confined output bytes with their
// digest. Success is never inferred from launch — only the waited
// status and the digest recorded here count downstream.
type workerExecution struct {
	exitCode int
	outPath  string
	outData  []byte
	digest   string
}

// workerTurnOutcome is phase 3's disposition. The native branch
// files its own downstream records inside runNativeTurnOnce and
// reports native=true with that call's disposition, so the
// coordinator returns directly. The subprocess branch reports
// native=false with the waited result, leaving persistence to
// phase 4.
type workerTurnOutcome struct {
	native bool
	done   bool
	exec   workerExecution
}

// workerExecuteAttempt runs the claimed attempt. The native-turn
// branch files its own downstream records inside runNativeTurnOnce.
// The subprocess branch launches, waits, and reads back the bounded
// output, leaving persistence to phase 4.
func workerExecuteAttempt(session string, a Adapter, cl workerClaim) (out workerTurnOutcome, err error) {
	asn := cl.asn
	t := asn.task
	defer func() { observeOps(opsPhaseExecution, asn.loopReq.RequestID, t.ID, t.Attempt, session, err) }()
	// Native turns bypass argv/exec entirely: the attempt executes
	// as one MSP turn on the session's bound native session, and
	// its terminal outcome files the same receipts downstream.
	if a.ManagedTurn != nil {
		done, nerr := runNativeTurnOnce(session, a, t, asn.prompt, asn.root, asn.loopReq)
		return workerTurnOutcome{native: true, done: done}, nerr
	}
	argv, aerr := a.ManagedArgv(asn.root, asn.prompt, asn.model)
	if aerr != nil {
		_ = refundClaim(t.ID, t.Attempt, "invocation refused: "+aerr.Error())
		return workerTurnOutcome{}, aerr
	}
	// The receipt stores the redacted invocation (structure kept,
	// bulk prompts and embedded secrets replaced); no reader parses
	// argv content, so nothing downstream depends on the raw form.
	argvJSON, _ := marshalRedactedArgv(argv)
	outPath := filepath.Join(asn.root, fmt.Sprintf("worker-output-%s-attempt%d.log", t.ID, t.Attempt))
	outFile, oerr := openWorkerOutput(outPath)
	if oerr != nil {
		_ = refundClaim(t.ID, t.Attempt, "output file failed: "+oerr.Error())
		return workerTurnOutcome{}, fmt.Errorf("worker output file failed: %w", oerr)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = asn.root
	// Closed projection, never the ambient controller environment:
	// inherited credentials were the defect (P0 security review).
	cmd.Env = workerEnv(t.ID, t.Attempt, session, asn.root, nil)
	// The attempt file is bounded during streaming, not after:
	// excess is drained and receipted, never stored.
	fileCap := &cappedWriter{w: outFile, limit: workerOutputCap}
	// Mirror to the bound session's live stream so the TUI shows
	// real execution where the operator looks. Best-effort: a
	// failed mirror never fails the execution.
	var outputs []io.Writer
	outputs = append(outputs, fileCap)
	var streamFile *os.File
	_ = os.MkdirAll(filepath.Dir(streamPath(session)), 0o700)
	if sf, serr := os.OpenFile(streamPath(session), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); serr == nil {
		streamFile = sf
		streamCap := &cappedWriter{w: sf, limit: workerStreamCap}
		fmt.Fprintf(streamFile, "--- worker-loop %s task %s attempt %d (%s) ---\n", session, t.ID, t.Attempt, a.Harness)
		outputs = append(outputs, streamCap)
	}
	cmd.Stdout = io.MultiWriter(outputs...)
	cmd.Stderr = cmd.Stdout
	if serr := cmd.Start(); serr != nil {
		_ = outFile.Close()
		_ = refundClaim(t.ID, t.Attempt, "provider launch failed: "+serr.Error())
		return workerTurnOutcome{}, fmt.Errorf("provider launch failed for task %s: %w", t.ID, serr)
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	if rerr := recordExecutionStart(t.ID, t.Attempt, session, a.Harness, string(argvJSON), pid, asn.loopID); rerr != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = outFile.Close()
		return workerTurnOutcome{}, rerr
	}
	waitErr := cmd.Wait()
	_ = outFile.Close()
	if streamFile != nil {
		fmt.Fprintf(streamFile, "--- exit pending, task %s attempt %d ---\n", t.ID, t.Attempt)
		_ = streamFile.Close()
	}
	exitCode := 0
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			exitCode = -1
		}
	}
	// The file holds at most workerOutputCap bytes by construction;
	// any excess was drained during streaming and is receipted here.
	if fileCap.discarded > 0 {
		recordReceipt(t.ID, "output_truncated", fmt.Sprintf("attempt=%d stored=%d discarded=%d cap=%d",
			t.Attempt, fileCap.stored, fileCap.discarded, workerOutputCap))
	}
	// Confined read: the worker owned this path after launch and
	// could have swapped in a symlink before we digest it.
	outData, rerr := readConfinedFile(asn.root, outPath)
	if rerr != nil {
		return workerTurnOutcome{}, fmt.Errorf("worker output unreadable for task %s: %v (execution recorded, report missing: task stays %s)", t.ID, rerr, TaskWorking)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(outData))
	return workerTurnOutcome{exec: workerExecution{exitCode: exitCode, outPath: outPath, outData: outData, digest: digest}}, nil
}

// workerPersistOutcome is phase 4: it records the waited execution
// result and files the evidence report for the exact attempt. A
// task cancelled mid-run keeps its recorded outcome but gains no
// report: filing on a non-working task would lie about the
// disposition, and the ledger already says cancelled.
func workerPersistOutcome(session string, a Adapter, cl workerClaim, ex workerExecution) (done bool, opErr error) {
	defer func() {
		observeOps(opsPhaseExecution, cl.asn.loopReq.RequestID, cl.asn.task.ID, cl.asn.task.Attempt, session, opErr)
	}()
	t := cl.asn.task
	if err := recordExecutionResult(t.ID, t.Attempt, ex.exitCode, ex.digest); err != nil {
		return false, fmt.Errorf("execution result unfiled for task %s: %v (task stays %s, orchestrator must reconcile)", t.ID, err, TaskWorking)
	}
	if cur, gerr := getTask(t.ID); gerr == nil && cur.Status != TaskWorking {
		recordReceipt(t.ID, "execution_superseded", fmt.Sprintf("attempt=%d exit=%d task is %s; outcome recorded, no report filed",
			t.Attempt, ex.exitCode, cur.Status))
		return true, nil
	}
	if _, aerr := attachArtifact(t.ID, ex.outPath); aerr != nil {
		recordReceipt(t.ID, "evidence_attach_failed", fmt.Sprintf("attempt=%d %v", t.Attempt, aerr))
	}
	tail := ex.outData
	truncated := ""
	if len(tail) > workerReportTail {
		tail = tail[len(tail)-workerReportTail:]
		truncated = " (output truncated to tail)"
	}
	report := fmt.Sprintf("worker-loop/%s executed task %s attempt %d (%s): exit=%d output=%s %s%s\n--- output tail ---\n%s",
		session, t.ID, t.Attempt, a.Harness, ex.exitCode, filepath.Base(ex.outPath), ex.digest, truncated, strings.TrimRight(string(tail), "\n"))
	if serr := submitReport(t.ID, ReportFiling{Report: report, Source: SourceRunnerObservation, Reporter: cl.asn.loopID}); serr != nil {
		return false, fmt.Errorf("result unfiled for task %s: %v (execution recorded, task stays %s)", t.ID, serr, TaskWorking)
	}
	recordReceipt(t.ID, "execution_completed", fmt.Sprintf("attempt=%d exit=%d %s", t.Attempt, ex.exitCode, ex.digest))
	return true, nil
}

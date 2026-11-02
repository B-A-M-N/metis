package main

// assurance_terminate.go: execution-specific terminators.
//
// Routes cancellation to the exact process bound to one attempt.
// The recorded PID is signaled only when /proc still shows the same
// process incarnation; a recycled PID, a missing receipt, or an
// unreadable identity refuses with an uncertain disposition instead
// of killing a stranger.
//
// Requested / confirmed / uncertain stay distinct: these functions
// report what happened, and the caller receipts it. A sent SIGTERM
// is requested, never confirmed — only a reaped, gone, or never-bound
// target counts as terminated. Native harnesses never carry an OS
// pid: the turn is addressed through MSP (stopNativeTurn), never
// signalled. Capability routing is by managed-turn support, never by
// string comparison against one harness name.

import (
	"database/sql"
	"fmt"
	"strings"
	"syscall"
	"time"
)

// workerSessionAlive reports whether a bound worker session currently
// has a live process behind it, for tmux and PTY sessions alike.
func workerSessionAlive(session string) bool {
	if isPtySession(session) {
		_, state, err := ptyStatus(ptyBare(session))
		return err == nil && state == "running"
	}
	return sessionRunning(session)
}

// stopWorkerSession stops a bound worker session through the same
// identity-checked paths as the stop commands. A nil return means
// termination was confirmed; any error means it was NOT confirmed
// and the caller must record exactly that (TD-038).
func stopWorkerSession(session string) error {
	if isPtySession(session) {
		return ptyStop(ptyBare(session))
	}
	return stopSession(session)
}

// otherLiveBindings returns live (non-terminal) tasks bound to the
// same session, excluding tasks already disposed by this cascade.
func otherLiveBindings(session string, exclude map[string]bool) ([]string, error) {
	tasks, err := listTasksByAgent(session)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range tasks {
		if exclude[t.ID] {
			continue
		}
		if !terminalTaskStatus(t.Status) {
			out = append(out, fmt.Sprintf("%s (%s)", t.ID, t.Status))
		}
	}
	return out, nil
}

// settleBoundWorker accounts for the worker of a freshly cancelled
// task (TD-038). Exactly one outcome is receipted: stopped and
// confirmed, retained for another live task, already gone, or stop
// attempted but unconfirmed. Termination is never claimed from a
// mere signal send.
func settleBoundWorker(tid, session string, cancelled map[string]bool, req OperationRequest) {
	attr := fmt.Sprintf("actor=%s req=%s session=%s", req.Actor, req.RequestID, session)
	others, err := otherLiveBindings(session, cancelled)
	if err != nil {
		recordReceipt(tid, "worker_stop_unconfirmed", attr+" error=live-binding check failed: "+err.Error())
		return
	}
	if len(others) > 0 {
		recordReceipt(tid, "worker_retained", attr+" in_use_by="+strings.Join(others, ", "))
		return
	}
	if !workerSessionAlive(session) {
		recordReceipt(tid, "worker_already_gone", attr)
		return
	}
	if serr := stopWorkerSession(session); serr != nil {
		recordReceipt(tid, "worker_stop_unconfirmed", attr+" error="+serr.Error())
		return
	}
	recordReceipt(tid, "worker_stopped", attr)
}

// stopManagedExecution routes cancellation to the exact process
// bound to one attempt (WP1c). The recorded PID is signaled only
// when /proc still shows the same process incarnation; a recycled
// PID, a missing receipt, or an unreadable identity refuses with
// an uncertain disposition instead of killing a stranger.
// Requested/confirmed/uncertain stay distinct: this function
// reports what happened, and the caller receipts it.
func stopManagedExecution(taskID string, attempt int, req OperationRequest) (string, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	var pid int
	var procStart uint64
	var harness, session string
	err = db.QueryRow(`SELECT pid, proc_start, harness, session FROM execution_receipts WHERE task_id = ? AND attempt_no = ?`,
		taskID, attempt).Scan(&pid, &procStart, &harness, &session)
	if err != nil {
		if err == sql.ErrNoRows {
			// No receipt is only "nothing to stop" for OS-process
			// execution. A native session may be executing
			// server-side with no local row (pre-submit failure
			// and unrecorded turn are indistinguishable here),
			// so a native harness fails closed to uncertain.
			t, terr := getTask(taskID)
			if terr == nil && sessionIsNative(t.Agent) {
				return ExecutionCancelUncertain, nil
			}
			return ExecutionCancelNoBinding, nil
		}
		return "", err
	}
	if isNativeHarness(harness) {
		// Native rows never carry an OS pid: the turn is
		// addressed through MSP, never signalled.
		return stopNativeTurn(taskID, attempt, session, req)
	}
	if pid <= 0 || procStart == 0 {
		return ExecutionCancelUncertain, nil
	}
	nowStart, serr := procStartTime(pid)
	if serr != nil || nowStart != procStart {
		// Gone, or a different process now owns the number:
		// never signal on a guess.
		return ExecutionCancelUncertain, nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if procAlive(pid) {
			if nowStart2, serr2 := procStartTime(pid); serr2 != nil || nowStart2 != procStart {
				return ExecutionCancelUncertain, nil
			}
			return ExecutionCancelUncertain, nil
		}
		return ExecutionCancelConfirmed, nil
	}
	for i := 0; i < 50; i++ {
		if !procAlive(pid) {
			return ExecutionCancelConfirmed, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Graceful request failed: escalate once, then report honestly.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for i := 0; i < 20; i++ {
		if !procAlive(pid) {
			return ExecutionCancelConfirmed, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ExecutionCancelUncertain, nil
}

// sessionHarness resolves the worker harness of a session for
// capability checks. Unknown sessions are an error; sessions without
// a recorded harness return "" and are treated as unknown capability,
// never as confirmed live-input support.
func sessionHarness(session string) (string, error) {
	if isPtySession(session) {
		m, err := ptyGet(ptyBare(session))
		if err != nil {
			return "", err
		}
		return m.Harness, nil
	}
	reg := loadRegistry()
	e, ok := reg[session]
	if !ok {
		return "", fmt.Errorf("no such session: %s", session)
	}
	return e.Harness, nil
}

// isNativeHarness reports whether a recorded harness executes
// through MSP turns (no OS process to signal). The check is
// capability-based — any adapter with a managed-turn path —
// never a string comparison against one harness name.
func isNativeHarness(harness string) bool {
	a, err := lookupAdapter(harness)
	if err != nil {
		return false
	}
	return a.ManagedTurn != nil
}

// sessionIsNative reports whether the session's harness executes
// through MSP turns. Unknown sessions and lookup failures are not
// native: only a positive native declaration routes cancellation
// down the MSP path.
func sessionIsNative(session string) bool {
	if ls, found, err := loadLogicalSession(session); err == nil && found && ls.Backend == TargetMuseNative {
		return true
	}
	h, err := sessionHarness(session)
	if err != nil || h == "" {
		return false
	}
	return isNativeHarness(h)
}

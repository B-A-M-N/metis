package main

// reconcile.go: attempt-bound exit/recovery probe (remediation
// Phase C).
//
// A dead process is not a task outcome. reconcileTask reports what
// is durably known about the current attempt — delivery state,
// execution receipt, report, verdicts — and names the next step the
// orchestrator can take: WAIT, INVESTIGATE, file verdicts, retry,
// cancel, or DONE. It is strictly read-only: it launches nothing,
// moves nothing, judges nothing.

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// TaskReconciliation is the orchestrator's answer to "what do I do
// about this task": the known facts plus one recommended next step.
type TaskReconciliation struct {
	TaskID           string            `json:"task_id"`
	Status           string            `json:"status"`
	Attempt          int               `json:"attempt"`
	AttemptState     string            `json:"attempt_state"`
	DeliveryState    string            `json:"delivery_state"`
	ExecutionStarted bool              `json:"execution_started"`
	ExecutionFinished bool             `json:"execution_finished"`
	ExitCode         *int              `json:"exit_code,omitempty"`
	OutputDigest     string            `json:"output_digest,omitempty"`
	ReportFiled      bool              `json:"report_filed"`
	ReportSource     string            `json:"report_source,omitempty"`
	Verdicts         map[string]string `json:"verdicts"`
	Recommendation   string            `json:"recommendation"`
	Reason           string            `json:"reason"`
}

// reconcileTask reads the ledger and names the next step. Every
// branch is a fact about durable rows; absence of a row is absence
// of knowledge, never evidence of success or failure.
func reconcileTask(taskID string) (TaskReconciliation, error) {
	t, err := getTask(taskID)
	if err != nil {
		return TaskReconciliation{}, err
	}
	r := TaskReconciliation{
		TaskID:   taskID,
		Status:   t.Status,
		Attempt:  t.Attempt,
		Verdicts: map[string]string{},
	}
	db, err := openTasksDB()
	if err != nil {
		return TaskReconciliation{}, err
	}
	defer db.Close()
	if att, aerr := latestAttempt(db, taskID); aerr == nil {
		r.AttemptState = att.State
		if dlv, derr := latestDelivery(db, att.ID); derr == nil {
			r.DeliveryState = dlv.State
		}
	}
	var exitCode sql.NullInt64
	var digest sql.NullString
	var started bool
	err = db.QueryRow(`SELECT exit_code, output_digest FROM execution_receipts
		WHERE task_id = ? AND attempt_no = ?`, taskID, t.Attempt).Scan(&exitCode, &digest)
	switch {
	case err == nil:
		started = true
		r.ExecutionStarted = true
		if exitCode.Valid {
			r.ExecutionFinished = true
			c := int(exitCode.Int64)
			r.ExitCode = &c
			r.OutputDigest = digest.String
		}
	case err == sql.ErrNoRows:
		// No execution bound to this attempt: nothing launched yet,
		// or a pre-receipt task from before the worker loop.
	default:
		return TaskReconciliation{}, err
	}
	var source sql.NullString
	rerr := db.QueryRow(`SELECT source FROM task_reports WHERE task_id = ? AND attempt_no = ?`,
		taskID, t.Attempt).Scan(&source)
	switch {
	case rerr == nil:
		r.ReportFiled = true
		r.ReportSource = source.String
	case rerr == sql.ErrNoRows:
	default:
		return TaskReconciliation{}, rerr
	}
	if vs, verr := verdictsForAttempt(db, taskID, t.Attempt); verr == nil {
		for id, v := range vs {
			r.Verdicts[id] = v.Verdict
		}
	} else {
		return TaskReconciliation{}, verr
	}

	// One recommendation, ordered by what blocks the orchestrator
	// first. Every reason names the durable fact behind it.
	// Terminal tasks report their full facts with DONE — history is
	// the answer, not an empty probe.
	if terminalTaskStatus(t.Status) {
		r.Recommendation = "DONE"
		r.Reason = fmt.Sprintf("task is terminal (%s); history is preserved, use followup for more work", t.Status)
		// P3 metrics hook (additive only): records the resolution
		// timestamp; the recommendation and reason above are
		// untouched.
		observeReconciliation(taskID, t.Attempt, r)
		return r, nil
	}
	switch {
	case !started && t.Status == TaskPending:
		r.Recommendation = "WAIT"
		r.Reason = "task is pending; no dispatch has committed it to a worker yet"
	case !started:
		r.Recommendation = "WAIT"
		r.Reason = fmt.Sprintf("task is %s with no execution receipt at attempt %d: dispatched but never picked up; wait for the worker loop or investigate the session", t.Status, t.Attempt)
	case !r.ExecutionFinished:
		r.Recommendation = "WAIT"
		r.Reason = fmt.Sprintf("execution started (attempt %d) with no result yet: outcome UNKNOWN; wait, do not retry or cancel blindly", t.Attempt)
	case r.ExitCode != nil && *r.ExitCode != 0 && !r.ReportFiled:
		r.Recommendation = "INVESTIGATE"
		r.Reason = fmt.Sprintf("abnormal exit %d at attempt %d with no report: failure detail preserved in the execution receipt; investigate before retry or cancel", *r.ExitCode, t.Attempt)
	case !r.ReportFiled:
		r.Recommendation = "INVESTIGATE"
		r.Reason = fmt.Sprintf("execution finished (exit %d) at attempt %d with no report filed: result unrecorded; investigate the worker output before deciding", exitOrZero(r.ExitCode), t.Attempt)
	case len(r.Verdicts) == 0:
		r.Recommendation = "FILE_VERDICTS"
		r.Reason = "report filed with no criterion verdicts: evidence integrity alone cannot verify; an independent reviewer must judge each criterion"
	default:
		r.Recommendation = "REVIEW_VERDICTS"
		r.Reason = "verdicts filed: the orchestrator decides — verify on unanimous PASS, correct on FAIL, wait on INCONCLUSIVE"
	}
	// P3 metrics hook (additive only): records the uncertain-since
	// or resolved timestamp; the recommendation above is untouched.
	observeReconciliation(taskID, t.Attempt, r)
	return r, nil
}

func exitOrZero(c *int) int {
	if c == nil {
		return 0
	}
	return *c
}

// reconcileExecLine renders the execution fact in one line: never
// started, open (outcome unknown), or finished with exit + digest.
func reconcileExecLine(r TaskReconciliation) string {
	switch {
	case !r.ExecutionStarted:
		return "not started (no execution receipt at this attempt)"
	case !r.ExecutionFinished:
		return "started, result open (outcome UNKNOWN)"
	default:
		return fmt.Sprintf("finished exit=%d output=%s", exitOrZero(r.ExitCode), orDash(r.OutputDigest))
	}
}

// reconcileReportLine renders the report fact: filed with its
// provenance, or absent.
func reconcileReportLine(r TaskReconciliation) string {
	if !r.ReportFiled {
		return "none at this attempt"
	}
	return "filed (source=" + orDash(r.ReportSource) + ")"
}

// reconcileVerdictsLine renders filed verdicts, or their absence.
func reconcileVerdictsLine(r TaskReconciliation) string {
	if len(r.Verdicts) == 0 {
		return "none at this attempt"
	}
	parts := make([]string, 0, len(r.Verdicts))
	for id, v := range r.Verdicts {
		parts = append(parts, id+"="+v)
	}
	return joinSorted(parts)
}

// joinSorted joins parts in stable order so CLI output is
// deterministic across map iteration.
func joinSorted(parts []string) string {
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

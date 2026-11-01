package main

// worker_protocol.go: the versioned envelope binding a worker
// execution to its ledger attempt (remediation §6.1).
//
// The stored task row remains authoritative. The envelope is the
// machine-readable claim a managed worker loop presents when it
// picks up work: identity first, then execution. A runner must
// never regard the mere existence of a task-*.md file as dispatch
// authorization — only a committed ledger dispatch for its own
// session activates it.

import (
	"encoding/json"
	"fmt"
	"strings"
)

// WorkerProtocolVersion is the only envelope version a worker loop
// may present. Bump it when a required field changes meaning.
const WorkerProtocolVersion = 1

// Criterion is one independently judgable acceptance requirement.
// IDs are stable within a task; reviewers cite them in verdicts.
type Criterion struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
}

// TaskEnvelope binds one execution to its ledger attempt.
type TaskEnvelope struct {
	SchemaVersion int         `json:"schema_version"`
	TaskID        string      `json:"task_id"`
	Attempt       int         `json:"attempt"`
	RunID         string      `json:"run_id"`
	SessionID     string      `json:"session_id"`
	ExecutionRoot string      `json:"execution_root"`
	Objective     string      `json:"objective"`
	Acceptance    []Criterion `json:"acceptance"`
}

// Validate rejects anything ambiguous before it can drive execution.
func (e TaskEnvelope) Validate() error {
	if e.SchemaVersion != WorkerProtocolVersion {
		return fmt.Errorf("unsupported worker protocol version %d (want %d)", e.SchemaVersion, WorkerProtocolVersion)
	}
	if strings.TrimSpace(e.TaskID) == "" {
		return fmt.Errorf("task envelope missing task_id")
	}
	if e.Attempt < 1 {
		return fmt.Errorf("task envelope for %s names attempt %d; executions bind to a claimed attempt (>= 1)", e.TaskID, e.Attempt)
	}
	if strings.TrimSpace(e.RunID) == "" || strings.TrimSpace(e.SessionID) == "" {
		return fmt.Errorf("task envelope for %s missing run or session identity", e.TaskID)
	}
	if strings.TrimSpace(e.ExecutionRoot) == "" {
		return fmt.Errorf("task envelope for %s missing execution root", e.TaskID)
	}
	if strings.TrimSpace(e.Objective) == "" {
		return fmt.Errorf("task envelope for %s missing objective", e.TaskID)
	}
	for i, c := range e.Acceptance {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Requirement) == "" {
			return fmt.Errorf("task envelope for %s has incomplete criterion %d", e.TaskID, i)
		}
	}
	return nil
}

// Marshal renders the envelope for transport to the worker loop.
func (e TaskEnvelope) Marshal() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(e)
}

// ParseTaskEnvelope parses and validates a received envelope. The
// caller must still confirm the attempt against the ledger — a
// well-formed envelope is a claim, not an authorization.
func ParseTaskEnvelope(data []byte) (TaskEnvelope, error) {
	var e TaskEnvelope
	if err := json.Unmarshal(data, &e); err != nil {
		return TaskEnvelope{}, fmt.Errorf("task envelope unreadable: %w", err)
	}
	if err := e.Validate(); err != nil {
		return TaskEnvelope{}, err
	}
	return e, nil
}

// ReportSource labels what a filed report actually is. A structured
// response emitted by the model is a worker claim; unstructured
// stdout packaged by the runner is a runner observation; text typed
// into the CLI is an operator submission. The label travels with
// the report so no consumer mistakes one for another — and none of
// them is independent verification.
type ReportSource string

const (
	// SourceWorkerClaim is a structured result the model emitted
	// through the managed worker loop.
	SourceWorkerClaim ReportSource = "worker-claim"
	// SourceRunnerObservation is worker stdout/stderr the runner
	// packaged because the model emitted no structured result.
	SourceRunnerObservation ReportSource = "runner-observation"
	// SourceCLISubmission is text filed through the CLI by an
	// operator. The reporter string is an assertion, not an
	// authenticated identity.
	SourceCLISubmission ReportSource = "cli-submission"
)

// ReportFiling is one report submission with its provenance attached.
type ReportFiling struct {
	Report   string
	Source   ReportSource
	Reporter string
}

// Validate refuses empty or unlabelled filings before they can touch
// the ledger.
func (f ReportFiling) Validate() error {
	if strings.TrimSpace(f.Report) == "" {
		return fmt.Errorf("report must not be empty")
	}
	switch f.Source {
	case SourceWorkerClaim, SourceRunnerObservation, SourceCLISubmission:
	default:
		return fmt.Errorf("unknown report source %q", string(f.Source))
	}
	if strings.TrimSpace(f.Reporter) == "" {
		return fmt.Errorf("report filing must name its reporter")
	}
	return nil
}

package main

// ops_diag.go: phase-level operation diagnostics (Package D).
//
// Structured, low-overhead observation of the existing durable state
// machines: dispatch, claim, execution, report, verification,
// cancellation, reconciliation. Every event carries consistent
// fields (request_id, task_id, attempt, session, phase,
// disposition, error class).
//
// What this is NOT:
//   - Not a second source of truth. Diagnostics OBSERVE the durable
//     ledger transitions; they never gate control flow and nothing
//     branches on them.
//   - No additional persistence. Events live in a bounded in-memory
//     ring (opsRingCap entries, mutex-guarded, oldest overwritten).
//     No per-event DB writes happen outside the receipts the
//     underlying operations already file.
//   - No sensitive content. The only free text recorded is the
//     (redacted, truncated) error string of the observed operation.
//     Call sites pass no prompts, credentials, or worker output, and
//     redactOpsDetail scrubs credential-looking key=value pairs from
//     error text before storage.
//
// Overhead: one small struct copy plus a mutex hold per observed
// operation — trivial next to the DB transactions those operations
// already perform. Stderr sampling is opt-in: when TUI_DASH_OPS_DIAG
// is set in the environment, error dispositions are also logged to
// stderr (one line each). Successes never touch stderr.
//
// The same opt-in also enables a size-bounded JSONL sink at
// $TUI_DASH_STATE/ops_diag.jsonl (opsSinkMaxLines lines,
// opsSinkMaxBytes bytes, 0600 under the 0700 state dir). The sink
// exists so an operator can read recent diagnostics with the `ops`
// command: the ring is process-local, so a separate CLI process
// can never see it, while the sink file outlives the process that
// wrote it. The sink carries exactly what the ring carries —
// already redacted detail, never prompts, credentials, or worker
// output — and it is NOT authoritative: appends are best-effort
// (dropped on any I/O error), old rows are discarded under the
// caps, and nothing branches on its contents. The durable ledger
// (task rows, reports, receipts) remains the only record of what
// happened.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// opsPhase is one durable-state-machine phase under observation.
type opsPhase string

const (
	opsPhaseDispatch       opsPhase = "dispatch"
	opsPhaseClaim          opsPhase = "claim"
	opsPhaseExecution      opsPhase = "execution"
	opsPhaseReport         opsPhase = "report"
	opsPhaseVerification   opsPhase = "verification"
	opsPhaseCancellation   opsPhase = "cancellation"
	opsPhaseReconciliation opsPhase = "reconciliation"
)

// opsDispositionOK and opsDispositionError are the only dispositions.
// Diagnostics record what happened, never why it should have.
const (
	opsDispositionOK    = "ok"
	opsDispositionError = "error"
)

// opsEvent is one observed phase outcome.
type opsEvent struct {
	At          time.Time
	Phase       opsPhase
	RequestID   string
	TaskID      string
	Attempt     int
	Session     string
	Disposition string
	ErrorClass  string
	Detail      string
}

// opsRingCap bounds the in-memory ring. Oldest events are
// overwritten; capacity is fixed so memory use is constant.
const opsRingCap = 256

// opsDetailCap bounds the stored detail string.
const opsDetailCap = 240

var (
	opsMu    sync.Mutex
	opsBuf   [opsRingCap]opsEvent
	opsCount int
	opsNext  int
)

// observeOps records one phase outcome. It never fails, never
// blocks on I/O, and never touches the database. A nil err records
// an "ok" disposition; a non-nil err records "error" with a
// classified, redacted detail.
func observeOps(phase opsPhase, requestID, taskID string, attempt int, session string, err error) {
	ev := opsEvent{
		At:          time.Now().UTC(),
		Phase:       phase,
		RequestID:   requestID,
		TaskID:      taskID,
		Attempt:     attempt,
		Session:     session,
		Disposition: opsDispositionOK,
	}
	if err != nil {
		ev.Disposition = opsDispositionError
		ev.ErrorClass = classifyOpsError(err)
		ev.Detail = redactOpsDetail(err.Error())
		if len(ev.Detail) > opsDetailCap {
			ev.Detail = ev.Detail[:opsDetailCap] + "…"
		}
	}
	opsMu.Lock()
	opsBuf[opsNext] = ev
	opsNext = (opsNext + 1) % opsRingCap
	if opsCount < opsRingCap {
		opsCount++
	}
	opsMu.Unlock()
	if err != nil && os.Getenv("TUI_DASH_OPS_DIAG") != "" {
		fmt.Fprintf(os.Stderr, "ops_diag phase=%s task=%s attempt=%d req=%s class=%s detail=%s\n",
			phase, taskID, attempt, requestID, ev.ErrorClass, ev.Detail)
	}
	opsSinkAppend(ev)
}

// opsSnapshot returns buffered events oldest-first. It is the only
// read path; operators and tests use it, control flow never does.
func opsSnapshot() []opsEvent {
	opsMu.Lock()
	defer opsMu.Unlock()
	out := make([]opsEvent, 0, opsCount)
	for i := 0; i < opsCount; i++ {
		out = append(out, opsBuf[(opsNext-opsCount+i+opsRingCap*2)%opsRingCap])
	}
	return out
}

// opsReset clears the ring. Tests only.
func opsReset() {
	opsMu.Lock()
	defer opsMu.Unlock()
	opsBuf = [opsRingCap]opsEvent{}
	opsCount = 0
	opsNext = 0
}

// classifyOpsError maps an operation error to a stable class for
// aggregation. Matching is substring-based over the durable
// state machines' existing error text; anything unrecognized is
// "unknown", never dropped.
func classifyOpsError(err error) string {
	if err == nil {
		return ""
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "concurrent dispatch won"),
		strings.Contains(s, "already reconciled"),
		strings.Contains(s, "already has a report"),
		strings.Contains(s, "stale report"):
		return "conflict"
	case strings.Contains(s, "is terminal"),
		strings.Contains(s, "terminal ("):
		return "terminal"
	case strings.Contains(s, "budget"),
		strings.Contains(s, "concurrency"),
		strings.Contains(s, "at limit"):
		return "budget"
	case strings.Contains(s, "no such task"),
		strings.Contains(s, "no such session"):
		return "not_found"
	case strings.Contains(s, "remote principal"):
		return "auth"
	case strings.Contains(s, "no evidence"),
		strings.Contains(s, "evidence"),
		strings.Contains(s, "digest"),
		strings.Contains(s, "verdict"),
		strings.Contains(s, "acceptance criteria"),
		strings.Contains(s, "criterion"):
		return "verification"
	case strings.Contains(s, "reconcile"),
		strings.Contains(s, "may still be active"),
		strings.Contains(s, "nothing uncertain"):
		return "reconcile_refused"
	default:
		return "unknown"
	}
}

// opsSecretRe matches credential-looking key=value pairs so error
// text can never carry a secret into the ring.
var opsSecretRe = regexp.MustCompile(`(?i)(api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|token|secret|password|passwd|pwd|bearer|private[_-]?key|credentials?)\s*[:=]\s*\S+`)

// opsBearerRe matches standalone bearer-style tokens.
var opsBearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/=]{8,}`)

// opsKeyRe matches standalone long sk-style keys.
var opsKeyRe = regexp.MustCompile(`\bsk-[A-Za-z0-9\-_]{8,}\b`)

// opsSinkName is the opt-in diagnostic log under the state dir.
// It is process-local durability of last resort: the ring dies
// with the process, this file does not, so a later `ops` read in
// a different process can still see recent outcomes.
const opsSinkName = "ops_diag.jsonl"

// opsSinkMaxLines and opsSinkMaxBytes bound the sink. Oldest rows
// are discarded first; the sink never grows without limit no
// matter how long the opt-in stays on.
const opsSinkMaxLines = 2000
const opsSinkMaxBytes = 512 << 10

// opsSinkMu serializes sink appends and reads inside this process.
// Cross-process appends interleave at line granularity (O_APPEND);
// readers tolerate torn or partial trailing lines by skipping
// unparseable rows.
var opsSinkMu sync.Mutex

// opsSinkLines counts rows appended by this process since the last
// compaction (or since startup, once initialized). It is an
// approximation only — another process may append concurrently —
// so the byte cap (checked with stat on every append) is the
// authoritative bound and this counter is just the cheap tripwire
// for the line cap.
var opsSinkLines = -1

// opsSinkEnabled reports whether the opt-in sink (and stderr
// sampling) is active. Same gate for both: TUI_DASH_OPS_DIAG set
// to anything.
func opsSinkEnabled() bool { return os.Getenv("TUI_DASH_OPS_DIAG") != "" }

// opsSinkPath is the sink location. It lives under stateDir so it
// inherits the owner-only posture (0700 dir, 0600 file): only the
// operator that owns the state can read diagnostics.
func opsSinkPath() string { return filepath.Join(stateDir(), opsSinkName) }

// opsEventJSON is the on-disk form of one opsEvent. Detail here is
// the already redacted, truncated string from the ring — redaction
// is preserved by construction, never re-applied.
type opsEventJSON struct {
	At          string `json:"at"`
	Phase       string `json:"phase"`
	RequestID   string `json:"request_id"`
	TaskID      string `json:"task_id"`
	Attempt     int    `json:"attempt"`
	Session     string `json:"session"`
	Disposition string `json:"disposition"`
	ErrorClass  string `json:"error_class,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

func opsEventToJSON(ev opsEvent) opsEventJSON {
	return opsEventJSON{
		At:          ev.At.UTC().Format(time.RFC3339Nano),
		Phase:       string(ev.Phase),
		RequestID:   ev.RequestID,
		TaskID:      ev.TaskID,
		Attempt:     ev.Attempt,
		Session:     ev.Session,
		Disposition: ev.Disposition,
		ErrorClass:  ev.ErrorClass,
		Detail:      ev.Detail,
	}
}

func opsEventFromJSON(j opsEventJSON) opsEvent {
	ev := opsEvent{
		Phase:       opsPhase(j.Phase),
		RequestID:   j.RequestID,
		TaskID:      j.TaskID,
		Attempt:     j.Attempt,
		Session:     j.Session,
		Disposition: j.Disposition,
		ErrorClass:  j.ErrorClass,
		Detail:      j.Detail,
	}
	if t, err := time.Parse(time.RFC3339Nano, j.At); err == nil {
		ev.At = t
	}
	return ev
}

// opsSinkAppend records one event to the opt-in JSONL sink.
// Best-effort and observe-only: it never fails, never blocks
// longer than a bounded file rewrite, and any I/O error silently
// drops the row. The ring (and the ledger beneath it) stays the
// record; this file is a convenience copy.
func opsSinkAppend(ev opsEvent) {
	if !opsSinkEnabled() {
		return
	}
	line, err := json.Marshal(opsEventToJSON(ev))
	if err != nil {
		return
	}
	line = append(line, '\n')
	opsSinkMu.Lock()
	defer opsSinkMu.Unlock()
	path := opsSinkPath()
	if opsSinkLines < 0 {
		opsSinkLines = opsSinkCountLines(path)
	}
	if st, serr := os.Stat(path); serr == nil &&
		(st.Size()+int64(len(line)) > opsSinkMaxBytes || opsSinkLines >= opsSinkMaxLines) {
		if !opsSinkCompact(path) {
			return
		}
		opsSinkLines = opsSinkCountLines(path)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_ = os.Chmod(path, 0o600)
	if _, err := f.Write(line); err != nil {
		return
	}
	opsSinkLines++
}

// opsSinkCountLines counts newline-terminated rows, tolerating a
// missing file (zero) and capping the read so a foreign large
// file cannot make observation expensive.
func opsSinkCountLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	if len(data) > opsSinkMaxBytes+(1<<20) {
		data = data[len(data)-opsSinkMaxBytes-(1<<20):]
	}
	return strings.Count(string(data), "\n")
}

// opsSinkCompact rewrites the sink keeping only the newest half
// of rows under both caps. Caller holds opsSinkMu. A false return
// means the row being appended must be dropped this time.
func opsSinkCompact(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	lines := strings.Split(string(data), "\n")
	// Drop trailing partial line: a torn concurrent append must
	// not pin old rows in the file forever.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	keep := opsSinkMaxLines / 2
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	var b strings.Builder
	for _, ln := range lines {
		// Enforce the byte cap on the compacted body too.
		if b.Len()+len(ln)+1 > opsSinkMaxBytes/2 {
			continue
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	// Drop oldest first until the body fits the byte half-cap.
	body := b.String()
	for len(body) > opsSinkMaxBytes/2 && len(body) > 0 {
		if i := strings.IndexByte(body, '\n'); i >= 0 {
			body = body[i+1:]
		} else {
			body = ""
		}
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return false
	}
	_ = os.Chmod(path, 0o600)
	return true
}

// readOpsSink returns up to limit newest sink rows, oldest-first.
// Read-only: it never creates, truncates, or otherwise mutates
// the sink. Unparseable rows are skipped — the sink is
// best-effort, never authoritative — while a missing file or an
// unreadable one is reported so the caller can say so honestly.
func readOpsSink(limit int) ([]opsEvent, error) {
	if limit <= 0 {
		return nil, nil
	}
	if limit > opsSinkMaxLines {
		limit = opsSinkMaxLines
	}
	opsSinkMu.Lock()
	defer opsSinkMu.Unlock()
	data, err := os.ReadFile(opsSinkPath())
	if err != nil {
		return nil, err
	}
	var out []opsEvent
	for _, ln := range strings.Split(string(data), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var j opsEventJSON
		if err := json.Unmarshal([]byte(ln), &j); err != nil {
			continue
		}
		out = append(out, opsEventFromJSON(j))
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// redactOpsDetail scrubs credential-looking values from detail
// text. Structure is kept (so the class of failure stays visible)
// while secret values are replaced.
func redactOpsDetail(s string) string {
	s = opsSecretRe.ReplaceAllString(s, `$1=<redacted>`)
	s = opsBearerRe.ReplaceAllString(s, "bearer <redacted>")
	s = opsKeyRe.ReplaceAllString(s, "<redacted-key>")
	return s
}

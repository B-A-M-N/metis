package main

// cli_ops.go: operator read surface for phase diagnostics.
//
// The in-memory ops ring is process-local: a separate `ops`
// invocation can never read it. The `ops` command therefore reads
// the opt-in JSONL sink ($TUI_DASH_STATE/ops_diag.jsonl, enabled
// with TUI_DASH_OPS_DIAG=1) that the owning process appends while
// it runs. Same gate, same redacted content, same caps.
//
// This command is observe-only by construction: its only state
// access is a bounded read of the sink file. It opens no database,
// files no receipts, transitions nothing, and treats the sink as
// the lossy convenience copy it is — unparseable rows are skipped,
// and the durable ledger (tasks, reports, receipts) remains the
// only authoritative record.
//
// Authorization rides on the state dir posture, not on a new
// mechanism: the sink is 0600 under the 0700 state dir (see
// hardenStatePerms), so only the operator owning the state can
// read it — the same trust that already guards sessions.json and
// tasks.db. No remote transport reaches this command; like every
// other CLI read here it runs as the local owner.
//
// Wiring: dispatched from main.go dispatchCLI (case "ops").
// This surface owns only flag parsing, filtering, and rendering.

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// opsHumanCap bounds one human-readable row so a pathological
// detail string cannot flood the terminal; the stored row is
// already capped at opsDetailCap, this is display-only.
const opsHumanCap = 300

// opsFilter selects correlated evidence within one bounded read.
// Empty strings and attempt < 0 match everything; since <= 0
// disables the time bound. Filters never widen the read: --limit
// still bounds the sink rows scanned.
type opsFilter struct {
	req, task, session, phase, class string
	attempt                         int
	errorsOnly                      bool
	since                           time.Duration
}

// filterOpsEvents keeps the events matching f, oldest-first in,
// oldest-first out. Pure: operators and tests share it, control
// flow never touches it.
func filterOpsEvents(evs []opsEvent, f opsFilter, now time.Time) []opsEvent {
	out := evs[:0:0]
	for _, ev := range evs {
		if f.req != "" && ev.RequestID != f.req {
			continue
		}
		if f.task != "" && ev.TaskID != f.task {
			continue
		}
		if f.session != "" && ev.Session != f.session {
			continue
		}
		if f.phase != "" && string(ev.Phase) != f.phase {
			continue
		}
		if f.class != "" && ev.ErrorClass != f.class {
			continue
		}
		if f.attempt >= 0 && ev.Attempt != f.attempt {
			continue
		}
		if f.errorsOnly && ev.Disposition != opsDispositionError {
			continue
		}
		if f.since > 0 && now.Sub(ev.At) > f.since {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// validOpsPhase reports whether p names a known phase (or all, when
// empty). Unknown names are refused so a typo cannot silently
// select nothing.
func validOpsPhase(p string) bool {
	if p == "" {
		return true
	}
	for _, known := range []opsPhase{
		opsPhaseDispatch, opsPhaseClaim, opsPhaseExecution,
		opsPhaseReport, opsPhaseVerification, opsPhaseCancellation,
		opsPhaseReconciliation,
	} {
		if p == string(known) {
			return true
		}
	}
	return false
}

// cliOps implements the `ops` command: bounded, read-only dump
// of recent operation diagnostics.
func cliOps(args []string) int {
	fs := flag.NewFlagSet("ops", flag.ContinueOnError)
	limit := fs.Int("limit", 50, "max events to show (newest kept, oldest-first)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	req := fs.String("req", "", "only events for this request ID")
	task := fs.String("task", "", "only events for this task ID")
	attempt := fs.Int("attempt", -1, "only events for this attempt number")
	session := fs.String("session", "", "only events for this session")
	phase := fs.String("phase", "", "only events in this phase (dispatch|claim|execution|report|verification|cancellation|reconciliation)")
	class := fs.String("class", "", "only events with this error class (conflict|terminal|budget|not_found|auth|verification|reconcile_refused|unknown)")
	errorsOnly := fs.Bool("errors-only", false, "only error dispositions")
	since := fs.Duration("since", 0, "only events from the last duration (e.g. 30m, 2h)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: tui-dash ops [--limit N] [--json] [--req ID] [--task ID] [--attempt N] [--session S] [--phase P] [--class C] [--errors-only] [--since D]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Read recent operation diagnostics (best-effort sample;")
		fmt.Fprintln(os.Stderr, "the ledger stays authoritative). Filters select within")
		fmt.Fprintln(os.Stderr, "the bounded read; --limit still bounds rows scanned.")
		fmt.Fprintln(os.Stderr, "Opt in with TUI_DASH_OPS_DIAG=1; reads $TUI_DASH_STATE/ops_diag.jsonl.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !validOpsPhase(*phase) {
		fmt.Fprintf(os.Stderr, "unknown phase %q (want dispatch|claim|execution|report|verification|cancellation|reconciliation)\n", *phase)
		return 2
	}
	if *class != "" && !opsKnownClass(*class) {
		fmt.Fprintf(os.Stderr, "unknown error class %q\n", *class)
		return 2
	}
	n := *limit
	if n <= 0 {
		n = 50
	}
	if n > opsSinkMaxLines {
		n = opsSinkMaxLines
	}
	evs, err := readOpsSink(n)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "no ops diagnostics recorded yet ("+opsSinkPath()+"; set TUI_DASH_OPS_DIAG=1 to opt in)")
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	evs = filterOpsEvents(evs, opsFilter{
		req: *req, task: *task, session: *session, phase: *phase,
		class: *class, attempt: *attempt, errorsOnly: *errorsOnly,
		since: *since,
	}, time.Now().UTC())
	if *asJSON {
		out := make([]opsEventJSON, 0, len(evs))
		for _, ev := range evs {
			out = append(out, opsEventToJSON(ev))
		}
		if out == nil {
			out = []opsEventJSON{}
		}
		emitJSON(out)
		return 0
	}
	if len(evs) == 0 {
		fmt.Println("(no ops diagnostics recorded)")
		return 0
	}
	for _, ev := range evs {
		fmt.Println(formatOpsEvent(ev))
	}
	return 0
}

// formatOpsEvent renders one event as a single stable row.
// Empty identity fields print as "-" so missing correlation is
// visible instead of looking like column drift.
func formatOpsEvent(ev opsEvent) string {
	req := ev.RequestID
	if req == "" {
		req = "-"
	}
	sess := ev.Session
	if sess == "" {
		sess = "-"
	}
	detail := ev.Detail
	if len(detail) > opsHumanCap {
		detail = detail[:opsHumanCap] + "…"
	}
	sb := strings.Builder{}
	fmt.Fprintf(&sb, "%s %-14s task=%s attempt=%d req=%s sess=%s %s",
		ev.At.UTC().Format("2006-01-02T15:04:05Z"),
		string(ev.Phase), ev.TaskID, ev.Attempt, req, sess, ev.Disposition)
	if ev.ErrorClass != "" {
		fmt.Fprintf(&sb, " class=%s", ev.ErrorClass)
	}
	if detail != "" {
		fmt.Fprintf(&sb, " %s", detail)
	}
	return sb.String()
}

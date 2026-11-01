package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// cli_tasks.go: task and run commands. Extracted from main.go; behavior unchanged.

// cliTaskCreate handles the 'task-create' command.
func cliTaskCreate(args []string) int {
	fs := flag.NewFlagSet("task-create", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	agent := fs.String("agent", "", "worker session name")
	objective := fs.String("objective", "", "task objective")
	acceptance := fs.String("acceptance", "", "acceptance criteria")
	key := fs.String("key", "", "idempotency key: repeat creates return the existing task")
	workdir := fs.String("workdir", "", "authorized workspace root (enforced at dispatch)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	t, err := createTaskIdem(*run, *agent, *objective, *acceptance, *key)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if strings.TrimSpace(*workdir) != "" {
		req := OperationRequest{Actor: "local-cli", RequestID: newRequestID()}
		if aerr := authorizeTaskRoot(t.ID, *workdir, req); aerr != nil {
			fmt.Fprintln(os.Stderr, aerr)
			return 1
		}
	}
	if *asJSON {
		emitJSON(t)
		return 0
	}
	fmt.Printf("created %s status=%s\n", t.ID, t.Status)
	return 0
}

// cliTaskList handles the 'task-list' command.
func cliTaskList(args []string) int {
	fs := flag.NewFlagSet("task-list", flag.ExitOnError)
	run := fs.String("run", "", "filter by run id (empty: all)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	tasks, err := listTasks(*run)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if tasks == nil {
			tasks = []Task{}
		}
		emitJSON(tasks)
		return 0
	}
	if len(tasks) == 0 {
		fmt.Println("(no tasks)")
		return 0
	}
	for _, t := range tasks {
		fmt.Printf("%s  run=%s  agent=%s  status=%s  attempt=%d  parent=%s\n",
			t.ID, t.Run, t.Agent, t.Status, t.Attempt, orDash(t.Parent))
	}
	return 0
}

// cliTaskStatus handles the 'task-status' command.
func cliTaskStatus(args []string) int {
	fs := flag.NewFlagSet("task-status", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	t, err := getTask(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(t)
		return 0
	}
	fmt.Printf("id: %s\nrun: %s\nagent: %s\nstatus: %s\nattempt: %d\nparent: %s\nobjective: %s\nacceptance: %s\nreport: %s\n",
		t.ID, t.Run, t.Agent, t.Status, t.Attempt, orDash(t.Parent), t.Objective, orDash(t.Acceptance), orDash(t.Report))
	// F-07: consumer-readiness rides alongside the ledger status so
	// a parked assignment never reads as a healthy handoff.
	ds := projectTaskDispatchState(t)
	fmt.Printf("dispatch_state: %s\nconsumer_attached: %v\n", ds.State, ds.Attached)
	if ds.Generation > 0 {
		fmt.Printf("consumer_generation: %d\n", ds.Generation)
	}
	if ds.PickupDeadline != "" {
		fmt.Printf("pickup_deadline: %s\n", ds.PickupDeadline)
	}
	if ds.BlockedReason != "" {
		fmt.Printf("blocked_reason: %s\n", ds.BlockedReason)
	}
	return 0
}

// cliTaskDispatch handles the 'task-dispatch' command.
func cliTaskDispatch(args []string) int {
	fs := flag.NewFlagSet("task-dispatch", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	session := fs.String("session", "", "worker session name")
	_ = fs.Parse(args)
	if err := dispatchTask(*id, *session); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("dispatched %s to %s\n", *id, *session)
	// F-07: the commit guarantees no consumer. Say so now, with the
	// deadline and blocked reason, instead of leaving a parked
	// assignment to read as a healthy handoff.
	if t, gerr := getTask(*id); gerr == nil {
		if ds := projectTaskDispatchState(t); ds.State == DispatchStatePickupPending {
			fmt.Fprintf(os.Stderr, "pickup_pending: %s (pickup deadline %s)\n", ds.BlockedReason, ds.PickupDeadline)
		}
	}
	return 0
}

// cliWorkerLoop handles the 'worker-loop' command.
func cliWorkerLoop(args []string) int {
	fs := flag.NewFlagSet("worker-loop", flag.ExitOnError)
	session := fs.String("session", "", "managed worker session name")
	harness := fs.String("harness", "", "harness to execute with (default: session's bound harness)")
	once := fs.Bool("once", false, "execute at most one task then exit")
	waitSecs := fs.Int("wait", 300, "idle seconds to wait for committed work before exiting (0 = exit when idle)")
	requester := fs.String("requester", "operator", "asserted identity driving the loop (receipts only)")
	_ = fs.Parse(args)
	if *session == "" {
		fmt.Fprintln(os.Stderr, "worker-loop requires --session")
		return 1
	}
	reg := loadRegistry()
	entry, ok := reg[*session]
	if !ok {
		fmt.Fprintf(os.Stderr, "no such session: %s\n", *session)
		return 1
	}
	h := *harness
	if h == "" {
		h = entry.Harness
	}
	if h == "" {
		fmt.Fprintln(os.Stderr, "worker-loop needs --harness or a harness-bound session")
		return 1
	}
	a, aerr := lookupAdapter(h)
	if aerr != nil {
		fmt.Fprintln(os.Stderr, aerr)
		return 1
	}
	if a.ManagedArgv == nil && a.ManagedTurn == nil {
		fmt.Fprintf(os.Stderr, "harness %q has no managed execution path; refusing to launch\n", h)
		return 1
	}
	req := OperationRequest{Actor: *requester, RequestID: newRequestID()}
	if err := requireRequest(req); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Restart reconciliation first: name any execution the
	// previous loop left uncertain before new work can
	// supersede it. Observation only — nothing moves.
	if n, rerr := reconcileLoopRestart(*session, req); rerr != nil {
		fmt.Fprintln(os.Stderr, rerr)
		return 1
	} else if n > 0 {
		fmt.Fprintf(os.Stderr, "worker-loop: %d incomplete execution(s) need reconciliation; see task-reconcile\n", n)
	}
	// F-07 supervisor path: (re)starting the loop (re)starts the
	// durable consumer identity — new generation, cleared stale
	// assignment, fresh heartbeat — so dispatch readiness flips
	// from pickup_pending to consumer_attached for this session.
	// (The --harness flag may override the session binding, so the
	// CLI registers the resolved adapter here rather than entering
	// through ensureManagedWorkerLoop, which supervises the bound
	// harness.)
	if gen, serr := startWorkerConsumer(*session, a); serr != nil {
		fmt.Fprintln(os.Stderr, serr)
		return 1
	} else {
		fmt.Fprintf(os.Stderr, "worker-loop: consumer generation %d attached for %s (%s)\n",
			gen, *session, adapterCapabilities(a))
	}
	if *once {
		ran, err := runWorkerOnce(*session, a, req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if !ran {
			fmt.Fprintln(os.Stderr, "worker-loop: idle, nothing committed")
		}
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// F-09: the long-lived loop owns autonomous stream retention so
	// unobserved sessions stay bounded while a worker is supervised.
	sweepStop := make(chan struct{})
	sweepDone := StartRetentionSweeper(0, sweepStop)
	defer func() { close(sweepStop); <-sweepDone }()
	for {
		ran, err := runWorkerOnce(*session, a, req)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if ran {
			continue
		}
		if *waitSecs <= 0 {
			break
		}
		if !waitForCommittedWork(*session, 2*time.Second, time.Duration(*waitSecs)*time.Second, ctx.Done()) {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "worker-loop: interrupted, exiting")
				break
			}
			fmt.Fprintln(os.Stderr, "worker-loop: idle wait expired, exiting")
			break
		}
	}
	return 0
}

// cliTaskReport handles the 'task-report' command.
func cliTaskReport(args []string) int {
	fs := flag.NewFlagSet("task-report", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	report := fs.String("report", "", "report text (or @file)")
	reporter := fs.String("reporter", "local-cli", "identity filing this report (asserted, not authenticated)")
	_ = fs.Parse(args)
	text := *report
	if strings.HasPrefix(text, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(text, "@"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		text = string(data)
	}
	// CLI filings are labelled cli-submission with the asserted
	// reporter: they are never recorded as worker claims.
	filing := ReportFiling{Report: text, Source: SourceCLISubmission, Reporter: *reporter}
	if err := submitReport(*id, filing); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("report recorded for %s\n", *id)
	return 0
}

// cliTaskVerdict handles the 'task-verdict' command.
func cliTaskVerdict(args []string) int {
	fs := flag.NewFlagSet("task-verdict", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	criterion := fs.String("criterion", "", "criterion id from the task acceptance list (e.g. C1)")
	verdict := fs.String("verdict", "", "PASS, FAIL, or INCONCLUSIVE")
	reason := fs.String("reason", "", "why this judgment holds")
	evidence := fs.String("evidence", "", "comma-separated artifact paths cited (must be current-attempt artifacts)")
	reviewer := fs.String("reviewer", "local-cli", "identity judging this criterion (asserted, not authenticated; never the worker)")
	_ = fs.Parse(args)
	var ev []string
	for _, p := range strings.Split(*evidence, ",") {
		if strings.TrimSpace(p) != "" {
			ev = append(ev, strings.TrimSpace(p))
		}
	}
	v := CriterionVerdict{CriterionID: *criterion, Verdict: strings.ToUpper(strings.TrimSpace(*verdict)),
		Reviewer: *reviewer, Evidence: ev, Reason: *reason}
	if got, gerr := getTask(*id); gerr == nil {
		v.Attempt = got.Attempt
	}
	if err := fileCriterionVerdict(*id, v, OperationRequest{Actor: *reviewer, RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("verdict recorded for %s %s\n", *id, *criterion)
	return 0
}

// cliTaskReconcile handles the 'task-reconcile' command.
func cliTaskReconcile(args []string) int {
	fs := flag.NewFlagSet("task-reconcile", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	r, err := reconcileTask(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(r)
		return 0
	}
	fmt.Printf("task: %s\nstatus: %s\nattempt: %d (attempt=%s delivery=%s)\nexecution: %s\nreport: %s\nverdicts: %s\nrecommendation: %s\nreason: %s\n",
		r.TaskID, r.Status, r.Attempt, orDash(r.AttemptState), orDash(r.DeliveryState),
		reconcileExecLine(r), reconcileReportLine(r), reconcileVerdictsLine(r),
		r.Recommendation, r.Reason)
	return 0
}

// cliTaskVerify handles the 'task-verify' command.
func cliTaskVerify(args []string) int {
	fs := flag.NewFlagSet("task-verify", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	pass := fs.Bool("pass", false, "mark verified (default: failed)")
	approve := fs.String("approve", "", "operator approval identity for evidence-free tasks (required with --pass when no artifacts verify)")
	verifier := fs.String("verifier", "local-cli", "identity deciding this verdict (never the worker)")
	_ = fs.Parse(args)
	if err := verifyTask(*id, *pass, *approve, OperationRequest{Actor: *verifier, RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *pass {
		fmt.Printf("verified %s\n", *id)
	} else {
		fmt.Printf("failed %s\n", *id)
	}
	return 0
}

// cliTaskCancel handles the 'task-cancel' command.
func cliTaskCancel(args []string) int {
	fs := flag.NewFlagSet("task-cancel", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	_ = fs.Parse(args)
	if err := cancelTask(*id, OperationRequest{Actor: "local-cli", RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("cancelled %s\n", *id)
	return 0
}

// cliTaskFollowup handles the 'task-followup' command.
func cliTaskFollowup(args []string) int {
	fs := flag.NewFlagSet("task-followup", flag.ExitOnError)
	id := fs.String("task", "", "prior task id")
	objective := fs.String("objective", "", "correction objective")
	_ = fs.Parse(args)
	t, err := followupTask(*id, *objective, OperationRequest{Actor: "local-cli", RequestID: newRequestID()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("created %s parent=%s\n", t.ID, t.Parent)
	return 0
}

// cliTaskDepend handles the 'task-depend' command.
func cliTaskDepend(args []string) int {
	fs := flag.NewFlagSet("task-depend", flag.ExitOnError)
	id := fs.String("task", "", "dependent task id")
	on := fs.String("on", "", "prerequisite task id (must verify before dependent starts)")
	_ = fs.Parse(args)
	if err := addTaskDep(*id, *on, OperationRequest{Actor: "local-cli", RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("depends %s on %s\n", *id, *on)
	return 0
}

// cliRunFinalize handles the 'run-finalize' command.
func cliRunFinalize(args []string) int {
	fs := flag.NewFlagSet("run-finalize", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	sum, err := finalizeRun(*run, OperationRequest{Actor: "local-cli", RequestID: newRequestID()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(sum)
		return 0
	}
	fmt.Printf("finalized %s total=%d verified=%d failed=%d cancelled=%d\n",
		sum.Run, sum.Total, sum.Verified, sum.Failed, sum.Cancelled)
	return 0
}

// cliRunLimit handles the 'run-limit' command.
func cliRunLimit(args []string) int {
	fs := flag.NewFlagSet("run-limit", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	maxLive := fs.Int("max-live", 0, "maximum simultaneously live (dispatched/working) tasks")
	_ = fs.Parse(args)
	if err := setRunLimit(*run, *maxLive, OperationRequest{Actor: "local-cli", RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("run %s limited to %d live tasks\n", *run, *maxLive)
	return 0
}

// cliTaskSetRoot handles the 'task-set-root' command.
func cliTaskSetRoot(args []string) int {
	fs := flag.NewFlagSet("task-set-root", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	workdir := fs.String("workdir", "", "authorized workspace directory")
	_ = fs.Parse(args)
	if err := authorizeTaskRoot(*id, *workdir, OperationRequest{Actor: "local-cli", RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("root authorized for %s\n", *id)
	return 0
}

// cliTaskShare handles the 'task-share' command.
func cliTaskShare(args []string) int {
	fs := flag.NewFlagSet("task-share", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	_ = fs.Parse(args)
	if err := authorizeSharedWorkspace(*id, OperationRequest{Actor: "local-cli", RequestID: newRequestID()}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("shared workspace authorized for %s\n", *id)
	return 0
}

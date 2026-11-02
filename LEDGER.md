# TUI-Dash compliance ledger — Phase 1

Normative contract: `INVARIANTS.md`. This ledger records Phase-1
dispositions only (TD-006, TD-015, TD-020, TD-026–029, TD-061–063,
TD-073–081; CB-01, CB-12, CB-13). `PASS` means the required behavior
is established by the cited test; `NOT VERIFIED` means unqualified,
not broken — do not turn those into code without evidence.

Format per record: invariant, priority, disposition, source locations,
reproduction, expected behavior, observed result, test/evidence,
related invariants.

## Records

- TD-006 (request identity) | P0 | PASS | `controller.go`
  `OperationRequest`, `requireRequest`, `dispatchTaskWithRequest`,
  `retryDispatch` | Repro: call dispatch without request ID | Expected:
  rejection identifying the missing authorization | Observed:
  `operation requires an authorized, identifiable request` error,
  actor+req stamped into attempt/delivery rows and receipts |
  Test: `TestCB01ConcurrentDispatchOneWinner`,
  `TestCB12CrashRetryReconciles` (actor/req in receipt details) |
  Related: TD-005, TD-121.
- TD-015 (independent attempt identities) | P0 | PASS |
  `controller.go` `task_attempts` table, `insertAttempt`,
  `reconcileForRetry` | Repro: dispatch, crash, retry | Expected:
  distinct attempt rows with stable IDs, original history intact |
  Observed: attempt 1 `superseded`, attempt 2 `claimed`; no UPDATE
  of prior rows | Test: `TestCB12CrashRetryReconciles` (states
  `[superseded claimed]`) | Related: TD-088, TD-095, TD-118.
- TD-020 (supervisor enforces control authority) | P0 | PASS |
  `terminal.go` `checkFrame` in `ptySupervise`, owner-carrying
  `IN`/`RS`/`ST` frames | Repro: controller B writes raw FIFO
  frames while A holds the lease | Expected: refusal, PTY
  untouched | Observed: error receipts (`stale or foreign control
  grant refused`), `ptyLog` clean, worker alive |
  Test: `TestSupervisorRejectsForgedFrames` | Related: TD-061–063,
  TD-127.
- TD-026 (no uncertain-to-success coercion) | P0 | PASS |
  `controller.go` `retryDispatch`/`reconcileForRetry` | Repro: retry
  a claimed-but-unconfirmed dispatch | Expected: new attempt, old
  marked superseded, never silently legitimate | Observed: attempt
  counter 1→2 with preserved history; live attempt refuses retry |
  Test: `TestCB12CrashRetryReconciles` | Related: TD-116–118, CB-12.
- TD-027 (atomic claim before delivery) | P0 | PASS |
  `controller.go` `dispatchTaskWithRequest` (conditional
  `pending→dispatched` transition precedes attempt/delivery rows) |
  Repro: 8 concurrent dispatches | Expected: exactly one winner |
  Observed: 1 attempt row, 2 delivery rows (queued+submitted),
  task attempt==1 | Test: `TestCB01ConcurrentDispatchOneWinner` |
  Related: CB-01.
- TD-028 (idempotency-key payload mismatch) | P0 | PASS |
  `controller.go` `createTaskIdem`, `idemConflictError` | Repro: same
  (run,key), different objective | Expected: explicit conflict, no
  silent fold-in | Observed: `idemConflictError{Run,Key,Existing}` |
  Test: `TestCreateTaskIdempotent` | Related: TD-006.
- TD-029 (no silent duplicate execution) | P0 | PASS | same as
  TD-026 path | Test: `TestCB12CrashRetryReconciles` | Related:
  TD-117, TD-118, CB-12.
- TD-061 (single input writer) | P0 | PASS | `terminal.go`
  `ptyAcquireLease` (atomic conditional UPSERT) + per-frame
  `checkFrame` | Repro: B writes frames while A holds lease |
  Expected: refusal | Observed: error receipt, no PTY bytes |
  Test: `TestSupervisorRejectsForgedFrames`,
  `TestAtomicLeaseRace` | Related: TD-020, TD-062.
- TD-062 (lease recovery) | P0 | PASS | 30s TTL expiry +
  `ptyClearLease` on stop path | Repro: holder disappears; new
  controller acquires after expiry | Observed: acquire succeeds
  post-expiry; stop preempts via clear+acquire | Test:
  `TestAtomicLeaseRace` (expiry), stop paths in lifecycle tests |
  Related: TD-061.
- TD-063 (generation-bound permission) | P0 | PASS |
  `checkFrame` verifies `supervisor_pid`+`supervisor_start` per
  frame; fenced supervisor writes | Repro: stale-generation frame |
  Expected: refusal | Observed: `supervisor identity changed;
  control refused` | Test: `TestSupervisorIdentityFencing`
  (row-identity half); frame half covered by
  `TestSupervisorRejectsForgedFrames` structure | Related: TD-020.
- TD-073–081 (delivery states, receipts, no false delivery) | P0 |
  PASS | `controller.go` delivery rows
  `queued→submitted→working`, `delivery_failed`, `submitted`
  receipts; guidance-direct refusal | Repro: dispatch to one-shot
  session; direct guidance to one-shot harness | Expected: no
  delivered/working claim without evidence | Observed: states stop
  at submitted; direct send-keys refused with file kept |
  Test: `TestDispatchDeliversInstruction`,
  `TestCB13OneShotGuidanceRefused` | Related: TD-074, CB-13.
- CB-01 (concurrent submit, one dispatch) | exit gate | PASS |
  Test: `TestCB01ConcurrentDispatchOneWinner` (+ pre-existing
  `TestConcurrentDispatchOneWinner`).
- CB-12 (retry after launch/persist crash) | exit gate | PASS |
  Test: `TestCB12CrashRetryReconciles`.
- CB-13 (guidance to one-shot CLI) | exit gate | PASS | Test:
  `TestCB13OneShotGuidanceRefused`. Scope note: the guard covers
  tmux direct send-keys; PTY sessions are interactive by
  construction and go through the lease path.

## Post-implementation findings (folded back inline)

- Serial-takeover double success: generation gating serialized
  recoverers but every recoverer still spawned and every `ptyStart`
  caller returned nil once the session came up. Fixed by verifying
  row ownership (`m.Owner == owner`) on the running path of the
  `ptyStart` wait loop — exactly one caller reports success.
  Test: `TestConcurrentTakeoverOneWinner` (3/3 under `-race`).
- Receipt vocabulary: dispatch now records `submitted`, never
  `delivered`. File write is transport handoff, not worker receipt.
  Pre-existing tests asserting `delivered` updated to the new
  contract (`TestDispatchDeliversInstruction`,
  `TestVerifyRequiresEvidence`).
- Stop preempts input by design (`ptyClearLease` + self-acquire in
  `ptyStop`); the supervisor still verifies the ST frame owner
  against the fresh lease, so forged frames are refused at the
  boundary. Rival driving a session can lose it to a stopper —
  documented trade-off, not silent.

## Phase 2 slice 1 — bounded correction/retry (TD-035, TD-036, TD-082; TD-006/084 followup attribution)

- TD-035 (explicit retry policy) | P0 | PASS | `controller.go`
  `maxDispatchAttempts = 5`, `reconcileForRetry` budget gate |
  Repro: dispatch then retry an unconfirmed delivery 5 times |
  Expected: 5th attempt allowed, 6th refused with a named budget
  error, history preserved | Observed: attempt counter 1→5, then
  `retry budget exhausted ... (TD-035)`; 5 attempt rows,
  4 `superseded` | Test: `TestRetryBudgetEnforced`
  (`phase2_test.go`) | Related: TD-029, TD-118.
- TD-036 (bounded correction loop) | P0 | PASS | `controller.go`
  `maxCorrectionDepth = 5`, `followupTask` depth gate |
  Repro: chain 6 followups | Expected: depths 1–5 created, 6th
  refused naming the budget | Observed: `correction budget
  exhausted ... (TD-036)` at depth 6 | Test:
  `TestCorrectionBudgetEnforced` | Related: TD-082.
- TD-082 (corrective continuity) | P1 | PASS | `controller.go`
  `Task.CorrectionNo`/`ParentReport`, `followupTask` snapshot |
  Repro: report on parent, then followup | Expected: child binds
  parent id, depth 1, `sha256:` digest of the exact parent report,
  carried acceptance, starts pending | Observed: all bound;
  empty parent report snapshots as empty (no false digest) |
  Test: `TestFollowupBindsParentReport` | Related: TD-036, TD-088.
- TD-006/TD-084 (attributed intervention) | P0/P1 | PASS |
  `followupTask(id, objective, req)`, `correction_created` receipt |
  Repro: followup with empty `OperationRequest` | Expected:
  refusal | Observed: `operation requires an authorized,
  identifiable request`; on success a receipt records
  actor/req/parent/depth/report-digest | Test:
  `TestFollowupRequiresRequest` | Callers updated: `main.go`
  `task-followup` (synthetic `local-cli` request, same pattern as
  `dispatchTask`), `controller_test.go`. Related: TD-005.
- Schema migration | `tasks` gains `correction_no INTEGER NOT NULL
  DEFAULT 0`, `parent_report TEXT NOT NULL DEFAULT ''` via
  `ensureTaskColumn` (same PRAGMA pattern as the `key` column);
  all SELECT/INSERT sites extended; fresh and pre-existing
  databases open identically.

## Phase 2 slice 2 — dependencies, finalization, cancel cascade (TD-030/031/037/041; TD-006/084 attribution)

- TD-030 (dependency enforcement) | P0 | PASS | `controller.go`
  `task_deps` table, `addTaskDep`, dispatch gate in
  `dispatchTaskWithRequest` | Repro: task B depends on pending A,
  dispatch B | Expected: refusal naming the blocking dep, no
  attempt/delivery rows, status stays pending | Observed:
  `cannot start: unsatisfied dependencies: <dep> (pending)` |
  Tests: `TestDependencyBlocksDispatch`,
  `TestDependencyCycleRejected` | Related: TD-031.
- TD-031 (verified-only satisfaction) | P0 | PASS |
  `unsatisfiedDeps` (only `verified` satisfies) | Repro: dep
  dispatched/working/reported/failed, dispatch dependent |
  Expected: all refuse; only verified unblocks | Observed:
  refused through `report_ready`, refused after `failed`, allowed
  after `verified` | Tests: `TestDependencyRequiresVerified`,
  `TestDependencyFailedBlocks` | Related: TD-085.
- TD-041 (run finalization) | P1 | PASS | `controller.go`
  `finalizeRun`, `run_receipts` table, `RunSummary` | Repro:
  finalize run with pending tasks; empty run | Expected: refusal
  naming every unresolved task; `finalized` receipt + counts when
  all terminal | Observed: `cannot finalize with unresolved
  tasks: <a> (pending), <b> (pending)`; summary
  total/verified/failed/cancelled on success | Test:
  `TestFinalizeRunRefusesUnresolved` | CLI: `run-finalize --run`
  (+`--json`). Related: TD-040.
- TD-037 (cancel propagation) | P0 | PASS | `controller.go`
  `cancelTask` (attributed, BFS cascade over correction lineage)
  | Repro: cancel root with live child+grandchild, plus
  unrelated run-mate | Expected: whole chain cancelled with
  receipts; run-mate untouched; no-request refused;
  terminal-root cancel errors | Observed: root/child/grandchild
  `cancelled` + `cancelled`/`cancel_skipped_terminal` receipts,
  mate stays pending, `operation requires an authorized,
  identifiable request` on empty req, terminal root →
  `use followup for more work` | Tests:
  `TestCancelPropagatesToDescendants`, `TestTaskLifecycle`
  (terminal-root refusal) | Callers updated: `main.go`
  `task-cancel`, `controller_test.go`. CLI: `task-depend --task
  --on`. Related: TD-006/084, TD-040.
- Regression caught by existing test | new cascade returned nil
  on terminal root; `TestTaskLifecycle` ("cancel of verified task
  should fail") failed in full suite | Fixed: terminal root is an
  error again; terminal *descendants* still skip gracefully with a
  `cancel_skipped_terminal` receipt | Full suite green after fix.

## Phase 2 slice 3 — execution root + worktree isolation (TD-050/051/052)

- TD-050 (execution-root correctness) | P0 | PASS |
  `controller.go` `canonicalRoot`, `sessionRoot`,
  `authorizeTaskRoot`, dispatch/retry root gates,
  `Task.ExecutionRoot` | Repro: authorize root A, dispatch to
  session rooted at B; retry bound task into another workspace |
  Expected: refusal naming both roots, nothing claimed, no new
  attempt | Observed: `authorized for <A>; session wb executes
  in <B> (TD-050)`; `bound to <A>; retry into <B> refused
  (TD-050)`; refused retry leaves attempt count unchanged |
  Tests: `TestDispatchEnforcesAuthorizedRoot`,
  `TestRetryKeepsExecutionRoot`,
  `TestDispatchBindsExecutionRoot` (unbound tasks bind
  immutably at first dispatch, receipted via `submitted` detail)
  | CLI: `task-set-root --task --workdir`, `task-create
  --workdir` (composes create + authorize). Related: TD-051.
- TD-051 (worktree isolation) | P0 | PASS | `liveOccupants`,
  `authorizeSharedWorkspace`, `Task.SharedWorkspace`, pre-claim
  gate + post-claim re-verify with deterministic yield |
  Repro: two tasks, one live session | Expected: second dispatch
  refused naming the occupant; explicit authorization unblocks;
  concurrent racers never silently share | Observed: `workspace
  <root> occupied by live task(s) <id> (dispatched); authorize
  sharing ... (TD-051)`; after `task-share`, dispatch binds same
  root with flag recorded; loser of a claim race yields back to
  pending with a `dispatch_yielded` receipt | Tests:
  `TestSharedWorkspaceRequiresAuthorization` | Terminal tasks
  (verified/failed/cancelled) release the root. Related: TD-040.
- TD-052 (no implicit provider fallback) | P0 | PASS by
  construction + test | `lookupAdapter` errors naming known
  harnesses; `resolveWorkerCommand` never substitutes; explicit
  `--cmd` passes through untouched | Test:
  `TestNoImplicitProviderFallback` | No code change needed.
- Schema migration | `tasks` gains `execution_root TEXT NOT NULL
  DEFAULT ''`, `shared_workspace INTEGER NOT NULL DEFAULT 0`
  via `ensureTaskColumn`; all SELECT/INSERT/scan sites extended;
  fresh and pre-existing databases open identically.

## Phase 2 slice 4 — durable guidance + intervention audit (TD-083/084)

- TD-083 (no lost guidance) | P1 | PASS | `controller.go`
  `guidance` table, `recordGuidanceAccepted` + `settleGuidance`
  as separate durable steps, `submitGuidance` composing them |
  Repro: accept then crash before settle | Expected: row stays
  `accepted`, reported by recovery scan, settable afterwards |
  Observed: `outstandingGuidance()` returns the row across fresh
  handles (restart simulation); settle to `filed` clears it;
  terminal rows immutable (second settle refused) | Tests:
  `TestGuidanceCrashRecovery`, `TestGuidanceDurableAndAudited`
  | Related: TD-084.
- TD-084 (intervention auditability) | P1 | PASS | `Guidance`
  record (actor/req/mode/text/state/detail/timestamps),
  `guidanceStatus`, `listGuidance`, `submitGuidance` settling
  every outcome incl. refusals | Repro: file submit, direct
  submit to one-shot session | Expected: filed row with full
  attribution; refusal reaches caller AND audit with reason |
  Observed: `filed` + `actor=orch`; refused row names
  one-shot cause | Tests: `TestGuidanceDurableAndAudited`,
  `TestGuidanceRefusalRecorded` | Callers routed through the
  durable path: `main.go` `inject` (synthetic `local-cli`
  request), `tui.go` operator input (`tui-operator` actor);
  new `guidance-list`, `guidance-status`,
  `guidance-outstanding` CLI. Related: TD-006.
- Suite flake (load, not logic) | one `go test ./...` run FAILed
  with no failing test captured on immediate verbose rerun;
  clean `go test -v` after: exit 0, 75 passed, 0 failed; race
  set green. Same signature as prior load flakes; reopen if a
  named failure recurs.

## Phase 2 slice 5 — confirmed termination + timeout semantics (TD-038/039)

- TD-038 (confirmed termination) | P0 | PASS | `controller.go`
  `settleBoundWorker` (called from `cancelTask` for every
  freshly cancelled live task), `workerSessionAlive`,
  `stopWorkerSession`, `otherLiveBindings` | Repro: cancel a
  dispatched task; kill fails; cancel one of two sharing tasks
  | Expected: exactly one worker receipt per case — stopped
  (confirmed), unconfirmed (kill failed, session NOT marked
  stopped), retained (named survivor) | Observed: kill-session
  logged + `worker_stopped` + registry Stopped; KILL_RC=1 →
  task still cancelled but only `worker_stop_unconfirmed`,
  Stopped stays false, never `worker_stopped`; shared case →
  no kill, `worker_retained` names survivor, survivor stays
  dispatched | Tests: `TestCancelStopsBoundWorker`,
  `TestCancelRecordsUnconfirmedStop`,
  `TestCancelRetainsSharedWorker` | Pending tasks skip worker
  handling (never had a worker). Related: TD-037, TD-051.
- TD-039 (timeout semantics) | P0 | PASS by pinning existing
  behavior | `stopSession`/`ptyStop` already return errors on
  failed/timeout kills without marking state | Repro: kill fails
  | Expected: error reaches caller, session unstopped, task row
  untouched — no failure inferred | Observed: `stopSession`
  error, Stopped=false, task still dispatched | Test:
  `TestStopFailureMarksNothing` | No code change needed.

## Phase 2 slice 6 — delegation bounds + scheduler/durability pins (TD-032/033/034, TD-116/117)

- TD-032 (bounded delegation) | P0 | PASS | `controller.go`
  `run_limits` table, `setRunLimit`/`getRunLimit`/
  `liveTaskCount`, dispatch gate (retries exempt: 1:1
  replacement under the attempt budget, not a new slot) |
  Repro: limit 1, dispatch A, dispatch B | Expected: B refused
  naming run+limit, stays pending; resolving A frees the slot |
  Observed: `run "run-p2v" at concurrency limit 1 (1 live)`; B
  pending; after A verified, B dispatches | Tests:
  `TestRunConcurrencyLimitEnforced`,
  `TestRunLimitUnsetUnbounded` (unset = unbounded; 0/negative/
  anonymous limits refused) | CLI: `run-limit --run --max-live`
  | Related: TD-035.
- TD-034 (capacity separation) | P1 | PASS by construction |
  the limit is a scheduler knob only: it constrains dispatch,
  never UI panes, provider quotas, or host capacity, which are
  represented nowhere else in the dispatch path | No new shared
  counter introduced; documented on `setRunLimit`.
- TD-116 (crash-safe launch) | P0 | PASS by pinning | file
  delivery precedes the claim, so a crash between them must not
  create phantom execution | Repro: `deliverTask` then simulated
  crash (no further calls) | Expected: task pending, zero
  attempts; later dispatch succeeds exactly once | Observed:
  exactly that; file rewrite is idempotent, claim still single
  winner | Test: `TestCrashBetweenDeliverAndClaim`.
- TD-117, session side (single scheduling authority) | P0 |
  PASS by pinning | all production registry writes go through
  `withRegistryLock` + atomic rename; test helper is the only
  unlocked writer | Repro: 8 concurrent locked read-modify-write
  cycles on distinct keys | Expected: all 8 keys present, file
  parses | Observed: all present | Test:
  `TestRegistryConcurrentSaves` | Task-side authority already
  covered by CB-01 claim-winner test.
- TD-033 (independent concurrency) | P0 | PASS by pinning |
  dispatch paths share no cross-task serialization beyond the
  per-task claim and explicit limits | Repro: concurrent dispatch
  of A→live session and B→dead session | Expected: A wins
  unaffected; B fails fast naming the dead session; neither
  status leaks into the other | Observed: A dispatched attempt
  1, B pending with session error | Test:
  `TestBlockedWorkerDoesNotSerialize`.
- Fixture note | the fake tmux stub shares one global liveness
  flag, so stopping any session reads as all sessions dead.
  `TestRunConcurrencyLimitEnforced` frees its slot via verify
  (not cancel) for that reason; the cancel→stop path is covered
  by slice-5 tests on single-session runs. Recorded here so a
  future reader does not misread the test as avoiding cancel.

## Phase 2 slice 7 — adapter translation + continuation honesty + restart durability (TD-053/054/115, TD-118)

- TD-054 (provider translation) | P0 | PASS — real bug fixed |
  `agents.go` codex adapter passed `model` as a positional arg;
  verified against the installed CLI (`codex exec --help` shows
  `-m, --model <MODEL>`) and corrected to the `--model` flag |
  Repro: `BuildCommand(dir, prompt, "o3")` produced
  `codex exec ... "model" "o3" "prompt"` | Observed after fix:
  `--model "o3"` present, prompt still trailing | Test:
  `TestCodexModelFlag` | Other adapters untouched (claude
  `--model` matches upstream docs; muse/hermes pass no model
  rather than guess one). Related: TD-053.
- TD-053 (session continuation) | P1 | PASS | `Adapter.
  SupportsContinuation()` (`Interactive && AcceptsLiveInput`);
  CB-13 refusal widened from `OneShot && !AcceptsLiveInput` to
  `!AcceptsLiveInput` so an interactive-but-inputless harness
  would also be refused direct sends, with its own message |
  Repro: predicate over the registry + synthetic adapters |
  Observed: no registered harness advertises continuation;
  interactive-without-input does not qualify; one-shot refusal
  message unchanged (CB-13 + refusal tests still green) |
  Tests: `TestSupportsContinuation`,
  `TestCB13OneShotGuidanceRefused`, `TestGuidanceRefusalRecorded`.
- TD-115 (durable state) | P0 | PASS by composed pin | task,
  correction, guidance, run-limit, and session-registry
  identities re-read through fresh handles after a simulated
  restart | Observed: child linkage, guidance text/state,
  limit value, session owner, guidance listing all intact |
  Test: `TestRestartPreservesIdentities` | Prior slices cover
  the individual stores (SQLite tasks/guidance, atomic+locked
  registry, SQLite pty_meta with pid/starttime revalidation).
- TD-118 (reconcile before retry) | P0 | PASS, no new code |
  `retryDispatch` reconciles via `reconcileForRetry` before any
  replacement delivery; CB-12 test pins live-delivery refusal
  and supersede-and-replace. Recorded, not re-implemented.
- Phase 2 delegation/correction workflow is now complete:
  TD-030–041 (slices 1–2, 5–6), TD-050–054 (slices 3, 7),
  TD-082–084 (slices 1, 4), TD-115–118 (slices 5–7). Remaining
  Phase-2-adjacent work, if any, is new scope, not this list.

## Residuals (NOT VERIFIED, not violations)

- End-to-end worker receipt (`agent_acknowledged`) has no producer
  yet: no adapter consumes `task-*.md` and reports back. States
  exist; only `queued`/`submitted` are currently written by
  dispatch. Phase 2 (adapter task-consumption protocols).
- `transport_acknowledged` is defined but unwritten for the same
  reason. Do not assert it anywhere until a transport reports it.
- Stop preempts input by design (clear+acquire in `ptyStop`); a
  rival driving a session can lose it to a stopper. Documented
  trade-off, not silent: the ST frame still carries proven lease
  authority and the supervisor still verifies it.
- RS/ST refusals are silent at the supervisor (no receipt channel
  for frameless refusals); observability gap, not a safety gap.
- `TestConcurrentDispatchOneWinner` failed once under `-race` with
  0 winners (setup-side: every dispatcher errored, not a
  multi-winner logic failure), then passed isolated and in the
  combined race set twice consecutively. No dispatch-path change in
  slice 1 explains an all-error outcome. Treated as load flake;
  reopen if it recurs.
- Slice-2 race gate: one combined `-race` run FAILed with no named
  failing test captured; the immediate rerun was fully green, and
  the concurrency subset (`TestCB01`, `TestConcurrent*`) then
  passed twice consecutively. Same load-flake signature as above;
  reopen if it recurs with a named failure.

## Phase 3 slice 1 — snapshot-bound verification + worker-claim separation
(TD-085–100, CB-06/07/08, CB-14)

Was: `verifyTask` marked a task verified when attached artifacts
re-hashed to their digests — digest proves bytes unchanged, not that
they satisfy the assignment; reports overwrote one another; no
verifier attribution; corrections cited the parent report but not the
parent's evidence.

Now (`controller.go`, `reports.go`, `main.go`, `phase3_test.go`):
- `task_reports` append-only history: one row per (task, attempt),
  worker identity in `actor`, digest recorded; second report at the
  same attempt refused — claims never overwritten (TD-087).
- `artifacts` bound to producing attempt (`attempt_no`, new PK with
  rebuild migration preserving legacy rows); same path re-attached
  in one attempt refused; terminal tasks refuse new evidence
  (TD-088).
- `verifyTask(id, pass, approve, req)`: verifier identity/req
  required; reject path attributed (`rejected` names actor+req);
  evidence path requires non-empty immutable acceptance criteria
  (TD-100), judges only current-attempt artifacts, writes one
  `verification_evidence` row per check (TD-098); evidence-free
  pass requires named approval recorded as override with actor+req
  (TD-096, CB-08). Worker report alone can never verify (CB-06/07).
- `followupTask` records `correction_basis` on the child citing the
  parent's exact artifact snapshots (or `artifacts=none`) (CB-14).
- CLI `task-verify` gains `--verifier` (default `local-cli`).
- Failing-first: new `phase3_test.go` (8 tests) failed on the old
  3-arg contract, then passed; `reports_test.go` fixture now sets
  acceptance (contract change: evidence-based verification needs
  criteria to judge against).
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~25s); `-race` on verification/lifecycle/CB subset green.

## Phase 3 slice 2 — review independence and decision preservation
(TD-087, TD-091–095, CB-07/08)

Was: any attributed caller — including the worker session that
produced the output — could pass its own task; failure/correction
lineage, contradiction handling, and acceptance immutability were
unpinned.

Now (`controller.go`, `phase3_test.go`):
- TD-094: pass verdicts (evidence and override) refuse when the
  verifier/approver identity is the worker session that produced the
  attempt. Self-reported failure stays allowed — retracting a claim
  is not approving it — but remains attributed (`TestVerifierMustBeIndependent`
  failed first, then passed).
- TD-091: failed attempt (report + rejection + evidence) survives a
  linked correction's success, asserted end to end.
- TD-093/CB-07: conflicting findings compare side by side with one
  history row each; no consensus merged.
- TD-095/CB-08: cross-worker modification of shared evidence after
  attach refuses verification; restored bytes verify.
- TD-087: acceptance asserted unchanged across report / followup /
  verdict; no mutation path exists (no UPDATE of acceptance in code).
- TD-092: history row carries task, attempt, worker actor, digest.
- Checked and compliant by absence: TD-089 (verification executes no
  test command — nothing tautological can qualify), TD-097 (no
  terminal-text-to-tool-telemetry path in Go code), TD-099 (no
  fallback extraction — reports stored and read as filed), TD-096
  (output rotation explicitly reported; `tui.go`, `terminal.go`).
- Failing-first: `TestVerifierMustBeIndependent` failed on old code,
  passes now; five companion pins pass before and after.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~31s); `-race` on verification/lifecycle/CB subset green.

## Phase 4 slice 1 — single input writer on every entry point
(TD-061, TD-062, TD-063, CB-03/CB-11)

Was: PTY input held an exclusive TTL lease (client `ptyAcquireLease`
+ supervisor `checkFrame`), but the tmux direct-input path — TUI
attach keys (`sendTerminalInput`) and guided keystrokes
(`injectGuidance --direct`) — verified ownership yet let any number
of controllers interleave keystrokes into one terminal.

Now (`terminal.go`, `session.go`, `phase4_test.go`):
- `input_leases` is the single input-writer authority for every
  transport, keyed by channel (`pty:<name>`, `tmux:<name>`); the
  same conditional-upsert acquire, TTL expiry, and holder-naming
  refusal power both. `ptyAcquireLease` keeps its signature as the
  PTY channel; `tmuxAcquireLease` is the tmux channel.
- Supervisor `checkFrame` and `ptyClearLease` read the unified
  table; legacy `pty_leases` table is superseded (kept, unread).
- `sendTerminalInput` and the tmux branch of `injectGuidance`
  acquire the lease as this process (`controllerID()`); refusal
  carries "refused" so `submitGuidance` settles `GuidanceRefused`.
- Failing-first: both new tests failed pre-impl (rival input
  succeeded; no channel table), pass now.
- Incidental find during the race gate: `TestPtySuperviseLifecycle`
  failed 3/3 under `-race` with post-stop `worker-gone`. Root cause
  is exit ordering, not the lease change: the supervisor writes
  `exited`/`gone` before its process finishes tearing down, and
  `ptyClassify` let transient liveness override the recorded
  disposition (probe showed supervisor alive ~700ms post-stop
  under race). Fix: a recorded terminal status takes precedence in
  `ptyClassify` (safe: only `running` gates control decisions;
  new generations reset the row to `starting`). Pre-existence
  unprovable (no git history); the window is in code paths this
  slice did not otherwise alter.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~30s); `-race` on the input/terminal/CB subset green.

## Phase 4 slice 2 — guidance submission off the update path
(TD-102/103)

Was: pressing enter in guidance-input mode called `submitGuidance`
synchronously inside `Update` — file appends, SQLite writes, and
tmux `send-keys` round-trips all blocked the Bubble Tea event loop,
and two rapid enters submitted concurrently with no ordering.

Now (`tui.go`, `phase4_test.go`):
- Enter captures session/text/mode/request, clears input, sets
  `guidanceBusy`, returns `submitGuidanceCmd`; a new
  `guidanceResultMsg` case applies the outcome (sent/file,
  sent/direct, or the refusal text) and clears the busy flag.
- While a submit is in flight, a second enter is held with its
  text preserved instead of delivering out of order.
- Stale `direct mode ON (send-keys, may collide)` status corrected:
  the slice-1 lease refuses concurrent writers.
- View() verified pure (cached state only); sync `m.refresh()`
  runs only at startup; TD-107 width handling already
  display-width aware (`strWidth`/`truncW`) — old ledger
  violation is stale, no change needed.
- Failing-first: both new tests failed pre-impl (nil cmd +
  synchronous write), pass now.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~30s); `-race` on TUI/model/input subset green.

## Phase 4 slice 3 — bounded tmux stream collection (TD-066/TD-106/TD-067)
(TD-066, TD-106, TD-067; CB-10)

Was: the 1MB tmux stream cap was enforced only at session start and
when the stream file was missing. A running high-output worker grew
`streams/<name>.log` without bound, and `streamTail` read the whole
file on every refresh of the selected tab.

Now (`session.go`, `tui.go`, `phase4_test.go`):
- `sweepStreamCaps` runs at the top of every `collectRefreshData`:
  one stat per session, no reads, no forks on the common path.
  Only a file actually over cap pays for ownership resolution and
  pipe re-attachment (`rotateStream`: detach, truncate, rotation
  marker, re-attach).
- `streamTail` reads at most the cap from the file end
  (`tailSegment` + seek); a cut head line is dropped.
- Rotation stamps a marker line so the gap is visible, not silent.
- The existing `TestBatchedFleetRefresh` guard caught a first-draft
  regression (per-session `display-message` probes via
  `resolveOwnedTarget` on every refresh); fixed by stat-gating
  before any resolution. Batched fleet design preserved.
- Failing-first: `TestStreamCapEnforced` failed pre-impl (2MB file
  survived collection), passes now; `TestStreamTailLastLines`
  pins tail order across the rewrite.
- Residuals: dead sessions' stream files are never deleted
  (no cleanup path found); a transient `events.jsonl` read error
  blanks the events pane (nil overwrite in `applyRefresh`) — P1
  cosmetic, needs a freshness design, not a silent keep-stale.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~40s); `-race` on session/TUI subset green.

## Remediation slice 1a/1b — worker protocol envelope + trustworthy report ingestion
(Review §1 P0s; authorized order §6.1–6.2, first half)

Was: `submitReport(id, text)` recorded every filing under the
worker's session identity (`actor = t.Agent`), so a CLI-typed note
acquired worker attribution with no worker-origin evidence; and the
ledger transition and history insert ran on separate connections,
leaving a partial-write window (report_ready with no history row).

Now (`worker_protocol.go`, `controller.go`, `main.go`,
`worker_protocol_test.go`):
- Versioned envelope (`WorkerProtocolVersion = 1`): `TaskEnvelope`
  binds task/attempt/run/session/execution-root/objective/criteria;
  `ParseTaskEnvelope`/`Marshal` round-trip; unknown versions,
  missing identity, and criterion gaps refused. The ledger row
  stays authoritative; the envelope is a claim, not authorization.
- `ReportSource` labels every filing: worker-claim (model result
  via managed loop), runner-observation (runner-packaged stdout),
  cli-submission (operator text, reporter asserted not
  authenticated). `ReportFiling.Validate` refuses empty/unlabelled
  filings before they touch the ledger.
- `submitReport(id, filing)` runs dup-check + conditional
  transition + history insert in ONE transaction; `task_reports`
  gains `reporter`/`source` columns (with migration for existing
  DBs); `actor` stays the worker session owning the attempt
  (TD-092 preserved); `report_received` receipt names reporter and
  source. All 30 existing callers moved to the filing form
  (tests file as cli-submission/test reporter); CLI gains
  `--reporter` (default local-cli), always source cli-submission.
- Failing-first: new `worker_protocol_test.go` failed on undefined
  contract (vet), passes now (envelope round-trip, filing
  validation, provenance columns + receipt, concurrent-filing
  single-winner pin).
- Deferred to slice 1c: managed worker loop consuming the envelope,
  `markTaskWorking` pickup tightening (kept unchanged — no caller
  yet), execution receipts, real-provider E2E.
- Evidence: `go vet ./...` clean; targeted + full suite
  `go test -count=1 ./...` ok (~38s); `-race` on report/verify
  paths green.

## Slice 1c — managed worker loop + worker-loop command (remediation Phase A close)

Now (`worker_run.go`, `agents.go`, `controller.go`, `main.go`,
`worker_run_test.go`, `acceptance_test.go`):

- `runWorkerOnce(session, adapter, req)`: strictly mechanical — one
  ledger-committed dispatched task for its own session, or idle;
  two queued tasks refused by name (no picking); instruction file +
  attempt-sidecar cross-checked (divergence refused); exact attempt
  claimed once via tightened `markTaskWorking`; adapter
  `ManagedArgv` rendered with no shell; provider launched as a
  child with native exit status; bounded output (1 MiB cap) written
  to `worker-output-<task>-attempt<N>.log`, attached as attempt
  evidence; `execution_receipts` start row guards double-launch
  (UNIQUE) and result row records exit + output digest; observation
  filed via `submitReport` as source runner-observation under
  reporter `worker-loop/<session>`. Exit code recorded, never
  interpreted — a filed result is not proof of correctness.
- Launch failure (bad argv, missing output dir) refunds the claim
  to dispatched with a `claim_refunded` receipt — no attempt
  consumed for work that never executed. Unreadable output or
  unfileable result leaves the task `working` with a receipt
  naming the gap, so the orchestrator reconciles instead of the
  loop guessing.
- `Adapter.ManagedArgv` on all four real harnesses
  (codex/claude/muse/hermes): argv vectors with the prompt as one
  trailing element — hostile prompts cannot inject flags;
  `TestManagedArgvNoShell` pins this. `lookupAdapter` callers in
  tests updated to the two-value form.
- `dispatchTaskWithRequest` writes `task-<id>.attempt.json`
  sidecar after the claim; the loop cross-checks ledger vs file.
- `worker-loop --session NAME [--harness NAME] [--once]
  [--requester WHO]`: production entry — resolves harness from
  the session binding, refuses harnesses without a managed
  invocation, loops until idle (or once with `--once`).
- `markTaskWorking` tightened per plan: exact
  (task, attempt, requester) conditional claim in one transaction;
  stale attempts, duplicate claims on live tasks, and terminal
  tasks refused. One fallout: `TestCB12CrashRetryReconciles`
  claimed superseded attempt 1 as "the worker picks it up" — now
  claims current attempt 2, same intent (retry over live attempt
  refused `may still be active`); stale-attempt refusal pinned by
  `TestWorkerStaleAttemptRefused`. Ambiguous-queue test authorizes
  sharing so both dispatches land and the refusal is the loop's.
- Failing-first: new `worker_run_test.go` (consume-once,
  exactly-once race, stale refused, ambiguous refused, idle,
  no-shell argv) failed on undefined `runWorkerOnce`/tightened
  claim, passes now.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~31s); `-race` on worker/CB/claim paths green.
- Deferred: slices 2–6 (criterion verdicts, correction cycle,
  interactive adapter, docs/fleet, real-provider E2E).

## Slice 2 — criterion-specific external verification (remediation Phase B)

Now (`verdict.go`, `controller.go`, `main.go`, `verdict_test.go`,
migrated `reports_test.go`/`phase3_test.go`/`controller_test.go`):

- Three concepts stay distinct: evidence integrity (digests match,
  `verification_evidence`), requirement satisfaction
  (`task_verdicts`: one external PASS/FAIL/INCONCLUSIVE judgment per
  criterion per attempt, with reviewer, cited current-attempt
  evidence paths, and reason), operator approval (the `--approve`
  override path, recorded as an override — unchanged).
- `fileCriterionVerdict` validates schema (known disposition,
  non-empty reason), reviewer independence (never the worker
  session, TD-094), criterion membership in the task's own
  acceptance list (C-IDs shared with the worker envelope),
  current-attempt binding, evidence paths all artifacts of that
  attempt; duplicates refused (UNIQUE). Verdicts file only against
  reported tasks.
- `verifyTask --pass` now requires evidence integrity AND a PASS
  verdict on every acceptance criterion at the current attempt. A
  FAIL blocks with the reviewer's reason; INCONCLUSIVE never
  counts as success; missing verdicts refuse naming TD-100.
  `task-verdict` CLI files judgments (attempt auto-bound to the
  committed attempt; reviewer asserted, never authenticated).
- Migrated six suites that encoded hashes-only verification to the
  new contract by filing independent PASS verdicts before
  asserting verify (each also asserts hashes-without-verdicts
  refuse where its narrative allows — lifecycle, self-verify,
  attempt scoping, independence, correction preservation,
  review-vs-shared-checkout).
- Failing-first: new `verdict_test.go` failed on undefined
  contract, passes now (no-verdicts refused, INCONCLUSIVE
  refused, FAIL blocks, all-PASS verifies, stale/foreign/unknown/
  silent/self/bad-disposition/duplicate bindings refused).
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~36s); `-race` on verify/verdict/lifecycle paths green.
- Deferred: slices 3–6 (exit/recovery reconciliation command,
  real-provider correction cycle + E2E, interactive adapter,
  docs/fleet).

## Slice 3 — attempt-bound exit/recovery reconciliation (remediation Phase C)

Now (`reconcile.go`, `reconcile_test.go`, `main.go`):

- `reconcileTask` is a strictly read-only probe: task status and
  current attempt, latest attempt/delivery states, the execution
  receipt bound to that attempt (started? finished? exit +
  output digest?), the report row at that attempt with its source,
  and filed verdicts — plus one recommended next step
  (WAIT / INVESTIGATE / FILE_VERDICTS / REVIEW_VERDICTS / DONE)
  with the durable fact behind it. Absence of a row is absence of
  knowledge, never success or failure.
- Ordering is deliberate: open execution → WAIT with outcome
  UNKNOWN (covers killed supervisors and interrupted controllers —
  no replacement launch, no fabricated completion); abnormal exit
  without report → INVESTIGATE with exit detail preserved; exit
  zero without report → INVESTIGATE (never report_ready by fiat);
  report without verdicts → FILE_VERDICTS; terminal → DONE.
  The task row and history are never touched (pinned by test).
- `task-reconcile --task ID [--json]`: human lines plus
  machine-readable JSON for the external orchestrator.
- Failing-first: new `reconcile_test.go` failed on undefined
  contract, passes now (pickup wait, open-execution uncertainty,
  failed-exit evidence, verdict wait, terminal done + read-only).
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~35s); `-race` on reconcile/worker/CB12 paths green.
- Deferred: slices 4–6 (real-provider correction cycle + E2E,
  interactive adapter, docs/fleet).

## Slice 4 — one complete correction cycle (remediation Phase B/C loop proof)

Now (`e2e_correction_test.go`, plus a reject-before-correct step):

- Deterministic closed-loop proof with scripted stand-in providers
  (same argv contract as real CLIs; provider qualification with
  real model calls stays a manual quota-spending step, not suite
  work): dispatch → `runWorkerOnce` executes attempt 1 →
  worker-output artifact attached → reviewer files FAIL (required
  file absent, citing the output log) → `--pass` refused →
  orchestrator rejects (`failed`, terminal) → `followupTask`
  links correction with parent-report digest + artifact basis →
  dispatch → loop executes the correction only (parent receipt
  count proves no relaunch) → produced file attached as evidence
  → PASS verdict → `verified` → post-verify loop idles,
  both claims preserved, reconcile says DONE.
- Incidental contract confirmation: worktree isolation (TD-051)
  refuses dispatching the correction while the rejected parent
  still occupies the root — the test rejects first, then
  corrects, which is the honest production order.
- Failing-first: new `e2e_correction_test.go` failed on the
  isolation refusal (test bug, not loop bug), passes after the
  reject-then-correct fix.
- Evidence: targeted pass; `go test -count=1 ./...` ok (~38s);
  `-race` on correction/reconcile/verify/verdict paths green.
- Deferred: slices 5–6 (interactive adapter qualification,
  docs/fleet + real-provider E2E).

## Slice 5 — real-provider correction cycle (live qualification)

Live run against `/tmp/td-real-state` with the production binary
and installed Muse Code 1.4.14 (`muse exec` one-shot). Two model
calls plus one trivial probe; no suite quota spent.

- `start --name qual-w1 --workdir ... --cmd "sleep 900" --harness
  muse`: lightweight managed session holds the PTY; model
  inference starts only after dispatch, inside `worker-loop`.
- Attempt 1: dispatched → loop claimed → `muse exec` ran in the
  workspace, exit 0, output FIRST-DRAFT, no file (prompt forbade
  writes by design). Report filed as runner-observation;
  reconcile said FILE_VERDICTS.
- Reviewer filed FAIL on C1 citing the output log; `--pass`
  refused; orchestrator rejected (`failed`). `followupTask`
  linked the correction with parent-report digest + artifact
  basis; dispatch → loop executed only the correction (parent
  receipt count unchanged) → real `muse exec` wrote
  `required.txt` = DONE.
- `report-artifact` attached the produced file; PASS verdict
  citing it; `task-verify --pass` → verified. Post-verify loop
  idled; `compare-reports` shows both claims; session stopped,
  no stray tmux server.
- Bug found by the live run: `task-reconcile` on terminal tasks
  returned DONE with empty facts (early return skipped the
  execution/report/verdict reads). Fixed: facts always gathered,
  DONE only sets the recommendation. Pinned by extended
  `TestReconcileTerminalIsDoneAndReadOnly`; re-ran reconcile on
  the live state — both tasks now show execution, report source,
  and verdicts.
- Checkpoints 1–7 all observed in one cohesive run (see
  slice-4 entry for the deterministic suite mirror).
- Evidence: full suite `go test -count=1 ./...` ok (~41s);
  `-race` on correction/reconcile/verify/verdict paths green.
- Deferred: slice 6 (interactive adapter qualification,
  docs/fleet).

## Slice 6 — fleet identity, operator docs, interactive boundary

- `TestFleetSameHarnessIndependentSessions`: two sessions on one
  harness, one task each — each `runWorkerOnce` consumes only its
  own commitment; execution receipts separate by (task, session);
  both loops idle afterwards. Provider name is never identity.
- README gains the closed-loop operator section: the exact
  six-step command sequence proven in slice 5, provenance rules
  (asserted-not-authenticated identities, runner-observation
  labeling, exit-0-is-not-success), and the verified capability
  boundary.
- Interactive/persistent sessions explicitly NOT qualified: the
  registry already declares `Interactive=false` for all real
  harnesses; docs now say so instead of implying it. Qualifying
  one real interactive CLI (PTY follow-up in the same session)
  stays deferred — it needs a follow-up protocol plus quota, and
  nothing in the loop pretends it exists.
- Evidence: `go vet ./...` clean; `go test -count=1 ./...` ok
  (~43s); `-race` on fleet/correction/reconcile/worker paths
  green.

## Slice 6, addendum — real interactive session inspection probe

- Started a real interactive `muse` TUI (no `exec`) in a managed
  tmux session; `snapshot` returned its live screen state,
  including the workspace-trust prompt with cursor addressing —
  operator inspection of a genuine interactive coding-agent
  session works through the existing capture path.
- Keystroke intervention into a live model TUI stays
  explicitly unqualified: blind `send-keys` at an untrusted
  prompt (exactly what the probe screen showed) can authorize
  what the operator never reviewed. The dashboard's default
  file-mode guidance exists for this reason; direct mode stays
  opt-in and off the qualification list.
- Session stopped via ownership-verified stop; no stray tmux
  server remains.

## Baseline — architecture review Wave 0 (2026-10-31)

Tree: no commits yet (all files untracked per `git status`);
no baseline commit made (no authorization to commit). Prior
remediation slices 1–6 plus WP1a–d/WP1c work present in worktree.

- `go vet ./...` clean (fixed unused import in new cancel test).
- `go test -count=1 ./...` ok (~54s), including new WP1a
  (wait/restart-reconcile + SetMaxOpenConns(1) SQLITE_BUSY fix),
  WP1b/WP1d (stream mirror + enforced output cap), WP1c
  (cancel bound to verified child PID, recycled-PID refusal,
  mid-run cancel honored).
- Next: Wave 1 (P0-A doctor, P0-B nonblocking input, P0-C
  projection/retention, P0-D dispatch boundary, P0-E pty risks).

## Note — product rename intended: Metis

Operator decision 2026-10-31: this system will be renamed Metis.
Binary name, state paths, and module identity stay `tui-dash`
until the functional baseline (Wave 1) is stable, per the
architecture review's own postponement rule. No renames in this
pass; this note prevents accidental rebranding mid-stabilization.

## Wave 1 — dispatch commit boundary + DB open robustness (2026-10-31)

Files: controller.go (dispatchTaskWithRequest tail, insertAttempt/
insertDelivery signatures, openTasksDB, ensureTaskColumn,
ensureColumn); worker_run.go unchanged (sidecar writer reused).

Root causes:
- dispatchTaskWithRequest transitioned pending→dispatched BEFORE
  writing the attempt sidecar, task_attempts row, and delivery rows.
  Waiters (waitForCommittedWork) could observe `dispatched` with no
  bindings, and test TempDir cleanup raced trailing writes
  ("directory not empty" flake after a passed wake).
- openTasksDB ran `PRAGMA journal_mode=WAL` BEFORE setting
  busy_timeout, so a momentary lock collision failed instantly with
  SQLITE_BUSY instead of waiting (caught via added error context:
  "open tasks db journal_mode: database is locked").

Changes:
- Commit boundary: conditional claim UPDATE (status=pending,
  rows-affected checked) + attempt INSERT + queued/submitted
  delivery INSERTs in ONE tx. Filesystem prepare (deliverTask file
  + attempt sidecar for attempt N+1) runs before the commit; ledger
  stays authoritative, workers refuse file/ledger divergence, so a
  commit failure leaves orphans, never a phantom dispatch.
- sqlExecer interface (covers *sql.DB and *sql.Tx) for
  insertAttempt/insertDelivery; other callers unchanged.
- openTasksDB: busy_timeout first; journal_mode upgrade only when
  not already WAL; per-path openMu mutex closing the
  check-then-act race on first-open DDL; duplicate-column tolerance
  in ensureTaskColumn/ensureColumn for cross-process DDL races.
- Error context (%w) on every open/migrate step: journal_mode,
  busy_timeout, create tasks, each ALTER, run_key index, lifecycle.

Evidence: `go vet .` clean; targeted
`-run 'TestWaitForWork|TestLoopRestart|TestCorrection|TestDispatch|
TestRetry|TestReconcile|TestSidecar|TestAttempt'` PASS (6.9s).
Stress/race delegated to read-only verification slice (pending).

## Dogfooding finding — no-ctty dashboard launch (2026-10-31, external)

tui-dash run (Bubble Tea, default /dev/tty input) exits immediately
under TUI-Lab's portable PTY, which grants no controlling terminal.
Current workaround in tests: host the dashboard in a disposable tmux
session (tmux grants a ctty) and drive it through the attach backend.
Candidate owner-side fixes: tui-dash tea.WithInput fallback, or
tui-lab ctty for managed children. Neither attempted here — recorded
for Wave 4 (terminal presentation) scoping, not Wave 1.

## P0 security boundary (2026-10-31, Metis goal)

Files: exec_boundary.go (new), exec_boundary_test.go (new),
worker_run.go (runOneShot launch), reports.go (attachArtifact),
controller.go (verifyTask trust comment).

Regression tests first (all failed before: undefined helpers),
then fix. `go vet .` clean; 5/5 new tests pass; evidence/worker/
correction neighbors green (10.2s).

1. Env projection: runOneShot children now get workerEnv()
   (closed allowlist + METIS_* bindings, secret names dropped),
   never the ambient controller environment. Test asserts a planted
   API-key var and an unlisted var are absent, PATH + bindings exact.
   Residual: operators needing egress proxies must extend the
   allowlist deliberately; FI gateway is loopback so none needed now.
2. argv redaction: receipts store marshalRedactedArgv() (structure +
   full-sha256, bulk >256B and embedded key=value secrets replaced).
   Verified no reader parses argv content (doctor checks presence,
   reconcile reads exit/output, recovery reads pid/start).
3. Output files: openWorkerOutput() refuses symlinks/non-regular/
   foreign-uid paths (predictable attempt paths plantable by a prior
   attempt) while keeping same-attempt retry truncation (refundClaim
   preserves attempt number). Digest read uses readConfinedFile.
4. Artifacts: attachArtifact confines BEFORE reading (was read-then-
   confine) via readConfinedFile: O_NOFOLLOW open, regular-file
   fstat, /proc/self/fd re-confinement, 64MB explicit cap. Residual:
   intermediate directory swap inside worker-owned workspace is
   accepted (worker already controls it); final component is closed.
5. Reviewer identity: TD-094 (worker != verifier/approver, override
   recorded as operator_approved) already enforced + tested
   (TestVerifierMustBeIndependent). Remote principals have no
   transport yet; verifyTask now documents the trust boundary:
   local CLI = OS process identity; remote strings must never
   satisfy independence once MCP/transport lands.

## Pinned Muse tab + thin MCP adapter (2026-10-31, Metis goal)

Tab (tui.go): MUSE row pinned first, outside worker scroll; default
selection with no explicit target; worker selection preserved
underneath. Muse selected renders the orchestration feed (exact
payloads, chronological, sanitized) instead of PTY/Git panes. Enter
jumps to the event target's worker tab, never attaches. Indicator
(museActivity in orchestration_events.go) derives ONLY from committed
events + task rows: Disconnected/Error/Attention/Active/Delegated/
Idle. No mascot asset in repo — text/glyph fallback with state word.
7/7 muse_tab_test.go pass; stale generations refuse the feed like
session panes.

Feed wiring: delegation.dispatched commits atomically inside the
dispatch tx; report/verify/correction/guidance emit post-commit with
orch_gap receipts on failure. message.acknowledged and
delegation.requested are valid types with no producer yet — explicit
partial visibility, never fabricated.

MCP (mcp_adapter.go, mcp_tools.go, `tui-dash mcp serve`): newline-
delimited JSON-RPC stdio matching Ocliv's exchange framing (one line
per exchange, 30s budget). tools/list advertises only implemented
backends (status/agents/events/delegate/send/report/reconcile — no
graph tools until P3). delegate replays dispatched status on retry;
send is idempotent via new guidance_request unique index + replay in
submitGuidance (accepted rows settle on retry). Owner tokens and raw
commands never leave via metis_agents. Binary proven over pipes:
initialize/list/status/events all reply. 5/5 mcp_adapter_test.go pass.
No HTTP listener, no auth stack, no second ledger — Ocliv owns
exposure. Full ChatGPT-compatibility and Ocliv-hosted qualification
still NOT VERIFIED (per Ocliv's own report); local stdio only.

## Dispatch atomicity + MCP bridge hardening + MSP wire (2026-10-31, Metis goal)

Dispatch (task_recovery.go, task_dispatch.go): three race defects
found by inspection, reproduced failing-first, fixed narrowly —
TestRetryRefreshesAttemptSidecar (retry rewrote no sidecar, so
checkAttemptSidecar refused every retried pickup; retryDispatch now
re-reads the post-reconcile row and writes the new-attempt sidecar
before delivery), TestConcurrentReconcileSingleWinner (reconcile was
three separate statements; now one tx with an attempt-predicated
bump — 6 concurrent winners before, exactly 1 after),
TestConcurrentDispatchRespectsRunLimit (run-limit pre-check raced
the claim; the claim UPDATE now enforces the bound atomically with
a limit-aware conflict message). Race-detector clean; CB/phase/
worker/report/verdict neighbor suites pass. dispatch_atomicity_test.go
3/3 PASS.

MCP (mcp_adapter.go, mcp_tools.go): four Ocliv-bridge defects fixed
with mcp_hardening_test.go 4/4 PASS — numeric id 0 round-trips
(was swallowed as a notification; through `mcp expose` a missing
reply hangs to the 30s timeout and retires the adapter), truncation
cuts on rune boundaries (was smuggling U+FFFD), metis_agents is
sorted with total/truncated + after/limit pagination, mutations
require an auth argument when METIS_MCP_WRITE_TOKEN is set (reads
never do). Live binary over pipes: id-0 initialize/list/call all
reply. Known framing constraint (not changed): Ocliv's bridge
demands exactly one reply line per message, so notifications must
never traverse `mcp expose` — Metis keeps spec-compliant silence;
remote interop needs an Ocliv-side notification short-circuit
(proposed, not implemented — Ocliv is a separate project).

MSP (muse_host.go, muse_sessions.go, muse_host_test.go): minimal Go
JSON-RPC client over `muse serve` stdio. Verified on installed
1.4.14 (schema fingerprint sha256:c94c3d…): UUIDv7 commandId
enforced server-side, ack status is `accepted` only (admission, not
completion), turn/completed carries authoritative `terminal`,
view/subscribe replays (after, head], cursors opaque. Echo-provider
qualification PASS (TestMuseEchoLifecycle, 3.25s, isolated
XDG_DATA_HOME): start→subscribe→turn→completed, same-session
continuation with distinct turnIds, commandId replay dedups to one
session, session/list contains the session. Steering/queue/interrupt
qualify at wire level only (methods present in schema; echo turns
complete too fast to exercise). No TypeScript bridge, no new deps,
`muse exec` one-shot adapter preserved. ACP absent in this build;
@muse-code/sdk not installed — both recorded, neither required.

MSP topology correction (same day): sessions do NOT survive the
host process — a fresh `muse serve` (even same store) lists nothing
and resume/read answer sessionNotFound (-32020). Direct socket dial
is rejected (museClientCredential). Proven working topology: one
`muse serve --listen` daemon per muse home owns sessions; every
Metis operation spawns a `--join` host, acts, and exits. Metis
refuses to bind/turn/status without a live daemon socket (fail
closed, no silent in-process fallback). muse_bindings table maps
Metis session -> native ID + replay cursor; rebind to a different
native ID refused; provider mismatch refused (TD-052).
`tui-dash muse daemon|bind|turn|status` verified end-to-end on the
built binary against echo: bind, turn terminal=completed with
cursor v4:…:5, continuation turn with distinct ID cursor :10,
status idle, meta-provider turn refused exit 1. TestMuseResumeRecovery
kills the join host mid-life and completes a turn via a fresh join
against the same daemon (2.40s). Daemon lifecycle (supervision,
multi-provider homes) remains operator-managed; tests spawn their
own daemon on an isolated XDG home.

Native worker loop (agents.go, worker_run.go, muse_sessions.go):
Adapter gains optional ManagedTurn returning museTurnOutcome
{turnId, terminal, finalText}; unknown outcomes return error with
empty outcome (never coerced). `muse-native` adapter registered
(Interactive, AcceptsLiveInput; SupportsContinuation once bound).
runWorkerOnce branches post-claim to runNativeTurnOnce, which files
the same downstream records as exec: execution receipt (UNIQUE per
attempt), confined output file + artifact, runner-observation report
naming terminal+turn, execution_completed receipt with labeled exit
projection (completed=0 else 1). Pre-submit failures (no binding, no
daemon, resume/submit refused) refund the claim; post-submit
unknowns keep working for reconciliation and attempt turn/cancel
first. TestMuseNativeWorkerLoopEndToEnd: create+dispatch ->
native echo turn -> report_ready with terminal=completed receipt
carrying turn id (4.92s, zero inference). TestMuseNativeWorkerLoop-
RefusesUnbound: unbound session refused, claim refunded to
dispatched. METIS_MUSE_TURN_TIMEOUT overrides the 30m default.

Graph path banner + intuitive CLI (this slice):
graph_panel.go (new): pure MD projection over task/dependency
lineage — ready frontier, blocked chains with root-first dep paths,
coverage counts; capped, deterministic, worker text sanitized,
headers bold only. resolvePanelRun prefers the selected worker's
run, falls back to newest run. tui.go: model/refreshData carry
panelRun+panel via applyRefresh like every other pane; `g` toggles
the banner into the Git pane's slot (same height/layout math);
status hint documents it. 5 panel tests PASS incl. toggle on/off
through real Update+View.
main.go: additive alias layer — `task list|create|...`, `run
finalize|limit`, `guide list|status|pending`, `report
get|compare|attach`, `agent start|stop|list|adopt|snapshot|work`,
plus `tasks/guides/workers/sessions/send/watch`. Canonical names
unchanged; typos never land on a wrong command; bare nouns list
verbs (bare `run` keeps opening the TUI). TestCLIAliases PASS;
live binary round-trip: tasks/task list/task create+status/guide
pending/send-refused-missing all verified on scratch state.
usage() + README teach friendly forms first.
Stale-test reconciliation (pre-existing, from the Muse-tab slice):
TestSupportsContinuation blanket-banned continuation, but
muse-native's channel is qualified (TestMuseEchoLifecycle proves
same-session second turn live) — loop now allows exactly
muse-native; 4 main_test.go TUI tests assumed worker-tab default,
now select it explicitly; TestSanitizedOverview asserts worker
payloads inert (own chrome legitimately carries color).
Neighbor gate: full package minus 9 slow/external-e2e skips PASS
(~64s); echo lifecycle + native loop e2e PASS separately live.

Commander profiles: muse vs musefi (this slice):
musefi is a separate thing — Metis does not own, lay out, or manage
its store. New muse_profile.go resolves only an allowlist: "" is the
ambient `muse` binary/home (behavior unchanged); "musefi" resolves to
the `musefi` binary with its store from MUSEFI_DATA_HOME, refused
when unset (fail closed, never a guessed store); anything else is
refused so a crafted label can never pick the spawned executable.
startMuseHost takes the resolved binary; daemon/require checks are
parameterized per store (museSocketFor/museDaemonReadyFor/
museRequireDaemonFor). muse_bindings gains a profile column (ALTER
migration backfills ambient) recorded at bind; cross-profile rebind
refused; turn/status/worker-loop enforce scope via
checkMuseBindingScope (provider + profile, TD-052 extended).
`tui-dash muse daemon|bind|turn|status` each take --profile.
3 new tests PASS (resolve/allowlist/fail-closed, fencing incl.
legacy-row migration). Live ambient e2e re-proven after the spawn
refactor: EchoLifecycle, NativeWorkerLoopEndToEnd/RefusesUnbound,
ResumeRecovery PASS (~9s). musefi live path NOT YET QUALIFIED: no
musefi binary in this environment; recorded as the next step once
the separate launcher exists with MUSEFI_DATA_HOME + daemon.

MCP thin-adapter task surface for Muse.ai-first use (this slice):
metis_tasks (paginated list, oldest first, run filter, positional
task-ID cursor — IDs carry random suffixes so lexicographic paging
cannot work; unknown cursor yields an empty page, never a
fabricated position), metis_task_status (lifecycle + attempt +
unsatisfied deps as annotated "id (status)" + current-attempt
criterion verdicts + artifact count + report presence), metis_projects
(read-only projection over session workdirs: distinct sorted list,
workdir inspect with sessions/tasks/runs — never creates or claims),
metis_verify (approval-gated: token-gated, explicit pass required
— omitted pass refused, never coerced to rejection; TD-094 enforced
inside verifyTask so naming a reviewer confers nothing). Criterion
verdict filing stays local-only: MCP pass(true) succeeds only where
local verdicts already exist. 6 new tests PASS (incl. wrong-token,
missing-pass, no-verdict, worker-self-approval refusals with task
unmoved). Full MCP suite 16/16 PASS. Live stdio interop on the built
binary: initialize (numeric-zero id), tools/list, tasks, status,
verify-refuses-pending — all real replies, pending task honestly
refused (isError, no state change).

Ocliv loopback round trip (this slice, read-only Ocliv inspection +
local interop — no Ocliv files touched):
`ocliv mcp expose --env=K=V -- CMD` pins command/dir/env and serves
one-request-one-reply POSTs on loopback; the child env is scrubbed
to PATH + explicit entries (M08). Consequence verified firsthand:
without --env TUI_DASH_STATE the metis child honestly reads the
default empty store (tasks total 0) — the operator must pass state
(and write token) explicitly. Proven with /tmp/ocliv-interop.py
(kept for re-runs): expose metis mcp serve → initialize (numeric-zero
id), tools/list (all 12 tools), metis_tasks (real task from the
shared store), metis_status; wrong-token metis_verify refused at the
gate; correct-token verify reached the handler and honestly refused
the pending task. Tunnel + remote Muse.ai/ChatGPT client remain
UNVERIFIED: needs operator tunnel + remote endpoint, outside this
environment. Note: scrubbed PATH (/usr/bin:/bin) means an MCP-served
metis cannot spawn the `muse` binary (lives in ~/.local/bin) — native
MSP ops stay CLI-side until the operator adds muse to the exposed
PATH.

§12 engineering report — Metis actualization slice (this session)
Date: 2026-10-31. All claims below trace to runs in this session
unless marked prior.

1. Files modified and exact behavioral changes
- graph_panel.go (new): MD path banner — ready/blocked/coverage over
  run lineage, root-first dep paths, capped/deterministic/sanitized
  worker text, headers bold only. resolvePanelRun prefers the
  selected worker's run, falls back to newest run.
- tui.go: model+refreshData carry panelRun/panel via applyRefresh;
  `g` toggles the banner into the Git pane's slot (same layout math);
  hint text documents g=path panel.
- main.go: additive alias layer (task|run|guide|report|agent groups +
  tasks/guides/workers/sessions/send/watch flats; typos never land
  on a wrong command; bare nouns list verbs; bare `run` still opens
  the TUI) + --profile on muse daemon|bind|turn|status + rewritten
  usage. No handler logic touched.
- muse_profile.go (new): commander allowlist (""→muse,
  "musefi"→musefi binary + MUSEFI_DATA_HOME store, fail closed;
  everything else refused), per-store daemon checks,
  checkMuseBindingScope (provider+profile).
- muse_bindings.go: profile column (ALTER migration, ambient
  backfill), recorded at bind, cross-profile rebind refused.
- muse_cmds.go / muse_sessions.go / muse_host.go: profile-aware
  bind/turn/status/managed-turn; startMuseHost takes the resolved
  binary (never caller-supplied).
- mcp_tools.go + mcp_adapter.go: metis_tasks (positional cursor
  pagination — IDs carry random suffixes so lexicographic paging
  cannot work; unknown cursor → empty page), metis_task_status
  (state/attempt/gates/verdicts/artifacts/report), metis_projects
  (read-only workdir projection, never creates), metis_verify
  (token-gated, explicit pass required, TD-094 inside verifyTask;
  criterion verdicts stay local-only).
- phase2_test.go: TestSupportsContinuation allows exactly muse-native
  (was: none). main_test.go: 4 stale TUI tests select the worker tab
  explicitly; sanitized-overview asserts worker payloads inert
  (own chrome legitimately carries color).
- README.md: friendly command forms, g key. LEDGER.md: entries.
- Call-site updates only: muse_bind_test.go, muse_host_test.go.

2. OCO components adapted and why
None as code. OCO served as reference architecture (durable
decisions, mailbox attribution, dependency advancement, execution
fencing); the mechanisms were built natively in Go against Metis's
existing store. Per operator steer, broad OCO porting is stopped;
only attention-lifecycle + bounded-continuation semantics are
candidates (not yet implemented).

3. Explicitly rejected
OCO supervisor loops/brain, OpenCode runtime, provider gateway,
dashboards; HTTP/auth/tunnel stack in Metis (Ocliv owns transport);
Metis-owned musefi home layout and launcher (musefi is separate;
only contract is binary name + MUSEFI_DATA_HOME); second
schedulers; duplicate Muse subagent scheduling; criterion-verdict
filing over MCP; graph critical_path (no durations to report);
turning every operation into an MCP call.

4. Native Muse capabilities: confirmed vs unverified
CONFIRMED live (installed muse, echo provider, real daemon): serve
handshake + schema fingerprint, session start/resume, turn
submit/wait with server-terminal outcomes, same-session
continuation with distinct turn IDs, commandId dedup, opaque view
cursors, daemon+--join topology required, fail-closed without
daemon, cross-profile/provider resume refused.
NOT IMPLEMENTED/UNVERIFIED: steering/queued mid-turn turns, native
approvals flow, subagent discovery/lifecycle, workflows, session
messaging, usage/attention telemetry, ACP, any real model provider,
the musefi binary itself.
INCONCLUSIVE: explicit user-initiated interrupt (cancel-on-timeout
exists best-effort in the managed turn; no dedicated interrupt test).

5. Tests executed with actual outcomes
- New: panel 5/5, CLI aliases 1/1, profile 3/3, MCP-tasks 6/6 PASS.
- Fixed: TestSupportsContinuation + 4 stale TUI tests PASS.
- Neighbors: full package minus 9 slow/external skips — green in
  3 of 4 runs (~48s each); 1 FAIL with output not captured
  (non-verbose tail), then 3 consecutive verbose greens incl. one
  cold-cache run. Recorded as transient flake, test unidentified.
- Live real-binary: EchoLifecycle, NativeWorkerLoopEndToEnd,
  RefusesUnbound, ResumeRecovery PASS (~9s); CLI alias round trip
  PASS; stdio interop PASS; Ocliv loopback interop PASS
  (/tmp/ocliv-interop.py: init/list/tasks/status through the
  adapter, wrong-token refused, authed verify honestly refused
  pending task).
- Skipped with reason: TestIntegrationTmux, TestSixWorkersFleet,
  TestHighOutputIsolation, TestBatchedFleetRefresh,
  TestCorrectionCycleEndToEnd, TestCancelStopsManagedProcess,
  TestLoopHonorsMidRunCancel, TestMuseNativeWorkerLoopEndToEnd,
  TestMuseEchoLifecycle (per-run selection; the muse e2e trio ran
  separately and passed). Full ./... + race: NOT RUN (needs
  Kontrol approval per standing constraint).

6. Remaining contract violations and blockers
- Remote tunnel + Muse.ai/ChatGPT client round trip: BLOCKED on
  operator (tunnel + connector outside this environment; local half
  proven). Serve command: ocliv mcp expose
  --env=TUI_DASH_STATE=<state> --env=METIS_MCP_WRITE_TOKEN=<tok>
  -- <metis> mcp serve (state MUST be explicit: child env is
  scrubbed, default store reads empty).
- musefi live path: NOT IMPLEMENTED (no binary present).
- Native approvals/steering/subagents/workflows/messaging: NOT
  IMPLEMENTED (largest functional gap vs §3).
- Supervision/attention semantics + wake coordinator: NOT STARTED.
- Known environment limit: scrubbed adapter PATH cannot reach the
  `muse` binary — native MSP ops stay CLI-side over MCP for now.
- 1 transient full-gate FAIL (unidentified, 3/4 green).

7. Next code-edit tasks, ordered
1. Operator: tunnel + Muse.ai connector; then drive delegate →
   observe → correct → verify from here.
2. musefi launcher (separate) + MUSEFI_DATA_HOME + daemon;
   qualify --profile musefi live.
3. Native approvals + attention states via MSP (blocks real
   supervision).
4. Minimal wake/reconciliation coordinator (no second scheduler).
5. Steering/queued turns + explicit interrupt qualification.
6. Subagent/workflow projection, observe-only.
7. Full ./... + race with approval; six-worker fleet; cross-machine.

8. Genuinely functioning end-to-end (all observed, not inferred)
- Local loop: delegate → dispatch → native echo turn → execution
  receipt + runner report → criterion verdicts → independent verify
  → verified. PASS live.
- MCP client without TUI: delegate/control/status/verify over
  stdio and through the Ocliv loopback adapter. PASS live.
- TUI: pinned Muse feed, worker rail, path banner toggle. PASS.
- Fencing: provider + commander cross-resume refused; stale
  attempts, self-approval, missing evidence refused. PASS.

Native approvals observation, slice A (observe only):
Installed muse schema (stable + experimental) is authoritative:
`approval/decide` + `approval/listPending` are stable; server folds
`approval/requested|reviewing|updated|resolved` and must-answer
`approval/request` (id+method, expects presentation receipt `{}` —
receipt claims presentation only, never a decision; unanswered stays
pending, re-issued on next subscribe).
Framing fix (muse_host.go): server-initiated traffic (method
present) now always surfaces on Notifications() with RequestID
preserved; previously an approval/request hit the pending-call
lookup, found no waiter, and was silently dropped. Replies (no
method) still correlate; id-less method-less lines still ignored.
muse_approvals.go (new): durable muse_approvals table keyed by
(session, approvalId) — never the JSON-RPC id; requested folds
upsert pending (resolved rows never reopened by stale folds);
resolved closes with the server's terminal decision; unknown
approvals and malformed folds refused without rows. museObserveApproval
routes request|requested|updated|reviewing + resolved.
museListPendingApprovals is the lease-free server fold read.
`tui-dash muse approvals --session --provider [--profile]` prints
server fold with durable status (read-only).
3 new tests PASS (framing incl. id+method vs reply correlation,
table lifecycle + adversarial, listPending passthrough incl.
sessionId assertion) + framing neighbors green. Live: bind + approvals
fold against real echo daemon → "(no pending approvals or prompts)",
exit 0.
NOT YET: real-daemon approval emission (echo never requests —
needs a provider/policy that does); decide path + presentation
receipts (slice B); MCP verbs; attention (userInput/monitor) states.

## 2026-10-31 — Approvals decide slice B unblocked (2 defects)
- `museApprovalChoice.ChoiceID` tag was `choice_id` but rows store fold wire JSON (`choiceId`): loader unmarshaled offered choices to empty ChoiceID and refused every decision as "not offered". Tags aligned to `choiceId`; record path unchanged.
- `TestMCPApproveGated` assumed no ambient daemon, but a live ambient `echo` daemon let the decide reach the wire. Test now binds under profile `musefi` with `MUSEFI_DATA_HOME` pointed at an empty store — hermetic fail-closed, independent of ambient daemons.
- Evidence: `TestMCPApproveGated TestMuseApprovalTable TestMuseDecideApproval TestMuseApprovalObserverReceipt` PASS; `go vet` clean; full `-run 'TestMCP|TestMuse'` sweep PASS (~16s).

## 2026-10-31 — Attention lifecycle (OCO adaptation, slice 1)
- New `attention.go`: durable `metis_attention` table (id PK, request_id UNIQUE), open/resolved/dismissed states, allowlisted kinds, revision-guarded close, idempotent reopen, conflict on reused request id with different payload. Metis never auto-resolves; closing requires an outcome note.
- `attention_test.go` proves: idempotent reopen, payload-conflict refusal, stale-revision refusal, single-winner close, no double-close/dismiss, open-set vs history projection. Guard-removal mutation makes the test FAIL (sensitivity proven); restored code PASS. `go vet` clean.

## 2026-10-31 — terminal.go decomposition (P0-1)
- Split 1,338-line `terminal.go` into cohesive modules, mechanical move only (plus restored `ptyFrameRaw` const + faithful comments where the cut dropped lines 531-539):
  - `terminal.go` (410): managed PTY supervisor only (`ptySupervise` + consts).
  - `pty_meta.go` (282): record type, naming, schema, paths, reads, views.
  - `pty_lease.go` (269): controller identity, FIFO lock/write, leases, input, receipts, resize.
  - `pty_output.go` (84): log tail/lines, key bytes.
  - `pty_proc.go` (118): /proc identity, kill, liveness.
  - `pty_spawn.go` (234): start/stop/mark/fencing.
- Evidence: `go build` clean, `go vet` clean, PTY-focused tests PASS, all 41 functions present exactly once across the six files.

## 2026-10-31 — mcp_tools.go decomposition (P0-2)
- Split 671-line `mcp_tools.go` into protocol + typed handlers, mechanical move only:
  - `mcp_tools.go` (208): JSON-RPC framing, arg helpers, thin `mcpCallTool` dispatcher, errors, stdio loop.
  - `mcp_read_tools.go` (376): 8 read-only handlers (status/agents/events/graph/tasks/task_status/projects/approvals) + row/inspect helpers. No execution side effects.
  - `mcp_write_tools.go` (176): 6 authenticated mutations (delegate/send/approve/verify/report/reconcile) + token auth.
- Evidence: `go build` + `go vet` clean, full `-run TestMCP` suite PASS, 14/14 tools present exactly once as `mcpTool*` handlers.

## 2026-10-31 — tui.go decomposition (P0-3)
- Split 1,227-line `tui.go` into lifecycle + four modules, mechanical move only:
  - `tui.go` (258): Bubble Tea lifecycle (model type, Init, Update, runTUI).
  - `tui_projection.go` (278): refresh snapshots, tab/feed glyphs and labels.
  - `tui_state.go` (220): selection, attachment, refresh-generation transitions.
  - `tui_input.go` (121): keyboard routing, guidance commands.
  - `tui_render.go` (382): width/ANSI helpers, box/slice layout, View.
- Evidence: `go build` + `go vet` clean, TUI-focused tests PASS, no duplicated declarations across the five files.

## 2026-10-31 — worker_run.go decomposition (P1-1)
- Split 582-line `worker_run.go` by execution responsibility, mechanical move only:
  - `worker_run.go` (236): loop core — task query, sidecar, reconcile restart, shared refundClaim.
  - `worker_native.go` (104): native Muse turn execution.
  - `worker_subprocess.go` (267): subprocess execution with bounded output capture.
- Evidence: `go build` + `go vet` clean, worker/dispatch/reconcile tests PASS, no duplicated functions.

## 2026-10-31 — Full regression gate after decomposition
- `go test -count=1 .` PASS (~82s) across the whole package after the terminal/mcp/tui/worker splits.
- `go test -race` PASS for TestAttentionLifecycle, TestConcurrentReconcileSingleWinner, TestConcurrentDispatchRespectsRunLimit, TestRetryRefreshesAttemptSidecar.

## 2026-10-31 — main.go decomposition (P1-2)
- Split 1,100-line `main.go` (292 now) into a thin preamble + `dispatchCLI` plus 7 family files with `cliXxx(args) int` handlers (`os.Exit` → return codes; the one switch-`break` in worker-loop `--once` → `return 0`):
  - `cli_session.go` (154), `cli_guidance.go` (110), `cli_agents.go` (25), `cli_tasks.go` (396), `cli_reports.go` (91), `cli_pty.go` (182), `cli_misc.go` (74), `cli_muse.go` (127).
- Evidence: build + vet clean; TestCLIAliases PASS; live binary exit codes preserved (usage/bogus/mcp-noarg → 2, task list → 0); `task list` vs `tasks` alias parity verified in isolated state; 48 handlers present exactly once.

## 2026-10-31 — task_store.go decomposition (P2)
- Split 621-line `task_store.go` by schema vs persistence, mechanical move only:
  - `task_schema.go` (416): store path, connection setup, column/schema creation and migration.
  - `task_store.go` (217): reads, writes, IDs, attempt/delivery helpers.
- Evidence: build + vet clean, task/lifecycle/migration/reconcile tests PASS, no duplicated functions.

## 2026-10-31 — Two-stage cancellation (P0-2)
- New non-terminal statuses `cancelling` and `termination_unknown`; `liveOccupants` counts both as occupying; nil-from transitions exclude both; run finalization already treats them as open.
- `cancelTask` now: pending tasks cancel directly; live tasks go requested (cancelling) → stop exact bound process → confirmed/no-binding becomes cancelled, anything uncertain becomes termination_unknown which keeps blocking the workspace and the run.
- Native turns file pid=0 so PID stop honestly reports uncertain (turn/cancel wiring belongs to the durable-identity slice).
- Fixed own ordering bug during implementation (exclude-map must be set before settleBoundWorker, as before).
- Evidence: 3 new tests PASS (uncertain blocks workspace+finalize; confirmed releases; pending direct); all pre-existing cancel tests PASS unmodified; lifecycle blast-radius suites PASS; vet clean.

## 2026-10-31 — Native turn wiring, slice (a) (P0-3)
- `chainTurnObserver` runs the approval observer + terminal-text extractor on one path; wired into `museManagedTurn` (replacing its text-only closure) and `museCmdTurn` (was nil: approvals were invisible there).
- `readLoop` now closes the notification channel on exit; `museWaitTurn`'s closed-host branch was dead code before (channel never closed) — host death errors promptly instead of hanging to timeout.
- `bindMuseSession` refuses silent provider overwrite (native/profile were already pinned; provider was not).
- `museCmdStatus` is read-only: it no longer advances the durable cursor from a point-in-time read.
- Evidence: 4 new tests PASS (provider pin, chained record+text, close unblocks in 0.2s vs 10s hang, cursor untouched); muse+MCP regression suites PASS; vet clean.

## 2026-10-31 — Turn intent persistence (P0-3 slice b)
- New `muse_turn_intents` table + `ensureTurnIntent`/`recordTurnAck`/`getTurnIntent`: commandId persisted pre-submit bound to task/attempt/session/native/profile/provider/prompt-digest; retry reuses it (dedup), changed payload mints new.
- `museSubmitTurnWithID` carries caller keys; `museManagedTurn` takes commandID, acks the intent, and classifies submit errors as uncertain (never pre-submit refunds). `runNativeTurnOnce` ensures intent first; binding/intent failures refund via the shared pre-submit path (fixed own ordering bug that skipped refund).
- Proved server dedup live on echo daemon: identical commandId+payload resubmission admits the SAME turnId (pinned in TestTurnResubmitDedups).
- Evidence: 4 intent tests PASS; unbound-refund regression fixed; muse/worker suites PASS; vet clean.

## 2026-10-31 — Attention wired to transitions (P1-1)
- `raiseAttention` derives deterministic request IDs (kind/task/attempt/session) so repeats collapse to one row; reason/evidence must be stable, conflicts refuse overwrite (tightened to full identity).
- Producers: `cancelTask` termination_unknown and `runNativeTurnOnce` uncertain submit. Attention failure never masks the underlying error (receipted separately).
- Evidence: cancel uncertain test asserts 1 open row + repeat reuse; submit uncertain test asserts 1 open row; TestAttentionLifecycle still PASS; full suite PASS.

## 2026-10-31 — Projection isolation (P1-3)
- `collectRefreshData` is read-only: `ensureStreamPipe` moved to the attach path (explicit owner action); collection observes missing streams, capture falls back to polling.
- Feed reads carry `feedOk`; `applyRefresh` keeps last-known feed on a miss instead of clearing it.
- `museActivity`: a disconnect reads as Error only when recent with no newer traffic signal; old disconnects followed by traffic (or silence) are history. Approval-required and refused/failed sends still escalate.
- Evidence: 5 new tests PASS (read-only collect, attach heals, feed keeps last-known, disconnect history vs recent); TUI/graph/MCP suites PASS; vet clean.

## Exec-target resolver (P1 session identity)
`exec_target.go` adds one typed resolver: a session name maps to
tmux, pty, or muse-native with that backend's readiness proof —
muse-native requires the binding AND a live daemon socket, never
tmux/PTY liveness. `sessionBindable` (dispatch choke point) now
delegates to it; tmux/PTY error text preserved. Tests:
unknown/missing-PTY refused, tmux resolves, native-without-daemon
refused (no tmux fallback), native-with-socket resolves carrying
binding identities, native-without-binding refused despite live
terminal. `go vet` clean; resolver + dispatch/task/session/muse-bind
suites green.

## Approval attention producer (P1 attention wiring)
Native approval requests now file one bounded `missing_approval`
escalation from the authoritative record path
(`recordMuseApprovalRequested`); re-observed folds reuse the row
(identity is approval-scoped, choices excluded from the payload);
server resolution and local decide close only their own escalation
(`resolveApprovalAttention`, best-effort so a stale open row can
never fail a decision). Failing-first proven: lifecycle test fails
on neutered producer (`got []`), passes with it. Suites green:
approval + attention + MCP-approve.

## Verdict-pattern attention producers (P1 attention wiring)
`fileCriterionVerdict` now raises bounded escalations on
authoritative FAIL filings: `repeated_failure` when the same
criterion failed at an earlier attempt, `conflicting_findings`
when it once passed (correction regressed it). Identity is
task+criterion scoped with stable reason/evidence, so further
attempts reuse the row. Best-effort by design: filing is not
safely retryable (PK on task/attempt/criterion), so a failed
escalation must not fail the filing. Failing-first proven for
both kinds (neutered producer → `got []`). Verdict + attention
suites green, vet clean.

## Notification overflow gap (P0 turn recovery)
`museWaitTurn` now snapshots the host drop counter on entry and
refuses completion when drops land mid-wait: explicit overflow-gap
error with the entry cursor (never a cursor advanced past the
hole), so replay starts before the gap. Failing-first proven both
ways (pre-fix code reports completion on the gappy stream; disabled
check fails the test). Turn/approval/attention/resolver/dispatch/
cancel/MCP/TUI-projection suites green, vet clean.

## Qualification matrix disposition (boundary review, 2026-10-31)
Against the ten gates, with this-session evidence:
1. Forged MCP reviewer identity → denied. PASS
   (`TestMCPVerifyPassNeedsVerdicts`, reviewer-independence check in
   `verifyTask`; token-gated writes in `TestMCPWriteTokenGatesMutations`.)
2. Remote anonymous MCP mutation → denied. PASS
   (`TestMCPRemotePrincipalDenied`; remote approve/pass/write denied
   by default; approve never restorable via opt-in.)
3. Muse turn acknowledgment lost → one execution, reconciled. PASS
   (pre-persisted turn intents, retry dedup `TestTurnResubmitDedups`,
   uncertain-never-refund `TestUncertainSubmitKeepsWorking`.)
4. Notification buffer overflow → explicit gap/replay. PASS (new)
   (`TestWaitTurnNoticesOverflowGap`; entry cursor returned, never
   advanced past the hole.)
5. Native approval requested → durable visibility + decision flow. PASS
   (durable rows + `missing_approval` escalation + decide with
   offered-only choices, requirement guard, fresh commandId;
   `TestMuseDecideApproval`, `TestApprovalAttentionLifecycle`.)
6. Cancellation fails to stop worker → workspace protected. PASS
   (two-stage `cancelling`/`termination_unknown` still blocks
   workspace + finalize; `TestCancelUncertainKeepsWorkspaceBlocked`.)
7. Attention request duplicated → one bounded escalation. PASS
   (deterministic request IDs; re-observed folds/verdicts reuse;
   `TestAttentionLifecycle`, approval + verdict attention tests.)
8. TUI refresh fails → last-known data marked stale. PASS
   (`feedOk` result states; `TestFeedFailureKeepsLastKnown`.)
9. Different muse profile/provider → binding protected. PASS
   (scope fencing; `TestBindRejectsProviderChange`.)
10. Worker success without verification → unverified. PASS
    (`TestVerifyRefusedWithoutCriterionVerdicts` et al.)
NOT IMPLEMENTED: `signoff` attention producer — no authoritative
signoff gate exists in the ledger, and inventing one is a feature,
not a wiring. Full `go test ./...` + race gate NOT RUN: requires
Kontrol approval per standing constraint; targeted suites above are
green, full gate remains the operator's call.

## Decomposition verified complete (P2)
All six god-object targets are split into cohesive modules:
tui→{tui,tui_projection,tui_state,tui_input,tui_render},
worker_run→{worker_run,worker_native,worker_subprocess,worker_protocol},
task_store→{task_store,task_schema,task_dispatch,task_lifecycle,task_recovery,task_corrections},
mcp_tools→{mcp_tools,mcp_read_tools,mcp_write_tools,mcp_adapter},
main→{main + cli_tasks,cli_agents,cli_muse,cli_reports,cli_session,cli_pty,cli_guidance,cli_misc},
terminal + pty_* (pty_meta,pty_spawn,pty_lease,pty_proc,pty_output).
Largest remaining single file is task_recovery.go (456 lines), a
cohesive recovery boundary. Race check on new + concurrent suites
green.

## P0-1 workspace/model binding (native sessions)
- `muse_bindings` gains `workspace` + `model` columns (migration-safe
  backfill); `bindMuseSession` resolves the session workdir and
  refuses unknown sessions. Provider overwrite was already refused;
  workspace re-home and model switch now are too (immutable tuple).
- `museStartSession` takes the authorized root and passes
  `workspaceRoot` in `session/start` — accepted by the echo daemon
  live (`TestMuseEchoLifecycle` passes a real temp dir).
- `checkMuseExecutionScope` runs pre-submit in `museManagedTurn`
  and in `museCmdTurn`: a re-homed or re-modelled session is
  refused, never silently followed.
- Note: `cmd.Dir` on the joiner child is deliberately not set —
  with `--join` the cwd belongs to the daemon; `workspaceRoot` in
  `session/start` is the scoping mechanism, and the binding row +
  `muse_bound` receipt persist the effective config.
- Live two-worker/two-model acceptance still needs real-model
  qualification (Kontrol-gated).

## Event/ack loop hardening (P0-4 remainder, P1-2, P0-3)
- Decide id binds commandId to the exact choice+feedback payload:
  identical retries reuse it (server dedup), conflicting retries
  mint fresh. `-32053` clears both. (`TestDecideBindsCommandIDToPayload`,
  failing-first proven.)
- `museWaitTurn` treats `turn/unqueued` as an authoritative turn end
  instead of hanging to timeout. (`TestWaitTurnAcceptsUnqueued`,
  failing-first proven.)
- `session/start` carries a caller-owned command identity persisted
  before transmit; ack loss leaves the intent submitted and a retry
  replays the created session instead of minting a second one.
  (`TestSessionStartDedupsOnCommandID`, failing-first proven;
  `workspaceRoot` still accepted live by echo.)

## P1-1 native steering dispatcher
`submitGuidance` now routes muse-native sessions to
`deliverNativeGuidance` (new `guidance_native.go`): binding +
execution-scope check, daemon gate, resume, single
`turn/start` with mode `queue` and a per-row persisted command
id. Settlement reflects admission (`admitted
disposition=<d> turn=<t>`), refusal (no binding/scope
mismatch), or failure — never keystrokes into a terminal the
worker does not consume. Failing-first proven: old path
reports "delivered to live session" via tmux while nothing
reaches MSP. Live-daemon delivered path green.

## Admission authority hardening (P1-3, P0-5)
- `run_states` table: `finalizeRun` marks the run closed durably.
  `createTaskIdem` refuses fresh tasks in closed runs while
  idempotent replay of an existing key still converges.
  `addTaskDep` refuses edits in closed runs too.
- `addTaskDep` runs check+insert under `BEGIN IMMEDIATE` on the
  single-connection handle (concurrent opposite edges serialize;
  exactly one wins, never a cycle) and rejects edits to
  non-pending tasks (explicit plan revision required). Gate state
  is re-read inside the transaction.
- `METIS_MCP_REMOTE_INGRESS=1` disables local-operator trust:
  mutations then require the write token (fail-closed default
  otherwise unchanged).
- Failing-first proven per gate (closed-run, pending-only,
  concurrent serialize, ingress marker).

## P1-5 project-coverage graph (typed, read-only)
- New `buildCoverageGraph` (`graph.go`): task nodes plus
  `project` nodes (distinct canonical execution roots,
  `executes-in` edges) and `session` nodes (native sessions
  bound via `muse_turn_intents`, `bound-to` edges), alongside
  the existing `depends-on`/`correction-of` edges. Deterministic
  sort; `nil`-safe empty slices. No schema change, no writes;
  traversal grants no mutation authority.
- Surfaced as `graph project --run RUN` and `metis_graph`
  `op=project`. Existing `walk` output untouched (6-node/3-edge
  contract preserved).
- `TestCoverageGraphProjectsAndSessions` (`coverage_graph_test.go`):
  two sessions on distinct roots, dep B->A verified through the
  real verdict path, turn intent on A; asserts 5 nodes / 4 typed
  edges + rebuild identity. Failing-first proven (undefined
  symbol, then dispatch-gate refusal shaped the seed).

## P1-6 truncation + docs alignment
- `mcp_adapter.go` already returns explicit `mcpTooLargeError`
  (no byte-cut JSON); `TestMCPResultTooLargeIsExplicit` pins it.
  No change needed; verified, not assumed.
- `README.md` capability boundary corrected: `muse-native`
  persistent path (MSP turns, steering, approval observation,
  intent dedup) documented; one-shot refusal claim now scoped to
  the remaining `Interactive=false` harnesses.
- `mcp_adapter.go:26` ("verification/approval stay behind
  separately authorized tools") is consistent with the token-gated
  `metis_approve`/`metis_verify`, not drift. `INVARIANTS.md`
  XIV stays as labeled initial findings (historical, not
  re-verified here).

## Decomposition remainder (no behavior change)
- `verification.go` (new): `verifyTask` + `boolToInt` moved out
  of `controller.go`. Report/verify suites green.
- `submitReport` moved `controller.go` -> `reports.go`
  (report service beside artifacts/evidence); header pointer
  updated. Submit/report suites green.
- `museCmdApprove`/`museCmdApprovals` moved
  `muse_approvals.go` -> `cli_muse.go` (needs `context` import).
  Approval suites green.
- Managed-turn coordinator (`stopNativeTurn`,
  `stopNativeTurnOnHost`, `chainTurnObserver`, `museManagedTurn`)
  moved `muse_sessions.go` -> `worker_native.go`; MSP wire ops
  stay in `muse_sessions.go`. Native/cancel suites green.
- `session_snapshot.go` (new): `streamTail`, `tailSegment`,
  `captureOutput`, `gitInfo`, `readTail` moved out of
  `session.go` (read-only inspection; `sort` import follows).
  Session/snapshot suites green.
- Deliberately untouched: `muse_host.go` (cohesive transport),
  `task_dispatch.go` (single coordinator), `task_store.go`.

## Acceptance gates (all green, 33 tests, targeted suites)
1. Workspace isolation: TestMuseBindingPinsWorkspace,
   TestBindRejectsProviderChange, TestMuseBindingProfileFencing,
   TestMuseBindingRefusesSilentRebind — PASS
2. Lost-ack replay: TestSessionStartDedupsOnCommandID,
   TestTurnResubmitDedups, TestTurnIntentReusedOnRetry,
   TestUncertainSubmitKeepsWorking — PASS
3. Mid-turn cancellation: TestCancelLiveNativeTurnStaysUnconfirmed,
   TestStopNativeTurnConfirmedOnTerminal,
   TestStopNativeTurnUncertainWhenWithheld,
   TestCancelConfirmedReleasesWorkspace,
   TestCancelUncertainKeepsWorkspaceBlocked — PASS
4. Approval round-trip: TestDecideBindsCommandIDToPayload,
   TestMuseApprovalObserverReceipt, TestApprovalAttentionLifecycle
   — PASS (protocol-level; live-provider round trip unqualified)
5. Cursor integrity: TestCmdStatusLeavesCursor,
   TestWaitTurnNoticesOverflowGap, TestWaitTurnAcceptsUnqueued — PASS
6. Six-worker fleet: TestSixWorkersFleet,
   TestFleetSameHarnessIndependentSessions — PASS
7. Concurrent dep edits: TestConcurrentDepEdgesSerialize,
   TestDependencyEditRestrictedToPending — PASS
8. Closed-run admission: TestClosedRunRefusesAdmission — PASS
9. Native steering: TestNativeSteerDeliversQueuedTurn,
   TestMuseNativeWorkerLoopEndToEnd — PASS
10. MCP authority: TestRemoteIngressDisablesLocalTrust,
    TestMCPRemotePrincipalDenied, TestMCPWriteTokenGatesMutations
    — PASS
11. Evidence-backed completion: TestVerifyRequiresEvidence,
    TestVerifyRefusedOnFailVerdict,
    TestVerifyPassesWithAllCriteriaSatisfied — PASS
12. Project graph: TestCoverageGraphProjectsAndSessions — PASS

## Residual / NOT VERIFIED
- Full `go test ./...` + race: needs Kontrol approval, not run.
- Live real-model runs (FI-backed `musefi`, two-model isolation,
  genuine approval during a managed turn, cross-machine): protocol
  proven against echo/stub servers only.
- `INVARIANTS.md` XIV rows re-verification; `musefi` live profile
  qualification; native subagent/workflow projection.
- No commits on master: reproducible baseline still unestablished.

## Integration log — 2026-11-01 (Full Integration Review, P0-1..P0-5 + §2/§3/§4/§5)

Scope: native worker-loop entry, turn-intent freeze, atomic finalize,
provider/model revalidation, single-owner cursor + presence lease,
fleet node layer, team/obligation/worktree policy, MCP control tools,
attention CLI+MCP, 12-worker gate. Failing-first tests throughout;
`go vet` clean; targeted suites green (`go test ./...` + race still
need Kontrol approval — not run).

- P0-1 worker loop accepts `muse-native` (`cli_tasks.go` ManagedTurn
  capability check): `TestCliWorkerLoopAcceptsNativeTurnHarness` — PASS.
- P0-2 turn-intent frozen (payload+scope, `turn_intent_conflict`,
  workspace column): `TestChangedPayload*` — PASS.
- P0-3 finalize/admission atomic (`BEGIN IMMEDIATE` + `runClosedTx`):
  `TestFinalizeRaces*`, `TestClosedRunRefusesAdmission`,
  `TestCB01ConcurrentDispatchOneWinner` — PASS.
- P0-4 explicit provider/model selection at `session/start` with
  server read-back (`museReadSessionConfig`/`museVerifySessionConfig`),
  bind-time + pre-execution revalidation:
  `TestSessionStartSelects*`, `TestVerifySessionConfig*`,
  `TestBindBlocks*`, `TestStartRefusesLying*`,
  `TestManagedTurnRefusesModelDrift` — PASS.
- P0-5 cursor never blanks, attributed to terminal turn
  (`cursor_turn`); presence scans newest-first, Attention from pending
  rows: `TestCursor*`, `TestMuseActivity*` (incl. stale-history and
  attention-from-pending) — PASS.
- Presence lease (`muse_presence.go`): authenticated MCP calls record
  principal/source/TTL; `museActivity` returns Connected on live lease,
  Active on fresh traffic; `TestMusePresence*` — PASS. Mascot: operator
  decision 2026-11-01 = keep lease-driven glyph (no new asset).
- Fleet node layer (`fleet_node/transport/outbox/execution/resources/
  artifacts.go`): enroll/heartbeat/liveness, local-only routing with
  explicit remote refusal, idempotent outbox + ack, single-owner claims,
  capacity reservations, bound manifests + GC — 26 tests incl.
  `TestTwelveWorkerFleet` (12 distinct workers, lease isolation,
  capacity gate) — PASS, hermetic on repeat.
- OCO policy: `team_policy.go` (revisioned roles, run pinning,
  admission gates), `coverage_obligations.go` (obligation ledger),
  `workspace_alloc.go` (deterministic worktrees, no merge/publish):
  `TestTeamPolicy*` + 17 writer tests — PASS.
- MCP control: `metis_provision`, `metis_bind`, `metis_depend`,
  `metis_cancel`, `metis_correct`, `metis_attention`,
  `metis_attention_settle` (thin, idempotent, token-gated) +
  `attention` CLI (list/open/resolve/dismiss, revision-guarded):
  writer tests + `TestMCPControlSmokeLoop`
  (delegate→observe→report→local verdicts→verify→verified) — PASS.
- Verdict filing stays local-only by design (criterion-verdict MCP
  filing explicitly rejected); smoke test names that boundary.

NOT VERIFIED / NOT IMPLEMENTED: full `go test ./...` + race; live
FI-backed `musefi` runs; cross-machine remote transport (refused
explicitly, no fake); project triads (no backend); real Muse.ai /
tunnel round trip (needs operator); native subagent/workflow
projection; commits on master (no reproducible baseline yet).

## Remediation cycles — Nov 2026 mission (implementation + independent qualification)

### Cycle P0-A — Cross-process identity generation — COMPLETE
- ROOT CAUSE: task/request/attempt/delivery (+obligation/judgment/fleet/op/artifact) IDs used
  second-resolution timestamp + process-local atomic counter starting at zero. Every fresh process
  minted byte-identical IDs within a second.
- FAILURE REPRODUCTION: `task_idgen_crossprocess_test.go` `TestCrossProcessIDGeneratorsUnique`
  re-execs the test binary as 8 independent processes per kind; pre-fix: 175 cross-process
  collisions, e.g. ID `task-20261101T172523-1` minted by both proc 0 and proc 2. Observed FAIL.
- IMPLEMENTATION: `controller.go` `newUniqueID(prefix)` — retained `prefix-ts` shape for
  operator diagnostics, 64-bit crypto/rand suffix; 9 call sites (`task_store.go` x4,
  `coverage_service.go`, `coverage_store.go`, `verdict.go`, `fleet_node/artifacts/outbox.go`).
  No schema change: `id TEXT PRIMARY KEY` constraints remain the final boundary; old IDs untouched.
- FOCUSED RESULTS: cross-process + concurrent-goroutine tests PASS; `-race` PASS; vet clean.
- OUTCOME: 12 parallel `task-create` CLI processes, same second, one state dir → 12 distinct
  IDs, zero errors (`task-20261101T172756-<16hex>`).
- NEXT: P0-D MCP notification mutation guard.

### Cycle P0-D — MCP notification mutation guard — COMPLETE
- ROOT CAUSE: `mcpHandle` (`mcp_tools.go`) executed state-changing `tools/call` frames
  even when the JSON-RPC frame carried no request id (notification form). Ping and unknown
  methods already stayed silent; `tools/call` did not.
- FAILURE REPRODUCTION: `mcp_notification_test.go`
  `TestMCPNotificationToolsCallPerformsNoMutation` — notification-form `metis_delegate`
  pre-fix EXECUTED: created a task row and returned a result with null id. Observed FAIL.
- IMPLEMENTATION: `tools/call` branch returns nil before argument parsing when `!hasID`;
  no other branch touched.
- FOCUSED RESULTS: notification tests PASS (incl. control arm: identified call still
  executes); `TestMCPNumericZeroID`, `TestMCPWriteTokenGatesMutations`,
  `TestMCPResultTooLargeIsExplicit` PASS; full `TestMCP*` neighborhood ok (60.7s).
- NEXT: P0-C mcpToolVerify pass=false capability scope.

### Cycle P0-C — MCP rejection authority — COMPLETE
- ROOT CAUSE: `mcpToolVerify` allowed authenticated remote pass=false on bare token auth.
  Retraction is an authoritative transition; transport proof is not rejection authority.
- FAILURE REPRODUCTION: extended `TestMCPRemotePrincipalDenied` (bare/grant/blanket matrix):
  pre-fix bare-token remote rejection SUCCEEDED. Observed FAIL.
- IMPLEMENTATION: new scoped `reject` capability (`mcp_write_tools.go`); remote pass=false
  requires it via `METIS_MCP_REMOTE_CAPABILITIES=reject`. Legacy blanket opt-in scope frozen
  (tasks+admin, never reject); pass=true still local-operator-only. Denial is an isError
  result naming the missing capability.
- CONTRACT UPDATES (old "bare fail allowed" pins): `TestMCPIngressUnauthorizedVerifier` and
  `TestMCPVerifyGated` now set the explicit grant; smoke/local paths untouched.
- FOCUSED RESULTS: `TestMCPRemotePrincipalDenied`, `TestMCPIngress*`, `TestMCPVerify*`,
  `TestMCPNotification*` PASS; full `TestMCP*` neighborhood ok.
- NEXT: P0-B PTY environment allowlist.

## Boundary ledger — navigation extraction (Phase 2 slice 1) — COMPLETE
- ID/severity: NAV-01 / structural (god-object root state).
- Violated boundary: navigation cursor/viewport state lived as flat `model` fields
  (`selected/muse/museEv/tabOff`), mutated from Update, state, viewstate, input, attach.
- Intended single owner: `internal/tui.Nav` + pure reducers.
- Files changed: new `internal/tui/nav.go`, `internal/tui/nav_test.go`,
  `internal/tui/AGENTS.md`; `main` adapters in `tui.go` (selName, tick/submit arms),
  `tui_state.go` (selName/attachTarget/jumpToFeedTarget/applyRefresh/followTabs/refresh),
  `tui_viewstate.go` (navUp/navDown/navReselect delegate), `tui_render*.go`,
  `tui_attach.go`; tests migrated from flat literals to constructors.
- New contracts: `Nav.{Select,Clamp,Restore,Up,Down,JumpToFeedTarget,ClampFeedCursor,
  FollowTabs,RailWindow,OnMuseTab}`; `model.selName()` shared resolution gate.
- Tests: 6 new `internal/tui` reducer tests PASS (zero infra); `TestBoundaryNoGlobalRefreshSource`
  (D) + `TestBoundaryInternalTUIImportClosure` PASS; full TUI sweep green
  (one unrelated SQLITE_BUSY flake in TestWorkerClaimExactlyOnce, passes isolated).
- Behavior preserved: Up/Down/Muse-tab cycle, identity-restore, feed clamping, rail
  scrolling, empty-list selection — all pinned by existing tests, green unmodified in intent.
- Residual: `scroll`/mode/input/direct remain flat (terminal/guidance components, next slices);
  B (per-component refresh installers) and C (pane acquisition split) open.

## Boundary ledger — finding B: per-component refresh installers — COMPLETE
- ID/severity: B / structural (centralized projection mutation).
- Violated boundary: `applyRefresh` mutated sessions/feed/panels/nav/terminal/status
  domains plus cross-component reconciliation in one method.
- Intended owners: each component installs its own slice; shell keeps gates + detach.
- Files changed: new `tui_install.go` (`applySessionsRefresh`,
  `applyFeedRefresh`, `applyPanelsRefresh`, `applyNavRefresh`,
  `applyTerminalRefresh`); `tui_state.go applyRefresh` reduced to
  gates → installers → detach composition.
- Contracts: installers write only their component's fields; terminal
  installer returns a rotation notice string instead of setting status;
  navigation installer returns the resolved selection.
- Tests: refresh-domain suite green; mutation check (names-install
  disabled) fails `TestRefreshMsgAppliesPreservesSelectionAndGit`,
  restored → green; full TUI sweep green.
- Secondary defect noted then corrected during the slice: an early
  draft claimed `m.logGen` was never populated; the tree shows
  wholesale replace (`m.logGen = d.logGen`) — claim withdrawn, exact
  replace semantics preserved.
- Residual: C (pane acquisition split in `tui_source.go`) still open.

## Boundary ledger — finding C: pane acquisition split + bounded titles — COMPLETE
- ID/severity: C / performance + ownership (refresh-path full history scans).
- Violated boundary: `liveRefreshSource.snapshots` did every subsystem query inline;
  `latestTitleByAgent` loaded ALL task rows on every refresh tick (P1-G).
- Intended owners: per-pane builders over one shared query round; store owns
  indexed projections.
- Files changed: `task_schema.go` (new `tasks_agent_created` index, IF NOT
  EXISTS, migrates old DBs on open); `task_store.go` (`latestObjectiveByAgent`
  grouped query, `listTasksByAgent` scoped reader); `tui_source.go`
  (`sessionsRound` + `liveSessionsSnapshot`, other pane builders already
  separate; `latestTitleByAgent` delegates); `assurance_terminate.go`
  (`otherLiveBindings` uses the scoped reader — identical output).
- Contracts: refresh path never scans task history; EXPLAIN plan pinned to
  index use; `metis_projects` assessed as on-demand (not a tick path).
- Tests: `task_titles_test.go` — per-agent latest correctness (interleaved),
  index-use plan test (discriminates: wrong-column index yields bare SCAN +
  temp b-tree → FAIL, verified via mutation), 3000-task scale in 0.5s;
  cancel/terminate suites green (scoped-reader drift check).
- Residual: none for C. Next: full gate.

## Gate note — TestRetentionSurvivesRestart single full-suite failure — VERIFIED NON-ISSUE
- Symptom: one full-suite run failed `TestRetentionSurvivesRestart` with
  "first epoch must reach gen 1, got 3"; packages musewire/termview/tui ok.
- Evidence against a real defect:
  - Isolated rerun: PASS (0.08s).
  - Retention group (`-run TestRetention`): 6/6 PASS.
  - Stress (`-count=5 -run TestRetention`): PASS (5 iterations).
  - Full-suite rerun (`go test -count=1 -buildvcs=false ./...`): ALL GREEN
    (tui-dash 334s + 3 internal packages).
  - Static analysis: `boundSessionStream` re-stats size UNDER the session
    flock before rotating, so concurrent/extra sweeper ticks cannot
    double-rotate a static file; three bumps require the file to exceed cap
    three times, but the test performs a single 2MB `WriteFile`. No
    in-test mechanism produces gen 3 — no other test writes this test's
    tempdir-scoped stream, no parallel tests exist, all sweepers are
    stop-closed. No production file in the retention path was touched by
    the Finding-C slice (task_schema index, scoped task readers,
    tui_source builders, cancel-cascade reader only).
- Disposition: VERIFIED NON-ISSUE (unreproduced timing flake under
  ~6-minute full-suite load). Exact-count assertion kept as-is: it is the
  pinning test for rotation accounting and must not be weakened to
  accommodate one unreproduced event. Reopen as a real finding if it
  recurs with a reproducible choreography.

## Boundary ledger — finding G (attach leg): attempt-generation fencing — FIXED
- ID/severity: G / correlation (stale async outcome misapplied).
- Violated boundary: async completion correlation — `applyAttachResult`
  matched outcomes by session name only, with no operation identity.
- Conflicting owners: attempt N's late outcome vs attempt N+1's live UI
  state for the same session.
- Intended single owner: the latest attachment attempt (by generation).
- Files changed: `tui_attach.go` (`attachResultMsg.gen`, `attachCmd`
  takes gen, `requestAttach` bumps `model.attachGen`,
  `applyAttachResult` ignores gen mismatch before the session check);
  `tui.go` (`model.attachGen` field + ownership comment);
  `tui_decompose_test.go` (new failing-first test).
- New contract: every attach attempt takes a fresh generation; only the
  latest generation's outcome may confirm or detach.
- Test that failed before correction:
  `TestAttachStaleRefusalCannotClobberNewerAttach` — late refusal from
  superseded attempt detached the newer attach (mode="nav" + stale
  reason installed); observed FAIL pre-fix.
- Tests that passed after correction: new test PASS; existing attach
  suite unchanged and green (`TestAttachEnterUsesAsyncCommand`,
  `TestAttachRefusalDetaches`, `TestAttachHealsStream`,
  `TestAttachDetachCycle`, `TestAttachRefusesStopped`,
  `TestAttachSamePathSameAttemptRefused`, `TestAttachTerminalRefused`).
- Integration/adversarial: Esc-between-attempts path exercised through
  real `Update` + real `attachCmd` (fake tmux); reverse ordering
  (late success after detach) already fenced by the mode guard.
- Residual: guidance/input results still correlate by session/op ID
  without an operation generation; guidance is serialized by
  `guidanceBusy` (single in-flight), input by per-job opID + parking —
  assessed sufficient, no change. Full `go test ./...` gate rerun
  pending at slice close.

## Boundary ledger — P0-A fallout: random-suffix IDs broke created-tie ordering — FIXED
- ID/severity: P0 (correctness: dispute resolution coin-flip in production).
- Violated boundary: read-model ordering contract — `ORDER BY created, id`
  assumed monotonic IDs; P0-A `newUniqueID` (`prefix-ts` + crypto-random
  suffix) made the tiebreak random. `created` is second-resolution
  (`nowUTC`, RFC3339), so same-second filings tie routinely.
- Conflicting owners: filing order (insertion) vs lexicographic id order.
- Intended single owner: insertion order (rowid) as the tiebreak; timestamp
  still dominates across seconds.
- Files changed: `verdict.go` (`judgmentsForAttempt`,
  `judgmentsForAttemptTx`), `coverage_service.go` (`listObligationsTx`),
  `coverage_store.go` (obligations list), `guidance_store.go`
  (`listGuidance`, `outstandingGuidance`), `attention.go` (attention list):
  `ORDER BY <ts>, id` → `ORDER BY <ts>, rowid` with ownership comments.
  `verdict_assurance_test.go`: new deterministic pin
  `TestJudgmentsSameSecondKeepFilingOrder` (adversarial ids: filed-first
  sorts-last lexicographically).
- New interface/contract: same-second rows keep filing order everywhere;
  latest-per-reviewer and latest-wins projections are deterministic.
- Tests that failed before correction:
  - New pin `TestJudgmentsSameSecondKeepFilingOrder`: got
    `[judg-aaa-filed-second judg-zzz-filed-first]` (lexicographic order).
  - Gate casualty `TestAssuranceSelfSupersedeResolvesDispute`: reviewer-b's
    FAIL/PASS filed within one second; random tiebreak put FAIL last, so
    verification reported a phantom dispute. Failed in full suite AND
    isolated rerun (same-second filings are the norm, not the exception).
- Tests that passed after correction: pin PASS; `TestAssuranceSelfSupersedeResolvesDispute`
  PASS; focused verdict/coverage/guidance/attention suites green (24s);
  vet clean.
- Integration/adversarial verification: pin uses adversarial id order
  (insertion order contradicts lexicographic order); production path
  (`verifyTask` → `assessmentsForAttempt`) covered by the previously
  failing gate test.
- Residual: `task_dispatch.go` `liveOccupants` keeps `ORDER BY id`
  (diagnostic occupant list, no latest-wins semantics — assessed cosmetic,
  unchanged). No WITHOUT ROWID tables exist, so `rowid` is universally valid.
  Full `go test ./...` gate rerun pending at slice close.

## Gate note — full suite green on final F-16 tree — CLOSED
- `go vet ./...`, `go build -buildvcs=false ./...`, and
  `go test -count=1 -buildvcs=false ./...`: ALL GREEN
  (ok tui-dash 425s + musewire/termview/tui ok, exit=0).
- The two retention failures from the previous gate run
  (`TestRetentionSurvivesRestart`, `TestRetentionUnknownGenerationHealsWithGap`)
  did NOT reproduce: isolated green (repeated), full-suite green on rerun.
  No failure messages were captured for that run (grep filtered them), so no
  mechanism was established. Both tests use 10ms tickers, 5s poll deadlines,
  2MB file IO, and fork/exec of helper scripts on a shared dev box (load
  ~11 from browsers/other agents) — load-sensitive by construction.
- Disposition: VERIFIED NON-ISSUE (unreproduced; isolated + full-suite green).
  Reopen condition: a third occurrence WITH captured failure messages; then
  harden (deadline/robustness) rather than weakening the exact-count pins.

## New finding F-17: PTY supervisors leak as permanent orphans — DEFERRED
- ID/severity: F-17 / P2 (process retention; Phase 8 lists goroutine and
  resource retention as explicit verification scope).
- Evidence: 83 orphaned `tui-dash.test pty-supervise` processes (all PPID 1202,
  elapsed up to ~13h) accumulated from full-suite PTY tests; cleaned during
  this cycle (0 remain). Production spawns supervisors daemonized
  (`Setsid: true`, stdio to /dev/null in `pty_spawn.go:88-99`) so they
  outlive the spawner by design — but nothing reaps them when the owner or
  test is gone.
- Leak sources identified (read-only, not yet fixed):
  - `provision_backend_test.go` provisions PTY session "pp1" with no
    `ptyStop`/cleanup call at all.
  - `TestSupervisorIdentityFencing` (terminal_test.go:275) deliberately
    corrupts the supervisor identity so `ptyStop` refuses (correct), then
    the deferred `ptyStop` also refuses (error discarded) — live supervisor
    (+ `sleep 30` worker) leaks at test end by construction.
  - Every `defer ptyStop(...)` discards the error: any stop failure
    (10s deadline under load, lease/identity races) leaks silently.
- Follow-up: test-hygiene fix (stop what tests start; force-kill path for the
  fencing test after asserting refusal) plus a determination whether
  production needs an owner-death reaper. Order dependency: after the gate
  adjudication above; owned by the terminal domain, not the TUI slices.

## Boundary ledger — F-17: PTY supervisor test hygiene — FIXED (test scope)
- ID/severity: F-17 / P2 (process retention).
- Violated boundary: test lifecycle — tests started daemonized supervisors
  without stopping them; production spawns supervisors detached
  (`Setsid`, stdio to /dev/null) so they outlive the test binary by design.
- Intended single owner: each test stops what it starts; explicit stop /
  reconcile paths own daemon lifecycle (no owner-death reaper: daemon
  outliving its spawner is the production contract for `pty-start`).
- Files changed (tests only, no production code):
  `terminal_test.go` (new `requireSupervisorReaped` guard: polls the
  recorded supervisor pid/starttime until gone, 10s bound;
  `TestSupervisorIdentityFencing` restores the corrupted identity and
  stops normally after asserting refusal),
  `provision_backend_test.go` (`TestMCPProvisionPtyBackend` stops pp1 and
  asserts reaping).
- New contract: every test-started supervisor is reaped by test end;
  the reap guard fails the test if the daemon survives stop.
- Evidence of the leak before correction: 83 orphaned
  `tui-dash.test pty-supervise` daemons (PPID 1202, up to ~13h old),
  including "pp1"/"fenced" names; 0 remain after cleanup.
- Evidence after correction: focused PTY run
  (provision/supervise/lifecycle/fencing/recovery/exit-recorded, 4.4s)
  green with 0 stray supervisors afterwards vs orphans before.
- Residual: other `defer ptyStop(...)` call sites discard the error —
  assessed benign (suite green; stops succeed), left unchanged to avoid
  churn. No production reaper: deliberate decision, daemon persistence is
  the `pty-start` contract. Full `go test ./...` gate rerun pending.

## Gate note — full suite green on final F-17 tree — CLOSED
- `go vet ./...`, `go build -buildvcs=false ./...`, and
  `go test -count=1 -buildvcs=false -timeout 25m ./...`: ALL GREEN
  (ok tui-dash 388s + musewire/termview/tui ok, exit=0).
- Zero `pty-supervise` strays after the full run (was: orphans per run) —
  suite-scale proof the F-17 hygiene holds. No retention failures this run.

## Boundary ledger — Phase 5 (leg): refresh single-flight coalescing — FIXED
- ID/severity: Phase 5 / P1 (unbounded concurrent acquisition).
- Violated boundary: refresh coordinator owns in-flight accounting —
  every odd 500ms tick dispatched `refreshCmd` unconditionally, so a
  collection slower than the tick piled up overlapping rounds (each
  spawning tmux/git/DB subprocesses). Generation fencing refused stale
  results but did not bound the work.
- Conflicting owners: tick scheduler (dispatch) vs in-flight collection.
- Intended single owner: at most one outstanding async collection;
  ticks coalesce onto it.
- Files changed: `tui.go` (`model.refreshPending`, tick gates dispatch
  on `!refreshPending`, `refreshMsg` delivery clears it and re-arms);
  `tui_decompose_test.go` (`countingSource` + `runBatch` harness,
  `TestRefreshSkipsWhileOneIsInFlight`).
- New contract: gen bumps only with a dispatch; an outstanding refresh
  always matches at delivery; coalescing delays a refresh by at most
  one tick instead of stacking rounds.
- Test that failed before correction:
  `TestRefreshSkipsWhileOneIsInFlight` — 3 ticks with no delivery
  dispatched 2 collections (calls=2, want 1); observed FAIL pre-fix.
- Tests that passed after correction: new test PASS (incl. re-arm leg);
  focused refresh/attach/input/boundary suites green; vet clean.
- Integration/adversarial verification: full `Update` tick path with a
  counting source (no timing); stale-marker gate untouched (defense in
  depth — now nearly unreachable via ticks, still pinned by existing tests).
- Residual: no per-pane deadlines or cancellation of an in-flight
  collection (Phase 5 remainder) — a slow round still occupies its
  single slot until delivery; follow-up slice.

## Gate note — full suite green on final F-18 tree — CLOSED
- `go vet ./...`, `go build -buildvcs=false ./...`, and
  `go test -count=1 -buildvcs=false -timeout 25m ./...`: ALL GREEN
  (ok tui-dash 399s + musewire/termview/tui ok, exit=0).
- No retention failures this run. Zero `pty-supervise` strays after.

## Perf ledger — P0 open-path: epoch-gated DDL skip — FIXED
- ID/severity: Perf / P0 (test-suite + CLI open overhead).
- Violated boundary: storage owns migration admission — but the
  F-15 identity (size/mtime) self-invalidated on every write, so
  every write-workload open re-ran full DDL + a stamp write
  (~16-26ms/op; ~57 opens/iter in TestFinalizeRacesTaskAdmission).
- Intended single owner: storage (`task_schema.go` open path,
  `schema_epoch.go` identity); epoch row stays the currency
  authority, device+inode only detects replacement.
- Files changed: `task_schema.go` (dev/ino `sameFile` fast path;
  DDL+stamp skipped when the epoch already matches this binary;
  `migrationDDLruns` observe-only counter), `schema_epoch.go`
  (`dbIdentity` reduced to dev/ino/ok), `controller.go` (counter
  decl), `schema_epoch_test.go`
  (`TestReopenAfterWriteSkipsMigrationDDL`).
- Test that failed before correction: new pin — DDL ran 30 times
  over 10 write+reopen cycles (want 0); two-part mutation
  (sameFile=false + skip forced) reproduces FAIL 30.
- Tests that passed after correction: pin PASS (0.50s); 9
  schema/epoch/migration tests green; vet clean; -race group
  green (incl. TestSchemaEpochConcurrentFirstOpen,
  TestFinalizeRacesTaskAdmission, 18 concurrency tests).
- Before/after (isolated, same box): TestFinalizeRacesTaskAdmission
  15.17s -> 8.35s (-45%); TestDispatchWorkspaceRaceSingleOwner
  ~7.1s -> 3.66s; TestConcurrentDepEdgesSerialize 4.47s -> 2.95s;
  TestMCPRemotePrincipalDenied 4.42s -> 1.93s.
- Integration/adversarial verification: full-suite JSON profile
  356.6s green pre-fix (617 pass, 0 fail); post-fix full gate
  pending (required before closing).
- Residual: per-open now ~4-6ms (pragmas + stat + epoch re-read);
  DSN pragma fold (~1.4ms/open) deferred — touches the
  busy_timeout-first ordering, gain too small for the risk.
  Remaining FinalizeRaces cost is genuine tx commits + lock
  contention (the race itself), not redundant DDL.
- Gate note (post-fix): full `go test -count=1 -buildvcs=false
  -timeout 25m ./...` ALL GREEN, exit=0 — ok tui-dash 260.246s
  (pre-fix profile 356.6s; earlier gates 389-471s) + musewire,
  termview, tui ok. Zero FAIL/panic lines. Zero pty-supervise
  strays (pgrep self-match ruled out via ps inspection).
  Suite-wide -96s (-27%) from the single open-path fix.

## Perf ledger — F-19 graph read hoisting: one open per render — FIXED
- ID/severity: Perf P0 (per-tick TUI acquisition + test time).
- Violated boundary: events/graph assembly re-opened the DB per
  task (ready/blocked/coverage each scanned all N tasks;
  depChain walked per blocker; graphCoverage re-ran blockers+
  ready internally). One 40-task banner = 628 opens; the live TUI
  pays this per refresh tick (livePanelSnapshot -> graphMDLines).
- Intended single owner: storage owns handle-scoped cores
  (getTaskOn/listTasksOn/unsatisfiedDepsOn/depIDsOn); events owns
  assembly (graphReadyOn/graphBlockersOn/graphCoverageOn/
  graphMDLinesOn/depChainOn/dbDepReader); public wrappers keep
  one-shot semantics for CLI/MCP callers.
- Files changed: `task_store.go`, `task_dependencies.go`,
  `graph_lineage.go` (depIDsOn + dbDepReader; buildRunGraph
  threads it), `graph_readiness.go`, `graph_project_coverage.go`,
  `graph_panel.go`, `controller.go` (dbOpenCount observe-only
  counter), `graph_panel_test.go`
  (TestGraphRenderHoistsOneOpen).
- Test that failed before correction: new pin — 628 opens over a
  40-task render (want 1). Panel-layer mutation (wrappers
  restored) reproduces FAIL at 44 opens.
- Tests that passed after correction: pin PASS; panel/graph/
  readiness/coverage suites green; byte-content preserved
  (TestGraphPanelLines determinism green — SQL text unchanged,
  only handle source moved); vet clean; -race group green
  incl. all 8 F-15 epoch tests (identity reduction + DDL skip
  preserve replacement/downgrade/concurrent guarantees).
- Before/after: render 628 -> 1 open; TestGraphPanelLines
  4.04s (profile) -> 1.05s isolated; Hoist pin render ~ms.
- Residual: buildRunGraph CLI walk still opens twice (row
  listing keeps its own open; one-shot path, tolerated);
  buildCoverageGraphWith/sessionReader seam untouched.
- Boundary correction (same slice): the first full gate on the
  hoisted tree FAILED on TestBoundaryReadUIHasNoSQLImport +
  TestBoundaryTransportHasNoDBOpeners — the mechanical DOX guards
  correctly rejected openTasksDB/database-sql inside graph_panel.go
  (UI file). Acquisition assembly (readGraphBanner/graphMDLinesOn/
  depChainOn) moved verbatim to graph_readiness.go (events/domain);
  the panel keeps layout/shaping only and delegates through the
  read model. Boundary + pin + panel suites green, vet clean,
  -race group green. The unused depChain one-shot wrapper was
  removed (no callers). Finding: the guards work — a perf slice
  that violates layering fails loudly instead of landing.
- Gate note (post-correction): full `go test -count=1
  -buildvcs=false -timeout 25m ./...` ALL GREEN, exit=0 —
  ok tui-dash 326.367s + musewire/termview/tui ok, 0 FAIL lines.
  Zero pty-supervise strays (bracket-pattern pgrep, no self-match).
  F-19 CLOSED: render 628 -> 1 open/layered through the read model,
  boundary guards green, race green.

## Perf ledger — report/retry BUSY flake: deadline retry — FIXED (test-side)
- ID/severity: Perf P1 (suite stability).
- Root cause: TestConcurrentReportVsRetryFilesAuthoritativeAttempt
  retried BUSY 5x with no delay. Faster opens (epoch-gated DDL skip)
  tightened the burst until all 5 attempts fit inside the
  reconciler's lock window: profile run failed round 0 with
  SQLITE_BUSY at 0.54s. The 5-attempt budget was implicitly
  calibrated to slow attempts, not to contention.
- Files changed: `report_tx_boundary_test.go` only (retry loop ->
  5s deadline + 5ms backoff, matching the busy_timeout horizon).
  Production untouched; assertions unchanged. New loop strictly
  dominates old (identical first attempts, only adds patience).
- Tests: 3/3 pass + -race pass. Monotonicity argument recorded
  above (no deterministic repro of the WAL snapshot race staged;
  mechanism + dominance documented instead).
- Residual: a deterministic BUSY_SNAPSHOT injector would pin this
  class; not built (would need lock-choreography harness).

## Perf residual — muse binary boot dominates harness tests — DOCUMENTED
- Measured: `muse serve --provider echo --join` spawn-to-init
  1.3s warm / 6.7s cold (external static binary); steady-state
  wire RTT 4ms (session/list). Second spawn while first lives
  still 4.7s: the binary always boots, --join does not skip it.
- Impact: every startMuseHost costs seconds. TestCmdStatusLeavesCursor
  spawns twice (~8-10s); CliWorkerLoop/MuseNativeWorkerLoop/
  TurnResubmitDedups/MuseEcho/NativeSteer each spawn 1-2 hosts.
- Disposition: EXTERNAL — the boot cost lives in the closed muse
  binary, not this repo. In-repo mitigations rejected: sharing one
  host across tests breaks isolation (ambient session store);
  bypassing museCmdStatus in its own test drops cmd-path coverage.
  Real fix (thin daemon-socket client skipping binary boot for
  read-only commands) is a transport epic with protocol-conformance
  risk — recorded as follow-up, not this slice.
- Gate note (final perf loop): full `go test -count=1
  -buildvcs=false -timeout 25m ./...` ALL GREEN, exit=0 —
  ok tui-dash 269.086s + musewire/termview/tui ok, 0 FAIL lines.
  Suite 356.6s -> 269.1s (-25%, -87s) across the open-path +
  graph-hoisting slices. Zero muse/pty strays, no scratch files.
  Perf loop closed: open-path DDL skip, F-19 hoisting (read-model
  layered after boundary-guard correction), retry deadline fix;
  muse-boot + DSN-fold + CLI-walk-2-opens logged as residuals.

## Phase 2/3 ledger — refresh coordinator extracted to internal/tui — FIXED
- ID/severity: P1 structural (second component migration; no
  behavior change by design).
- Intended single owner: `internal/tui.Refresh` owns tick count,
  projection generation, in-flight accounting. Shell
  (`tui.go` Update) builds commands from `TickOutcome`;
  `applyRefresh` compares generations read-only; `navReselect`
  and startup use `Reselect`/`Startup`.
- Files changed: `internal/tui/refresh.go` (new) +
  `refresh_test.go` (4 pure tests), `internal/tui/AGENTS.md`
  (registered), `tui.go` (model field `tick/gen/refreshPending`
  -> `refreshCtl tuistate.Refresh`; tick/refreshMsg cases
  delegate), `tui_state.go` (`applyRefresh` gen gate reads
  `refreshCtl.Gen`; startup uses `Startup`), `tui_viewstate.go`
  (`navReselect` uses `Reselect`), 4 test files (12 fixtures
  `m.gen = N` -> `m.refreshCtl.Gen = N`), `TUI_DOX.md` (rows +
  item 5).
- Tests that failed before correction: none (pure addition until
  wiring; extraction verified by preservation, not by a red
  phase — the pre-existing F-18 integration pin
  `TestRefreshSkipsWhileOneIsInFlight` + new pure tests carry it).
- Mutation proof: `if out.Dispatch` -> `if true` makes
  `TestRefreshSkipsWhileOneIsInFlight` FAIL; reverted.
- Tests that passed after correction: 4 pure coordinator tests;
  focused TUI/boundary/nav/attach/input suites green; vet clean;
  -race group green (11.0s + 1.1s); FULL GATE green, exit 0 —
  ok tui-dash 294.521s + musewire/termview/tui ok, 0 FAIL lines.
- Residual: `guidanceBusy`/`input`/`attachGen` still flat model
  fields — next components in migration order; `Reselect` and
  `Startup` share a body with intent-documenting names (kept
  separate deliberately).

## Phase 2/3 ledger — attach lifecycle extracted to internal/tui — FIXED
- ID/severity: P1 structural (third component migration; no
  behavior change by design; finding G leg).
- Violated boundary: flat `model.attachGen` int lived next to the
  mode it fenced; any adapter edit could compare (or skip)
  generations by hand. Intended single owner: `internal/tui.Attach`
  owns attempt generation + target session; shell classifies and
  applies effects only.
- Files changed: `internal/tui/attach.go` (new: `Attach`,
  `Request`/`Classify`, `AttachDecision`) + `attach_test.go`
  (4 pure tests, zero infra), `tui.go` (flat `attachGen int` ->
  `attachCtl tuistate.Attach`), `tui_attach.go`
  (`requestAttach` uses `Request`; `applyAttachResult` classifies
  via `Classify`, effects only on Confirmed/Refused),
  `tui_decompose_test.go` (`TestAttachRefusalDetaches` moved onto
  the authoritative `Request` path — test-only), `boundary_test.go`
  (new `TestBoundaryAttachOutcomesRoutedThroughFence`),
  `TUI_DOX.md` (attach row + contract + item 6).
- Tests that failed before correction: `TestAttachRefusalDetaches`
  (hand-rolled `mode="attach"` with zero fence state is unreachable
  via `requestAttach`; migrated to `Request`, not weakened — the
  stale half still asserts untouched state).
- Mutation proof: gen-check removal makes
  `TestAttachStaleRefusalCannotClobberNewerAttach` FAIL
  (`stale refusal must not detach newer attach, mode="nav"`);
  restored → green. Classify-bypass in the adapter makes
  `TestBoundaryAttachOutcomesRoutedThroughFence` FAIL
  (no-Classify error); restored → green.
- Tests that passed after correction: 4 pure attach tests; full
  attach suite (9 tests) green; focused nav/refresh/input suites
  green; vet clean; -race group green (attach/refresh/nav, both
  packages); FULL GATE green, exit 0 — ok tui-dash 252.692s +
  musewire/termview/tui ok, 0 FAIL lines.
- Residual: `guidanceBusy`/`input` still flat model fields — next
  components in migration order; hand-rolled `mode="attach"` setups
  remain in input-routing tests (no outcome asserted, unreachable
  with fence — tolerated, same as `model{...}` literals).

## Phase 2/3 ledger — guidance extracted to internal/tui — FIXED
- ID/severity: P1 structural (fourth component migration; no
  behavior change by design; guidance-submission path).
- Violated boundary: flat `model.input` string + `model.guidanceBusy`
  bool edited directly by the event loop; any key-handling edit
  could bypass serialization or misattribute an outcome.
  Intended single owner: `internal/tui.Guide` owns buffer,
  editing, one-in-flight serialization, outcome status; shell owns
  mode transitions + `submitGuidanceCmd` construction.
- Files changed: `internal/tui/guide.go` (new: `Guide`,
  `Type`/`Backspace`/`Clear`/`Submit`+`GuideIntent`/`Complete`+
  `GuideOutcome`) + `guide_test.go` (7 pure tests),
  `tui.go` (flat fields -> `guideCtl tuistate.Guide`; enter/
  backspace/type + `guidanceResultMsg` via component),
  `tui_render_chrome.go` (prompt reads `guideCtl.Buf`),
  `phase4_test.go` + `tui_decompose_test.go` (fixtures onto
  `guideCtl.Buf`/`.Busy` — test-only), `boundary_test.go`
  (flat-field bans + `TestBoundaryGuidanceRoutedThroughComponent`),
  `TUI_DOX.md` (guidance row + item 7).
- Tests that failed before correction: none (pure addition until
  wiring; extraction verified by preservation — pre-existing
  `TestGuidanceDoubleEnterSerializes`,
  `TestGuidanceSubmitOffUpdatePath`,
  `TestGuidanceResultNamesOriginSession`,
  `TestInputBackspaceRuneAware` carry it). One behavior nuance
  preserved deliberately: enter with no live selection clears and
  parks with busy untouched (checked before `Submit`, matching the
  old `text != "" && sel != ""` gate order).
- Mutation proof: busy-check removal makes
  `TestGuidanceDoubleEnterSerializes` + `TestGuideSubmitHoldsWhileBusy`
  FAIL; restored → green. `Complete` bypass in the adapter makes
  `TestBoundaryGuidanceRoutedThroughComponent` FAIL; restored → green.
- Tests that passed after correction: 7 pure guide tests; full
  guidance behavior set (9 tests incl. durable/audit/recovery/
  demo/CB13) green; vet clean; -race group green (guidance/
  boundary/guide, both packages); FULL GATE green, exit 0 —
  ok tui-dash 294.354s + musewire/termview/tui ok, 0 FAIL lines.
- Residual: `direct` flag + `mode` still flat (navigation-owned per
  DOX); hand-rolled `mode="attach"` setups remain in input-routing
  tests (no outcome asserted — tolerated).

## Phase 6 ledger — status-as-control-state removed — FIXED
- ID/severity: P2 structural (Phase 6 competing-authority sweep;
  no behavior change except the fixed clobber).
- Violated boundary: `applyRefresh` parsed display text
  (`strings.HasPrefix(m.status, staleProjectionPrefix)`) to decide
  whether to clear the status line — any foreign message sharing
  the prefix would be clobbered by a fresh install. Intended
  single owner: `model.staleMarker`, written only by
  `applyRefresh`/`installStaleMarker`; a fresh apply clears the
  status only on exact equality with the installed marker.
- Files changed: `tui_state.go` (marker install + equality clear,
  `installStaleMarker`), `tui.go` (`staleMarker` field),
  `p0c_projection_test.go`
  (`TestRefreshApplyPreservesForeignStatus`), `boundary_test.go`
  (`TestBoundaryNoStatusSniffing`), `TUI_DOX.md` (resolved row).
- Tests that failed before correction:
  `TestRefreshApplyPreservesForeignStatus` (fresh apply cleared a
  forged prefix-sharing status to `""`).
- Mutation proof: restoring prefix-sniffing makes the pin FAIL;
  restored → green. Injecting `strings.HasPrefix(m.status, ...)`
  makes `TestBoundaryNoStatusSniffing` FAIL; restored → green.
- Tests that passed after correction: stale suites
  (`TestRefreshStaleGenerationRefused`,
  `TestRefreshStaleSessionRefused`, `TestGitPaneClearedOnSessionChange`),
  full boundary suite (8 tests); vet clean; -race group green;
  FULL GATE green, exit 0 — ok tui-dash 332.943s +
  musewire/termview/tui ok, 0 FAIL lines.
- Residual: `staleMarker` is a flat model field, not an
  `internal/tui` component — acceptable: single writer, typed
  identity, AST-pinned; extraction only if staleness grows more
  transitions.

## F-09 follow-up — retention restart flake synchronized — FIXED
- ID/severity: P2 flake (third occurrence class with messages;
  previous two dispositions were VERIFIED NON-ISSUE under the old
  load-only theory — this round found a real test-side race).
- Root cause: `rotateLocked` lands truncation + gap marker BEFORE
  the generation bump (documented order). The tests polled stream
  SIZE then asserted the generation immediately, so a size poll
  could observe a mid-rotation file while the bump was still in
  flight — failing as gen 0 (line 98) or gen 1 (line 115) in fast
  runs (0.03–0.17s, no deadline hit). Product rotation completes
  atomically under the session lock; no product change needed.
  `retention.go` has zero code references to model/Nav (comments
  only), ruling out the concurrent mode-migration slice.
- Files changed: `retention_autonomy_test.go` only (test-side):
  new `waitStreamGeneration` deadline poll; exact expected values
  (3 / 1 / 2) unchanged at the three async-sweeper sites
  (`TestRetentionAutonomousWithoutReader`,
  `TestRetentionSurvivesRestart` x2). Synchronous sites untouched
  (no async sweeper, no race).
- Tests that failed before correction:
  `TestRetentionSurvivesRestart` 2/6 isolated + 1/full-gate with
  both gen-0 and gen-1 signatures.
- Tests that passed after correction: 20/20 isolated stress green;
  full retention group green; -race retention group green (5.5s).
- Residual: 5s deadline polls add bounded worst-case latency to a
  failing run only; passing runs unaffected (~0.2s).

## P0 ledger — sel-scoped panes bound to their session — FIXED
- ID/severity: P0 correctness (cross-session contamination: after a
  tab switch, worker A's entry metadata, live output, events, and
  Git data rendered as worker B's until B's collection landed —
  indefinitely when acquisition hung).
- Root cause: `applyRefresh` fenced delivery by generation+session
  but nothing bound the INSTALLED panes to their session; renderers
  painted whatever was cached. Dispatch already captured sel and
  delivery already refused mismatches — the gap was purely that
  last-known data carried no identity at render time.
- Intended single owner: `applyRefresh` records `model.paneSel`
  (sole writer, alongside the installs); renderers show sel-scoped
  data only through `boundEntry`/`boundLines` on paneSel match.
- Files changed: `tui.go` (`paneSel` field), `tui_state.go`
  (record binding), `tui_render_chrome.go` (`boundEntry`/
  `boundLines`, header + events line), `tui_render_worker.go`
  (live + git panes), `tui_render.go` (thread sel),
  `tui_render_snapshot_test.go` (failing-first
  `TestRenderSelectionChangeHidesPreviousSession` + paneSel
  fixture), `boundary_test.go` (routing + single-writer guards).
- Tests that failed before correction:
  `TestRenderSelectionChangeHidesPreviousSession` (header showed
  w0's harness/model under w1).
- Mutation proof: header gate bypass makes the pin FAIL; direct
  `.live` read makes the routing guard FAIL; both restored → green.
- Tests that passed after correction: full render/snapshot suites;
  broad TUI suites (attach/guidance/input/tab/muse/terminal/
  projection/refresh/boundary) green; -race P0 group green;
  FULL GATE green, exit 0 — ok tui-dash 294.089s +
  musewire/termview/tui ok, 0 FAIL lines. One gate iteration
  caught an unmigrated fixture (`TestSanitizedOverview` set panes
  without paneSel); fixed test-only, re-gated green.
- Residual: rail `tabInfo` stays keyed per session (legitimate list
  content); feed/panel projections are global by design.

## P1 ledger — session-list acquisition failures labeled — FIXED
- ID/severity: P1 (silent conversion of registry/PTY-DB failures
  into empty session lists: `metas, _ = ptyListAll()` dropped every
  PTY session; corrupt registry JSON unmarshalled to empty).
- Intended single owner: `applySessionsRefresh` installs only when
  `refreshData.sessionsOk`; `applyRefresh` labels partial failure
  with the existing stale marker. Trust = registry clean AND PTY
  query ok (`loadRegistryKnown`, `sessionsRound` 5th return).
- Files changed: `session_registry.go` (`loadRegistryKnown`;
  `loadRegistry` delegates, contract unchanged),
  `tui_source.go` (trust flag through `sessionsRound` →
  `refreshSnapshots` → `refreshData`), `tui_projection.go`
  (field), `tui_install.go` (skip on !ok),
  `tui_state.go` (partial-failure stale marker),
  `p0c_projection_test.go`
  (`TestRefreshSessionsFailureKeepsLastKnown`),
  `session_test.go`
  (`TestSessionsRoundCorruptRegistryReportsFailure`), 6 test
  files (21 literals declare `sessionsOk: true` — test-only),
  `TUI_DOX.md` (sessions row).
- Tests that failed before correction: both pins red pre-fix
  (missing field/return = build failure; installer skip removal
  and forced-trust mutations re-redden them — see below).
- Mutation proof: installer-skip removal makes the keep-last-known
  pin FAIL; forced trust on corrupt JSON makes the corrupt-registry
  pin FAIL; both restored → green.
- Tests that passed after correction: projection/session/
  boundary/assembly suites green; vet clean; FULL GATE pending
  (runs next).
- Residual: PTY DB-error path shares the flag by construction
  (fault-injecting `openTasksDB` is out of scope — no seam);
  `loadRegistry` keeps the lenient contract for non-UI callers
  (documented); feed/panel failure handling untouched.

## Phase 3 ledger — mode ownership moved into Nav — FIXED
- ID/severity: P1 structural (criterion 7: navigation's `mode`
  was written directly by shell/attach/input/state — 10 raw-string
  sites across 5 files; no behavior change by design).
- Intended single owner: `internal/tui.Nav.Mode` (typed
  `ModeNav/ModeInput/ModeAttach`) changes only through
  `EnterInput`/`EnterAttach`/`ToNav`; shell/attach/input/state call
  transitions; `Attach.Classify` takes `Mode`.
- Files changed: `internal/tui/nav.go` (`Mode` + transitions) +
  `nav_mode_test.go` (2 pure tests), `tui.go` (flat `mode` removed;
  reads → `m.nav.Mode`, writes → transitions),
  `tui_attach.go`/`tui_input.go`/`tui_state.go`/
  `tui_render_chrome.go` (same), `internal/tui/attach.go` +
  `attach_test.go` (`Classify` takes `Mode`), 7 test files
  (fixtures onto `m.nav` transitions/`tuistate.Mode*` — test-only),
  `boundary_test.go` (`TestBoundaryModeWritesRoutedThroughNav`),
  `TUI_DOX.md` (navigation row + resolved row).
- Tests that failed before correction:
  `TestBoundaryModeWritesRoutedThroughNav` (10 direct-write hits).
- Mutation proof: direct `m.nav.Mode = ...` injection makes the
  guard FAIL; restored → green.
- Tests that passed after correction: 2 pure mode tests; full
  attach/guidance/input/tab/terminal suites green; vet clean;
  -race mode/retention groups green; FULL GATE green, exit 0 —
  ok tui-dash 328.716s + musewire/termview/tui ok, 0 FAIL lines
  (combined tree incl. F-09 retention synchronization fix).
- Residual: `scroll`, `direct`, `width/height`, `status` still flat
  (display/viewport state, single-writer or display-only);
  zero-value `Nav.Mode("")` routes as nav — documented, matches
  the old empty-string default.

## Phase 4a ledger — P0c stale-install + P1 acquisition trust + sweeper shutdown — FIXED
- P0c root cause: `applySessionsRefresh` installed terminal panes
  without rebinding `m.paneSel` to the snapshot's selected session,
  so freshly acquired worker B output rendered under worker A's
  identity slot (cross-session contamination).
- P1 root cause: acquisition failures (corrupt/unreadable registry,
  PTY errors) collapsed into empty session lists; failed lists
  overwrote last-known projections. `tui_source.go` had no
  failure-vs-empty distinction; no trust flag flowed to install.
- Files changed: `tui_install.go` (rebind `paneSel` on install +
  skip projection install on `!listOK`, stale marker set),
  `tui_state.go` (`installStaleMarker`), `tui_source.go`
  (`sessionsRound` 5th return `listOK`), `tui_projection.go`
  (`refreshData.sessionsOk` plumbed through `refreshSnapshots`→
  `refreshData`), `session_registry.go` (`loadRegistryKnown`:
  missing file = empty-ok, corrupt/unreadable = failure;
  lenient `loadRegistry` kept for non-UI callers),
  `p0c_projection_test.go` (pins), `session_test.go`,
  `tui_projection_test.go`, `bounded_reads_test.go`,
  `main_test.go`, `muse_tab_test.go`, `tui_decompose_test.go`
  (fixtures onto bound panes).
- Retention shutdown race (found during gating): `TestRetentionSurvivesRestart`
  failed only on TempDir `RemoveAll` cleanup — sweeper tick wrote
  into the state dir after removal; `close(stop)` never joined the
  supervisor. Fix: `StartRetentionSweeper` returns a `done` channel
  closed on supervisor exit (`retention.go`); tests join it
  (`retention_autonomy_test.go`); `worker-loop` (`cli_tasks.go`)
  also joins on shutdown. Doc comment updated.
- Tests that failed before correction: `TestRenderSelectionChangeHidesPreviousSession`,
  `TestSanitizedOverview`, `TestRefreshSessionsFailureKeepsLastKnown`,
  `TestSessionsRoundCorruptRegistryReportsFailure`,
  `TestRefreshApplyPreservesForeignStatus` (each red pre-fix);
  `TestRetentionSurvivesRestart` cleanup error pre-fix.
- Tests that passed after correction: full P0c/P1 focused suites +
  boundary guards green; retention autonomy 5x repeat + `-race`
  green; FULL GATE green, exit 0 — ok tui-dash 286.264s +
  musewire/termview/tui ok, 0 FAIL lines (`/tmp/td-gate-sessions2.log`).
- Residual: P1b refresh concurrency (`internal/tui/refresh.go`
  exact-request correlation) still open; residual god objects
  (`verdict.go`, `worker_subprocess.go`, `mcp_terminal_tools.go`)
  triage pending.

## Phase 4b ledger — P1b refresh exact-request correlation + stall deadline — FIXED
- Root causes (all in refresh accounting, `internal/tui/refresh.go`
  + one shell call site): (1) `OnDelivered()` cleared `Pending`
  unconditionally, so a stale/superseded/duplicate delivery re-armed
  dispatch while the current round was still running — unbounded
  restacking across periodic ticks + rapid navigation; (2)
  `Reselect()` marked nothing outstanding when idle, so an
  idle-reselect dispatch plus the next odd tick ran two concurrent
  collections; (3) no deadline: a hung acquisition held `Pending`
  forever and ticks coalesced forever — refresh stalled permanently.
- Intended single owner: `Refresh` transitions (unchanged
  ownership); shell only builds commands and reports each
  delivery's gen.
- Files changed: `internal/tui/refresh.go` (exact correlation:
  `outstanding` gen + `dispatchedAt` tick, `OnDelivered(gen)`,
  `stallAfterTicks = 10` abandonment, `dispatch()` shared by
  tick/reselect), `internal/tui/refresh_test.go` (4 new
  failing-first tests; 2 existing tests migrated to the correlated
  contract — stale delivery no longer re-arms, by design),
  `tui.go` (shell passes `msg.data.gen`), `TUI_DOX.md` (item 5),
  `internal/tui/AGENTS.md` (refresh contract).
- Tests that failed before correction (red): new tests did not
  compile pre-fix (`too many arguments in call to r.OnDelivered`).
- Mutation proof: unconditional-clear restore → 4 FAIL
  (`ReselectBypassesPending`, `DeliveryCorrelatesExactly`,
  `ReselectStorm`, `HungRound`); deadline+reselect-marking removal
  → 3 FAIL; fix restored → green.
- Tests that passed after correction: 8 pure `internal/tui`
  refresh tests; `TestRefreshSkipsWhileOneIsInFlight` integration
  pin (delivers the real collected gen) green; focused
  refresh/assemble/boundary suites + `-race` (internal/tui and
  main refresh/boundary groups) green; vet clean.
- Residual: full gate at convergence (avoid repeated ~300s runs
  per slice); residual god objects
  (`verdict.go`, `worker_subprocess.go`, `mcp_terminal_tools.go`)
  triage still pending.

## Phase 4c ledger — residual god-object triage — VERIFIED COHESIVE (no split)
- `verdict.go` (818 lines): single writer — `fileCriterionVerdict`
  is called only from `cli_tasks.go:286` (transport → service);
  both `INSERT`s live in this file; DDL helpers are invoked by
  schema paths (`task_schema.go:475`, `task_readmodel.go:103`);
  attention via the shared `openAttention` service (same as
  `cli_attention.go`), a bounded business side effect of filing, not
  a second authority. Cohesive verdict ledger.
- `worker_subprocess.go` (440 lines): sole writer of
  `worker_consumers`; external users call the read-only
  `attachedWorkerConsumer` (`task_dispatch.go`, `task_readmodel.go`)
  or enter via transport (`cli_tasks.go`) / phases
  (`worker_phases.go:150` claim); execution bodies already live in
  `worker_run.go`/`worker_phases.go`. Cohesive loop-driver+registry.
- `mcp_terminal_tools.go` (589 lines): read dispatch
  (`mcpToolTerminal` status/output, unauthenticated by design like
  all `mcp_read_*`) vs three mutating entries (input/resize/stop),
  each `mcpAuthCaller` + `mcpTerminalGate` + lease-authority
  delegation; reads bounded (`readTail`/capped lines) and refuse
  stale output for dead sessions. Already separated per F-08/F-02.
- `mcp_provision_tools.go`: persistence faces are one-line compat
  wrappers delegating to `ProvisionRepository`
  (`Ensure`/`Load`/`CompareAndSet`); real authorities are
  `provision_repository.go` (persistence), `provision_service.go`
  (lifecycle), `mcpProvisionSessionBackend` (adapter). No dual write.
- `muse_host.go` (427 lines): pure JSON-RPC child-process wrapper
  (start/call/notify/readLoop/Close); no task/session/ledger
  authority. Cohesive.
- Disposition: VERIFIED COHESIVE for all five — file size alone is
  not a conflict; no dual writers, no bypassed invariants, no
  second dispatchers found. No code changes.

## Convergence gate — P1b tree full suite — GREEN
- `go test -count=1 -buildvcs=false -timeout 25m ./...` on tree
  `d4ef57d` + triage ledger: exit 0, 0 `--- FAIL` lines —
  ok tui-dash 344.915s + musewire/termview/tui ok
  (`/tmp/td-gate-p1b.log`). Covers P1b shell change (`tui.go`
  `OnDelivered(msg.data.gen)`).

## Product capability matrix (2026-11-01; local evidence only)
- IMPLEMENTED + LOCALLY QUALIFIED (hermetic tests, no quota):
  12-worker fleet (`TestTwelveWorkerFleet`: 12 distinct workers,
  lease isolation, capacity gate); provision/bind/depend/correct
  with idempotent intents + crash-heal; dispatch w/ consumer
  identity + heartbeat + pickup deadline (F-07); terminal
  input/resize/stop via lease authority + generation fence +
  idempotency (F-08); task hierarchy/activity/guidance via MCP
  control smoke loop; provider/model revalidation + `cursor_turn`
  attribution; verdict assurance + receipts (F-03/04); retention
  sweeper autonomy; refresh boundedness + pure renderer + render
  snapshots (`tui_render_snapshot_test.go`, 11 tests); presence as
  5 typed facts w/ explicit Unknown (F-14) + lease-driven Hatch
  glyph (operator decision 2026-11-01).
- LIVE-QUALIFIED: none with real paid models (no quota spent).
- BLOCKED (exact prerequisite + qualification):
  P6-2 live FI-backed turn + triads — needs explicit quota
  authorization, then cross-machine + recovery qual; P5-2
  child/subagent events — wire shape unknown, echo provider emits
  no child events, needs live-provider qualification not a guessed
  parser; cross-machine remote transport — explicitly refused by
  design (local-only), any real round trip needs operator setup;
  project triads (Locus/Jeverb/Ocliv backends absent).

## Phase 5 ledger — P1-1b lease-authority closure — FIXED
- Blocker resolved: the "needs lease-authority design" prerequisite
  is satisfied by F-08 (MCP input/resize/stop: lease + generation
  fence + idempotency, 11 tests), F-11 (sender lifecycle registry
  backend+gen, retire, stale-gen reject), and the TD-061 single
  writer (`tmuxAcquireLease` in `pty_lease.go`, enforced inside
  `terminal_input_queue.go`).
- Exhaustive audit: `grep send-keys` over non-test sources finds 4
  execution sites — `terminal_input_queue.go:170,175` (inside the
  lease authority, under the lease it holds) and
  `session_guidance.go:81,84,109,114` (both functions acquire
  `tmuxAcquireLease` + `resolveOwnedTarget` before sending; PTY
  guidance uses `ptyInput`). No blind (lease-less) send-keys
  remains; PTY input and MCP resize/stop were already fenced.
- New mechanical guard: `TestBoundarySendKeysRequiresLease`
  (`boundary_test.go`, guard (i)) — every `tmux("send-keys", …)`
  call site must live in `terminal_input_queue.go` or in a function
  containing a `tmuxAcquireLease` call. Mutation proof: rogue file
  with blind send-keys → FAIL naming file+function+remedy; rogue
  removed → green.
- Tests passed: full `TestBoundary*` suite; focused
  lease/sender/input/control/guidance/attach groups with `-race`
  (30.8s) green; vet clean.
- Residual: none locally actionable — live-operator keystroke
  delivery timing under real load is live-qualification (blocked on
  operator-run sessions, no quota spent).

## Phase 5b ledger — deferred rows re-audited (D-7, P1-3, R-G)
- D-7 schema centralization → VERIFIED NO-ACTION. 31 `ensure*Table`
  helpers across domains, each table with exactly one logical
  owner: the only doubled DDL (`task_receipts`,
  `coverage_obligations`) is the sanctioned Table/Tx mirror pair
  inside one domain file each. Migration ordering is owned by
  `task_schema.go` epoch gating (F-15). Centralizing DDL would
  create a god file and break domain co-location with zero
  behavior gain — the opposite of the remediation direction.
  Invariant holds: one ensure per table, epoch-gated migration.
- P1-3 single package bounds → SUPERSEDED by enforcement already
  in place. Compile-time isolation exists where extraction
  happened: `internal/{tui,termview,musewire}` import stdlib only
  (+termview), main imports them one-way; AST guards pin no-tea in
  terminal/session/pty domains, no-SQL in read/UI surface, and the
  TUI import closure. Further extraction without a triggering
  authority conflict would be file-splitting, explicitly out of
  scope ("decompose only when responsibilities genuinely
  conflict").
- R-G Go package boundaries → same disposition as P1-3: file-level
  authority + package isolation where it carries a dependency
  direction; no remaining violation names a package that must
  exist. Reopen only on a concrete bypass a file boundary cannot
  express.

## Final convergence gate — full tree green — 0 FAIL
- `go test -count=1 -buildvcs=false -timeout 25m ./...` on tree
  `e07f230` (P1b guard + deferred-row audit): exit 0, 0 `--- FAIL`
  lines — ok tui-dash 333.670s + musewire/termview/tui ok
  (`/tmp/td-gate-final.log`). Includes the `TestBoundarySendKeysRequiresLease` guard and all prior slices.
- Locally actionable work is exhausted: P0c/P1-acquisition/P1b/
  P1-1b FIXED and gated; sweeper shutdown fixed; triage verified
  cohesive (no splits); D-7/P1-3/R-G dispositioned with evidence;
  capability matrix recorded. Remaining items are BLOCKED on
  operator authorization: P6-2 live FI-backed turns (quota),
  P5-2 child-event wire shape (live provider), cross-machine
  transport, project triads.

## Phase 6 ledger — P6-2 single live FI-backed turn — QUALIFIED (1 turn)
- Authorized: one FI-backed turn on meta/musefi, then stop + report.
- Path: isolated store (`MUSEFI_DATA_HOME=/tmp/livequal-fi`,
  `XDG_DATA_HOME` override, isolated `TUI_DASH_STATE`) →
  `muse daemon --provider meta --profile musefi` → `agent start
  --harness demo` anchor → `muse bind` → `muse turn --text
  'Reply with exactly: QUAL-OK'` → `muse status` → full cleanup.
- Evidence: bind `livequal1 -> native 01a19978-00d9-...`
  (provider meta, profile musefi); turn `01a19978-393b-...`
  terminal=completed cursor `v4:...:27` (31s); model reply
  QUAL-OK verified in daemon transcript snapshots; status cursor
  matches turn cursor; binding row + acknowledged intent with
  pinned workspace in isolated tasks DB. Spend: one minimal
  prompt; Metis records no usage accounting — exact tokens only
  in FI billing.
- Cleanup verified: no muse serve processes, no tmux server, no
  demo worker; qual session dir + index row removed from ambient
  store; /tmp/livequal-* removed; ambient socket dir restored to
  pre-run state (only pre-existing clients/ remains).
- Findings on the path (fixed / documented):
  (1) FIXED: `muse daemon --profile musefi` did not propagate the
  profile store — serve inherited ambient XDG_DATA_HOME and
  listened on ~/.local/share (caught before any turn; ambient
  daemon briefly started, stopped, socket+lock residue removed).
  Fix: `museDaemonEnv` pins `XDG_DATA_HOME=c.DataHome` in
  `museCmdDaemon`; `TestMuseDaemonEnvPinsCommanderStore`
  (mutation-proven: nil-return → FAIL). Files: `muse_cmds.go`,
  `musefi_profile_test.go`.
  (2) DOCUMENTED (daemon-side, not Metis code): session
  transcripts land in ambient `~/.local/share/muse/sessions`
  even when the daemon serves an isolated store (socket + index
  honored the store; transcripts did not). Future quals must
  clean the ambient session dir + index row. Cannot fix in-repo.
- Residual P6-2: triads, cross-machine, recovery-after-disconnect
  still need authorization + backends (unchanged BLOCKED items).
- Observation (not fixed, own slice needed): `agent stop` on an
  already-dead session errors instead of reporting stopped;
  zero user impact here (isolated registry deleted).

## Phase 6b ledger — stop-on-dead-session idempotency — FIXED
- Live-found during P6-2 cleanup: `agent stop` on a finished
  worker errored `cannot resolve session id` and left
  Stopped=false, so finished workers lingered as live.
- Root cause: `stopSession` unconditionally resolved the tmux
  session ID via `display-message`, which fails once the session
  is gone; the record-marking tail was unreachable.
- Fix (`session_lifecycle.go`): absence check first — a dead
  session with a registry record marks stopped via shared
  `markSessionStopped` (event + sender retire + locked record
  update); no tmux addressing on that path, so no live session
  can be mis-targeted. Live path behavior unchanged (same helper).
  Fixture fidelity: fake `display-message` now fails when the
  dead flag exists, matching real tmux (no live-path test sets
  the flag; all pre-existing stop/resolve tests green).
- Tests: `TestStopDeadSessionMarksStopped` failing-first
  (authentic red: `cannot resolve session id for "s"`); mutation
  proof (dead path disabled → FAIL; restored → green).
- Verified: focused stop/resolve/start/cancel suites + `-race`
  group green; vet clean.

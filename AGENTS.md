# Metis — agent working agreement

## Gates

- Build: `go build -buildvcs=false -o tui-dash .` (see Makefile; plain `go build` fails on VCS stamping).
- Test during work: `GOCACHE=/tmp/td-gocache go test -count=1 -run '<Name>' .` plus `go vet .`.
- Full gate before calling work done: `go test ./...` (Makefile `test`).

## Audit dispositions

Every audit must produce a disposition for each material finding:

- FIXED — code changed AND independently verified (a test exercising the
  failure, not the implementation; green tests that predate the fix prove nothing).
- VERIFIED NON-ISSUE — evidence shows the finding does not reproduce.
- ACCEPTED RISK — documented with owner, expiry, and why not fixed now.
- DEFERRED — moved to a named follow-up with order dependency stated.
- BLOCKED — external prerequisite missing; name it.

A fix is not complete until independently verified. Advance to the next
audit only when no unresolved finding invalidates it.

## Current finding ledger (Oct 31 review)

| ID   | Finding                              | Disposition | Evidence                              |
| ---- | ------------------------------------ | ----------- | ------------------------------------- |
| P0-A | Native provisioning nil BuildCommand | FIXED       | TestResolveWorkerCommandNativeRefuses |
| P0-B | MCP bind contaminates stdout         | FIXED       | TestMCPBindKeepsStdoutClean           |
| P0-C | Provision replay/crash-safety        | FIXED       | TestMCPProvision{ReplayConflict,IntentPrecedesLaunch} |
| P0-C2 | depend/correct stale-key convergence | FIXED       | TestMCP{Depend,Correct}ReplayConflict |
| P3-b | provision result states distinct      | FIXED       | TestMCPProvisionResultStates (+reconciled flag on crash-heal) |
| P1-1 | PTY/tmux split provisioning paths    | FIXED       | TestMCPAgentsListsUnifiedSessions, TestMCPProvisionPtyBackend, metis_terminal status/output |
| P1-1b | Model-facing input/resize/terminate  | FIXED       | Lease authority delivered (F-08/F-11/TD-061); 4 send-keys sites all lease-mediated; AST guard TestBoundarySendKeysRequiresLease (mutation-proven); lease/input/attach/boundary race green |
| P5-1 | worker.reported native-session join   | FIXED       | TestWorkerReportedCarriesNativeSession |
| P5-2 | Native child/subagent event projection | BLOCKED     | wire shape unknown; echo provider never emits child events; needs live-provider qualification, not a guessed parser |
| P6-1 | musefi profile path (no-inference)    | FIXED       | TestMuseFiProfileServesEchoSession |
| P6-2 | Live FI-backed turn + four triads     | BLOCKED     | spends real quota; needs explicit authorization, then cross-machine + recovery qual |
| P1-2 | Arbitrary workdir/command, no policy | FIXED       | TestMCPProvision{RefusesUnregisteredRoot,CustomCommandNeedsCapability,HarnessCommandAllowedOnPlainRoot}; project-root-register CLI-only |
| P1-3 | Single package, no enforced bounds   | SUPERSEDED  | Enforced where extraction carries direction (internal/tui,termview,musewire + AST guards); further splits need a triggering conflict |
| P1-4 | No committed revision                | BLOCKED     | repo hook fails closed: no review config |
| D-1  | Dispatch monolith (P0-A)             | FIXED       | TestDispatchConcurrentCrossSessionOneWinnerNoOrphan, TestDispatchCommitRevalidatesDependencies; full suite green |
| D-2  | Session god module (P0-B)            | FIXED       | TestStartSessionRegistryFailureCleansUpTmux (failing-first); full suite green |
| D-3  | Provisioning in MCP (P0-C)           | FIXED       | TestProvisionStaleTransitionMustNotClobber, TestProvisionConflictCreatesNoWorkdir (failing-first); full suite green |
| D-4  | TUI application model (P0-D)         | FIXED       | TestInputBackspaceRuneAware (failing-first), TestAttachEnterUsesAsyncCommand, TestViewRendersFromCacheOnly; full suite green |
| D-5  | Workspace alloc (P1-A)               | FIXED       | workspace_extract_test.go (3 tests); full suite green |
| D-6  | Approvals + recovery (P1-B/D)        | FIXED       | Verbatim extraction (muse_host.go untouched); full suite green + race subset green |
| D-7  | Schema centralization (P1-C)         | VERIFIED NON-ISSUE | One ensure per table (Tx mirrors only); epoch migration owns ordering; centralizing would create a god file |
| D-8  | coverage_obligations (P2)            | FIXED       | Superseded by R-A (store/policy/service/gate split) |
| R-0  | MCP depend/correct atomicity (P0)    | FIXED       | TestMCPDepend/CorrectConcurrentConflictAtomic + rollback tests (-race); full suite green |
| R-A  | Coverage split (P1-A)                | FIXED       | TestCoverageRevisedEvidenceReopensGate (failing-first); full suite green |
| R-B  | input_sender tea split (P1-B)        | FIXED       | TerminalInput session-binding tests; no tea imports (boundary_test.go); full suite green |
| R-C  | Workspace repo authority (P1-C)      | FIXED       | workspace_authority_test.go (6 tests); zero direct record SQL in alloc; full suite green |
| R-D  | Provision transport split (P1-D)     | FIXED       | Zero mcpTool refs in domain; 39 Provision/Bind tests; full suite green |
| R-E  | Team policy + session intent (P1-E/F)| FIXED       | Focused + live Muse wire tests; vet clean; full suite green |
| R-F  | DOX ownership overlaps               | FIXED       | Map corrected (terminal/placement/workspace rows); boundary_test.go enforces tea/SQL splits |
| R-G  | Go package boundaries                | SUPERSEDED  | Same as P1-3: isolation where it enforces direction; reopen only on a concrete bypass file boundaries cannot express |
| S-0  | Report/coverage tx (P0) + reports split | FIXED  | TestDuplicateReportLeavesCoverageUnchanged (failing-first); single-tx submit; receipt policy explicit; full suite + race green |
| S-1  | run_policy split (P0-A)              | FIXED       | One tx-scoped dependency admission; convergence tests; race green |
| S-2  | Worker phase extraction (P1-C)       | FIXED       | 5 typed phases + coordinator; 4 phase tests; race green |
| S-3  | TUI renderer split (P1-D)            | FIXED       | 53-line composition root; 10 snapshot tests; 55 focused pass |
| S-4  | Provision repository (P1-E)          | FIXED       | provision_repository.go owns persistence; lifecycle untouched |
| S-5  | Graph split (P1-F)                   | FIXED       | lineage/readiness/coverage/cli_graph; injected read seams |
| S-6  | Muse protocol split (P1-G)           | FIXED       | session/turn/observation; wire verbatim; lost-ack tests pass |
| S-7  | Attention + read handlers (P2)       | FIXED       | Characterization tests green pre/post move; vet/build clean |
| S-8  | TestMuseApprovalObserverReceipt race | FIXED       | Buffered-chan sync (happens-before edge); 10/10 -race PASS + approval group green; test-only change |
| F-01 | MCP bind loses retry identity | FIXED | Durable bind intent keyed by MCP request_id; retry ≤1 native session, conflict refused, acked-but-unbound reconciles; focused suite 23.6s green |
| F-03/04 | Verdict assurance + conflicts | FIXED | Append-only judgments, typed assurance, verified blocked on dispute/stale/operator-only; disposition+assurance wired into task_status; full suite 226s + race green |
| F-02 | MCP ingress trust-boundary | FIXED | Explicit Local/Remote ingress, fail-closed, scoped caps, doctor diagnostic; 12 failing-first tests + full suite 247s green |
| F-07 | Dispatch w/o consumer guarantee | FIXED | Durable consumer identity + heartbeat/generation, pickup deadline + blocked reason, 4-state projection; failing-first tests green |
| F-09 | Retention only on read | FIXED | Supervised sweeper (flock-serialized, file-backed generation, observable failures); `StartRetentionSweeper` wired into long-lived `worker-loop` (`cli_tasks.go`); `--once`/read paths use backstop + direct calls; autonomy tests green |
| F-05/06 | Provision liveness + native-first | FIXED | resolveOwnedTarget liveness, logical session record, native observation adapter; 12 new tests green (deliverTask→native follow-up open) |
| F-08 | MCP terminal control surface | FIXED | metis_terminal_input/resize/session_stop via lease authority + generation fence + idempotency; 11 tests green |
| D-01 | Dispatch late-admit sidecar race | FIXED | AdmitDispatch now refuses non-pending (was: late admitter overwrote winner sidecar w/ unbound attempt); pinning test + 20/20 stress green |
| P0   | Native pre-submit refund vs receipt  | FIXED       | Atomic exact-attempt cleanup in refundClaim; retry reusable, ambiguous fenced; 26-test race group green |
| P1   | Fleet settlement errors discarded    | FIXED       | Typed settlement result + idempotent reconcile; full suite + 67s race group green; crash/replay covered |
| P1   | Ops diagnostics not operator-usable  | FIXED       | Real req/task/attempt/session identity + `ops` CLI over bounded JSONL sink; E2E via real binary verified |
| P2   | Read-model cross-op tearing          | FIXED       | Single-read-txn snapshot in taskStatusDetail; parity + concurrent-transition pinning tests, race green |
| C-A  | Report tx boundary (P0)              | FIXED       | Attempt-fenced single-tx submit; rollback-before-receipt (5.18s wedge → ms, fault-injected); race-stable |
| C-B  | Transport/UI read-model (P1)         | FIXED       | task_readmodel.go seam; AST boundary test catches helper-smuggled DB access; zero-tree-hits verified |
| C-C  | internal/termview + musewire (P1)    | FIXED       | Byte-identical wire (byte-exact tests); 14x faster local rebuild; child DOX contracts registered |
| C-D  | Phase diagnostics (P1)               | FIXED       | ops_diag.go ring (256 cap, redaction tested); call sites observe-only; cancellation site wired |
| C-E  | AGENTS.md ownership sync             | FIXED       | Map matches extracted modules; stale D-8 corrected; internal/ contracts registered |
| F-10 | Unbounded reads/dir traversal (P2)   | FIXED       | bounded_tail.go reverse tails + capped git/dir sampling w/ truncation markers; 5 failing-first tests; full suite 290s + race green |
| F-11 | Input senders never retire (P2)      | FIXED       | Lifecycle registry keyed backend+gen, Close/Retire, stale-gen reject; 8 failing-first tests + 300-cycle baseline/heap; race found pre-existing test leak (2 old tests lacked cleanup, fixed test-only); full race set green |
| F-14 | Presence is traffic projection (P2)  | FIXED       | 5 typed facts w/ explicit Unknown; Active requires live lease + open work, traffic-only yields labeled Active (est); 7 tests green |
| F-15 | Implicit schema evolution (P2)       | FIXED       | schema_epoch.go durable epoch + identity-fenced admission, downgrade/newer refused; 8 failing-first tests incl. concurrent first-open; full suite + race green |
| P2-gate | Final-tree full-suite receipt      | FIXED       | `go build -buildvcs=false ./...` + `go test -count=1 -buildvcs=false ./...` on final tree (incl. input_sender_test.go cleanup): ok tui-dash 304s, musewire + termview ok; race set green |
| F-16 | Random-suffix IDs broke created-tie ordering (P0) | FIXED | `ORDER BY <ts>, id` → `, rowid` in verdict x2, coverage x2, guidance x2, attention (7 sites); deterministic pin TestJudgmentsSameSecondKeepFilingOrder; gate casualty TestAssuranceSelfSupersedeResolvesDispute green; full gate green 425s exit 0 |
| F-17 | PTY supervisors leak as permanent orphans (P2) | FIXED | Test hygiene: pp1 provision test stops + reaps, fencing test restores identity + stops + reaps, new requireSupervisorReaped guard; 83 orphans cleaned, 0 after focused run and 0 after full gate (389s exit 0); no production reaper (daemon persistence is the pty-start contract) |
| F-18 | Unbounded overlapping refresh collections (P1) | FIXED | Single-flight coalescing: odd ticks dispatch only when none outstanding (model.refreshPending, cleared on delivery); gen bumps with dispatch; failing-first TestRefreshSkipsWhileOneIsInFlight; full gate green 399s exit 0 |
| P3-a | Pickup latency + queue depth        | FIXED       | `pickup_metrics.go` (pickup_observations table, first-write-wins, drift-free gauge, pure projection) + additive dispatch hook + claim-side wiring in `workerClaimAttempt` (best-effort); failing-first tests green |
| P3-b | Unknown-state age + recovery duration | FIXED     | `recovery_metrics.go` (mutex-guarded, 256-cap, first-write-wins age, pure projection) + additive reconcile hooks; 4 failing-first tests green |
| P3-c | Resource-usage snapshot             | FIXED       | `resource_metrics.go` observe-only (goroutines/heap/lanes/stream bytes vs cap); no goroutines/writes/wiring; failing-first test green |
| P3-gate | P3 combined gate                   | FIXED       | build/vet clean; focused P3+claim suites green; full `go test -count=1 ./...` ok tui-dash 301s + musewire/termview ok; `-race` P3 set green (42s) |
| P4 | Cleanup/consolidation scan           | VERIFIED NON-ISSUE | Scanned for dead compat paths (`legacy/compat/placeholder/dummy/deprecat`): all hits are live fallbacks with owners (fleet/worker legacy paths, legacy CLI path, legacy env flags, `readTail` compat wrapper still called by `mcp_terminal_tools.go:558`); no unserved mechanism found, nothing deleted |

## Domain ownership map (DOX root contract)

Package main plus `internal/` packages below; these file groups are the authority boundaries.
Read this map plus every applicable row before editing; update the nearest
row after a consequential edit. Child `internal/<domain>/AGENTS.md` files
exist for each real Go package — never for folders that do not yet exist.

| Domain     | Owns (files) | Must never do |
| ---------- | ------------ | ------------- |
| tasks      | `task_*.go`, `dispatch_*.go`, `task_dependencies.go`, `run_capacity.go`, `run_finalization.go`, `task_corrections.go` | terminal input, process spawn, MSP, review decisions, UI/MCP auth, policy persistence |
| sessions   | `session_*.go`, `exec_target.go`, `terminal.go`, `bounded_tail.go` (registry, backends, lifecycle, bounded observation reads; no PTY internals, no model mutation) | task transitions, verification verdicts, placement, PTY process ownership, Bubble Tea |
| terminal   | `pty_*.go`, `terminal_input*.go`, `input_sender*.go` (delivery, leases, output sequencing; no `tea.*` imports) | task dispatch, attempt claims, approvals, UI imports |
| placement  | `workspace_reserve.go`, `team_policy.go` (pure evaluation), `team_policy_store.go`, `placement_capacity.go`, `fleet_resources.go` | launching agents, choosing objectives |
| muse       | `muse_*.go` (orchestration; wire lives in `internal/musewire`), `approval_*.go`, `guidance_native.go` | strategic delegation, task acceptance |
| assurance  | `assurance_*.go`, `verification.go`, `verdict.go`, `report_service.go`, `artifact_*.go`, `task_receipts.go`, `report_projection.go`, `reconcile.go`, `coverage_*.go` | changing execution history, self-approval |
| workspace  | `workspace_*.go`, `project_roots.go` (checkout allocation; execution-root reservation belongs to placement) | merging/publishing, acceptance decisions |
| fleet      | `fleet_*.go` | reinterpreting task objectives |
| events     | `orchestration_events.go`, `attention*.go`, `graph*.go`, `ops_diag.go`, `*_metrics.go` (observe-only; metric hooks additive, never alter control flow) | creating/approving work, control flow on diagnostics |
| storage    | `task_schema.go`, `schema_epoch.go`, `task_store.go`, `*_store.go`, `*_repository.go`, `task_readmodel.go` (reads only) | UI/transport imports |
| transport  | `mcp_*.go`, `cli_*.go` (validate + authorize + delegate; reads via `task_readmodel.go`; writes own atomic domain tx) | direct `openTasksDB` from read paths, process launch, Git, business authority |
| ui         | `tui*.go`, `graph_panel.go`, `demo.go` (consume `task_readmodel.go` projections; render via `internal/termview`) | direct state mutation, SQL, direct tmux calls |
| contracts  | `contracts.go` (CommandContext, CmdDisposition) | domain logic, imports of any domain |
| termview   | `internal/termview/` (pure terminal math; stdlib only) | imports of main, DB, processes, transports |
| musewire   | `internal/musewire/` (wire framing/codecs; stdlib only) | session orchestration, task ledger, process supervision |

Cross-domain rule: every authoritative state transition has exactly one
owner. Transports (CLI/MCP/TUI) validate and forward; they never mutate
domain state directly. `PrincipalID` comes from transport context, never
from a caller-supplied actor string. No `internal/contracts` dumping ground:
only stable cross-boundary IDs, request context, and shared value types.

## Child DOX index

- `internal/termview/AGENTS.md` — pure terminal math (width, ANSI, sanitize).
- `internal/musewire/AGENTS.md` — Muse wire framing/codecs (no orchestration).
- `internal/tui/AGENTS.md` — dashboard UI-state components (navigation first); stdlib-only, never domain imports.
- `TUI_DOX.md` — TUI component ownership map (shell/navigation/sessions/terminal/guidance/feed/panels/refresh/renderer contracts).

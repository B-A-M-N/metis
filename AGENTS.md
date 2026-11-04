# Metis — agent working agreement

## Docs index

- `docs/TESTING.md` — gate ladder, inventory, Short policy.
- `docs/DEBUGGING.md` — failure class → command → owning domain.
- `docs/OWNERSHIP.md` — cross-domain rules (map itself stays below).
- `docs/CAPABILITIES.md` — implemented vs qualified vs blocked.
- `docs/CONCURRENCY.md` — roles, worktrees, coordinator gates.
- `LEDGER.md` — historical findings and evidence (append-only).
- `TUI_DOX.md` — TUI component contracts. `internal/*/AGENTS.md`
  for package contracts. Start here before touching a subsystem.

## Gates

- Build: `make build` (produces `metis` and the `tui-dash` compatibility symlink; plain `go build` fails on VCS stamping).
- Test during work: `GOCACHE=/tmp/td-gocache go test -count=1 -run '<Name>' .` plus `go vet .`.
- Gate ladder (Makefile; see `docs/TESTING.md`): `make test-quick` every
  iteration, `make test-domain AREA=<name>` after a coherent fix,
  `make test-fast` for broad smoke qualification,
  `make test-integration` after workflow changes (builds a fresh binary
  first), `make test-full` at checkpoints and release (full + race subset).
  `make test` is the release alias. Never add `t.Parallel`; slow
  integration tests carry `testing.Short` (see `test-inventory.json`).

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
| P6-2 | Live FI-backed turn + four triads     | PARTIAL     | single meta/musefi turn QUALIFIED (bind/turn/status/receipts + QUAL-OK reply, cleanup verified); triads + cross-machine + recovery still need authorization/backends |
| P1-2 | Arbitrary workdir/command, no policy | FIXED       | TestMCPProvision{RefusesUnregisteredRoot,CustomCommandNeedsCapability,HarnessCommandAllowedOnPlainRoot}; project-root-register CLI-only |
| P1-3 | Single package, no enforced bounds   | SUPERSEDED  | Enforced where extraction carries direction (internal/tui,termview,musewire + AST guards); further splits need a triggering conflict |
| P1-4 | No committed revision                | VERIFIED NON-ISSUE | Current checkout has populated Git history (`HEAD 3d1acb0` at review); old no-revision blocker is stale |
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
| F-20 | Fleet/report tests hit ambient DB (flake + user-state mutation) | FIXED | 8 files (30 tests: fleet_* + report_tx_boundary) never set TUI_DASH_STATE, so `TestFleetArtifactsGCRespectsLive` counted ambient manifests (want 1, got 3) and tests wrote/deleted real user rows. Added per-test `t.Setenv(TUI_DASH_STATE, t.TempDir())` per repo convention. Verified: GC test 3/3 green, ambient tasks.db hash identical before/after, fleet/report domains + full `make test-full` (incl. race) green |
| P2-c | Diagnose evidence bundle | FIXED | `metis diagnose --session NAME [--json]` (main.go dispatch): identity/backend/alive, latest task + waiting reasons, recent session transitions (sink, redacted), active input leases (read-only handle), process+stream pressure; missing sections noted, never invented; transcripts never read, credentials redacted. Fixed real filter bug found failing-first (zero-value attempt filtered to attempt 0; diagnose passes -1). 6 tests + transport domain (18s) green + live binary smoke |
| P2-b | Ops correlation: filters + typed codes + sink benchmark | FIXED | `ops` gains --req/--task/--attempt/--session/--phase/--class/--errors-only/--since over the same bounded JSON sink (pure `filterOpsEvents`, unknown phase/class refused exit 2, stale unwired-comment corrected: main.go dispatches `ops`). Stable closed code set (`opsKnownClass`); `opsErrorCoder` interface checked before substring fallback via errors.As; `ProvisionError.Conflict` is the first typed producer. Benchmark: ~55us/op sequential, ~95us/op 20-way parallel — immaterial vs ms DB txns, no queue. 2 failing-first tests + 22-test ops suites + classify-adjacent domains (49s) green + live CLI smoke |
| P2-a | Backend-neutral session inventory (doctor + agents) | FIXED | `doctorSessions` no longer returns on empty registry: tmux/PTY/native report independently, missing/unreadable stores yield UNKNOWN (`tableProbe` distinguishes missing from unreadable; doctor stays migration-free). `metis_agents` includes `native:<name>` (`muse-native` backend, recorded-state Alive) via read-only `listLogicalSessions`; listing stays observation-only. 6 failing-first tests (PTY-only, native-only, mixed, nil-DB, closed-DB, agents-native) + doctor/agents/transport domains green (68s) + live `doctor` smoke |
| P1-gate | Test gates + slow-test inventory | PARTIAL | Added `test-quick`, `test-inventory-check`, and `test-inventory-refresh`; broad `test-fast`, domain, integration, and full gates remain. Refresh inventory after final tree qualification; historic count 654 is stale |
| F-19 | Fixture global-dead regression (1bb6283 shrapnel) | FIXED | 1bb6283's `display-message`-fails-when-dead fixture line (unneeded by its own stop test: dead path never reaches resolve) broke 10 suites that launch/adopt after `markFakeTmuxDead` (global flag could never model launch-then-verify: has-session dead + display-message dead + static owner). Fix is fixture-only, no production change: per-session live/dead markers (`new-session` revives only its name, `kill-session` retires per target incl. ID-kill via killmap), per-target `set-environment` stamp tracking in `show-environment`, `TestProvisionReplayHealthyAdoptsLive` dead-mark removed (pins live-adopt as documented; stale-entry adopt stays refused). New pinning `TestFakeTmuxPerSessionLiveness`; `TestCmdStatusLeavesCursor` skip-gated on ambient daemon (PASS with echo daemon, SKIP without). Full `go test -count=1 -buildvcs=false ./...` green 287s, build/vet clean |
| F-21 | Execution-scope check refused native-only sessions (P1) | FIXED | `checkMuseExecutionScope` (muse_profile.go) resolved the workspace via shell-only `sessionWorkdir`, so CLI `muse turn` + worker-native delivery + native observe + native guidance all failed `no such session` on bound native-only sessions — found live during step-6 qual (bind OK, turn refused). Fix: resolve via decoupled `resolveSessionWorkspace` (live shell backing stays authoritative; logical record covers native), matching `resolveSessionModel` and the resolver's documented turn promise. Failing-first `TestCheckMuseExecutionScopeNativeOnly` (failed `no such session: nl`, now passes incl. re-home refusal); shell-backed re-home test untouched green; full gate green 188s + vet clean |
| F-22 | Dispatch/worker paths refused native-only sessions (P1) | FIXED | Same shell-only-resolver class as F-21, five sites, all found live (delegate OK→`no such session: qa`; loop gate→assignment→reconcile each refused in turn): `dispatchSessionScope` (dispatch_plan.go, shell stores else active native logical record; stopped/unknown/non-native refused) feeds AdmitDispatch root+harness+profile; `deliverTask` + retry path use `resolveSessionWorkspace`; `cliWorkerLoop` gate + `workerSessionBase` (worker_phases.go) + `reconcileLoopRestart` accept active native records. Shell-backed behavior and error text unchanged (shared helpers untouched). Failing-first `TestDispatchNativeScope` + `TestCliWorkerLoopAcceptsNativeOnlySession` (both failed pre-fix with the production error, incl. stopped/unknown refusals); worker/dispatch/reconcile/consumer suites 30s green; full gate green 206s + vet clean; live full lifecycle on echo (delegate→work→report→override-verify) in CAPABILITIES |
| F-23 | Diagnose filters after global limit (P1) | FIXED | Reads the bounded sink cap, filters canonical/display aliases, deduplicates and returns newest matching events; explicit invalid-limit and empty/unmatched/evicted-history notes. `TestDiagnoseFiltersBeforeLimit`, `TestDiagnoseRejectsInvalidLimit`, `TestDiagnoseNoMatchExplainsRetainedHistory` green |
| F-24 | Native execution lifecycle/binding drift (P1) | FIXED | Workspace fallback requires readable shell-store absence plus active muse-native logical identity; execution resolver also refuses unreadable logical authority instead of falling back to tmux. Shared validator checks backend, lifecycle, native ID, workspace, model, provider, and profile. `TestMuseTurnRefusesStoppedNativeBeforeSubmit`, `TestNativeUseRejectsLogicalBindingDrift`, `TestWorkspaceResolverDoesNotFallbackOnStoreFailure`, `TestResolveExecTargetDoesNotFallbackOnLogicalStoreFailure` green |
| F-25 | Dashboard omitted native-only workers (P1) | FIXED | Read-only native projection uses `listNativeSessions`, stable `native:<name>` display key plus canonical identity, durable local activity, task/turn record, and update time. Enter/input/direct mode are fenced; guidance routes through structured Muse. `TestTUIListsNativeOnlySession`, `TestTUISelectNativeDoesNotAttachTmux`, `TestTUIRejectsTerminalBytesForNative`, `TestFeedJumpResolvesNativeIdentity`, `TestNativeProjectionCannotLeakAcrossTabs` green |
| F-26 | Refresh reselection exceeded acquisition budget (P1) | FIXED | Fixed one collector permit, 10s cancellable source context, tmux subprocess cancellation, generation-fenced coalescing to latest selection; uncooperative sources retain the permit until return. `TestRefreshReselectStormKeepsOneAcquisitionAndLatestSelection`, `TestRefreshDeadlineCancelsSupportedSourceWithoutEarlyPermitRelease`, `TestRefreshNeverAbandonsExecutingAcquisition`, `TestRefreshStaleCompletionCannotReleaseNewPermit` green |
| P2-resource | Diagnose counters misattributed to worker (P2) | FIXED | Resource snapshots name `observer_process`; native `alive` remains backward-compatible while `lifecycle` and `observation=not_probed` separate stored state from reachability. `TestDiagnoseLabelsNativeLifecycleAndObserverMetrics` green |
| P1-gate | Quick loop + inventory drift (P1) | FIXED | `test-quick` measured ~1s; source check matches 687 tests; refresh runs full uncached suite and records durations. Final `make test-full` green: fresh build, suite 194.042s, race subset 15.207s; `go vet ./...`, `go build -buildvcs=false ./...` green |
| P2-fixtures | Broad suite fixture/setup cost (P2) | DEFERRED | Follow-up `test-fixture-profile` (owner: test maintainers; after the `test-quick` baseline): measure fake-tmux setup/process startup before changing shared fixtures. Warm `openTasksDB()` is 0.814ms/op and no test exceeds 4.31s, so no DB or individual-test optimization is justified; preserve per-test state isolation |
| P2-concurrency | Development worktree collisions (P2) | FIXED | `scripts/metis-worktree.sh` creates role-specific branches, worktrees, and private state directories; refuses collisions and never removes state. Smoke-tested create + collision refusal in an isolated temporary Git repo. Verifier workflow needs no remote push |
| P2-docs | Active docs drift and FI/echo qualification wording (P2) | FIXED | Root finding table clears stale no-commit blocker; testing gates and 686-test inventory sync; CAPABILITIES states one FI-backed meta/musefi turn separately from echo-only qualifications |
| Q-01 | Unified lifecycle projection incomplete (P0) | FIXED | `session_lifecycle_unified.go` + `sessionBusyAll` (task_readmodel): running/ready/busy/stopped/failed/unknown across tmux/PTY/native; `unified_lifecycle` on terminal status (prior partial) completed on `metis_agents` (busy map preloaded once, alive derived from unified) and TUI info pane (new `UnifiedLifecycle` identity field, recorded `Lifecycle` preserved). Failing-first `TestAgentsCarryUnifiedLifecycle` failed pre-fix, green after; `TestUnifiedLifecycle*`, `TestTerminalStatusCarriesUnifiedLifecycle`, `TestTUI*Native*` green |
| Q-02 | Hardcoded Meta.ai controller identity (P1) | FIXED | TUI controller surface renamed provider-agnostic: CONTROLLER rail row, MCP Controller feed title, Controller header identity; statuses no longer name a provider. Verified identities/activity/targets/outcomes were already displayed (controllers + feed). Failing-first `TestControllerTabProviderAgnostic` (no Meta.ai in View, op-1/mcp-stdio shown) green; `TestMuseTab*`, `TestMuseFeed*` updated + green |
| Q-03 | PTY log rendered as stacked fragments; no screen model (P1) | FIXED | `pty_screen.go` replays tail bytes (CR/LF/BS/TAB, CSI cursor/erase, SGR/OSC stripped) so overwrites paint as final text; styles + alt-screen explicitly not emulated (pane title says so). Failing-first `TestPTYScreen*` (CR overwrite, erase line/display, cursor moves, style/OSC strip) green; bounded-refresh test unchanged green |
| Q-04 | TUI never resized PTY workers (P1) | FIXED | Dashboard pane geometry forwarded to selected PTY master via lease-fenced RS frame on WindowSizeMsg, selection change, and attach (`ptyResizeCmd`); tmux panes untouched. `TestPTYResizeCmdGates` green; live E2E: worker `stty size` went 24 80 -> 40 120 after `pty-resize` |
| Q-05 | PTY/tmux keyboard maps incomplete (P1) | FIXED | `ptyKeyBytes` gains Home/End/Ins/Del, Ctrl-A/E/K/U/W/L/D/Z, Ctrl-arrows, Shift-Tab, Alt-rune (ESC prefix); tmux attach lane gains matching send-keys. Local keys (esc/pgup/pgdn) stay local. `TestPTYKeyBytesExtended` green |
| Q-06 | Hermes adapter argv rejected by real CLI (P1) | FIXED | Was `hermes chat --oneshot <prompt>` (no positional exists; requires -q). Now `hermes chat --oneshot -m <model> -q <prompt>` via dedicated `hermesAdapter` (ordering: -m before -q). Muse exec now carries `--model` (was silently dropped). codex/claude re-verified against installed `--help`. Failing-first `TestHermesAdapterMatchesCLI`, `TestMuseAdapterModelFlag` green; argv parse-checked with real `hermes chat -h` (no inference) |
| Q-07 | doctor omitted muse-native adapter (P1) | FIXED | `doctorAdapters` now includes muse-native (the only interactive/continuation adapter); `adapterViews`/agents already listed it. Verified by code inspection against `adapterRegistry`; doctor domain green |
| Q-08 | Remote MCP lacks terminal resize (P1) | VERIFIED NON-ISSUE | Deliberate pinned boundary: `TestMCPHTTPToolsListIsRemoteSubset` + `TestMCPHTTPUnknownToolRejected` forbid resize remotely alongside delegate/approve/verify. Remote is reads + messaging + provision/input/send/stop only. Attempted addition reverted; resize logic extracted to transport-neutral `mcpTerminalResizeOp` (local behavior unchanged) for future alignment |
| Q-09 | Responsibility overlap mcp_http/m2m_bridge/provision_service/tui_source (P2) | VERIFIED NON-ISSUE | Function inventories show clean split: HTTP = transport/auth/dispatch only, provision_service = domain admission/launch/converge, m2m_bridge = M2M adapter + inbox dispatch, tui_source = read model. No decomposition warranted; boundary tests green |
| Q-10 | Full-suite failures in this sandbox (P0 gate) | VERIFIED NON-ISSUE | Daemon/native tests fail with default scratch TMPDIR: unix socket path >108 chars (`bind: invalid argument`, `daemon socket never appeared`). With `TMPDIR=/tmp` all pass, incl. clean-HEAD worktree control. Suite must run with a short TMPDIR in this environment |
| Q-11 | Real-process E2E: two agents, one directory (P0/P1) | FIXED | Isolated `METIS_STATE`, real binary: pty-start e1+e2 same workdir; input to e2 only (receipt `written_to_pty`, e1 log clean); resize applied (stty 24x80 -> 40x120); stop e1 leaves e2 running; ps scan shows no orphaned workers after cleanup |
| Q-12 | PTY screen unbounded expansion from untrusted output (P0) | FIXED | `pty_screen.go`: cursor row/col clamped to retained grid (2000 lines x 2048 cols), CSI params capped in `paramOr`, tab/line expansion bounded. Failing-first `TestPTYScreenAdversarialBounds` (ESC[999999999C/B/H, 100k tabs, malformed CSI) green |
| Q-13 | Idle read as interactive-ready (P0) | FIXED | Lifecycle split: `unified_lifecycle` = process/harness readiness only (ready requires `harnessInteractive`: muse-code or SupportsContinuation + bound native), `task_occupancy` = separate busy/idle/unknown ledger fact. `TestUnifiedLifecycle*` rewritten + `TestHarnessInteractive`, `TestUnifiedLifecycleOccupancyFollowsOpenTask` green |
| Q-14 | Cross-backend occupancy identity collision (P0) | FIXED | `sessionOccupancyFor` matches only the dispatch-canonical spelling per backend (pty:bare / bare-native / bare-tmux); old code matched all spellings so tmux "w" task marked pty:w busy. `TestOccupancyCanonicalSpellings`, `TestOccupancyNoCrossBackendCollision` green |
| Q-15 | Async PTY resize could land out of order / on a replacement (P1) | FIXED | `ptyResizeLatest`: per-session serialization + monotonic sequence (latest-wins, superseded = no-op success) + supervisor pid/start re-verified around the write; model issues persistent per-session sequences and ignores stale results. `TestResizeSeqLatestWins`, `TestPTYResizeLatestLive`, `TestPTYResizeCmdSequencesIncrease` green |
| Q-16 | Newline-free truncated PTY log read as empty (P1) | FIXED | `boundedPTYScreenLines` + `readTailLines`: a cut segment with no newline is one huge line whose tail is real output — kept (truncation flag still marks head loss) instead of dropped to "(no output yet)". Covered in `TestPTYScreenAdversarialBounds` |
| Q-17 | Native liveness overstated from durable state (P1) | FIXED | Binding-only native sessions project unknown (was: active fabricated from binding alone); TUI info pane labels native Process as "recorded <state> (not probed)"; `metis_agents` description documents recorded-state-only native entries. Existing diagnose `not_probed` labeling unchanged |
| Q-18 | Intermittent supervisor-stop timeout (P1 reliability risk) | DEFERRED | `TestSameDirTwoEchoWorkersIsolatedSteering` intermittently fails `stop w1: supervisor did not stop "w1"` (10s stop deadline): 1 failure in full-suite load + 1/3 in one loaded batch; clean HEAD 4/4 and this tree 6/6 consecutive (~8.8s each), full gate green on re-run. NOT claimed fixed — follow-up owner: PTY lifecycle maintainers, after reproducing under controlled load (candidate: ST-frame delivery vs muse echo worker teardown latency under CPU contention). Assertions unchanged |

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

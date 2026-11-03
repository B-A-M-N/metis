# Capability matrix: implemented vs qualified

Implemented means code exists with failing-first tests. Qualified
means the stated evidence was observed. Live-qualified means
observed against a live backend. Do not claim more than the
evidence row states.

## Implemented + locally qualified (repo tests green)

- Three-backend session discovery (`metis_agents`, `doctor`):
  tmux registry, `pty_sessions`, `logical_sessions`; UNKNOWN on
  unreadable stores. Evidence: `TestDoctorSessions*`,
  `TestMCPAgentsLists*` (P2-a).
- Dashboard native-only projection: backend-qualified rail identity,
  local activity details, feed navigation, and structured Muse
  steering; terminal attach and byte input are refused. Evidence:
  `TestTUI*Native*`, `TestFeedJumpResolvesNativeIdentity`.
- Correlated ops (`ops --req/--task/--attempt/--session/--phase/
  --class/--errors-only/--since`), stable closed error codes
  (`opsKnownClass`), typed-code-before-substring classification.
  Evidence: `TestCliOpsFilters`, `TestOpsDiagTypedCodeBeatsSubstring`
  (P2-b); sink cost ~55us/op, no queue warranted.
- One-command bundle (`diagnose --session [--json]`): identity,
  task+waiting, transitions, leases, resources; redacted.
  Evidence: `TestDiagnose*` (P2-c).
- Test gates + inventory: quick/fast/domain/integration/full,
  `test-inventory.json`, source drift check, fresh-binary integration gate.
  Evidence: full gate green + race subset (P1-gate).
- Provision replay/crash-safety, intent precedence, atomic
  depend/correct, dispatch admission fencing, stop idempotency.
  Evidence: `TestMCPProvision*`, `TestProvisionReplay*`,
  `TestDispatch*` (ledger P0-C, R-0, D-01, F-07).
- Retention sweeper (`StartRetentionSweeper` in worker-loop),
  pickup/recovery/resource metrics (observe-only). (F-09, P3-a/b/c)
- Direct Muse message operation (`metis_muse_message`): bound
  native session only, scope-verified, idempotent by request_id,
  delivery-vs-execution states (refused/accepted/unknown/terminal).
  Evidence: `TestMuseMessage*`, `TestDuplicateMessageSubmitsOnce`,
  `TestStoppedSessionRefused`,
  `TestDisconnectAfterSubmissionReconciles`.
- Managed-PTY default for new muse-code workers (explicit tmux
  still supported); PTY-aware `list`/`stop`; command-coherent PTY
  replay. Evidence: `TestProvisionMuseCode*`,
  `TestProvisionPtyCommandMatches`, `TestStopSession*`.
- M2M bridge seam: `muse_inbox_dispatch` claim/recovery,
  fail-closed sender/type/expiry policy, stdin-only subprocess
  invocation of the existing Python client (no crypto port).
  Evidence: `TestInboxDispatch*`, `TestRelayReconnectPreservesQueue`,
  `TestExpiredEnvelopeRejected`, `TestUnregisteredSenderRejected`,
  `TestRevokedSenderRefusedTurn`,
  `TestTrustedInboxItemBecomesOneTurn`,
  `TestUntrustedInboxItemSubmitsNothing`,
  `TestM2MBridgeKeepsSecretsOffArgv`.
- Remote gate for `metis_muse_message` (ungranted remote refused
  with no intent persisted; granted remote reaches the domain).
  Evidence: `TestUnauthorizedMetisWriteRefused`.
- Ocliv loopback leg: `ocliv mcp expose` over `metis mcp serve`
  verified with real binaries (initialize, 26-tool list incl.
  `metis_muse_message`, refused-domain call). Evidence:
  `TestOclivLoopbackExposeServesMetisTools` (skip-gated on
  METIS_OCLIV_BIN).
- Ocliv ingress serves MCP path routes only (`/r/<id>/mcp`); no
  generic HTTPS publication exists, so an M2M relay cannot ride it
  without an Ocliv feature or separate ingress. Metis-side bridge
  stands alone.

## Live qualified

- Interactive Muse Code/TUI MVP (2026-11-02, isolated tmux server,
  `METIS_STATE` and Muse data under `/tmp`, echo provider): launched two
  Muse Code processes in `/home/bamn/Locus` as `locus-01` and `locus-02`;
  both had distinct tmux session IDs and stayed on the same pane PIDs while
  Metis switched the selected tab. The rail showed `Locus · 01` and
  `Locus · 02`; each `F5` attach opened its exact session and detach returned
  to Metis. One prompt was delivered only to `locus-01` through the input
  lease; the other pane remained at its initial prompt. The text snapshot was
  readable but does not preserve full terminal control state. `F6` showed the
  cached info drawer. No Meta/local provider prompt or inference was run.
- Interactive launcher uses a curated `muse-code` command through
  `ProvisionService` and the tmux lifecycle. Provision confirmation waits
  briefly for the tmux shell to exec Muse Code, then checks the recorded
  identity, owned tmux target, workspace, and actual pane process; a
  shell-only pane is not accepted as a completed launch. The F6 drawer reuses
  the existing observer resource snapshot and session-filtered ops diagnostics.
- The pinned Meta.ai tab is an MCP controller/activity surface, not a web
  conversation mirror. Prompt, direct-toggle, and terminal-attach actions are
  disabled there unless a worker tab is selected.
- Fresh no-inference qualification (2026-11-02, isolated tmux + Muse config):
  `locus-01` and `locus-02` launched in `/home/bamn/Locus` and were confirmed
  as Muse processes on distinct pane IDs/PIDs. The Metis TUI showed
  `Locus · 01` and `Locus · 02`; selection changed between them while PIDs
  stayed fixed. Separate normal-terminal tmux clients attached to each exact
  target and detached back without restarting either process. `F6` showed the
  process/resource snapshot and diagnostic section. Muse displayed its
  workspace-trust prompt under the isolated config; it was left unanswered,
  so this re-run submitted no prompt and made no trust decision.

- One FI-backed `meta`/`musefi` turn (P6-2): bind, turn, status,
  receipts, and a `QUAL-OK` reply were observed. This is the only
  recorded FI-backed turn.
- Other native lifecycle, continuity, concurrency, and task-workflow
  qualifications used the local zero-inference echo provider. They do
  not qualify FI-backed behavior.
- Multi-turn continuity (one native session, 3 turns) + daemon-crash
  resilience: client ops succeed via short-lived hosts against the
  durable store (commit `008ae9f` ledger note).
- Concurrent two-session native workflow via production surfaces
  (F-21 qual, $0 echo, isolated METIS_STATE + XDG_DATA_HOME):
  MCP `metis_provision` (muse-native) + `metis_bind` for qa/qb →
  distinct native IDs; concurrent CLI `muse turn` on both
  (completed, ~0.4s each); second turn on qa advanced the same
  native cursor :12 → :24; `metis_bind` replay under the same
  request_id returned the same native ID (no duplicate);
  `muse status` idle + correct cursors; `metis_agents` listed
  both `native:*` alive; `doctor` reported both backends/roots;
  `diagnose --session` bundle complete. Evidence: F-21 ledger row;
  driver + logs in /tmp/qual-step6 (mcp.py, daemon.log).
- Full native task lifecycle via production surfaces (F-22 qual,
  $0 echo, same isolated state): `metis_delegate` to qa →
  dispatched, instruction + sidecar in the logical workspace;
  `agent work --session qa --once` attached consumer gen 1
  (muse-native) and executed the turn (terminal=completed);
  `metis_task_status` attempt 1 report_ready; `metis_report`
  carried the native-turn outcome; `metis_verify` pass correctly
  refused without evidence, then operator override (`approve:
  qual`, TD-094 independent of worker `qa`) verified the task
  (assurance operator-accepted, disposition override).
- `TestCmdStatusLeavesCursor` PASS against a live echo daemon
  (SKIP without one).

## ACP/MSP verdict (P0-C inspected; CORRECTED 2026-11-03 — native ACP exists)

- Host baseline: Muse Code 1.4.14 (`muse-bin-1.4.14-R7309.1`).
  CORRECTION: the earlier claim "no `muse acp`" is FALSE — native
  ACP exists but is env-gated:
  `MUSE_EXPERIMENTAL_ACP=on muse acp --help` serves ACP v1 over
  stdio (`--provider echo|meta|local`, `--no-session-log`).
  `initialize` against `--provider echo` returns protocolVersion 1
  with agent caps (loadSession false, prompt audio false, image +
  embedded-context true) — no auth needed for echo. Separately,
  `muse serve` is the MSP session host over stdio (70 methods +
  45 notifications in the embedded v1 schema, fingerprint
  `sha256:c94c3d1f…`). The unrelated `acp` Python launcher crashes
  on `--help` (broken typer app, not a Muse extension); `vibe-acp`
  is a dangling symlink; `hermes-acp --check` passes but needs
  provider auth for anything live.
- Live no-spend MSP probe (`muse serve --provider echo`,
  `--no-session-log`): `initialize` → `initialized` →
  `session/start` (UUIDv7 `commandId`, `workspaceRoot`) →
  `session/started` + `turn/start` → `turn/started`,
  `item/started` ×11, `item/delta` (`echo: …`), `item/completed`
  ×12, `turn/foregroundCompleted`, `turn/completed`
  (`terminal: completed`, `durationMs`), `session/statusChanged`,
  `session/tokenUsage`, `session/contextUsage`, `session/closed`.
- Decision (unchanged): PTY stays the authoritative worker session.
  No Metis ACP/MSP client is built — deferred, not a release
  prerequisite. TUI-backed processes expose no structured
  turn/tool/permission events (only PTY bytes); ACP/MSP-owned
  sessions would need an explicitly supervised child bound to a
  worker identity — recorded here as unbuilt, not as present.

## NOT live qualified (blocked, not attempted)

- Managed-PTY muse-code launch beyond echo: one `muse --provider
  echo` worker ran 51s stable under its supervisor with correct
  cwd, then stopped cleanly (isolated state, no prompt, no
  inference). meta/local providers and prompt-bearing turns on PTY
  workers are not qualified (paid quota / operator approval needed).
- Ocliv publication of the Metis MCP and the M2M relay behind
  Ocliv (routes, TLS pinning, public ingress): the composed
  Ocliv-to-Metis MCP path is now qualified in isolation — a real
  `mcp serve-http` Metis was published through Ocliv's
  `upstream-oauth` profile and reached by an authenticated client
  through the relay (discovery resource matched the published route
  URL exactly; `initialize`, `tools/list` (15 tools) and a read-only
  call succeeded; 7 token negatives refused at Metis, the scope
  negative refused without executing, and unpublish tombstoned the
  route). Public Funnel ingress for that same chain and the M2M
  relay behind Ocliv remain unqualified here.
- Twelve-worker fleet (3 per project across Locus/Jeverb/Ocliv/
  Metis) under real workloads, faults, and recovery (audit step 6).
- Cross-machine identity/state and recovery qualification.
- Live provider child/subagent events: wire shape unknown, echo
  provider never emits them (ledger P5-2 BLOCKED) — needs live
  provider qualification, not a guessed parser.
- Any capability requiring paid quota (ledger P6-2 BLOCKED):
  needs explicit authorization first.

## Explicitly out of scope (not reopened without new evidence)

- Repository-wide redecomposition for file size alone
  (`verdict.go` et al. re-evaluated cohesive; ledger 1a2a2ca).
- `t.Parallel` adoption (shared env/state; fixtures unproven).

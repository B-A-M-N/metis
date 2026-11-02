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
- Correlated ops (`ops --req/--task/--attempt/--session/--phase/
  --class/--errors-only/--since`), stable closed error codes
  (`opsKnownClass`), typed-code-before-substring classification.
  Evidence: `TestCliOpsFilters`, `TestOpsDiagTypedCodeBeatsSubstring`
  (P2-b); sink cost ~55us/op, no queue warranted.
- One-command bundle (`diagnose --session [--json]`): identity,
  task+waiting, transitions, leases, resources; redacted.
  Evidence: `TestDiagnose*` (P2-c).
- Test gates + inventory: fast/domain/integration/full,
  `test-inventory.json`, fresh-binary integration gate.
  Evidence: full gate green + race subset (P1-gate).
- Provision replay/crash-safety, intent precedence, atomic
  depend/correct, dispatch admission fencing, stop idempotency.
  Evidence: `TestMCPProvision*`, `TestProvisionReplay*`,
  `TestDispatch*` (ledger P0-C, R-0, D-01, F-07).
- Retention sweeper (`StartRetentionSweeper` in worker-loop),
  pickup/recovery/resource metrics (observe-only). (F-09, P3-a/b/c)

## Live qualified (real backend, $0 echo/local only — no inference spend)

- Single FI-backed turn + native profile serving an echo session
  (P6-1, P6-2): echo provider, local daemon.
- Multi-turn continuity (one native session, 3 turns) + daemon-crash
  resilience: client ops succeed via short-lived hosts against the
  durable store (commit `008ae9f` ledger note).
- `TestCmdStatusLeavesCursor` PASS against a live echo daemon
  (SKIP without one).

## NOT live qualified (blocked, not attempted)

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

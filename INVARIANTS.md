# Metis — Normative Invariants Specification v1.0

Target: `/home/bamn/tui-dash` — Go/Bubble Tea implementation\
Scope: External AI orchestration, multi-agent delegation, live terminal interaction, evidence-backed reviews, corrective workflows, and operational correctness.

These are target invariants, not claims that the current repository satisfies them.

An invariant is a requirement that must hold across every supported execution path, including concurrency, failure, restarts, and adversarial conditions. A passing unit test on one happy path does not establish an invariant.

## Compliance rules

Every invariant receives one of five audit dispositions:

| Disposition  | Meaning                                                                                  |
| ------------ | ---------------------------------------------------------------------------------------- |
| PASS         | Supported by implementation evidence and successful falsification attempts               |
| VIOLATION    | A demonstrated execution path breaks the invariant                                       |
| NOT VERIFIED | Evidence is insufficient, the feature is untested, or it is not yet implemented          |
| CONFLICT     | Two or more requirements cannot simultaneously hold under the current contract           |
| N/A          | The capability is explicitly outside the authorized implementation scope, with rationale |

No implied passes. No treating a missing feature as implemented. No treating a unit test as production qualification.

Priority: P0 = required for safe/correct operation; P1 = required for intended product completeness; P2 = improvement beyond the minimum product.

## I. Product and authority invariants

- TD-001 [P0] — External Orchestrator Authority: Metis MUST execute, observe, and report orchestration operations without autonomously deciding the user's next strategic objective.
- TD-002 [P0] — No Shadow Orchestrator: Internal scheduling logic MUST NOT independently reinterpret or expand an external orchestrator's requested work.
- TD-003 [P0] — Controller Independence: Task execution and monitoring MUST continue independently of whether the Bubble Tea interface is running.
- TD-004 [P0] — UI Non-Authority: The TUI MUST NOT be the authoritative owner of worker lifecycle, task state, verification, or report disposition.
- TD-005 [P0] — No Implicit Dispatch: Opening a dashboard, reading status, listing agents, or retrieving reports MUST NOT start additional work.
- TD-006 [P0] — Explicit Operations: Every state-changing operation MUST correspond to an authorized, identifiable request.
- TD-007 [P0] — Separation of Observation and Control: Permission to observe an agent MUST NOT imply permission to interact with, interrupt, cancel, or redirect it.
- TD-008 [P0] — No Provider Privilege Inference: An AI's ability to request an operation MUST NOT imply that the target provider supports or authorizes it.
- TD-009 [P1] — Harness Agnosticism: The controller MUST NOT require all workers to use the same coding harness, model, or provider.
- TD-010 [P1] — No Fixed Agent Ceiling: The backend MUST NOT impose an arbitrary four-worker limit or tie execution capacity to the number of visible terminal panes.
- TD-011 [P1] — Homogeneous Fleet Support: Multiple instances of the same CLI, including an all-Muse-Code fleet, MUST remain independently addressable.
- TD-012 [P1] — External Reasoning Boundary: Report comparison MUST preserve evidence and attribution; decisions about disputed findings remain with the authorized orchestrator or operator.

## II. Identity and ownership invariants

- TD-013 [P0] — Unique Run Identity: Every orchestration run MUST have a stable identifier distinct from every other run.
- TD-014 [P0] — Unique Task Identity: Each task MUST retain its identity through execution, recovery, and reporting.
- TD-015 [P0] — Unique Attempt Identity: Retries MUST create distinct execution attempts without overwriting the previous attempt's history.
- TD-016 [P0] — Unique Session Identity: Two sessions of the same agent CLI MUST NOT share terminal identity, output, input, or lifecycle state.
- TD-017 [P0] — Stable Worker Identity: Managed workers MUST be associated with verified process identity, including start-time or equivalent reuse protection, rather than PID alone.
- TD-018 [P0] — Host Identity: Remote process identities MUST be scoped to a verified host identity.
- TD-019 [P0] — Terminal Identity: PTY, tmux, and remote terminal endpoints MUST be uniquely associated with their actual sessions.
- TD-020 [P0] — Ownership Verification: A controller MUST NOT terminate, interrupt, resize, or inject input into a worker unless the required control authority has been established.
- TD-021 [P0] — Unowned Process Protection: Discovery of an unmanaged process MUST NOT confer ownership of that process.
- TD-022 [P0] — No Cross-Session Attribution: Events, output, guidance, files, and reports from one agent MUST NOT appear under another agent's identity.
- TD-023 [P0] — No Cross-Provider Attribution: A common provider or model name MUST NOT be used as the unique identity of an agent instance.
- TD-024 [P0] — Stale Identity Rejection: A recycled PID, replaced session, changed host identity, or invalid owner credential MUST invalidate prior control authority.
- TD-025 [P1] — Identity Preservation: UI selection, reconnects, discovery rescans, and pagination MUST preserve selected entities by stable identifier rather than list position.

## III. Task lifecycle and scheduling invariants

- TD-026 [P0] — Explicit Lifecycle: Every task MUST occupy a defined lifecycle state. Unknown or uncertain conditions MUST NOT be coerced into success or failure.
- TD-027 [P0] — Atomic Dispatch Claim: Concurrent schedulers MUST NOT independently claim the same pending attempt for execution.
- TD-028 [P0] — Idempotent Request Handling: Retrying the same state-changing request with the same idempotency identity MUST NOT create duplicate workers or tasks.
- TD-029 [P0] — No Duplicate Launch After Reconnect: A controller reconnect MUST NOT relaunch a worker whose execution outcome is still uncertain.
- TD-030 [P0] — Dependency Enforcement: A task with unsatisfied mandatory dependencies MUST NOT start.
- TD-031 [P0] — Dependency Evidence: A dependency requiring verified completion MUST NOT be satisfied merely because its worker process exited.
- TD-032 [P0] — Bounded Delegation: Recursive and parallel delegation MUST respect explicit depth, worker, and resource limits.
- TD-033 [P0] — Independent Concurrency: A slow or blocked worker MUST NOT serialize unrelated workers unless an explicitly shared resource limit requires it.
- TD-034 [P0] — Capacity Separation: UI pane count, scheduler concurrency, provider quotas, and host capacity MUST be independently represented.
- TD-035 [P0] — No Silent Retry: Retry behavior MUST follow an explicit policy and MUST preserve the original failure or uncertainty.
- TD-036 [P0] — No Unbounded Correction Loop: Corrective task creation MUST respect an explicit stopping condition or iteration budget.
- TD-037 [P0] — Cancellation Propagation: Cancellation of a parent workflow MUST account for active descendants; it MUST NOT falsely claim complete cancellation while owned descendants remain running.
- TD-038 [P0] — Confirmed Termination: A task MUST NOT be marked terminated solely because a termination signal was sent.
- TD-039 [P0] — Timeout Semantics: An observation timeout MUST mean that an outcome was not obtained within the wait period; it MUST NOT automatically mean the worker failed.
- TD-040 [P1] — Parallel Completion Independence: One worker's completion MUST NOT cause unrelated active tasks to be finalized.
- TD-041 [P1] — Run Finalization: A run MUST NOT be finalized successfully while required work, review, or verification remains unresolved.

## IV. Agent adapters and execution-mode invariants

- TD-042 [P0] — Capability Declaration: Every adapter MUST declare the operations its actual implementation supports.
- TD-043 [P0] — No Capability Fabrication: Terminal attachment, persistent interaction, streaming telemetry, or session resumption MUST NOT be advertised unless implemented and qualified.
- TD-044 [P0] — One-Shot Honesty: One-shot execution MUST NOT be presented as a persistent conversational session.
- TD-045 [P0] — Headless Independence: Headless execution MUST NOT imply an absence of monitoring, cancellation, or progress reporting where those operations are supported.
- TD-046 [P0] — Unsupported Operation Rejection: Requests for unsupported capabilities MUST return explicit non-success outcomes rather than pretending the operation was applied.
- TD-047 [P0] — Real Worker Requirement: Production tasks MUST launch the requested real coding agent, not the demonstration worker as an undeclared fallback.
- TD-048 [P0] — Native Exit Attribution: A worker's exit result MUST come from its actual process or authoritative execution protocol, not from the status of an output-forwarding utility.
- TD-049 [P0] — No Hosted Delegation Fiction: Starting a local Muse CLI MUST NOT be described as dispatching to hosted Muse Sparks without an actual authorized provider integration.
- TD-050 [P0] — Execution Root Correctness: Every worker MUST execute against the assigned and authorized workspace, not an unrelated directory that merely contains a reference to it.
- TD-051 [P0] — Worktree Isolation: Independent writable assignments MUST use isolated workspaces or an explicitly authorized shared-workspace policy.
- TD-052 [P0] — No Implicit Provider Fallback: An unavailable provider MUST NOT silently dispatch to a different provider or model.
- TD-053 [P1] — Session Continuation: An adapter may advertise continuation only when it can reliably preserve or restore the relevant worker context.
- TD-054 [P1] — Provider-Specific Translation: Provider-specific command arguments and result schemas MUST be translated by the adapter, not guessed by generic controller logic.

## V. Live terminal invariants

- TD-055 [P0] — Native Terminal Semantics: A managed interactive worker MUST receive a real terminal interface when its CLI requires terminal capabilities.
- TD-056 [P0] — Raw Output Integrity: Terminal output MUST preserve byte ordering and control sequences up to the declared transport limits.
- TD-057 [P0] — No Cross-Terminal Stream Mixing: Terminal bytes from separate workers MUST remain independently sequenced and attributable.
- TD-058 [P0] — Terminal Rendering Correctness: ANSI control sequences, cursor movement, alternate-screen behavior, colors, and clearing operations MUST be interpreted by a terminal-aware renderer.
- TD-059 [P0] — No Snapshot-as-Stream Fiction: Periodic `tmux capture-pane` snapshots MUST NOT be represented as a complete raw terminal event stream.
- TD-060 [P0] — Non-Destructive Observation: Reading a terminal's output MUST NOT consume, modify, or redirect its input.
- TD-061 [P0] — Single Input Writer: No two controllers may concurrently hold effective input authority for the same terminal.
- TD-062 [P0] — Lease Recovery: A disconnected input controller MUST NOT permanently prevent another authorized controller from obtaining input authority.
- TD-063 [P0] — Input Target Binding: Keystrokes MUST be delivered only to the authenticated target session identified by the request.
- TD-064 [P0] — Resize Correctness: Terminal resize operations MUST update the owned PTY's dimensions and notify the worker where supported.
- TD-065 [P0] — Detach Independence: Detaching a viewer MUST NOT terminate or interrupt the worker.
- TD-066 [P0] — Terminal Output Backpressure: A high-output terminal MUST NOT cause unbounded memory growth or block the controller's lifecycle operations.
- TD-067 [P0] — Output Gap Visibility: If output history is discarded, subsequent readers MUST receive an explicit gap indication.
- TD-068 [P0] — Terminal State Isolation: Resizing, scrolling, switching, or selecting one terminal MUST NOT mutate another worker's terminal state.
- TD-069 [P1] — Live Versus Historical: A stored terminal snapshot MUST be explicitly distinguishable from fresh live output.
- TD-070 [P1] — Resume Cursor Integrity: Reconnecting observers MUST continue from the correct sequence cursor or receive an explicit indication that earlier data is unavailable.
- TD-071 [P1] — Quiet Does Not Mean Idle: Absence of recent terminal output MUST NOT establish that an agent is waiting for input, dead, or finished.
- TD-072 [P1] — Tmux Compatibility: Existing tmux sessions MAY be observed through a compatibility adapter without making tmux mandatory for native managed workers.

## VI. Guidance, intervention, and command invariants

- TD-073 [P0] — Guidance State Accuracy: Recorded, queued, delivered, acknowledged, applied, rejected, and failed guidance MUST be distinguishable where supported by the transport.
- TD-074 [P0] — No False Delivery: Writing a guidance file MUST NOT establish that an arbitrary worker actually read it.
- TD-075 [P0] — No False Application: An acknowledgement of receipt MUST NOT establish that the worker followed the instruction.
- TD-076 [P0] — Guidance Attribution: Every guidance operation MUST identify its requesting actor, target session or task, and delivery attempt.
- TD-077 [P0] — Input Versus Guidance: Structured agent guidance MUST NOT be silently converted to raw terminal input.
- TD-078 [P0] — No Competing Input: AI guidance and human terminal typing MUST NOT interleave through concurrent uncontrolled writes.
- TD-079 [P0] — No Historical Intervention: Input or guidance MUST NOT be marked as delivered to a process that is already definitively terminated.
- TD-080 [P0] — Safe Cancellation: Cancellation MUST use an authorized, owned worker termination mechanism.
- TD-081 [P0] — No Success on Signal Send: Sending SIGINT, SIGTERM, or an equivalent cancellation command MUST NOT itself establish the resulting worker state.
- TD-082 [P1] — Corrective Continuity: A corrective task MUST reference the exact preceding task, report, artifact, and intended continuation state.
- TD-083 [P1] — No Lost Guidance: A successfully accepted durable guidance request MUST remain recoverable through controller restart until its final disposition is established.
- TD-084 [P1] — Intervention Auditability: An operator or orchestrator MUST be able to determine who attempted an intervention, when it occurred, whether it was delivered, and its observed outcome.

## VII. Reports, evidence, and verification invariants

- TD-085 [P0] — Independent Verification: A worker's declaration of success MUST NOT be accepted as independent proof that its task was completed correctly.
- TD-086 [P0] — Execution Versus Acceptance: Process exit, task execution, requirement verification, and final approval MUST be distinct concepts.
- TD-087 [P0] — Immutable Acceptance Criteria: The criteria used to accept an active task MUST NOT be silently weakened or replaced after execution begins.
- TD-088 [P0] — Correct Artifact Binding: Verification MUST evaluate the artifact produced by the specified task attempt, not an unrelated or subsequently modified checkout.
- TD-089 [P0] — No Tautological Verification: A test that always succeeds MUST NOT qualify as meaningful proof of a task's intended behavior.
- TD-090 [P0] — No Missing-Evidence Pass: Missing required evidence MUST produce an unresolved or failed qualification outcome, never an inferred PASS.
- TD-091 [P0] — Failure Preservation: An agent's failed attempt MUST remain recorded even when a subsequent attempt succeeds.
- TD-092 [P0] — Report Provenance: Every finding MUST remain attributable to the worker, task, and attempt that produced it.
- TD-093 [P0] — Contradiction Preservation: Conflicting findings from different reviewers MUST remain distinguishable and MUST NOT be merged into an invented consensus.
- TD-094 [P0] — No Self-Approval: An implementation agent MUST NOT satisfy a required independent-review condition merely by reviewing or approving its own output.
- TD-095 [P0] — Review Snapshot Binding: A reviewer MUST evaluate the specified submission snapshot and MUST NOT silently substitute the current mutable repository state.
- TD-096 [P0] — No Truncation Concealment: Truncated logs, diffs, findings, or reports MUST be explicitly marked as incomplete.
- TD-097 [P0] — No Fabricated Tool Events: Terminal text MUST NOT be converted into authoritative tool-use telemetry without a qualified attribution method.
- TD-098 [P1] — Structured Report Contract: Supported report types MUST have validated schemas with explicit required fields and versioning.
- TD-099 [P1] — Report Fallback Honesty: An unstructured log fallback MUST be identified as such and MUST NOT be treated as a complete structured report.
- TD-100 [P1] — Purpose-Correctness: A task MUST NOT be accepted solely because files exist and tests pass when its declared functional requirements remain unverified.
- TD-101 [P1] — Correction Traceability: A correction MUST be traceable to the specific findings and requirements that motivated it.

## VIII. TUI and observability invariants

- TD-102 [P0] — Pure Rendering: Bubble Tea's `View()` MUST render cached state without executing external commands, mutating worker state, or performing blocking I/O.
- TD-103 [P0] — Nonblocking Updates: Slow terminal collection, filesystem operations, or unavailable workers MUST NOT block the TUI event loop.
- TD-104 [P0] — Honest Freshness: Every observed status MUST distinguish current evidence from stale or historical data when freshness affects interpretation.
- TD-105 [P0] — No Global Failure Propagation: A failing collector for one worker MUST NOT erase or invalidate the known state of unrelated workers.
- TD-106 [P0] — Bounded Rendering: Displaying many agents or high-volume output MUST NOT cause unbounded rendering work or memory growth.
- TD-107 [P0] — Unicode Width Correctness: Visible layout MUST use terminal display width rather than UTF-8 byte length.
- TD-108 [P0] — Focus Isolation: Keyboard input MUST apply to the selected control or terminal, never an unrelated hidden pane.
- TD-109 [P0] — Destructive Action Confirmation: Destructive UI operations MUST require the prescribed authorization and confirmation before execution.
- TD-110 [P1] — Stable Selection: Selection MUST remain attached to the same agent or task through list reordering and refreshes.
- TD-111 [P1] — Unlimited Logical List Navigation: Every discovered worker MUST remain accessible through scrolling, filtering, or pagination regardless of viewport size.
- TD-112 [P1] — Terminal Maximization: An operator MUST be able to inspect an individual terminal at sufficient size without requiring every worker to be shown simultaneously.
- TD-113 [P1] — Correct Display Modes: Overview, live terminal, reports, and control views MUST reflect the same authoritative controller state.
- TD-114 [P1] — No Quota-Burning Observation: Passive dashboard refreshes MUST NOT issue model inference requests merely to display progress.

## IX. Persistence, crash recovery, and remote invariants

- TD-115 [P0] — Durable Authoritative State: Task, attempt, session, event, and outcome identities MUST survive controller restarts.
- TD-116 [P0] — Crash-Safe State Transitions: Partial process launch, persistence failure, or controller crash MUST NOT silently convert uncertain execution into successful completion.
- TD-117 [P0] — Single Scheduling Authority: Multiple controllers or monitors MUST NOT concurrently perform conflicting lifecycle transitions for the same execution attempt.
- TD-118 [P0] — Reconciliation Before Retry: After uncertain process or network failure, the controller MUST attempt to reconcile the original execution before starting a replacement.
- TD-119 [P0] — Disconnect Is Not Termination: Network disconnection MUST NOT establish that a remote worker has stopped.
- TD-120 [P0] — No Remote-to-Local Substitution: An unreachable remote target MUST NOT silently execute the task locally.
- TD-121 [P0] — Remote Control Authentication: Remote session operations MUST verify the intended host and requested control authority.
- TD-122 [P0] — Event Cursor Integrity: Event pagination MUST NOT advance past actionable events that were not delivered to the requesting reader.
- TD-123 [P0] — Replay Without Relaunch: Replaying historical activity MUST NOT trigger new agent execution.
- TD-124 [P0] — Shutdown Independence: Closing the UI or detaching the orchestrator MUST NOT terminate surviving workers unless explicitly requested.
- TD-125 [P1] — Bounded Recovery: Recovery MUST converge to an established state or remain explicitly unresolved rather than retrying indefinitely.
- TD-126 [P1] — Remote Capability Qualification: Remote execution MUST NOT be advertised as working solely because SSH connectivity succeeds.

## X. Security and resource invariants

- TD-127 [P0] — Least-Privilege Control: Read-only observers MUST NOT receive worker input, cancellation, or execution privileges.
- TD-128 [P0] — State Confidentiality: Session credentials, control tokens, private logs, and sensitive work artifacts MUST be protected by appropriate local permissions and transport controls.
- TD-129 [P0] — No Secret Leakage: Read-only status, reports, and telemetry MUST NOT disclose control credentials or unrelated provider secrets.
- TD-130 [P0] — Filesystem Containment: Agent workspace paths and generated artifact paths MUST NOT escape their explicitly authorized filesystem boundaries through traversal or symlinks.
- TD-131 [P0] — No Command Injection: Task data, agent names, session identifiers, and guidance text MUST NOT inadvertently become executable shell syntax.
- TD-132 [P0] — Explicit Execution Authority: Executing arbitrary worker-supplied commands or verifier commands MUST require the configured execution authorization and isolation policy.
- TD-133 [P0] — Provider Credential Isolation: One worker MUST NOT gain access to another worker's private provider credentials merely because they share a controller.
- TD-134 [P0] — Quota and Budget Enforcement: Worker spawning and model-using operations MUST respect configured concurrency, expenditure, and retry limits.
- TD-135 [P0] — No Unbounded Polling: Monitoring loops MUST use bounded polling or event subscriptions and MUST NOT generate uncontrolled API traffic.
- TD-136 [P0] — Bounded Retention: Terminal logs, event histories, cached snapshots, and temporary artifacts MUST have explicit retention and resource policies without silently discarding required audit evidence.
- TD-137 [P0] — Atomic Persistence: Concurrent registry updates MUST NOT lose committed session records.
- TD-138 [P0] — Explicit Trust Boundary: Worker-produced reports, terminal output, and repository files MUST be treated as untrusted content, not as instructions to the controller.
- TD-139 [P1] — Resource Failure Isolation: Exhaustion of one provider, host, or terminal stream MUST NOT consume unrelated providers' reserved capacity without an explicit shared policy.

## XI. Authorized exceptions

An exception must be a documented, testable branch of the contract. It cannot simply disable an invariant.

| Exception                              | Permitted behavior                                                           | What remains prohibited                                           |
| -------------------------------------- | ---------------------------------------------------------------------------- | ----------------------------------------------------------------- |
| EX-01 — One-shot worker                | No live conversational continuation; use a linked follow-up                  | Pretending guidance was applied                                   |
| EX-02 — Unmanaged terminal             | Read-only observation when supported                                         | Input injection or termination without control authority          |
| EX-03 — Unsupported provider operation | Return unsupported/blocked                                                   | Silent replacement or fabricated success                          |
| EX-04 — Network partition              | Preserve last-known state and report disconnection                           | Treating the worker as definitively dead                          |
| EX-05 — Output buffer overflow         | Drop history according to retention policy and identify gaps                 | Silent loss or invented continuity                                |
| EX-06 — Explicit shared workspace      | Controlled shared-workspace execution under an authorized concurrency policy | Unrecorded concurrent modifications                               |
| EX-07 — Operator-forced shutdown       | Attempt authorized termination and retain its observed result                | Claim termination before it is established                        |
| EX-08 — Missing independent verifier   | Return not verified or inconclusive                                          | Approving from agent self-report                                  |
| EX-09 — Tmux compatibility             | Read-only capture of an external pane                                        | Claiming full native PTY-stream correctness from snapshots        |
| EX-10 — Resource exhaustion            | Queue, reject, or block according to policy                                  | Infinite retries, concealed fallback, uncontrolled provider usage |

Exception precedence: Security and ownership guarantees outrank operational convenience. An unavailable tool does not justify bypassing access controls. An explicit override may authorize a supported operation, but cannot make a false claim of verification true.

## XII. Anti-specification — prohibited implementations

These are architecture-level prohibitions intended to prevent the implementation from satisfying superficial tests while missing the actual product goal.

| ID     | Metis MUST NOT                                                                              |
| ------ | ---------------------------------------------------------------------------------------------- |
| AS-001 | Become another autonomous reasoning/orchestration framework                                    |
| AS-002 | Depend on tmux as the mandatory execution substrate for all managed interactive workers        |
| AS-003 | Treat a captured terminal screen as an authoritative tool-event stream                         |
| AS-004 | Hardcode a maximum of four workers or four Muse Code instances                                 |
| AS-005 | Spawn workers merely because the operator opened a screen                                      |
| AS-006 | Poll an AI model to determine what can be obtained from process and controller state           |
| AS-007 | Accept report creation as proof of successful task completion                                  |
| AS-008 | Assign the same writable checkout to independent agents without a declared sharing policy      |
| AS-009 | Treat queued guidance as delivered or applied                                                  |
| AS-010 | Kill a session solely because its tmux name matches a registry entry                           |
| AS-011 | Hide conflicting agent findings behind synthesized consensus                                   |
| AS-012 | Retry uncertain remote executions without reconciling the prior attempt                        |
| AS-013 | Hide lost events, stream gaps, or failed collectors                                            |
| AS-014 | Create new provider integrations by claiming local CLI access proves hosted API access         |
| AS-015 | Require an MCP server when an authorized CLI already supports the necessary operations         |
| AS-016 | Introduce distributed infrastructure without a demonstrated requirement                        |
| AS-017 | Replace the existing Go TUI with a different implementation merely to acquire missing features |
| AS-018 | Claim production qualification from a mock worker, a unit test, or a simulated terminal alone  |
| AS-019 | Run unbounded corrective-agent loops                                                           |
| AS-020 | Make critical lifecycle or report state depend on the TUI process remaining open               |

## XIII. Composed behavioral correctness tests

Individual invariants are necessary but insufficient. The test suite must also target combinations of invariants that are likely to fail together.

| Test  | Scenario                                                                                   | Required outcome                                                                                      |
| ----- | ------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------- |
| CB-01 | Two orchestrators submit the same task concurrently                                        | One effective dispatch; no duplicate workers                                                          |
| CB-02 | Six Muse Code instances execute simultaneously                                             | Six independent identities, output streams, and task states                                           |
| CB-03 | User opens a live terminal while the orchestrator is monitoring                            | Observation continues; input ownership remains unambiguous                                            |
| CB-04 | TUI closes during an active multi-agent workflow                                           | Workers and authoritative orchestration state survive                                                 |
| CB-05 | External orchestrator disconnects while an agent completes                                 | Report and event are recoverable without relaunch                                                     |
| CB-06 | Worker reports success but acceptance tests fail                                           | Task remains unverified or failed; original claim retained                                            |
| CB-07 | Two auditors produce contradictory findings                                                | Both findings remain independently attributed                                                         |
| CB-08 | A reviewer evaluates an implementation after another worker modifies the source repository | Review remains bound to the intended snapshot                                                         |
| CB-09 | Controller loses contact with a remote worker during cancellation                          | Outcome remains unresolved until termination is established                                           |
| CB-10 | One worker produces continuous full-screen animation while five others run                 | UI remains responsive, streams stay isolated, critical events aren't starved                          |
| CB-11 | Terminal input controller disconnects while holding a lease                                | Authorized recovery is possible without simultaneous writers                                          |
| CB-12 | Agent retry occurs after a controller crash at the launch/persist boundary                 | No duplicate execution is silently treated as legitimate                                              |
| CB-13 | Guidance is submitted to a one-shot CLI that doesn't consume live messages                 | Operation is not falsely marked delivered or applied                                                  |
| CB-14 | Original implementation agent receives a correction after exiting                          | New linked attempt uses the appropriate prior artifacts                                               |
| CB-15 | Remote SSH connection succeeds but no worker runtime exists                                | Remote execution is blocked, not considered qualified                                                 |
| CB-16 | Eight workers exceed the terminal's visible list height                                    | All eight remain navigable and addressable                                                            |
| CB-17 | Worker changes files outside its assigned workspace                                        | Violation detected and not accepted as authorized output                                              |
| CB-18 | Operator attempts to stop an unowned tmux session                                          | Termination refused; session remains alive                                                            |
| CB-19 | Process PID is recycled between discovery and control                                      | Stale control authority rejected                                                                      |
| CB-20 | Model quota is exhausted while other providers remain healthy                              | Affected worker is blocked or deferred; unrelated worker execution continues within configured limits |

These tests should exercise actual component composition, not mocks that bypass the relevant boundaries.

## XIV. Initial code-grounded findings

From the Go source inspected in `/home/bamn/tui-dash`, these invariants already warrant specific attention.

| Invariant      | Source                                            | Initial assessment                                                                        |
| -------------- | ------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| TD-020, TD-021 | `session.go` → `stopSession()`                    | VIOLATION — termination by tmux name without authenticated ownership                      |
| TD-063         | `session.go` → `injectGuidance()`                 | VIOLATION — direct input lacks verified session-control ownership                         |
| TD-074, TD-075 | `injectGuidance()`                                | NOT VERIFIED — generic worker guidance consumption/application cannot be established      |
| TD-102, TD-103 | `tui.go` → `View()`, `refresh()`                  | VIOLATION — synchronous tmux operations occur on the UI execution path                    |
| TD-107         | `tui.go` → `pad()`                                | VIOLATION — uses byte length rather than terminal display width                           |
| TD-115, TD-137 | `session.go` → `loadRegistry()`, `saveRegistry()` | NOT VERIFIED — concurrent, crash-safe registry behavior has not been established          |
| TD-058, TD-059 | `tui.go` → `captureOutput()`                      | NOT VERIFIED — native terminal emulation is absent; snapshot-based capture is in use      |
| TD-085–TD-101  | Go controller/report functionality                | NOT VERIFIED — the target Go orchestration and verification system is not yet implemented |

These are preliminary source-level dispositions, not a completed independent falsification audit.

## XV. Required audit output

For each of the 139 positive invariants, 20 anti-specifications, and 20 composed tests, the implementation audit should record:

```

id: TD-020
priority: P0
requirement: "Control requires verified ownership"
disposition: VIOLATION

evidence:
  source:
    - "session.go: stopSession"
  reproduction: "Unowned session termination scenario"
  observed_result: "Termination not ownership-gated"

exception:
  applicable: false
  reason: null

remediation:
  root_cause: "Session name used as control authority"
  required_change: "Verified ownership identity before kill"

qualification:
  positive_test: "Owned session can be stopped"
  negative_test: "Unowned session cannot be stopped"
  regression_test: "PID/session reuse cannot bypass ownership"
  cross_invariants:
    - TD-017
    - TD-019
    - TD-021
    - TD-024
    - CB-18
    - CB-19

audit_record:
  code_revision: "record actual revision/digest"
  tested_at: null
  evidence_location: null

```

The audit should first identify conflicting requirements, then group violations by shared root cause, implement bounded corrections, and requalify neighboring invariants. A fix that repairs one invariant while breaking another is not a successful remediation.

## Definition of done

Metis reaches its intended baseline when an external AI can delegate to multiple real coding agents—including several simultaneous Muse Code instances—receive reports, cross-reference those reports, assign corrective work, verify outcomes, and continue after disconnection, while the human can observe or interact with any eligible live session.

The system must preserve authority, identity, evidence, and uncertainty throughout that workflow.

That is the governing contract: 139 positive invariants, 20 anti-specifications, 10 conditional exception classes, and 20 composed behavioral tests. No invariant should be declared PASS without evidence appropriate to its claim.&#x20;

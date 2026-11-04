# Metis

Metis is a terminal-based multi-agent control plane: it manages real
agent sessions (tmux, managed PTY, and native Muse sessions) behind one
dashboard, and exposes a control surface to authenticated AI
controllers through the Model Context Protocol (MCP). A controller —
for example ChatGPT or Meta.ai acting through the Ocliv MCP bridge —
can discover tools, provision workers, deliver input and steering,
observe execution, and stop sessions. Task-ledger delegation
(`metis_delegate`) is local-only by design: it is available over stdio
and the CLI, never over the remote HTTP transport. Every action lands
in a durable, attributable orchestration-event ledger that the TUI
renders verbatim. Metis never reasons and never fabricates activity:
it runs what is committed and reports what it observed.

Metis is the orchestration and observability layer for the hackathon
architecture: Ocliv provides the authenticated external ingress,
Metis provides sessions, dispatch, receipts, and the controller's
view of what actually happened.

## What it actually does

- **Sessions, one identity each.** Provision, list, snapshot, message,
  resize, and stop interactive agent sessions across three backends:
  tmux (external attach with F5), managed PTY (screen-rendered log,
  lease-fenced input), and `muse-native` (MSP turns on a bound native
  Muse session, no child process). A session name is an identity; the
  harness name never is.
- **Task ledger and dispatch.** Tasks are created, dispatched to one
  session, claimed exactly once (attempt-fenced), executed by a
  managed worker loop, reported, independently verified against
  evidence, and corrected or followed up. Readiness (`unified_lifecycle`)
  and task occupancy are separate projections — absence of work never
  implies interactive readiness.
- **Controller surface.** MCP over stdio (`metis mcp serve`) and
  authenticated HTTP (JWT, principal-scoped reads, fleet-wide reads
  only with the explicit `metis:fleet` grant). The TUI's pinned
  `CONTROLLER` tab shows verified controller presence (connection,
  lease, operation, progress, blocking as separate facts), execution
  progression (dispatched → working → report_ready → verified/failed),
  and the durable events behind both.
- **Visibility without invention.** `delegation.requested/dispatched`,
  `worker.claimed`, `worker.reported`, `terminal.input/resized`,
  `transport.disconnected`, and verification events are emitted only
  after the underlying fact is durable. Dispatched is assignment, not
  execution; a presence lease alone never implies active orchestration.

## What this is, and is not

- **It is:** a session manager for real Muse Code terminals, with
  separate session identity, provider/model metadata, prompt delivery,
  external tmux attach, and a provider-agnostic controller surface.
- **It is not:** a replacement for Muse Code or a mirror of any
  provider's web conversation. The controller tab shows Metis activity;
  it does not invent a chat transcript.

## Architecture overview

```
Controller (ChatGPT / Meta.ai / Muse …)
    │  MCP stdio or JWT-authenticated HTTP (via Ocliv for external)
    ▼
Metis transport ──── validate + authorize + delegate (never mutate domain state)
    ▼
Domain services ──── tasks/dispatch · sessions · terminal · placement ·
                     muse · assurance · workspace · fleet · events · storage
    ▼
SQLite ledger (tasks, attempts, receipts, orchestration_events, presence)
    ▼
TUI (Bubble Tea) ─── renders read-model projections only; PTY output via
                     internal/termview; Muse wire via internal/musewire
```

Domain ownership is enforced by package/file boundaries (see
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) and `AGENTS.md`): every
authoritative state transition has exactly one owner, transports never
mutate domain state directly, and `PrincipalID` always comes from the
transport context, never from a caller-supplied actor string.

## Key capabilities (verified, not assumed)

See [docs/CAPABILITIES.md](docs/CAPABILITIES.md) for the evidence
matrix. In short, locally qualified with passing tests and real-process
E2E: unified lifecycle/occupancy projection; managed-PTY terminal
(screen model, resize forwarding, extended keyboard); harness adapters
reconciled against installed CLIs (muse, codex, claude, hermes,
muse-native); durable orchestration events with principal-scoped remote
reads; X-01 multi-controller presence (per-session transport registry —
one controller's disconnect never marks a peer disconnected); worker
claim evidence and execution progression in the CONTROLLER tab.

Not qualified: live-provider orchestration (ChatGPT/Meta.ai through
Ocliv end-to-end), remote task-ledger delegation (local-only by design),
multi-host TUI aggregation, paid-inference paths. See
[docs/LIMITATIONS.md](docs/LIMITATIONS.md).

## Requirements

- Linux (developed and qualified on pop-os / Ubuntu 22.04-class hosts)
- Go 1.25+ (see `go.mod`), `tmux` for tmux-backed sessions, `muse` on
  PATH for `metis launch`
- A terminal at least 80x24 for the dashboard
- Optional: harness CLIs you intend to run (codex, claude, hermes)

## Quick start

From a clean checkout:

```bash
make build          # builds ./metis and the ./tui-dash compatibility symlink
./metis doctor      # read-only health snapshot (state, adapters, sessions)
./metis sessions    # lists registered sessions (start empty, that's fine)
./metis watch       # opens the session dashboard
```

Headless zero-cost smoke (no inference): this creates and dispatches
a task and shows the claim/status path. It illustrates assignment,
not completion — the `sleep 900` worker does not write the required
file, so the task stays unreported until a real worker executes it:

```bash
./metis agent-start --name muse-01 --workdir /work/a \
  --cmd "sleep 900" --harness muse
./metis task-create --run rel-1 --agent muse-01 \
  --objective "Write DONE into required.txt" \
  --acceptance "required.txt exists containing DONE"
./metis task-dispatch --task <id> --session muse-01
./metis agent-work --session muse-01 --once
./metis task-status <id>   # dispatched/working; unreported until a real worker runs it
```

## Installation

```bash
cd <checkout>       # this repository's root
make build          # or ./build.sh; produces ./metis (git-ignored)
```

Plain `go build` fails on some hosts ("error obtaining VCS status:
exit status 128") because Go stamps VCS info and git reports the folder
is not a repository; use `make build` (which passes `-buildvcs=false`).

Defaults for `up`: session `demo`, workdir `~/metis-work` (reuses
`~/tui-dash-work` if present), built-in demo worker, 120 steps.

## Configuration

Canonical reference. State root (`METIS_STATE`, legacy `TUI_DASH_STATE`)
defaults to `~/.local/share/metis` (or reuses `~/.local/share/tui-dash`
when present). Session names registered before the rename keep the old
`tui-` tmux prefix; new sessions use `metis-`.

| Variable | Purpose | Default |
| --- | --- | --- |
| `METIS_STATE` | State root (SQLite, registry, logs) | `~/.local/share/metis` |
| `TUI_DASH_STATE` | Legacy alias of `METIS_STATE` | — |
| `METIS_MCP_INGRESS` | MCP ingress mode (`local`/`remote`) | fail-closed; `doctor` diagnoses |
| `METIS_MCP_REMOTE_CAPABILITIES` | Remote capability set (e.g. `admin`) | none granted |
| `METIS_MCP_WRITE_TOKEN` | Write token for stdio / hosted-stdio ingress. HTTP MCP uses JWT bearer authentication instead (the `auth` tool argument is stripped remotely) | unset |
| `METIS_MCP_ISSUER` | JWT issuer accepted by HTTP MCP | — |
| `METIS_MCP_JWKS_URL` / `METIS_MCP_JWKS_FILE` | JWT verification keys | — |
| `METIS_MCP_RESOURCE` / `METIS_MCP_RESOURCE_FILE` | Expected MCP resource | — |
| `METIS_MCP_ALLOWED_ORIGINS` | Allowed CORS origins for HTTP MCP | — |
| `METIS_MCP_ALLOWED_SUBJECTS` | Allowed JWT subjects | — |
| `METIS_OCLIV_BIN` | Ocliv binary for the loopback expose leg (tests skip-gated on it) | absent → leg unavailable |

Remote reads are principal-scoped by default; fleet-wide observation
requires the explicit `metis:fleet` JWT scope. One scope never implies
another.

## Usage

Real entry points (see [docs/USAGE.md](docs/USAGE.md) for workflows):

```bash
./metis watch                                   # dashboard
./metis up --session mywork --workdir ~/mywork --cmd "hermes chat"
./metis launch --name locus-01 --workdir ~/Locus --provider meta --model muse-spark-1.3
./metis send --name mywork --text "pause new steps and check step-003"
./metis agent-snapshot --name mywork            # headless check
./metis mcp serve                               # MCP stdio control plane
```

Project-root registration is CLI-only by design (a deliberate security
boundary): register once with `./metis project-root-register --root
<dir>`, then `launch`/`provision` under it.

Session view and keys:

- **Session tabs:** left; labels show project and instance number,
  while the session ID remains the control identity.
- **Text snapshot:** selected pane, sanitized and read-only as a screen
  preview. It is not a full terminal emulator. Managed PTY panes render
  the log through a screen model (CR/erase overwrites applied); styles
  and alt-screen apps are not emulated — full terminal for tmux workers
  is F5 native attach, for PTY workers use `pty-log` / MCP terminal
  output. Dashboard resize forwards pane geometry to the selected PTY
  worker (lease-fenced).
- **Full terminal:** `F5` suspends Metis and attaches the normal
  terminal to that exact owned tmux session. Detaching returns to Metis.
- **Info drawer:** `F6` shows provider/model, profile, working
  directory, process state, observer/resource snapshot, and recent
  session-scoped operation errors.
- **Prompt bar:** `i`, type, Enter. Submission captures the selected
  session ID and uses the input lease; `d` switches to durable guidance-file
  delivery. On the controller tab the prompt is disabled; use MCP tools
  to target a worker explicitly.
- **Controller tab:** provider-agnostic controller slot and activity
  feed (verified controller identities, targeted workers,
  dispositions). The feed is Metis activity, not a web conversation.

Keys: `Up/Down` or `j/k` select, `i` prompt, `Esc` back to nav,
`Enter` limited keyboard attach, `F5` full terminal attach, `F6` info
drawer, `d` toggle prompt delivery, `PgUp/PgDn` scroll snapshot, `q` quit.

## Repository structure

Single Go module `metis` (package `main`) plus three `internal/`
packages. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

- `main.go`, `cli_*.go` — CLI dispatch and commands
- `mcp_*.go` — MCP transport (stdio + HTTP), read/write tool surface
- `task_*.go`, `dispatch_*.go`, `worker_*.go` — ledger, dispatch, claim, execution
- `session_*.go`, `pty_*.go`, `terminal*.go` — session backends and terminal delivery
- `muse_*.go`, `internal/musewire/` — Muse orchestration and wire framing
- `tui*.go`, `internal/tui/`, `internal/termview/` — dashboard and pure terminal math
- `orchestration_events.go`, `muse_presence.go` — durable events and presence facts
- `docs/` — architecture, onboarding, usage, capabilities, limitations
- `LEDGER.md` — append-only historical findings and evidence
- `AGENTS.md`, `TUI_DOX.md`, `INVARIANTS.md` — contributor contracts

## Testing and verification

```bash
make test-quick     # fast loop (~seconds)
make test-domain AREA=<name>   # one domain area
make test-fast      # broad smoke
make test-integration          # after workflow changes
make test-full      # full suite + race subset (release gate)
go vet ./...
```

`TMPDIR=/tmp` is required on hosts where the default scratch path
breaks unix socket length for the test harness. Test count is tracked
in `test-inventory.json` (`make test-inventory-check`). A known
intermittent failure (`TestSameDirTwoEchoWorkersIsolatedSteering`
supervisor-stop timeout under load, Q-18) is DEFERRED and recorded in
`AGENTS.md` — it is not hidden and not weakened by reruns. Details:
[docs/TESTING.md](docs/TESTING.md).

## Current project status

Qualified local release candidate (frozen, clean tree; local tag
`metis-rc-ce488b0`, artifact under `/home/bamn/releases/` on the
development host). Locally staged and test-qualified; **production
not yet qualified**, deployment pending separate authorization. The
material remaining proof is a genuine external-controller
orchestration sequence (ChatGPT/Meta.ai → Ocliv → Metis MCP) in
isolated staging — not another general development pass.

## Known limitations

- Remote task-ledger delegation is intentionally local-only.
- No multi-host TUI aggregation; cross-machine presence is out of scope.
- Live-provider and paid-inference paths are unqualified.
- Q-18 (above) remains an unresolved intermittent reliability risk.
- The in-TUI preview is a text screen model, not a full terminal
  emulator (styles/alt-screen not emulated).

Full detail: [docs/LIMITATIONS.md](docs/LIMITATIONS.md).

## Contributing

Read `AGENTS.md` first (gates, audit dispositions, domain ownership),
then [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) for the local loop. One
writer per checkout; failing-first regression tests for every fix; no
`t.Parallel`; slow integration tests carry `testing.Short`.

## Licensing and security

No `LICENSE` file ships with this repository yet — that is an explicit
owner decision, not an oversight to be filled by contributors. For
security reports, follow the vulnerability-disclosure practice of the
umbrella hackathon project; do not open public issues containing
exploit detail. Authenticated MCP boundaries (ingress modes,
principal-scoped reads, ownership fences) are documented in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#security-boundaries);
treat changes there as security-sensitive.

## Documentation map

- [Architecture](docs/ARCHITECTURE.md) — components, boundaries, execution flow
- [Getting started](docs/GETTING_STARTED.md) — clean-checkout onboarding
- [Usage](docs/USAGE.md) — workflows, CLI and MCP examples
- [Development](docs/DEVELOPMENT.md) — local loop and debugging
- [Capabilities](docs/CAPABILITIES.md) — implemented vs qualified evidence matrix
- [Testing](docs/TESTING.md) — gates and qualification methodology
- [Limitations](docs/LIMITATIONS.md) — unsupported scenarios and constraints
- [Troubleshooting](docs/TROUBLESHOOTING.md) — known failure modes and remedies
- [Concurrency](docs/CONCURRENCY.md), [Ownership](docs/OWNERSHIP.md), [Debugging](docs/DEBUGGING.md)

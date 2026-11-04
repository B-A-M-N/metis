# Usage

All commands are `./metis <command>` after `make build`. Canonical
names are dashed (`task-create`, `agent-work`); `--help` on any
command lists its flags.

## Sessions

```bash
./metis up --session mywork --workdir ~/mywork --cmd "hermes chat"
./metis launch --name locus-01 --workdir ~/Locus --provider meta --model muse-spark-1.3
./metis sessions
./metis agent-snapshot --name mywork   # headless check
./metis send --name mywork --text "pause new steps and check step-003"
./metis agent-stop --name mywork
./metis pty-start / pty-log / pty-input / pty-resize / pty-status / pty-stop
```

Project-root registration is CLI-only (security boundary):

```bash
./metis project-root-register --root ~/Locus
```

## Tasks and verification

```bash
./metis task-create --run rel-1 --agent muse-01 --objective "..." --acceptance "..."
./metis task-dispatch --task <id> --session muse-01
./metis agent-work --session muse-01 --once     # managed loop, exactly-once claim
./metis task-status <id>                         # read-only; never launches or judges
./metis task-reconcile --task <id>               # WAIT / INVESTIGATE / resolved
./metis report-artifact --task <id> --path /work/a/required.txt
./metis task-verdict --task <id> --criterion C1 --verdict PASS --reason "..." \
  --evidence /work/a/required.txt --reviewer reviewer-01
./metis task-verify --task <id> --pass --verifier reviewer-01
./metis task-followup --task <id> --objective "fix ..."
```

Provenance rules: `--reporter/--reviewer/--verifier/--approve` are
asserted names, not authentication; a worker-loop result is filed as
`runner-observation`, never a worker claim or verification; exit code 0
proves the process ended, not that the requirement is met.

## MCP control plane

```bash
./metis mcp serve    # stdio: one session per transport, principal-scoped attribution
```

Tools include `metis_status`, `metis_agents`, `metis_events`,
`metis_tasks`, `metis_task_status`, `metis_report`, `metis_reconcile`,
`metis_projects`, `metis_provision`, `metis_terminal_input/resize`,
`metis_session_stop`, and delegated work via `metis_delegate`
(local-only). Remote reads are principal-scoped; fleet-wide reads
require the `metis:fleet` JWT scope. Event reads are bounded
(default 100, clamp 500, 10-page scan budget with continuation
cursors).

## Diagnostics

```bash
./metis doctor       # state, adapters, sessions, ingress diagnosis
./metis diagnose --session <name> [--json]
./metis ops [--req ...] [--errors-only]   # bounded JSON sink of ops events
```

## Dashboard keys

`Up/Down` or `j/k` select · `i` prompt · `Esc` nav · `Enter` limited
attach · `F5` full terminal attach · `F6` info drawer · `d` guidance
delivery toggle · `PgUp/PgDn` scroll · `q` quit.

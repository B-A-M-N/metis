# Metis

Metis is a local control plane and Bubble Tea dashboard for coding
agents. It coordinates work, records evidence, and gives operators a
shared view across tmux, PTY, and native Muse sessions. The command is
`metis`; `tui-dash` remains available as a compatibility name.
The `tui-dash` command and existing state/work directories remain
compatible. New sessions use the `metis-` tmux prefix; registered
sessions with the old `tui-` prefix remain usable. `METIS_STATE` is
preferred; `TUI_DASH_STATE` remains accepted.

## What this is, and is not

- **It is:** an operator dashboard and CLI for tmux, PTY, and native
  Muse workers, with task dispatch, evidence, and session guidance.
- **It is not:** a view into Muse's own chat work. Muse in chat has
  no attachable TUI. Metis shows workers and durable orchestration
  records; it does not mirror the chat conversation.

## Build and run

```bash
cd tui-dash
./build.sh
# or: make build
# Builds ./metis and the ./tui-dash compatibility command.
# -buildvcs=false is required on pop-os: plain `go build` fails
# there with "error obtaining VCS status: exit status 128",
# because Go tries to stamp VCS info and git reports this
# folder is not a git repository (exit 128). Disabling the
# stamp changes nothing else about the binary.

# The main entry point: start a demo worker if needed, then open the TUI.
./metis up

# With your own session / workdir / worker:
./metis up --session mywork --workdir ~/mywork --cmd "hermes chat"

# TUI only, no auto-start:
./metis watch --session mywork

# Attach directly to the raw agent pane (no dashboard):
tmux attach -t metis-mywork

# Existing sessions created before the rename still use tui-<name>.
# Their registered target is retained so attach and control still work.

# From another terminal, or from Muse, message the worker:
./metis send --name mywork --text "pause new steps and check step-003"

# Headless check (what Muse uses to verify without a terminal):
./metis agent snapshot --name mywork
./metis sessions
./metis agent stop --name mywork
```

Defaults for `up`: session `demo`, workdir `~/metis-work`
(reuses `~/tui-dash-work` if it already exists),
worker is this binary's built-in demo (`metis demo`), 120 steps, 1
second per step.

`METIS_STATE` selects the state root. Without it, Metis reuses
`~/.local/share/tui-dash` when present and uses
`~/.local/share/metis` for a fresh install.

Requirements: Go 1.23+, `tmux`, `git` (for the git pane; without a
repo the pane lists files instead), a terminal at least 80x24.
Go was installed on the build machine at `~/workspace/tools/go`
because it was not preinstalled there; on pop-os use the Go you
already have. No other runtime is needed once the binary is built.

## Panes and keys

- **Sessions:** left, `*` = running, `>` = selected.
- **Live:** right top, `tmux capture-pane` for the selected session.
- **Git:** right middle, `git status --short` + `git diff --stat`.
- **Guidance input:** bottom. Default is file mode: guidance is
  appended to `<workdir>/guidance.md` and to `events.jsonl`, and the
  worker polls that file, so your text never races its typing.
  Press `d` to toggle direct mode, which also sends the text to
  the tmux pane with `send-keys`; that can collide with agent
  input, so it is off by default.

Keys: `Up/Down` or `j/k` select, `i` or `Tab` input, `Esc` back to
nav, `Enter` send, `g` path-traversal panel, `d` direct toggle,
`r` tail, `PgUp/PgDn` scroll live, `q` quit.

## Limits, verified not assumed

- A worker that only runs once and exits (`hermes chat --oneshot`
  style) shows process output, not a live tool-call trace. The
  fleet trace endpoint is process-level for the same reason:
  `--oneshot` exposes no per-tool-call stream. A live view needs
  an interactive/streaming worker.
- This dashboard is for a direct host session, not for driving a
  fleet node through its gateway or SSH, per the fleet transport
  rule.

## Verification

```bash
go vet ./...
make build
go test ./...
```

`go test` includes unit tests (validation, inject/snapshot,
demo guidance ack) and a tmux integration test that is skipped
unless `./metis` is built in this folder.
The integration test starts the demo in tmux, checks snapshot live
output, injects guidance, waits for `GUIDANCE RECEIVED`, and checks git
status.

TUI rendering was also checked by running `metis up` inside a
tmux session and capturing its pane, and by typing guidance in
the TUI and watching the demo acknowledge it.

## Files

- `main.go` — CLI, including `up`.
- `tui.go` — Bubble Tea model and view.
- `session.go` — tmux registry, git, guidance, snapshot.
- `demo.go` — simulated worker, not a model.
- `main_test.go` — unit + tmux integration tests.
- `go.mod`, `go.sum` — module and pinned dependencies.
- `controller.go` — task ledger, dispatch, pickup, reports,
  verification, corrections, retries.
- `worker_protocol.go` — versioned task envelope.
- `worker_run.go` — managed worker loop (mechanical execution).
- `verdict.go` — criterion-specific external verification.
- `reconcile.go` — read-only attempt/recovery probe.
- `reports.go` — artifacts and evidence cross-references.
- `agents.go` — harness registry and managed invocations.

## Closed-loop orchestration (external orchestrator pattern)

Muse (or another orchestrator) delegates; Metis executes,
records, and reports. The loop never reasons — it only runs what
is committed and files what it observes.

```bash
# 1. Hold the session open (model starts only after dispatch).
./metis agent start --name muse-01 --workdir /work/a \
  --cmd "sleep 900" --harness muse

# 2. Orchestrator creates and dispatches.
./metis task create --run rel-1 --agent muse-01 \
  --objective "Write DONE into required.txt" \
  --acceptance "required.txt exists containing DONE"
./metis task dispatch --task <id> --session muse-01

# 3. Managed worker consumes the committed attempt exactly once,
#    runs `muse exec` as a child, files output + exit receipt.
./metis agent work --session muse-01 --once

# 4. Where things stand (read-only; never launches or judges).
./metis task reconcile --task <id>

# 5. Independent reviewer judges each criterion against evidence.
./metis report attach --task <id> --path /work/a/required.txt
./metis task verdict --task <id> --criterion C1 --verdict PASS \
  --reason "reproduced from the filed artifact" \
  --evidence /work/a/required.txt --reviewer reviewer-01

# 6. Verify (needs evidence integrity AND unanimous PASS),
#    or reject and issue linked correction work.
./metis task verify --task <id> --pass --verifier reviewer-01
./metis task followup --task <id> --objective "fix ..."
```

Provenance and identity rules:

- `--reporter`, `--reviewer`, `--verifier`, `--approve` are
  asserted names, not authentication. The ledger records them
  so a CLI submission can never acquire worker attribution.
- A worker-loop result is filed as `runner-observation`, never
  as a worker claim and never as verification.
- Exit code 0 proves the process ended, not that the
  requirement is met. An exit without a report stays
  unresolved; reconcile says WAIT or INVESTIGATE.
- One session = one identity. Ten Muse Code instances are ten
  sessions with ten output streams; the harness name is never
  an identity.

Capability boundary (verified, not assumed):

- One-shot `exec` harnesses (muse, codex, claude, hermes) are
  qualified through the managed loop: exact-attempt claim,
  argv-vector launch, native exit status, bounded output,
  attempt-bound receipts.
- The `muse-native` harness is the persistent path: it submits
  MSP turns on the bound native session (no child process), with
  native steering, queued-turn admission, approval observation,
  and turn-intent dedup. Other harnesses still declare
  `Interactive=false` rather than promising continuity.
  A one-shot result never implies a conversation.

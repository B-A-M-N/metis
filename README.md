# Metis

Metis is a tabbed terminal manager for interactive Muse Code sessions,
with an AI-accessible controller through the existing MCP tools. It
starts and tracks sessions in tmux; the command is `metis`, and
`tui-dash` remains available as a compatibility name. Task, evidence,
fleet, PTY, and native Muse services remain available behind the
session interface.
The `tui-dash` command and existing state/work directories remain
compatible. New sessions use the `metis-` tmux prefix; registered
sessions with the old `tui-` prefix remain usable. `METIS_STATE` is
preferred; `TUI_DASH_STATE` remains accepted.

## What this is, and is not

- **It is:** a session manager for real Muse Code terminals, with
  separate session identity, provider/model metadata, prompt delivery,
  external tmux attach, and a pinned Meta.ai controller slot.
- **It is not:** a replacement for Muse Code or a mirror of a Meta.ai
  web conversation. The pinned controller tab shows Metis activity;
  it does not invent a chat transcript.

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

# Open the session manager:
./metis watch

# With your own session / workdir / worker:
./metis up --session mywork --workdir ~/mywork --cmd "hermes chat"

# TUI only, no auto-start:
./metis watch --session mywork

# Legacy demo entry point remains available:
./metis up

# Register a project once, then launch one interactive Muse Code session:
./metis project-root-register --root ~/Locus
./metis launch --name locus-01 --workdir ~/Locus \
  --provider meta --model muse-spark-1.3

# Attach directly to the exact raw agent terminal (no dashboard):
tmux attach -t metis-locus-01

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

Requirements: Go 1.25+, `tmux`, `muse` on PATH for `metis launch`, and
a terminal at least 80x24.
Go was installed on the build machine at `~/workspace/tools/go`
because it was not preinstalled there; on pop-os use the Go you
already have. No other runtime is needed once the binary is built.

## Session view and keys

- **Session tabs:** left; labels show project and instance number,
  while the session ID remains the control identity.
- **Text snapshot:** selected tmux pane, sanitized and read-only as a
  screen preview. It is not a full terminal emulator.
- **Full terminal:** press `F5` to suspend Metis and attach the normal
  terminal to that exact owned tmux session. Detaching returns to Metis.
- **Info drawer:** press `F6` to show provider/model, profile, working
  directory, process state, and recent session events. It is closed by
  default.
- **Prompt bar:** press `i`, type text, and press Enter. Submission
  captures the selected session ID and uses the input lease; `d`
  switches to durable guidance-file delivery. On the Meta.ai tab the
  prompt is disabled; use MCP tools to target a worker explicitly.
- **Meta.ai tab:** pinned controller slot and activity feed. Use the
  existing MCP tools to provision, list, target, instruct, and observe
  sessions. The feed is Metis activity, not a web conversation; no chat
  stream is linked.

Keys: `Up/Down` or `j/k` select, `i` prompt, `Esc` back to nav,
`Enter` limited keyboard attach, `F5` full terminal attach, `F6` info drawer,
`d` toggle prompt delivery, `PgUp/PgDn` scroll snapshot, `q` quit.

The in-TUI preview strips terminal control state. Muse Code full-screen
interaction uses native tmux attach, where colors, cursor movement,
resizing, and terminal modes are handled by tmux and the normal terminal.

## Limits, verified not assumed

- A worker that only runs once and exits (`hermes chat --oneshot`
  style) shows process output, not a live tool-call trace. The
  fleet trace endpoint is process-level for the same reason:
  `--oneshot` exposes no per-tool-call stream. A live view needs
  an interactive/streaming worker.
- This dashboard is for a direct host session, not for driving a
  fleet node through its gateway or SSH, per the fleet transport
  rule.

## Verification and qualification

```bash
go vet ./...
make build
go test ./...
```

For the Muse Code MVP change, `go build -buildvcs=false` and `go vet .`
passed. A zero-inference echo-provider demo launched two Muse Code
sessions in the same Locus directory, switched between their Metis tabs,
attached through `F5` to each exact session, detached without restarting
either process, sent one prompt only to the selected session, and opened
the `F6` info drawer. No Meta/local provider prompt was submitted. The
in-TUI screen remains a text snapshot; full-screen interaction uses the
normal tmux terminal attach.

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

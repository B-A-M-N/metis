# Limitations

## Implementation limitations

- Remote task-ledger delegation is intentionally local-only
  (pinned by tests); remote controllers can read (scoped), provision,
  message, input, resize (local MCP only), and stop within policy, but
  cannot delegate ledger work remotely.
- No multi-host TUI aggregation; cross-machine presence is out of scope.
- The in-TUI preview is a text screen model (CR/erase/cursor applied),
  not a full terminal emulator: styles and alt-screen applications are
  not emulated (F5 native attach for tmux; `pty-log` for PTY).
- One-shot harnesses show process output, not a live tool-call trace;
  a live view needs an interactive/streaming worker.

## Unqualified / external dependencies

- Live-provider orchestration (ChatGPT/Meta.ai → Ocliv → Metis MCP
  end-to-end) is not yet demonstrated; local staging used zero-cost
  echo workers and real stdio MCP clients.
- Paid-inference paths (FI-backed triads, cross-machine, child-event
  wire shape) are blocked pending authorization/backends.
- Q-18: `TestSameDirTwoEchoWorkersIsolatedSteering` intermittently
  times out stopping a supervisor under load. Controls are green and
  full gates pass on reruns; recorded DEFERRED in `AGENTS.md`. Do not
  weaken the test to obtain a pass.
- No LICENSE file and no CI ship with the repository (explicit owner
  decisions); production readiness is not claimed.

## Environment constraints (not Metis defects)

- Default `TMPDIR` on some hosts yields unix socket paths >108 chars
  (`bind: invalid argument`); workaround `TMPDIR=/tmp`.
- Plain `go build` may fail on VCS stamping; use `make build`.

# Getting started

## Prerequisites

Linux, Go 1.25+, `tmux`, `muse` on PATH for `metis launch`, terminal
>= 80x24. Optional harness CLIs: codex, claude, hermes.

## From a clean checkout

```bash
git clone https://github.com/B-A-M-N/metis.git && cd metis
make build          # ./metis + ./tui-dash symlink; -buildvcs=false required on some hosts
./metis doctor      # read-only health snapshot
./metis sessions    # empty is fine on first run
./metis watch       # dashboard
```

Set `METIS_STATE` to an isolated directory for staging or tests
(Metis reuses `~/.local/share/tui-dash` for legacy installs, otherwise
`~/.local/share/metis`).

## First task (zero-cost, no inference)

Assignment illustration only: the `sleep 900` worker does not write
the required file, so the task stays unreported until a real worker
executes it.

```bash
./metis agent-start --name muse-01 --workdir /work/a --cmd "sleep 900" --harness muse
./metis task-create --run rel-1 --agent muse-01 \
  --objective "Write DONE into required.txt" \
  --acceptance "required.txt exists containing DONE"
./metis task-dispatch --task <id> --session muse-01
./metis agent-work --session muse-01 --once
./metis task-status <id>
```

## Verify your checkout

```bash
export TMPDIR=/tmp   # required on some hosts (unix socket length)
make test-quick
go vet ./...
```

Full instructions: [Testing](TESTING.md). If you touch a subsystem,
read its row in `AGENTS.md` and the matching contract first.

# Testing gates

Single source for commands is the `Makefile`; this file explains when
to run what. Measured numbers come from `test-inventory.json`
(regenerate after major suite changes with a fresh
`go test -json -count=1 -buildvcs=false ./...` run).

| Gate | Command | When | Cost (measured) |
| ---- | ------- | ---- | --------------- |
| fast | `make test-fast` (`-short` + `./...`) | every code-edit iteration | ~250s; skips 24 integration tests |
| domain | `make test-domain AREA=<name>` | after a coherent fix | ~20-70s; areas: sessions provision dispatch tui muse assurance workspace fleet transport terminal |
| integration | `make test-integration` | after workflow changes | ~300s; builds a fresh `./tui-dash` first (the integration test exercises the binary — a stale one would silently qualify an older tree) |
| full | `make test-full` | checkpoints and release | full suite + `-race` on the dispatch/provision/coverage subset |
| release alias | `make test` | release | same as full |

Focused runs: `GOCACHE=/tmp/td-gocache go test -count=1 -run '<Name>' .`
plus `go vet .`. Relevant `-race` after concurrency changes.

## Rules with reasons

- `testing.Short()` marks integration-class tests that measured
  >=1.5s: daemon/host turns, PTY/fleet scale, perf characterization,
  timing/race stress, fresh-binary. Pure-logic slow tests stay in
  every gate — their cost is per-test fixture overhead (a future
  optimization target), not an integration dependency.
- Never add `t.Parallel`: tests share env vars, process-global
  state, and daemon resources. Parallelize only after a fixture
  proves isolation.
- Never shorten waits to make a gate pass: waits need
  synchronization or a bounded deadline, not smaller constants.
  Never weaken assertions or skip to green.
- The fast gate is smoke, not proof: release qualification is
  always the full uncached suite (`-count=1`, no cache).

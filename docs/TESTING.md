# Testing gates

Single source for commands is the `Makefile`; this file explains when
to run what. Measured numbers come from `test-inventory.json`. Run
`make test-inventory-check` to detect source drift; run
`make test-inventory-refresh` after suite changes to execute the full
uncached suite and refresh measured durations.

| Gate | Command | When | Cost (measured) |
| ---- | ------- | ---- | --------------- |
| quick | `make test-quick` (internal packages + selected root invariants) | every code-edit iteration | ~1s in the current tree; not a domain qualification |
| domain | `make test-domain AREA=<name>` | after a coherent fix | ~20-70s; areas: sessions provision dispatch tui muse assurance workspace fleet transport terminal |
| fast | `make test-fast` (`-short` + `./...`) | broad smoke qualification | ~250s; skips 24 integration tests |
| integration | `make test-integration` | after workflow changes | ~194s; builds fresh `./metis` and the legacy `./tui-dash` symlink first; the integration test exercises `./metis`, so stale binaries cannot qualify an older tree |
| full | `make test-full` | checkpoints and release | ~194s full suite + ~15s `-race` on the dispatch/provision/coverage subset |
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
- Inventory profile (687 tests): 672 tests measured at <=1.5s total
  172.92s, with the slowest single test at 4.31s. A warm
  `openTasksDB()` microbenchmark measured 0.814ms/op, too small to
  justify weakening per-test state isolation or adding a schema cache.
  No single fixture hotspot is established yet; profile fixture and
  subprocess setup separately before changing those shared helpers.
- The quick gate is a feedback loop, not proof. The fast gate is smoke,
  not proof: release qualification is
  always the full uncached suite (`-count=1`, no cache).

# Development

## Local loop

```bash
export TMPDIR=/tmp            # required on some hosts (socket length)
make test-quick               # fast loop
GOCACHE=/tmp/td-gocache go test -count=1 -buildvcs=false -run '<TestName>' .
go vet ./...
make build
```

Gates: `make test-quick` every iteration, `make test-domain
AREA=<name>` after a coherent fix, `make test-fast` for broad smoke,
`make test-integration` after workflow changes, `make test-full` at
checkpoints/release (full + race subset). Never add `t.Parallel`;
slow integration tests carry `testing.Short`. Test inventory is
tracked in `test-inventory.json` (`make test-inventory-check`).

## Working agreement

Read `AGENTS.md` before touching a subsystem: gate ladder, domain
ownership map, and audit dispositions (FIXED / VERIFIED NON-ISSUE /
ACCEPTED RISK / DEFERRED / BLOCKED). Every fix needs a failing-first
regression test that exercises the failure, not the implementation.
One writer per checkout; worktree isolation is not guaranteed — check
`git status` before starting and never overwrite concurrent changes.

## Debugging

Start with `docs/DEBUGGING.md` (failure class → command → owning
domain), then `./metis doctor` / `./metis diagnose --session <name>`
/ `./metis ops --errors-only`. Transcripts are never read by diagnose;
credentials are redacted.

## Release

Frozen RCs are tagged locally (`metis-rc-<short-sha>`) with the tested
binary, SHA-256 manifest, and qualification report under
`/home/bamn/releases/` on the development host. Never push, merge,
or deploy without explicit authorization; rollback uses a detached
worktree of the previous revision and restores binary + compatible
state, never a reset of `master`.

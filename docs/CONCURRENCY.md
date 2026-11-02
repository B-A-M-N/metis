# Concurrent development: roles, isolation, integration

Three responsibilities, three kinds of isolation. No two agents
mutate the same authoritative state machine at the same time.

## Roles

- Investigator: reproduces, traces ownership, writes the
  failing-first regression test. Changes test files and fixtures
  only — never the owning component.
- Implementer: changes the owning component, runs the focused
  gate, then the domain gate. One implementer per state machine.
- Verifier: independently re-runs the gates on the slice (fresh
  checkout or the author's tree after push to the shared branch),
  runs the integration-affected domains, and checks for coupling
  the author missed. A green suite the author ran themselves is
  evidence, not verification.

## Isolation mechanics (all verified in this repo)

- Worktrees: `git worktree add /tmp/<name> <base>` per agent.
  The tree at `/home/bamn/tui-dash` is the integration point;
  feature work never happens there directly.
- Test state: every test sets `TUI_DASH_STATE` to a temp dir
  (`t.Setenv`), and fixtures (`installFakeTmux`) are per-test
  temp dirs. Two suites on one machine do not share state files.
  Ambient exceptions an agent must NOT touch concurrently:
  the ambient tmux server, the ambient muse daemon socket, and
  `$TUI_DASH_STATE` when unset (developers' real state).
- GOCACHE may be shared read/write (`/tmp/td-gocache`); Go
  handles concurrent use. Do not share `go test` result files.

## Collision rules

- One active implementation owner per authoritative state
  machine (registry, provision intents, leases, task ledger).
  Claim it in the session/plan before editing; release on commit.
- No two agents run the full suite (`make test-full`,
  `test-integration`) at the same time on one machine: both
  hammer CPU/IO and each other's timing-sensitive tests.
  The coordinator owns the expensive gate; everyone else uses
  `test-fast` and `test-domain`.
- Never commit in another agent's worktree. Never
  `git clean -fdx` a shared checkout. Untracked files belong to
  whoever created them.
- Pushes: only the coordinator pushes the integrated branch, and
  only after the full gate is green on the integration tree.
  No force-push to shared branches, ever.

## Coordinator loop

1. Collect verified slices (each: failing-first test, focused +
   domain green, `go vet` clean).
2. Merge in dependency order (storage → domain → transport → UI).
3. Run `make test-full` once on the merged tree.
4. On red: bisect to the slice, hand back to its owner with the
   failing output. Do not fix-forward inside the coordinator
   turn except for mechanical conflicts.
5. Record the gate receipt (command, elapsed, head SHA) in the
   commit message or ledger row.

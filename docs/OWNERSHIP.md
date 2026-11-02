# Ownership map (pointer)

The authoritative map lives in `AGENTS.md` ("Domain ownership map
(DOX root contract)") with per-package contracts in
`internal/termview/AGENTS.md`, `internal/musewire/AGENTS.md`, and
`internal/tui/AGENTS.md`. This file states the rules that cross
every domain so agents stop re-deriving them.

## One writer per transition

Every authoritative state transition has exactly one owner.
Transports (CLI/MCP/TUI) validate and forward; they never mutate
domain state directly. `PrincipalID` comes from transport context,
never from a caller-supplied actor string.

## Read paths

- Transport reads go through read seams (`task_readmodel.go`,
  `listNativeSessions()`, `ptyListAll()`), never `openTasksDB()`
  in a transport file. Doctor's explicit `db` handle is the
  failure-injectable exception, and it is read-only
  (`openReadOnlyDB`, never migrates).
- Storage owns persistence (`task_schema.go`, `schema_epoch.go`,
  `task_store.go`, `*_store.go`, `*_repository.go`,
  `logical_session.go`, `task_readmodel.go` reads only).

## What each domain must never do

- tasks: terminal input, process spawn, review decisions, UI/MCP auth.
- sessions: task transitions, verdicts, placement, PTY internals.
- terminal: task dispatch, attempt claims, approvals, `tea.*` imports.
- placement: launching agents, choosing objectives.
- muse: strategic delegation, task acceptance.
- assurance: changing execution history, self-approval.
- workspace: merging/publishing, acceptance decisions.
- fleet: reinterpreting task objectives.
- events: creating/approving work, control flow on diagnostics.
- ui: direct state mutation, SQL, direct tmux calls.
- transport (`mcp_*`, `cli_*`): direct `openTasksDB` from read
  paths, process launch, Git, business authority.
- No domain backend depends on TUI types; the TUI package cannot
  import concrete session/PTY/storage implementations
  (compile-time boundary, AST-tested in `boundary_test.go`).

## Adding a cross-boundary read

1. If a storage-owned list helper exists
   (`listNativeSessions`, `ptyListAll`), use it.
2. Else add the helper in the owning domain with the narrowest
   return shape the caller needs — not a generic DB handle pass.
3. Diagnostics that must survive broken stores take an explicit
   `db` handle and report UNKNOWN on failure instead of skipping.

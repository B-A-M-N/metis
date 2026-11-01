# internal/tui — dashboard UI-state components

## Purpose

Own the TUI's UI-state components (navigation first; sessions,
terminal, guidance, feed, panels, refresh, renderer follow one at a
time) as private state plus pure reducers. No Bubble Tea program
lives here yet; the root `main` package adapts its event loop to
these components.

## Ownership

- `nav.go` — navigation state (`Nav`) and all cursor/viewport
  transitions. Only navigation mutates a `Nav`.
- `nav_test.go` — pure reducer tests; no tmux, registry, DB, or
  event loop.
- `refresh.go` — refresh coordinator state (`Refresh`): tick count,
  projection generation, in-flight accounting with exact-request
  correlation (outstanding gen + dispatch tick; `stallAfterTicks`
  abandonment so hung rounds cannot stall refresh). Only its
  transitions mutate a `Refresh`; the shell builds commands from
  the returned decisions and reports each delivery with its gen.
- `refresh_test.go` — pure coordinator tests (single-flight, exact
  delivery correlation, reselect storms, hung-round abandonment,
  gen fencing, git cadence); no tmux, registry, DB, or event loop.

## Local Contracts

- `internal/tui` imports stdlib only (plus `internal/termview`
  when rendering moves). It must never import the application's
  concrete persistence, process, or domain implementations — the
  `main` package depends on `tui`, never the reverse (compiler
  enforced: `main` cannot be imported).
- Component state is a plain struct with pure transition methods;
  effects (refresh commands, stream heals, submissions) stay in
  `main` adapters until their component moves.
- Resolved identities cross the boundary, never raw cursors:
  callers pass counts/names, components return clamped state.

## Verification

- `go test ./internal/tui/` — pure reducer suite.
- Root `boundary_test.go` may assert this package's import closure.

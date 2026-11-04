# Architecture

Metis is a single Go module (`metis`, package `main`) plus three
`internal/` packages. The layout is flat by design: Go package
boundaries are few, so domain ownership is enforced by file-group
contracts (`AGENTS.md`) and AST boundary tests, not by package splits.

## Components

| Area | Owns | Key files |
| --- | --- | --- |
| CLI | dispatch, flag parsing, human output | `main.go`, `cli_*.go` |
| Transport | MCP stdio + HTTP; validate, authorize, delegate | `mcp_*.go` |
| Tasks | ledger, dispatch, claims, reports, verification | `task_*.go`, `dispatch_*.go`, `worker_*.go`, `verdict.go` |
| Sessions | registry, tmux/PTY/native backends, lifecycle | `session_*.go`, `pty_*.go`, `terminal*.go` |
| Muse | orchestration, approvals, presence, guidance | `muse_*.go`, `internal/musewire/` |
| Placement | workdir reservation, fleet resources, team policy | `workspace_reserve.go`, `fleet_*.go`, `team_policy*.go` |
| Assurance | receipts, verdicts, artifacts, reconcile | `report_*.go`, `artifact_*.go`, `reconcile.go` |
| Events | durable orchestration events, metrics (observe-only) | `orchestration_events.go`, `ops_diag.go`, `*_metrics.go` |
| Storage | schema, epoch, read models | `task_schema.go`, `schema_epoch.go`, `task_readmodel.go` |
| TUI | dashboard, panes, input, projection rendering | `tui*.go`, `internal/tui/`, `internal/termview/` |

## Data and control flow

Controller → MCP transport (stdio per-session identity, or JWT HTTP
with principal-scoped reads) → domain service → SQLite ledger →
read-model projection → TUI pane. Transports never mutate domain state
directly; `PrincipalID` comes from transport context. Events are
written only after the underlying fact is durable (e.g. `worker.claimed`
after claim ownership checks pass).

## Security boundaries

- MCP ingress is explicit (`local`/`remote`), fail-closed; `doctor`
  diagnoses misconfiguration. Remote reads are principal-scoped via
  `ReadScope` (verified JWT principal + optional explicit `metis:fleet`
  grant); task_status/report/reconcile refuse inaccessible ids as
  not-found (no existence leak).
- Transport-session identity is distinct from principal identity
  (durable `muse_transport_sessions` registry): one controller's
  disconnect never marks a peer disconnected.
- Project-root registration is CLI-only. Provisioning requires
  registered roots + capability checks. Input delivery is
  lease-fenced with generation fencing.
- HTTP requests never infer client disconnects; HTTP is stateless
  (lease expiry only).

## Specialized packages

- `internal/musewire/` — Muse wire framing/codecs; stdlib only, no
  orchestration, no ledger, no supervision.
- `internal/termview/` — pure terminal math (width, ANSI, sanitize);
  stdlib only.
- `internal/tui/` — dashboard UI-state components; stdlib only, never
  domain imports.

See `AGENTS.md` (domain ownership map + audit dispositions),
`TUI_DOX.md` (TUI component contracts), and `INVARIANTS.md`.

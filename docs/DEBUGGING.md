# Debugging: failure class → evidence → owner

One-command investigation first; manual log search last.

## Start here

- `tui-dash diagnose --session NAME [--json]` — bounded bundle:
  identity/backend/alive, latest task + waiting reasons, recent
  transitions, input leases, resource pressure. Missing sections
  are noted, never invented. Transcripts are never read;
  credentials are redacted at the sink.
- `tui-dash doctor` — fleet/process health (registry, DB, schema,
  leases, adapters, MCP ingress). Read-only; never migrates.
- `tui-dash ops [--req ID] [--task ID] [--attempt N] [--session S]
  [--phase P] [--class C] [--errors-only] [--since 30m] [--json]`
  — correlated operation events. Needs `TUI_DASH_OPS_DIAG=1` at
  the producing end; unknown phase/class is refused (exit 2) so
  typos can't silently select nothing.

## Failure classes

| Symptom | Commands | Owner (fix where the state lives) |
| ------- | -------- | --------------------------------- |
| Session invisible to model/agents | `metis_agents`, `doctor` sessions | transport projection (`mcp_read_sessions.go`); backends: registry / `pty_sessions` / `logical_sessions` |
| Session shows under the wrong worker | `diagnose`, refresh gen markers | TUI projection fencing (`internal/tui` + `tui_install.go`); never display-name matching |
| Operation failed, unclear why | `ops --req/--task --errors-only`, `diagnose` | owning domain service; error `class` is stable (`opsKnownClass`), text is fallback |
| Provision conflict / unknown | `ops --class conflict`, `diagnose` | provision service (`provision_service.go`); unknown = unverified launch, retry same request_id |
| Input refused / stale generation | `diagnose` leases, `ops --phase` | terminal lease authority (`pty_lease.go`); generation fencing, never blind resend |
| Dispatch won by another worker | `ops --class conflict --task ID` | dispatch admission (`dispatch_plan.go`/`dispatch_commit.go`); late admitter refused |
| Lease held / stuck | `diagnose` leases section | input lease owner (`pty_lease.go`); leases expire, holders are named |
| Stopped session still live (or reverse) | `doctor` sessions, `diagnose` alive | sessions lifecycle (`session_lifecycle.go`); dead path never kills |
| Doctor reports UNKNOWN | `doctor` source column | named source is down (DB/table/registry); UNKNOWN is the finding, not a gap |
| No ops history at all | `ops` honest-empty message | opt-in was off when the op ran (`TUI_DASH_OPS_DIAG=1`); history is not backfilled |

## Evidence rules

- Reproduce with the owning component's test first
  (`make test-domain AREA=<name>`), not the full suite.
- Failing-first: a new test must fail for the defect's reason
  before the fix, pass after; stale-fixture passes prove nothing.
- Bounded reads only: `--limit` caps sink scans, `--limit` caps
  diagnose transitions, doctor output is fixed-shape.
- Redaction is load-bearing: `redactOpsDetail` covers secrets and
  tokens; never add raw transcripts or key material to output.

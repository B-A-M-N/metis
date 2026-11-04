# Troubleshooting

| Symptom | Likely cause / remedy |
| --- | --- |
| `bind: invalid argument` from harness tests | `TMPDIR` socket path too long; run with `TMPDIR=/tmp` |
| `error obtaining VCS status: exit status 128` on build | Use `make build` (`-buildvcs=false`) |
| `no such session` on `muse turn` / worker delivery | Session may be native-only; resolution now covers native logical records (F-21). Check `./metis doctor` session rows |
| `doctor` reports ingress unpinned | Set `METIS_MCP_INGRESS=local` (or remote vars); see README configuration table |
| Worker claimed but pane shows no execution | `dispatched` ≠ execution: check `worker.claimed` in `metis_events` and task status; claim happens in `agent-work` |
| Events empty for a remote controller | Reads are principal-scoped: check session ownership (`provision_owner`) or add explicit `metis:fleet` scope |
| Cursor stalls / missing events remotely | Paged reads advance past foreign windows; if still stuck, check `latest_event_seq` via `metis_status` and rescan with the returned cursor |
| `TestSameDirTwoEchoWorkersIsolatedSteering` timeout | Known Q-18 flake under load; rerun in isolation. Do not weaken the test |
| State appears in old directory | `METIS_STATE` unset reuses `~/.local/share/tui-dash` when present; set `METIS_STATE` explicitly |

For anything else: `./metis doctor`, `./metis diagnose --session
<name> [--json]`, `./metis ops --errors-only`, and
[Debugging](DEBUGGING.md).

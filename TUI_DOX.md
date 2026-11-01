# TUI architectural ownership map (DOX contract)

Package `main`, files `tui*.go` + `graph_panel.go`. This map is the Phase 1
contract for the boundary-first decomposition: every TUI state field and
transition has exactly one owning component; cross-component effects travel
as typed messages, never direct writes.

## Components

| Component | Owns (state) | Transitions live in | Must never do |
| --------- | ------------ | ------------------- | ------------- |
| shell | program lifecycle, event routing, command dispatch, output composition | `tui.go` Update/View/Init | terminal/process ops, persistence, domain decisions, direct session mutation |
| navigation | selected index, `muse` tab membership, feed cursor `museEv`, rail offset `tabOff`, interaction `Mode` | state + `Mode` + transitions (`Up/Down/Restore/Clamp/Select/FollowTabs/RailWindow/JumpToFeedTarget/EnterInput/EnterAttach/ToNav`) in `internal/tui`; shell adapts (`tui_viewstate.go`, `jumpToFeedTarget` in `tui_state.go`) | load sessions, deliver input, fetch events, manage session lifecycle |
| sessions | cached `names`, `running`, `entry`, `tabInfo`, `logGen`, preselect | installed by `applyRefresh` from `refreshData` only when `sessionsOk` (registry clean + PTY query ok); failure keeps last-known + stale marker | start/stop sessions, create PTYs, heal streams, acquire ownership, write persistence |
| terminal | `live`, `gitLines`, `events`, `input`, `direct`, parked `inputPending` | `tui_input.go`, guidance arms in `tui.go` | PTY management, lease issuance, tmux control, FIFO writes, queue ownership |
| muse-feed | `feed`, derived `museState/museOpen/museBlocked` | installed by `applyRefresh`; jump via navigation | session authority, delegation, approval decisions, event persistence |
| panels | `graphPanel`, `panelRun`, `panel` | toggle in `tui_viewstate.go`; content in `graph_panel.go` | Git commands, graph persistence, workspace mutation, task decisions |
| refresh | triggers, in-flight accounting (`Pending`), coalescing, deadlines, freshness | state + transitions in `internal/tui` (`Refresh.Tick/Reselect/Startup/OnDelivered`); shell builds commands in `tui.go`; acquisition in `tui_source.go` | own pane data, define business truth, mutate presentation state, retry mutations |
| attach | attempt generation, target session, outcome fencing | `internal/tui` (`Attach.Request/Classify`, pure); adapter classifies in `tui_attach.go`, applies mode/status | heal streams, own terminal output, mutate anything but its own gen/session |
| guidance | input buffer, editing, submit serialization (`Busy`), outcome status | `internal/tui` (`Guide.Type/Backspace/Clear/Submit/Complete`, pure); shell owns mode, builds `submitGuidanceCmd` from the intent | execute guidance, mutate tasks, authorize, second-dispatch |
| status | `status`, `guidanceBusy`, `width/height` | `tui.go` Update, `applyRefresh` staleness markers | — (presentation only; guidance-busy guard serializes submits) |
| renderer | formatting, layout, clipping, sanitize, compose | `tui_render*.go`, `tui_layout.go`, `tui_text.go` | I/O, persistence, processes, mutation, scheduling, caching |

## Typed cross-component contracts

- `refreshMsg{data}` — async projection; refused on gen/sel mismatch
  (`staleProjectionPrefix`), never silently installed.
- `attachResultMsg{session, gen, attached, reason}` — async attach
  outcome; `applyAttachResult` classifies it through
  `internal/tui` `Attach.Classify` (gen + target session + live
  target must all match), then applies mode/status effects.
  Stale or foreign outcomes change nothing.
- `inputResultMsg{session, opID, disposition, detail}` — input outcome;
  displayed only while attached to that session, else parked per-session.
- `guidanceResultMsg{session, gid, direct, err}` — submit outcome;
  status names the origin `session` (finding G).
- `refreshSource` interface — acquisition seam behind
  `collectRefreshData`; production `liveRefreshSource`, tests inject fakes.
  The package-global `activeRefreshSource` is the D finding: migrate to
  instance-scoped injection (constructors, no mutable global).

## Resolved findings

- **P0 pane binding**: sel-scoped panes (entry, live, events, git)
  render only for the session they were collected for.
  `applyRefresh` records `model.paneSel` alongside the installs;
  renderers read through `boundEntry`/`boundLines`, so a selection
  change never presents worker A's output, events, Git data, or
  metadata as worker B's — even when B's own collection is still in
  flight or hung (`TestRenderSelectionChangeHidesPreviousSession`,
  mutation-proven; `TestBoundaryPaneReadsRoutedThroughBinding` +
  `TestBoundaryPaneBindingSingleWriter` pin the routing).
- **Mode ownership**: flat `model.mode` string is gone. `internal/tui`
  `Nav.Mode` (`ModeNav/ModeInput/ModeAttach`) changes only through
  `EnterInput`/`EnterAttach`/`ToNav`; the shell routes keys through
  them (`TestBoundaryModeWritesRoutedThroughNav` pins zero direct
  writes, mutation-proven; `Attach.Classify` takes `Mode`).
- **Status as control state**: the status line is display-only.
  Staleness is owned state (`model.staleMarker`, written only by
  `applyRefresh`/`installStaleMarker`); a fresh apply clears the
  status only when it still holds the installed marker, so foreign
  messages that share the prefix survive
  (`TestRefreshApplyPreservesForeignStatus`, mutation-proven;
  `TestBoundaryNoStatusSniffing` pins the ban).

- **E (attachment)**: single async lifecycle `requestAttach → attachCmd →
  attachResultMsg → applyAttachResult`. The sync `attach()` competing
  implementation is removed; `TestAttachHealsStream` covers the async path.
- **F (selection)**: `selectedSummary` clamps; empty → `(none)`.
  Renderers and operation requests share this gate.
- **G (correlation)**: every async completion carries its session identity;
  stale results never rewrite another session's UI (`applyInputResult`
  ignores foreign sessions; guidance status names origin; attach
  outcomes are classified by the `internal/tui` `Attach` component —
  flat `model.attachGen` removed, `TestAttachRefusalDetaches` moved
  onto the authoritative `Request` path).

## Resolved convergences

- **Navigation/Phase 2 slice 1**: `internal/tui.Nav` owns selection,
  Muse-tab membership, feed cursor, rail viewport + all transitions
  (Up/Down/Restore/JumpToFeedTarget/ClampFeedCursor/FollowTabs/
  RailWindow). `main` adapts (`navUp/navDown/navReselect/followTabs/
  jumpToFeedTarget/applyRefresh/selName`); `TestBoundaryInternalTUIImportClosure`
  pins the package's stdlib-only import closure; `internal/tui` nav
  tests run with zero infrastructure.
- **D**: instance-scoped `refreshSource` (`model.source` +
  `newModelWithSource`; no global). `TestRefreshSourceIsInstanceScoped`
  + `TestBoundaryNoGlobalRefreshSource` pin it.

## Open convergences (ordered)

1. ~~**D**~~ done (above).
2. **B** done: `applyRefresh` (`tui_state.go`) keeps freshness gates +
   cross-component detach; per-component installers in `tui_install.go`
   (`applySessions/Feed/Panels/Nav/TerminalRefresh`) own their fields.
   Mutation-verified (disabled names-install fails
   `TestRefreshMsgAppliesPreservesSelectionAndGit`).
3. **C** done: acquisition split into pane builders (`sessionsRound`
   shared round + `liveSessionsSnapshot` / `liveContentSnapshot` /
   `liveFeedSnapshot` / `livePanelSnapshot`); tab titles via indexed
   `latestObjectiveByAgent` (GROUP BY over `tasks_agent_created`,
   plan-pinned); per-session readers use `listTasksByAgent`.
   `metis_projects` assessed: on-demand scoped read, out of scope.
4. **G (attach leg)** done: attachment attempts carry an operation
   generation (`model.attachGen`, bumped per `requestAttach`,
   echoed in `attachResultMsg.gen`); `applyAttachResult` ignores
   generation mismatches even when the session name matches, so a
   superseded attempt's late refusal cannot detach a newer confirmed
   attach. Failing-first `TestAttachStaleRefusalCannotClobberNewerAttach`
   (Enter, Esc, Enter, success, late refusal).
5. **Refresh single-flight** done (Phase 5 leg), now a component:
   odd ticks dispatch only when no collection is outstanding
   (`Refresh.Pending` in `internal/tui`, cleared on `refreshMsg`
   delivery); gen bumps only with a dispatch or an explicit
   reselect/startup transition. Failing-first
   `TestRefreshSkipsWhileOneIsInFlight` (3 ticks, no delivery →
   exactly 1 collection; delivery re-arms) plus pure
   `internal/tui` reducer tests.
6. **Attach lifecycle component** done (finding G leg): `internal/tui`
   `Attach` owns attempt generation + target session
   (`Request`/`Classify`, pure, no tmux/registry/DB); `main` keeps
   `attachCtl` and classifies outcomes in `applyAttachResult`, then
   applies mode/status. 4 pure tests; integration pin
   `TestAttachStaleRefusalCannotClobberNewerAttach` mutation-proven
   (gen-check removal detaches the live attach → FAIL, restored →
   green); `TestBoundaryAttachOutcomesRoutedThroughFence` pins the
   routing (bypass → FAIL with no-Classify error, restored → green).
7. **Guidance component** done: `internal/tui` `Guide` owns the
   buffer, rune-aware editing, one-in-flight serialization, and the
   origin-naming outcome status (`Type/Backspace/Clear/Submit/
   Complete`, pure, no tmux/registry/DB); shell owns mode + command
   construction from `GuideIntent`. Flat `model.input`/`guidanceBusy`
   removed. 7 pure tests; `TestGuidanceDoubleEnterSerializes`
   mutation-proven at both levels (busy-check removal → FAIL,
   restored → green); `TestBoundaryGuidanceRoutedThroughComponent`
   pins the routing (Complete bypass → FAIL, restored → green).
2. **B**: distribute `applyRefresh` per-component installers behind one
   freshness gate; recipients decide how valid results change their state.
3. **C**: split `liveRefreshSource.snapshots` acquisition per pane with
   independent failure flags (feed already has `feedOk`; extend the pattern).
4. **A/Phase 2**: migrate components to `internal/tui` behind port
   interfaces, one component at a time; replace `model{...}` test literals
   with constructors as each component moves.

## Verification

- `TestBoundaryReadUIHasNoSQLImport`, `TestBoundaryTransportHasNoDBOpeners`,
  `TestBoundaryTerminalHasNoTeaDependency` (`boundary_test.go`).
- `TestViewRendersFromCacheOnly` — View purity (no I/O).
- `TestCollectLeavesMissingStreamAlone` — refresh is read-only.
- Decomposition seam tests: `TestAssembleRefreshFromSource`,
  `TestCollectRefreshDataUsesReadModel`, attach/guidance/session tests in
  `tui_decompose_test.go`, `terminal_input_session_test.go`.

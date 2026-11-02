package main

// Bubble Tea dashboard for Metis.
// Panes: Sessions (left), Live output (right top), Git (right middle),
// Events + guidance input (bottom).
// Guidance defaults to file mode; `d` toggles direct send-keys mode.
// Keys: Up/Down or j/k select, i/Tab input, Esc nav, Enter send,
// d direct toggle, r tail, PgUp/PgDn scroll live, q quit.

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "metis/internal/tui"
)

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// model is the single Bubble Tea program state. Its fields group into
// six view-state components (navigation, sessions, terminal, muse
// feed, panels, status — see tui_viewstate.go); transitions live with
// their component, collection behind refreshSource (tui_source.go),
// and presentation in tui_render.go. The struct stays flat because a
// same-package test pins a model{...} composite literal.
type model struct {
	// source is the read-model behind this instance's refreshes
	// (finding D): production wires liveRefreshSource, tests inject
	// scripted fakes. It is instance state, never a package global,
	// so two dashboards never share acquisition.
	source refreshSource
	// nav is the navigation component's private state (selected
	// worker cursor, Muse-tab membership, feed cursor, rail
	// viewport). Only navigation transitions mutate it; every other
	// component reads resolved identities. See internal/tui.
	nav       tuistate.Nav
	names     []string
	running   map[string]bool
	entry     Session
	preselect string
	// feed is the latest orchestration events (chronological) and
	// museState its event-derived projection.
	feed        []OrchEvent
	museState   string
	museOpen    int
	museBlocked int
	// logGen remembers the last seen output generation per session.
	logGen map[string]int
	// tabInfo holds the current rail identity blocks by session name.
	tabInfo    map[string]tabDetail
	identities map[string]sessionIdentity
	live       []string
	gitLines   []string
	events     []string
	// guideCtl owns guidance editing and submit serialization
	// (internal/tui): input buffer, one-in-flight busy flag, and
	// outcome status. Only its transitions mutate it; the shell
	// owns mode changes and builds the submit command. See
	// internal/tui.
	guideCtl tuistate.Guide
	// attachCtl is the attachment lifecycle (internal/tui): the
	// current attempt generation and its target session. Only its
	// transitions mutate it; outcomes are classified against the
	// live target here in the adapter. See internal/tui.
	attachCtl tuistate.Attach
	direct    bool
	scroll    int
	// inputPending parks terminal-input outcomes that arrived
	// while attached elsewhere: last disposition per session, so
	// a stale result never rewrites another session's UI state.
	inputPending map[string]string
	// graphPanel toggles the MD path-traversal banner on the right.
	// panelRun/panel carry its last-collected projection, installed
	// only through applyRefresh like every other pane.
	graphPanel           bool
	infoOpen             bool
	infoSession          string
	infoDiagnostics      []opsEvent
	infoResources        resourceUsageSnapshot
	infoStream           streamResourceUsage
	infoStreamFound      bool
	infoDiagnosticSource string
	panelRun             string
	panel                []string
	status               string
	// paneSel records which session the installed sel-scoped panes
	// (entry, live, events, git) were collected for. Only
	// applyRefresh writes it, alongside the installs; renderers show
	// sel-scoped data only when it matches the viewed session, so a
	// selection change never presents the previous session's panes
	// as the new session's (P0). Empty means no projection yet.
	paneSel string
	// staleMarker is the exact stale-projection text installed by
	// the last refused refresh (empty when the panes are fresh).
	// Only applyRefresh/installStaleMarker write it; a fresh apply
	// clears the status line only when it still holds this marker,
	// so foreign statuses are never clobbered by prefix-sniffing.
	staleMarker string
	width       int
	height      int
	// refreshCtl is the refresh coordinator (internal/tui): tick
	// count, projection generation, and in-flight accounting. Only
	// its transitions mutate it; the shell builds commands from the
	// returned decisions and applyRefresh compares generations
	// read-only. See internal/tui.
	refreshCtl tuistate.Refresh
}

// guidanceResultMsg carries an off-update-path guidance outcome
// back to the model (TD-102/103). The submit runs as a command so
// filesystem and tmux latency never block the event loop; the status
// line reports the outcome only when this message arrives.
func (m model) Init() tea.Cmd { return tickCmd() }

// attachTarget returns the selected session name if it is cached as
// running. Keystroke delivery re-verifies ownership live; this gate
// only keeps the UI honest about read-only state.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		// Single-flight lives in the coordinator: while a
		// collection is outstanding the tick only reschedules
		// itself. The next tick after delivery dispatches again,
		// so coalescing delays a refresh by at most one tick
		// instead of stacking concurrent rounds.
		var out tuistate.TickOutcome
		m.refreshCtl, out = m.refreshCtl.Tick()
		if out.Dispatch {
			sel := m.selName()
			return m, tea.Batch(tickCmd(), m.refreshCmd(out.Gen, sel, false))
		}
		return m, tickCmd()
	case refreshMsg:
		// The acquisition completed (installed or refused inside
		// applyRefresh): re-arm dispatch exactly when this is the
		// outstanding round's delivery. Stale, superseded,
		// duplicate, and foreign deliveries leave accounting
		// untouched; the install-side gen gate in applyRefresh
		// refuses their data all the same.
		m.refreshCtl = m.refreshCtl.OnDelivered(msg.data.gen)
		// Freshness is enforced inside applyRefresh: mismatched
		// projections are refused with an explicit stale marker
		// instead of being silently dropped or, worse, installed.
		m.applyRefresh(msg.data)
		if msg.data.gen != m.refreshCtl.Gen && !m.refreshCtl.Pending {
			var out tuistate.TickOutcome
			m.refreshCtl, out = m.refreshCtl.DispatchLatest()
			if out.Dispatch {
				return m, m.refreshCmd(out.Gen, m.selName(), false)
			}
		}
		return m, nil
	case guidanceResultMsg:
		// Outcome status is the component's decision: it always
		// names the origin session, never the viewed one.
		var status string
		m.guideCtl, status = m.guideCtl.Complete(tuistate.GuideOutcome{
			Session: msg.session, GID: msg.gid, Direct: msg.direct, Err: msg.err,
		})
		m.status = status
		return m, nil
	case inputResultMsg:
		m = applyInputResult(m, msg)
		return m, nil
	case attachResultMsg:
		m.applyAttachResult(msg)
		return m, nil
	case externalAttachPreparedMsg:
		if m.nav.OnMuse || m.selName() != msg.display {
			return m, nil
		}
		if msg.err != nil {
			m.status = "terminal attach refused: " + msg.err.Error()
			return m, nil
		}
		m.status = "attached to " + msg.display + " in the normal terminal; detach to return"
		return m, externalAttachProcess(msg.target, msg.display)
	case externalAttachFinishedMsg:
		if msg.err != nil {
			m.status = "terminal attach ended: " + msg.err.Error()
		} else {
			m.status = "returned from terminal attach for " + msg.display
		}
		return m, nil
	case sessionInfoMsg:
		if m.infoOpen && !m.nav.OnMuse && m.selName() == msg.display {
			m.infoSession = msg.display
			m.infoDiagnostics = msg.diagnostics
			m.infoResources = msg.resources
			m.infoStream = msg.stream
			m.infoStreamFound = msg.streamFound
			m.infoDiagnosticSource = msg.diagnosticSource
		}
		return m, nil
	case tea.KeyMsg:
		if m.nav.Mode == tuistate.ModeAttach {
			return m.attachKey(msg)
		}
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.nav.Mode == tuistate.ModeInput {
			if m.nav.OnMuse {
				m.guideCtl = m.guideCtl.Clear()
				m.nav = m.nav.ToNav()
				m.status = "Meta.ai controller has no linked chat stream; use the existing MCP tools"
				return m, nil
			}
			switch msg.String() {
			case "esc":
				m.nav = m.nav.ToNav()
			case "enter":
				// No live selection: park the text and leave, busy
				// untouched — there is nothing to submit to.
				sel := m.selName()
				if sel == "" {
					m.guideCtl = m.guideCtl.Clear()
					m.nav = m.nav.ToNav()
					break
				}
				var intent tuistate.GuideIntent
				m.guideCtl, intent = m.guideCtl.Submit()
				switch intent.Kind {
				case tuistate.GuideSend:
					m.status = "guidance sending…"
					m.nav = m.nav.ToNav()
					return m, submitGuidanceCmd(
						m.canonicalSession(sel), intent.Text, m.direct,
						OperationRequest{Actor: "metis-operator", RequestID: newRequestID()})
				case tuistate.GuideHold:
					// A submit is already in flight: hold this
					// one rather than delivering out of order.
					// The typed text is kept so nothing is lost.
					m.status = "guidance in flight; ENTER again after it lands"
				default:
					// Blank buffer: park and leave.
					m.nav = m.nav.ToNav()
				}
			case "backspace":
				m.guideCtl = m.guideCtl.Backspace()
			default:
				if len(msg.Runes) > 0 {
					m.guideCtl = m.guideCtl.Type(msg.Runes)
				}
			}
			return m, nil
		}
		// nav mode: selection, feed-cursor, panel, and mode
		// transitions live with the navigation component
		// (tui_viewstate.go) and the attach command
		// (tui_attach.go). Update performs no blocking external
		// work: collection runs in refreshCmd, guidance in
		// submitGuidanceCmd, stream healing in attachCmd, and key
		// delivery on the session senders.
		var navCmd tea.Cmd
		priorSel := m.selName()
		switch msg.String() {
		case "q":
			return m, tea.Quit
		case "enter":
			cmd := m.requestAttach()
			return m, cmd
		case "f5":
			if m.nav.OnMuse {
				m.status = "Meta.ai controller has no terminal; select a worker tab before full terminal attach"
				break
			}
			sel := m.selName()
			if sel == "" || !m.running[sel] {
				m.status = "select a running tmux session before terminal attach"
				break
			}
			if m.isNativeSelection(sel) || isPtySession(sel) {
				m.status = "normal terminal attach is available for tmux sessions only"
				break
			}
			m.status = "checking owned tmux session " + sel
			return m, externalAttachPrepareCmd(sel, m.canonicalSession(sel))
		case "up", "k":
			navCmd = m.navUp()
		case "down", "j":
			navCmd = m.navDown()
		case "i", "tab":
			if m.nav.OnMuse {
				m.status = "Meta.ai controller is available through MCP; no web conversation is mirrored here"
			} else {
				m.nav = m.nav.EnterInput()
			}
		case "d":
			if m.nav.OnMuse {
				m.status = "Meta.ai controller uses MCP tools; no worker prompt is active"
			} else {
				m.toggleDirect()
			}
		case "f6":
			if m.nav.OnMuse {
				m.status = "select a worker tab to open its session info drawer"
				break
			}
			m.infoOpen = !m.infoOpen
			if m.infoOpen {
				m.status = "session info drawer open (F6 closes)"
				if sel := m.selName(); sel != "" {
					navCmd = sessionInfoCmd(sel, m.canonicalSession(sel))
				}
			} else {
				m.status = "session info drawer closed"
			}
		case "r":
			m.scroll = 0
		case "pgup":
			m.scroll += 10
			if m.scroll > len(m.live)-1 {
				m.scroll = len(m.live) - 1
			}
			if m.scroll < 0 {
				m.scroll = 0
			}
		case "pgdn":
			m.scroll -= 10
			if m.scroll < 0 {
				m.scroll = 0
			}
		}
		if m.infoOpen && !m.nav.OnMuse && m.selName() != "" && m.selName() != priorSel {
			sel := m.selName()
			navCmd = tea.Batch(navCmd, sessionInfoCmd(sel, m.canonicalSession(sel)))
		}
		return m, navCmd
	}
	return m, nil
}

// runeWidth is a compact wcwidth: combining marks take no columns,
// East Asian wide/fullwidth runes take two, everything else one.
func runTUI(preselect string) error {
	m := newModel(preselect)
	m.refresh()
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err := p.Run()
	return err
}

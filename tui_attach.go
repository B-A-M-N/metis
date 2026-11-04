package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"

	tuistate "metis/internal/tui"
)

type externalAttachPreparedMsg struct {
	display string
	target  string
	err     error
}

type externalAttachFinishedMsg struct {
	display string
	err     error
}

// externalAttachPrepareCmd resolves the owned tmux session away from the
// Bubble Tea update path. The eventual attach uses the immutable tmux target
// returned by the ownership check, so a reused display name cannot redirect it.
func externalAttachPrepareCmd(display, canonical string) tea.Cmd {
	return func() tea.Msg {
		if isPtySession(display) {
			return externalAttachPreparedMsg{display: display, err: fmt.Errorf("native terminal attach is available for tmux sessions only")}
		}
		target, err := resolveOwnedTmuxTarget(canonical)
		return externalAttachPreparedMsg{display: display, target: target, err: err}
	}
}

func externalAttachProcess(target, display string) tea.Cmd {
	return tea.ExecProcess(ownedTmuxAttachCommand(target), func(err error) tea.Msg {
		return externalAttachFinishedMsg{display: display, err: err}
	})
}

// tui_attach.go: terminal attachment as an async command.
//
// Attaching used to heal the stream pipe (tmux pipe-pane, a
// blocking external operation) inside Update. Update now only flips
// the local UI mode and returns attachCmd; the pipe healing runs
// off the update path and reports back as a typed attachResultMsg.
// A refusal (ownership or liveness lost between keypress and heal)
// detaches with a reason instead of stranding the UI in attach mode.

// attachResultMsg carries one attach attempt's typed outcome back to
// the model: success, or refusal with the reason. gen is the attempt
// generation from requestAttach (finding G): a superseded attempt's
// late outcome never touches a newer attempt's UI state, even when
// both name the same session.
type attachResultMsg struct {
	session  string
	gen      int
	attached bool
	reason   string
}

// attachedStatusLine reports an established attachment.
func attachedStatusLine(sel string) string {
	return "attached to " + sel + ": typing goes to the session (esc=detach, ctrl+c=interrupt pane)"
}

// attachRefusedStatus is the read-only gate: the cached view says
// the selection is not a live worker.
const attachRefusedStatus = "read-only: selected session is not running (attach refused)"

// attachCmd heals the stream pipe for sel off the UI goroutine and
// wraps the outcome — attached or refused — as an attachResultMsg.
// PTY sessions carry no pipe to heal and always succeed. gen pins
// the outcome to its originating attempt.
func attachCmd(sel string, gen int) tea.Cmd {
	return func() tea.Msg {
		if isPtySession(sel) {
			return attachResultMsg{session: sel, gen: gen, attached: true}
		}
		reg := loadRegistry()
		if _, _, err := resolveOwnedTarget(reg, sel); err != nil {
			return attachResultMsg{session: sel, gen: gen, reason: err.Error()}
		}
		ensureStreamPipe(sel)
		return attachResultMsg{session: sel, gen: gen, attached: true}
	}
}

// ptyResizeResultMsg carries one dashboard-driven PTY resize
// outcome back to Update. Resize failures are status-only: they
// never detach or block input, and a late result for a session
// that is no longer selected is ignored.
type ptyResizeResultMsg struct {
	session string
	seq     int
	err     error
}

// ptyResizeCmd forwards the dashboard pane geometry to the
// selected PTY worker's master (lease-fenced RS frame, same
// authority as pty-resize / metis_terminal_resize). Nil when
// the selection is not a running PTY or no geometry is known.
// The worker that is not selected keeps its last size: two
// agents in one directory resize independently, never jointly.
func (m *model) ptyResizeCmd(sel string) tea.Cmd {
	if sel == "" || !isPtySession(sel) || !m.running[sel] || m.width < 80 || m.height < 24 {
		return nil
	}
	lay := computeLayoutWithInfo(m.width, m.height, m.infoOpen && !m.nav.OnMuse)
	cols, rows := lay.mainW-2, lay.contentH-2
	if cols < 10 {
		cols = 10
	}
	if cols > 500 {
		cols = 500
	}
	if rows < 3 {
		rows = 3
	}
	if rows > 200 {
		rows = 200
	}
	bare := ptyBare(sel)
	// Latest-wins sequencing: each issued resize takes the next
	// per-session sequence; ptyResizeLatest drops any command
	// that executes after a newer one applied, so rapid
	// resize/reselection storms converge on the latest geometry.
	if m.ptyResizeSeq == nil {
		m.ptyResizeSeq = map[string]int{}
	}
	m.ptyResizeSeq[sel]++
	seq := m.ptyResizeSeq[sel]
	return func() tea.Msg {
		return ptyResizeResultMsg{session: sel, seq: seq, err: ptyResizeLatest(bare, seq, rows, cols)}
	}
}

// requestAttach is the Update-path half of attachment: it validates
// against the cached (pure) view, flips the local UI mode, and
// returns the command that heals the stream off-path. It performs no
// blocking external work itself. Every attempt takes a fresh
// generation so a superseded attempt's late outcome is recognizable.
func (m *model) requestAttach() tea.Cmd {
	if m.nav.OnMuse {
		m.jumpToFeedTarget()
		return nil
	}
	if sel := m.selName(); sel != "" && m.isNativeSelection(sel) {
		m.status = "native worker: use i for structured Muse steering (terminal attach unavailable)"
		return nil
	}
	sel, ok := m.attachTarget()
	if !ok {
		m.status = attachRefusedStatus
		return nil
	}
	var gen int
	m.attachCtl, gen = m.attachCtl.Request(sel)
	m.nav = m.nav.EnterAttach()
	m.status = attachedStatusLine(sel)
	if isPtySession(sel) {
		// Size the worker to the pane it is viewed through;
		// batch with the (trivial) PTY attach outcome.
		return tea.Batch(attachCmd(sel, gen), m.ptyResizeCmd(sel))
	}
	return attachCmd(sel, gen)
}

// applyAttachResult installs one async attach outcome. An outcome
// from a superseded attempt (generation mismatch) is ignored even
// when it names the currently attached session: only the latest
// attempt may confirm or detach. A success confirms the status only
// while still attached to that exact session; a refusal detaches
// with the reason under the same guard. Outcomes for any other
// session are ignored: a stale result never rewrites another
// session's UI state.
func (m *model) applyAttachResult(res attachResultMsg) {
	// Liveness is the adapter's input to the pure fence: the
	// component decides, the shell applies mode/status effects.
	sel, ok := m.attachTarget()
	live := ""
	if ok {
		live = sel
	}
	switch m.attachCtl.Classify(m.nav.Mode, res.gen, res.session, res.attached, live, ok) {
	case tuistate.AttachConfirmed:
		m.status = attachedStatusLine(sel)
	case tuistate.AttachRefused:
		m.nav = m.nav.ToNav()
		if res.reason != "" {
			m.status = "attach refused for " + sel + ": " + res.reason
		} else {
			m.status = "attach refused for " + sel
		}
	default:
		// Stale or foreign outcome: change nothing.
	}
}

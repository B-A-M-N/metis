package main

import (
	tea "github.com/charmbracelet/bubbletea"

	tuistate "metis/internal/tui"
)

// tui_input.go: keyboard and guidance interactions. Extracted from tui.go; behavior unchanged.

// guidanceResultMsg carries an off-update-path guidance outcome
// back to the model (TD-102/103), correlated with the session it
// was submitted to (finding G): the selection may have moved while
// the submit was in flight, and a bare "sent" would misattribute
// the outcome to the newly viewed session.
type guidanceResultMsg struct {
	session string
	gid     string
	direct  bool
	err     error
}

// submitGuidanceCmd submits guidance off the UI goroutine and wraps
// the outcome — success or refusal — as a guidanceResultMsg.
func submitGuidanceCmd(session, text string, direct bool, req OperationRequest) tea.Cmd {
	return func() tea.Msg {
		gid, err := submitGuidance(session, text, direct, req)
		return guidanceResultMsg{session: session, gid: gid, direct: direct, err: err}
	}
}

// refreshData is collected off the UI goroutine and delivered as a
// message, so tmux/filesystem latency never blocks rendering.
func (m *model) attachKey(msg tea.KeyMsg) (model, tea.Cmd) {
	if msg.String() == "esc" {
		m.nav = m.nav.ToNav()
		m.status = "detached (session keeps running)"
		return *m, nil
	}
	if sel := m.selName(); sel != "" && m.isNativeSelection(sel) {
		m.nav = m.nav.ToNav()
		m.status = "native workers have no terminal input surface"
		return *m, nil
	}
	sel, ok := m.attachTarget()
	if !ok {
		m.nav = m.nav.ToNav()
		m.status = "detached: session stopped"
		return *m, nil
	}
	if isPtySession(sel) {
		return m.attachKeyPty(sel, msg)
	}
	var literal string
	var keys []string
	local := false
	switch msg.String() {
	case "enter":
		keys = []string{"Enter"}
	case "backspace":
		keys = []string{"BSpace"}
	case "tab":
		keys = []string{"Tab"}
	case "up":
		keys = []string{"Up"}
	case "down":
		keys = []string{"Down"}
	case "left":
		keys = []string{"Left"}
	case "right":
		keys = []string{"Right"}
	case "pgup":
		m.scroll += 10
		local = true
	case "pgdn":
		m.scroll -= 10
		if m.scroll < 0 {
			m.scroll = 0
		}
		local = true
	case "ctrl+c":
		keys = []string{"C-c"}
	default:
		if len(msg.Runes) > 0 {
			literal = string(msg.Runes)
		} else {
			return *m, nil
		}
	}
	if local {
		return *m, nil
	}
	job, ok := enqueueInputJob(sel, false, literal, keys, "", controllerID())
	if !ok {
		m.status = "input lane full for " + sel + "; keypress refused, retry after it drains"
		return *m, nil
	}
	m.status = "input " + job.opID + " queued (" + sel + ")"
	return *m, waitInputResultCmd(job)
}

// attachKeyPty routes a keypress into a PTY-backed session. Like
// the tmux path it never blocks the update loop: the FIFO write
// and lease handshake run on the session sender, and the typed
// outcome returns as an inputResultMsg. Local keys never reach
// the worker.
func (m *model) attachKeyPty(sel string, msg tea.KeyMsg) (model, tea.Cmd) {
	switch msg.String() {
	case "pgup":
		m.scroll += 10
		return *m, nil
	case "pgdn":
		m.scroll -= 10
		if m.scroll < 0 {
			m.scroll = 0
		}
		return *m, nil
	}
	out, send := ptyKeyBytes(msg.String(), msg.Runes)
	if !send {
		return *m, nil
	}
	job, ok := enqueueInputJob(ptyBare(sel), true, "", nil, out, controllerID())
	if !ok {
		m.status = "input lane full for " + sel + "; keypress refused, retry after it drains"
		return *m, nil
	}
	m.status = "input " + job.opID + " queued (" + sel + ")"
	return *m, waitInputResultCmd(job)
}

// waitInputResultCmd delivers the job's typed outcome to Update.
func waitInputResultCmd(job inputJob) tea.Cmd {
	return func() tea.Msg {
		return <-job.reply
	}
}

// applyInputResult installs a job outcome: on the status line only
// while still attached to that exact session, parked per-session
// otherwise. A stale result never rewrites another session's UI.
func applyInputResult(m model, res inputResultMsg) model {
	if m.nav.Mode == tuistate.ModeAttach {
		if sel, ok := m.attachTarget(); ok && sel == res.session {
			m.status = "input " + res.opID + " " + res.disposition + " (" + res.session + ")"
			if res.detail != "" {
				m.status += ": " + res.detail
			}
			return m
		}
	}
	if m.inputPending == nil {
		m.inputPending = map[string]string{}
	}
	m.inputPending[res.session] = res.opID + " " + res.disposition
	return m
}

// staleProjectionPrefix marks statuses installed when a mismatched
// projection is refused. A later successful refresh clears it; any
// other status text is left untouched.

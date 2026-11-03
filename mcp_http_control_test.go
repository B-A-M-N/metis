package main

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// httpTermText strips terminal control sequences from bounded
// terminal output so a cursor-addressed render (which splits one
// reply across escape sequences) is still comparable as text.
// Cursor movement never reorders the write stream, so stripping
// preserves what the worker application actually emitted.
var httpTermEsc = regexp.MustCompile("\x1b\\][^\x07]*(?:\x07|\x1b\\\\)|\x1b\\[[0-9;? ]*[a-zA-Z]|\x1b[()][0-2A-Z]|\x1b[=>]|\x1b[0-9 ]+q")

func httpTermText(raw json.RawMessage) string {
	// Unwrap the double encoding: outer MCP envelope -> inner
	// result JSON text -> lines array holding real ESC bytes.
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw)
	}
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	var sb strings.Builder
	for _, c := range content {
		text, _ := c.(map[string]any)["text"].(string)
		if text == "" {
			continue
		}
		var inner map[string]any
		if err := json.Unmarshal([]byte(text), &inner); err != nil {
			sb.WriteString(text)
			continue
		}
		lines, _ := inner["lines"].([]any)
		for _, l := range lines {
			if s, _ := l.(string); s != "" {
				sb.WriteString(s)
				sb.WriteString("\n")
			}
		}
	}
	return httpTermEsc.ReplaceAllString(sb.String(), "")
}

// P0-A remote control surface: scope and capability boundaries.
// A provision-scoped token must not reach input/stop tools, and a
// token without the admin domain capability must fail closed
// before any session is touched.
func TestMCPHTTPControlScopeAndCapability(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	provOnly := f.mint("op-1", f.res, "metis:provision", time.Now().Add(time.Hour), nil)

	inputArgs := map[string]any{
		"session": "pty:nowhere", "text": "hi", "actor": "cloud", "request_id": "ws-1"}
	_, out := f.call(provOnly, "2024-11-05", mcpCallBody(1, "metis_terminal_input", inputArgs))
	raw, _ := json.Marshal(out["error"])
	if !strings.Contains(string(raw), "metis:input") {
		t.Fatalf("provision-scoped input must name the missing input scope: %s", raw)
	}
	_, out = f.call(provOnly, "2024-11-05", mcpCallBody(2, "metis_send", map[string]any{
		"session": "nowhere", "text": "hi", "actor": "cloud", "request_id": "ws-2"}))
	raw, _ = json.Marshal(out["error"])
	if !strings.Contains(string(raw), "metis:input") {
		t.Fatalf("provision-scoped send must name the missing input scope: %s", raw)
	}
	_, out = f.call(provOnly, "2024-11-05", mcpCallBody(3, "metis_session_stop", map[string]any{
		"session": "pty:nowhere", "actor": "cloud", "request_id": "ws-3"}))
	raw, _ = json.Marshal(out["error"])
	if !strings.Contains(string(raw), "metis:stop") {
		t.Fatalf("provision-scoped stop must name the missing stop scope: %s", raw)
	}

	// Right scopes, no admin capability: fail closed, no side effects.
	weak := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "message"})
	weakTok := weak.mint("op-9", weak.res, "metis:input metis:stop", time.Now().Add(time.Hour), nil)
	_, out = weak.call(weakTok, "2024-11-05", mcpCallBody(1, "metis_terminal_input", inputArgs))
	raw, _ = json.Marshal(out)
	if !strings.Contains(strings.ToLower(string(raw)), "admin") {
		t.Fatalf("input without the admin capability must fail on the capability, got: %s", raw)
	}
	_, out = weak.call(weakTok, "2024-11-05", mcpCallBody(2, "metis_session_stop", map[string]any{
		"session": "pty:nowhere", "actor": "cloud", "request_id": "ws-4"}))
	raw, _ = json.Marshal(out)
	if !strings.Contains(strings.ToLower(string(raw)), "admin") {
		t.Fatalf("stop without the admin capability must fail on the capability, got: %s", raw)
	}
}

// P0-A negative transport guards on seeded sessions: a rival human
// lease blocks remote delivery (never interleaves), a stale
// generation refuses before any lease is touched, an identical
// retry converges, and a stopped worker refuses further input.
func TestMCPHTTPControlGuards(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	tok := f.mint("op-1", f.res, "metis:input metis:stop", time.Now().Add(time.Hour), nil)
	// Owner-scoped policy: these seeded sessions belong to the
	// calling controller, so the test keeps exercising the lease
	// and generation layers beneath the ownership fence.
	func() {
		db, err := openTasksDB()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		for _, s := range []string{"g1", "g2"} {
			if other, err := claimProvisionOwner(db, s, "jwt:op-1"); err != nil || other {
				t.Fatal(err)
			}
		}
	}()
	call := func(id any, name string, args map[string]any) map[string]any {
		_, out := f.call(tok, "2024-11-05", mcpCallBody(id, name, args))
		return out
	}

	// Rival human lease: remote delivery conflicts, naming the holder.
	seedLiveSelfPtyRow(t, "g1")
	openFifoReader(t, "g1")
	if err := ptyAcquireLease("g1", "human:operator"); err != nil {
		t.Fatalf("seed human lease: %v", err)
	}
	out := call(1, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "cloud-sneak", "actor": "cloud", "request_id": "g-1"})
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "human:operator") {
		t.Fatalf("rival lease must refuse naming the holder, got: %s", raw)
	}
	// A different controller never reaches the lease layer: the
	// owner fence refuses before arbitration is consulted.
	ptyClearLease("g1")
	if err := ptyAcquireLease("g1", "mcp:jwt:op-1:cloud"); err != nil {
		t.Fatalf("seed first-principal lease: %v", err)
	}
	other := f.mint("op-2", f.res, "metis:input", time.Now().Add(time.Hour), nil)
	_, hijack := f.call(other, "2024-11-05", mcpCallBody(2, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "intrude", "actor": "intruder", "request_id": "g-2"}))
	hraw, _ := json.Marshal(hijack)
	if !strings.Contains(string(hraw), "another controller") {
		t.Fatalf("second-controller input must refuse on ownership, got: %s", hraw)
	}
	// Lease arbitration still applies beneath the fence: the same
	// owner under a different actor identity conflicts instead
	// of interleaving.
	_, hijack = f.call(tok, "2024-11-05", mcpCallBody(2, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "intrude", "actor": "intruder", "request_id": "g-2"}))
	hraw, _ = json.Marshal(hijack)
	if !strings.Contains(string(hraw), "op-1") {
		t.Fatalf("same-owner second-actor input must conflict on the first lease, got: %s", hraw)
	}
	ptyClearLease("g1")

	// Stale generation refuses before touching the transport.
	meta, merr := ptyGet("g1")
	if merr != nil {
		t.Fatalf("pty get: %v", merr)
	}
	out = call(3, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "hi", "actor": "cloud",
		"request_id": "g-3", "generation": meta.Generation + 1})
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), "generation changed") {
		t.Fatalf("stale generation must be refused as such, got: %s", raw)
	}

	// Identical retry converges with replay=true; a reused key
	// with different text conflicts instead of double-sending.
	first := call(4, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "once-only", "actor": "cloud", "request_id": "g-4"})
	fraw, _ := json.Marshal(first)
	if strings.Contains(string(fraw), `replay\":true`) {
		t.Fatalf("fresh input must not be a replay: %s", fraw)
	}
	second := call(5, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "once-only", "actor": "cloud", "request_id": "g-4"})
	sraw, _ := json.Marshal(second)
	if !strings.Contains(string(sraw), `replay\":true`) {
		t.Fatalf("identical retry must converge with replay=true, got: %s", sraw)
	}
	out = call(6, "metis_terminal_input", map[string]any{
		"session": "pty:g1", "text": "something else", "actor": "cloud", "request_id": "g-4"})
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), "conflict") {
		t.Fatalf("reused key with different text must conflict, got: %s", raw)
	}

	// Stop ends the worker; further input is refused, never queued.
	// (A dead-seeded row exercises the domain stop path without a
	// live supervisor process; the live-supervisor stop is proven
	// by TestMCPHTTPControlRoundTrip below.)
	seedPtyRow(t, "g2", t.TempDir())
	stopOut := call(7, "metis_session_stop", map[string]any{
		"session": "pty:g2", "actor": "cloud", "request_id": "g-5", "generation": 0})
	sraw, _ = json.Marshal(stopOut)
	if !strings.Contains(string(sraw), "stopped") {
		t.Fatalf("remote stop must report stopped, got: %s", sraw)
	}
	out = call(8, "metis_terminal_input", map[string]any{
		"session": "pty:g2", "text": "after death", "actor": "cloud", "request_id": "g-6"})
	raw, _ = json.Marshal(out)
	if strings.Contains(string(raw), `replay\":true`) || strings.Contains(string(raw), "queued") {
		t.Fatalf("stopped worker must refuse further input, got: %s", raw)
	}
}

// P0-A happy path over the authenticated transport against a real
// muse-code echo worker: provision, inspect, instruct, observe the
// marker in bounded output, stop. No fake terminal, no parked
// sleeper, no synthetic completion.
func TestMCPHTTPControlRoundTrip(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": "admin"})
	tok := f.mint("op-1", f.res, "metis:provision metis:input metis:stop", time.Now().Add(time.Hour), nil)
	call := func(id any, name string, args map[string]any) map[string]any {
		_, out := f.call(tok, "2024-11-05", mcpCallBody(id, name, args))
		return out
	}

	// Provision the real echo worker remotely.
	out := call(1, "metis_provision", map[string]any{
		"session": "rctl", "actor": "cloud", "request_id": "rctl-1",
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "model": "spark", "backend": "pty"})
	raw, _ := json.Marshal(out)
	if !strings.Contains(string(raw), "running") {
		t.Fatalf("remote provision must admit running: %s", raw)
	}
	t.Cleanup(func() { _ = ptyStop("rctl") })

	// Inspect: bounded status names the live worker.
	out = call(2, "metis_terminal", map[string]any{"session": "pty:rctl", "op": "status"})
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), "rctl") {
		t.Fatalf("remote status must name the worker: %s", raw)
	}

	// Readiness gate: the echo TUI boots asynchronously
	// (trust check, skills load). Poll bounded remote output
	// until the interactive prompt renders — only then is a
	// follow-up instruction genuinely receivable.
	var interactive bool
	for i := 0; i < 40 && !interactive; i++ {
		out = call(3, "metis_terminal", map[string]any{"session": "pty:rctl", "op": "output"})
		raw, _ = json.Marshal(out)
		if strings.Contains(string(raw), "❯") {
			interactive = true
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if !interactive {
		t.Fatalf("worker never reached the interactive prompt: %.400s", raw)
	}
	// Instruct through the authorized path: text plus Enter
	// (\r, matching tmux Enter semantics in raw mode; \n is
	// Ctrl-J and does not submit). The admitted outcome is
	// honest (queued with an op id), never claimed agent work.
	out = call(4, "metis_terminal_input", map[string]any{
		"session": "pty:rctl", "text": "remote-followup-marker\r",
		"actor":   "cloud", "request_id": "rctl-2"})
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), "queued") && !strings.Contains(string(raw), "confirmed") {
		t.Fatalf("remote input not admitted: %s", raw)
	}
	// Observe: the TUI renders the submitted instruction and the
	// echo reply in bounded remote output. In raw mode there is
	// no line-discipline echo, so the marker on screen proves
	// the worker application processed it.
	var seen string
	for i := 0; i < 30 && seen == ""; i++ {
		out = call(5, "metis_terminal", map[string]any{"session": "pty:rctl", "op": "output"})
		raw, _ = json.Marshal(out)
		if text := httpTermText(raw); strings.Contains(text, "remote-followup-marker") {
			seen = text
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if seen == "" {
		t.Fatalf("remote output never showed the delivered marker")
	}

	// Stop through the scoped path; the worker is gone after.
	out = call(6, "metis_session_stop", map[string]any{
		"session": "pty:rctl", "actor": "cloud", "request_id": "rctl-3"})
	raw, _ = json.Marshal(out)
	if !strings.Contains(string(raw), "stopped") {
		t.Fatalf("remote stop must report stopped, got: %s", raw)
	}
	if m, merr := ptyGet("rctl"); merr == nil && ptyWorkerAlive(m) {
		t.Fatalf("worker still alive after remote stop: %+v", m.Status)
	}
	// P0-D: one controller timeline shows the whole chronology —
	// provision, instruction delivery, and stop — under the
	// verified JWT principal with the actual dispositions. No
	// admission is presented as completion: the input row carries
	// queued/confirmed, never agent work.
	feed, ferr := listLatestOrchEvents(500)
	if ferr != nil {
		t.Fatalf("feed read: %v", ferr)
	}
	rows := map[string]string{}
	for _, e := range feed {
		if e.OriginPrincipal != "jwt:op-1" {
			continue
		}
		switch e.EventType + "/" + e.RequestID {
		case "provision.requested/rctl-1",
			"terminal.input/rctl-2",
			"session.stop/rctl-3":
			rows[e.EventType+"/"+e.RequestID] = e.TargetAgent + "|" + e.Disposition
		}
	}
	if got := rows["provision.requested/rctl-1"]; got != "rctl|running" && got != "rctl|admitted" && got != "rctl|replayed" {
		t.Fatalf("timeline provision row wrong: %q (all: %v)", got, rows)
	}
	if got := rows["terminal.input/rctl-2"]; got != "pty:rctl|queued" && got != "pty:rctl|confirmed" {
		t.Fatalf("timeline input row must carry the honest delivery state, got: %q", got)
	}
	if got := rows["session.stop/rctl-3"]; got != "pty:rctl|stopped" {
		t.Fatalf("timeline stop row wrong: %q (all: %v)", got, rows)
	}
}

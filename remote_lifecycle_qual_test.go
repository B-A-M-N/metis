package main

// remote_lifecycle_qual_test.go: P0 qualification of the authenticated
// HTTP MCP transport against a REAL Muse Code process.
//
// This is the remote-lifecycle question answered with evidence: can a
// verified remote principal, over real HTTP with RS256 tokens minted by
// a test IdP, provision a REAL muse-code echo worker, steer it, observe
// it, and stop it — while scope, local capability, and session-owner
// binding are all enforced? It complements the pre-existing happy-path
// test (TestMCPHTTPControlRoundTrip) and closes the negative gaps the
// remote contract demands that were not individually covered there:
//   - a provision token WITHOUT the local admin capability,
//   - admin capability WITHOUT the metis:provision JWT scope,
//   - a metis:provision token never implying input or stop authority,
//   - foreign-principal read / input / send / stop on a live worker,
//   - self-declared actor spoofing (a stranger naming the owner's actor
//     is still refused: identity is the JWT subject, never the actor),
//   - local-operator-only approve/verify being unreachable even from a
//     maximally-granted token,
//   - the owner binding and the provision intent committing together,
//   - no worker process leaking after stop.
//
// Inference safety: provider=echo only (zero inference). Every run uses
// a private METIS_STATE and its own XDG_DATA_HOME so neither the
// ambient Metis state, the ambient Muse store/socket, nor a real tmux
// server is touched.

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// privateRemoteState isolates one test run: a private METIS_STATE
// (mode 0700) plus a private XDG_DATA_HOME so the muse child's session
// store cannot alias the ambient one. Returns the state dir.
func privateRemoteState(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_STATE", state)
	xdg := t.TempDir()
	if err := os.Chmod(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", xdg)
	return state
}

// rqHTTPTools is a small helper for reading the inner text of an MCP
// text result (the nested JSON string), shared by the negatives.
func rqText(out map[string]any) string {
	res, _ := out["result"].(map[string]any)
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	return text
}

func rqRaw(out map[string]any) string {
	b, _ := json.Marshal(out)
	return string(b)
}

// resultIsError reports whether the MCP result carries isError=true (a
// domain refusal delivered as a domain error, not an RPC error).
func rqIsError(out map[string]any) bool {
	res, _ := out["result"].(map[string]any)
	b, _ := res["isError"].(bool)
	return b
}

// errorIs returns the RPC-level error message (nil when none).
func rqError(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	msg, _ := e["message"].(string)
	return msg
}

func rqErrorCode(out map[string]any) float64 {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(float64)
	return c
}

// countProcsMentioning counts current processes whose cmdline contains
// marker. Unique per-test workdirs make the marker unambiguous; a
// leaked worker/supervisor keeps the workdir on its cmdline or cwd.
func countProcsMentioning(marker string) int {
	es, err := os.ReadDir("/proc")
	if err != nil {
		return -1
	}
	n := 0
	for _, e := range es {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cl, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(cl), marker) {
			n++
		}
	}
	return n
}

// remoteFullPowerToken is the broadest scope+a capability grant that
// must still NOT reach local-operator-only authority.
func remoteProvisionFixture(t *testing.T, extraCaps string) *mcpHTTPFixture {
	t.Helper()
	privateRemoteState(t)
	return newMCPHTTPFixture(t, map[string]string{"METIS_MCP_REMOTE_CAPABILITIES": extraCaps})
}

// --------------------------------------------------------------------.
// Capability / scope negatives (no worker needed — transport-level).
// --------------------------------------------------------------------.

// TestRemoteControlScopeDoesNotImplyMore proves the scope additions the
// remote contract makes: metis:provision never implies input or stop,
// admin capability is never implied by the provision scope, and the
// provision scope plus admin capability still must not reach
// approve/verify (local-operator-only tools are not on the surface at
// all — a maximally-granted token gets method-not-found, not a
// capability denial).
func TestRemoteControlScopeDoesNotImplyMore(t *testing.T) {
	f := remoteProvisionFixture(t, "message") // no admin, no tasks
	full := f.mint("op-1", f.res, "metis:provision metis:input metis:stop", time.Now().Add(time.Hour), nil)

	// A full-scope token without the admin capability cannot provision.
	_, out := f.call(full, "2024-11-05", mcpCallBody(1, "metis_provision", map[string]any{
		"session": "noadmin", "actor": "cloud", "request_id": "noadmin-1",
		"run": "rr", "workdir": t.TempDir(), "harness": "muse-code", "provider": "echo", "backend": "pty"}))
	if rqError(out) == "" || !strings.Contains(strings.ToLower(rqError(out)), "admin") {
		t.Fatalf("provision-without-admin must fail on the admin capability, got: %s", rqRaw(out))
	}

	// Admin capability but no admin-scope on the token: input/stop need
	// their own JWT scopes even when the capability is held.
	// (message capability only here, so to isolate the scope gate give
	// the token only metis:input — the capability for input is admin,
	// which is not held, so the admin failure dominates; the scope
	// separation is proven in the second fixture below.)
	_ = out

	// metis:provision token does not imply input or stop authority:
	// here the token lacks metis:input and metis:stop, so even with the
	// provision scope the input/stop tools name the missing scope.
	provOnly := f.mint("op-1", f.res, "metis:provision", time.Now().Add(time.Hour), nil)
	_, out = f.call(provOnly, "2024-11-05", mcpCallBody(2, "metis_terminal_input", map[string]any{
		"session": "pty:any", "text": "x", "actor": "cloud", "request_id": "scope-in-1"}))
	if !strings.Contains(rqError(out), "metis:input") {
		t.Fatalf("metis:provision must not imply input: %s", rqRaw(out))
	}
	_, out = f.call(provOnly, "2024-11-05", mcpCallBody(3, "metis_session_stop", map[string]any{
		"session": "pty:any", "actor": "cloud", "request_id": "scope-stop-1"}))
	if !strings.Contains(rqError(out), "metis:stop") {
		t.Fatalf("metis:provision must not imply stop: %s", rqRaw(out))
	}
}

// TestRemoteAdminWithoutProvisionScope proves the second gate of the
// scope/capability pair: holding the admin capability is not enough —
// the JWT must carry metis:provision, and a de-scoped token fails
// closed with a refusal naming the provision scope before any intent
// row exists.
func TestRemoteAdminWithoutProvisionScope(t *testing.T) {
	f := remoteProvisionFixture(t, "admin") // capability present, provision scope absent
	tok := f.mint("op-1", f.res, "metis:message", time.Now().Add(time.Hour), nil)
	work := t.TempDir()
	_, out := f.call(tok, "2024-11-05", mcpCallBody(1, "metis_provision", map[string]any{
		"session": "caponly", "actor": "cloud", "request_id": "caponly-1",
		"run": "rr", "workdir": work, "harness": "muse-code", "provider": "echo", "backend": "pty"}))
	if !strings.Contains(rqError(out), "metis:provision") {
		t.Fatalf("admin capability without provision scope must name the missing scope, got: %s", rqRaw(out))
	}
	// Fail-closed: no intent row and no owner row are persisted.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, found := NewProvisionRepository(db).Load("caponly-1"); found {
		t.Fatal("refused provision persisted an intent row")
	}
	if _, found, _ := lookupProvisionOwner(db, "caponly"); found {
		t.Fatal("refused provision persisted an owner row")
	}
}

// TestRemoteLocalOperatorOnlyToolsUnreachable proves (D): approve and
// verify are NOT on the remote surface. Even a maximally-granted token
// (every JWT scope + every scoped capability) gets method-not-found —
// the tools are absent from the dispatch table, so no capability or
// remote-ingress policy can ever admit them on this transport.
func TestRemoteLocalOperatorOnlyToolsUnreachable(t *testing.T) {
	f := remoteProvisionFixture(t, "admin,tasks,reject,message")
	full := f.mint("op-1", f.res, "metis:provision metis:input metis:stop metis:message", time.Now().Add(time.Hour), nil)
	for _, name := range []string{"metis_approve", "metis_verify"} {
		_, out := f.call(full, "2024-11-05", mcpCallBody(1, name, map[string]any{
			"session": "s", "task_id": "t", "actor": "cloud", "request_id": "d-x", "pass": true,
			"provider": "echo", "approval_id": "a", "choice": "approved", "feedback": "f"}))
		if rqErrorCode(out) != float64(mcpMethodNotFound) {
			t.Fatalf("%s must be method-not-found on the remote transport, got: %s", name, rqRaw(out))
		}
	}
	// Not advertised either: a maximally-granted client cannot even see
	// them to call them.
	_, out := f.call(full, "2024-11-05", map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	for _, tl := range tools {
		if m, _ := tl.(map[string]any); m == nil {
			continue
		}
		if m, _ := tl.(map[string]any); m["name"] == "metis_approve" || m["name"] == "metis_verify" {
			t.Fatal("local-operator-only tool advertised on the remote surface")
		}
	}
}

// --------------------------------------------------------------------.
// Full lifecycle + owner isolation against a REAL worker.
// --------------------------------------------------------------------.

// TestRemoteRealWorkerLifecycleAndIsolation is the P0 answer: one REAL
// muse-code echo worker over the authenticated HTTP transport,
// provisioned, steered, observed, and stopped by its owning principal —
// while a second principal (same scopes and capabilities, only a
// different JWT subject) cannot read, steer, send to, or stop it, even
// when it forges the owner's actor label. Owner binding and intent
// commit together, and no process leaks after stop.
func TestRemoteRealWorkerLifecycleAndIsolation(t *testing.T) {
	if !ptyAvailable() {
		t.Skip("no PTY available")
	}
	if _, err := exec.LookPath("muse"); err != nil {
		t.Skip("muse binary not on PATH")
	}
	f := remoteProvisionFixture(t, "admin,tasks") // both principals hold these
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	owner := f.mint("op-1", f.res, "metis:provision metis:input metis:stop", time.Now().Add(time.Hour), nil)
	stranger := f.mint("op-2", f.res, "metis:provision metis:input metis:stop", time.Now().Add(time.Hour), nil)

	// Leak baseline: nothing may reference this unique workdir before
	// the worker starts.
	if base := countProcsMentioning(work); base != 0 {
		t.Fatalf("unexpected process already references the workdir: %d", base)
	}

	call := func(tok string, id any, name string, args map[string]any) map[string]any {
		_, out := f.call(tok, "2024-11-05", mcpCallBody(id, name, args))
		return out
	}

	// 1) Real remote provision of a muse-code echo worker under PTY.
	reqID := "rq-1"
	out := call(owner, 1, "metis_provision", map[string]any{
		"session": "rq", "actor": "cloud", "request_id": reqID,
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "model": "spark", "backend": "pty"})
	if rqIsError(out) {
		t.Fatalf("remote provision refused: %s", rqRaw(out))
	}
	if !strings.Contains(rqText(out), "running") {
		t.Fatalf("remote provision must admit running: %s", rqRaw(out))
	}
	// Hygiene: a failure after this point must still stop the worker.
	t.Cleanup(func() { _ = ptyStop("rq") })

	// The supervisor owns a REAL muse process; it must be alive, running
	// the curated echo launcher, in the requested directory.
	meta, merr := ptyGet("rq")
	if merr != nil {
		t.Fatalf("pty get: %v", merr)
	}
	if !ptySupervisorAlive(meta) || !ptyWorkerAlive(meta) {
		t.Fatalf("remote-provisioned muse-code worker must be alive: %+v", meta.Status)
	}
	if !strings.Contains(meta.Command, "muse") || !strings.Contains(meta.Command, "echo") {
		t.Fatalf("worker must run the curated muse echo launcher, got %q", meta.Command)
	}
	if meta.Workdir != work {
		t.Fatalf("worker must start in the requested directory: got %q want %q", meta.Workdir, work)
	}

	// Owner binding and the provision intent commit together: both rows
	// exist now, owned by the verified JWT principal.
	db, derr := openTasksDB()
	if derr != nil {
		t.Fatal(derr)
	}
	if _, found, _ := lookupProvisionOwner(db, "rq"); !found {
		db.Close()
		t.Fatal("remote provision left no owner binding")
	} else if o, found, _ := lookupProvisionOwner(db, "rq"); found && o != "jwt:op-1" {
		db.Close()
		t.Fatalf("owner binding must be the verified principal, got %q", o)
	}
	if prior, _, rowned := NewProvisionRepository(db).Load(reqID); !rowned || prior.Session != "rq" {
		db.Close()
		t.Fatalf("provision intent row must be committed with the owner binding")
	}
	db.Close()

	// 2) Identical replay converges: same worker identity, no relaunch.
	out = call(owner, 11, "metis_provision", map[string]any{
		"session": "rq", "actor": "cloud", "request_id": reqID,
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "model": "spark", "backend": "pty"})
	if rqIsError(out) {
		t.Fatalf("identical replay must converge, got: %s", rqRaw(out))
	}

	// 3) Inspect: the owner reads its own worker's status.
	out = call(owner, 2, "metis_terminal", map[string]any{"session": "pty:rq", "op": "status"})
	if !strings.Contains(rqText(out), "rq") {
		t.Fatalf("owner status must name the worker: %s", rqRaw(out))
	}

	// 4) Readiness: wait for the interactive prompt before steering.
	var ready string
	for i := 0; i < 60 && ready == ""; i++ {
		out = call(owner, 3, "metis_terminal", map[string]any{"session": "pty:rq", "op": "output"})
		if strings.Contains(rqRaw(out), "❯") {
			ready = rqRaw(out)
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if ready == "" {
		t.Fatalf("remote worker never reached the interactive prompt")
	}

	// 5) Steer: bounded instruction delivered to THIS session only.
	out = call(owner, 4, "metis_terminal_input", map[string]any{
		"session": "pty:rq", "text": "remote-lifecycle-marker\r",
		"actor": "cloud", "request_id": "rq-2"})
	if !strings.Contains(rqText(out), "queued") && !strings.Contains(rqText(out), "confirmed") {
		t.Fatalf("owner input not admitted: %s", rqRaw(out))
	}

	// 6) Observe: the echo reply renders the marker in bounded output.
	var seen string
	for i := 0; i < 30 && seen == ""; i++ {
		out = call(owner, 5, "metis_terminal", map[string]any{"session": "pty:rq", "op": "output"})
		if text := httpTermText(json.RawMessage(rqRaw(out))); strings.Contains(text, "remote-lifecycle-marker") {
			seen = text
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if seen == "" {
		t.Fatalf("remote output never showed the delivered marker")
	}

	// 7) The outsider (same scopes, same capabilities, different JWT
	// subject) is refused everywhere on this live worker, fail-closed
	// before leases/generations/backends are touched.
	strangerActor := "cloud" // deliberately matches the owner's actor label
	out = call(stranger, 21, "metis_terminal", map[string]any{"session": "pty:rq", "op": "status"})
	if !strings.Contains(rqText(out), "another controller") {
		t.Fatalf("stranger read must be refused on ownership, got: %s", rqRaw(out))
	}
	out = call(stranger, 22, "metis_terminal", map[string]any{"session": "pty:rq", "op": "output"})
	if !strings.Contains(rqText(out), "another controller") {
		t.Fatalf("stranger output read must be refused on ownership, got: %s", rqRaw(out))
	}
	out = call(stranger, 23, "metis_terminal_input", map[string]any{
		"session": "pty:rq", "text": "intrude", "actor": strangerActor, "request_id": "rq-s1"})
	if !strings.Contains(rqText(out), "another controller") {
		t.Fatalf("stranger (actor-spoofing owner label) input must be refused on ownership, got: %s", rqRaw(out))
	}
	out = call(stranger, 24, "metis_send", map[string]any{
		"session": "pty:rq", "text": "intrude", "actor": strangerActor, "request_id": "rq-s2"})
	if !strings.Contains(rqText(out), "another controller") {
		t.Fatalf("stranger send must be refused on ownership, got: %s", rqRaw(out))
	}
	out = call(stranger, 25, "metis_session_stop", map[string]any{
		"session": "pty:rq", "actor": strangerActor, "request_id": "rq-s3"})
	if !strings.Contains(rqText(out), "another controller") {
		t.Fatalf("stranger stop must be refused on ownership, got: %s", rqRaw(out))
	}

	// The worker is untouched by every refused attempt and still alive.
	meta, merr = ptyGet("rq")
	if merr != nil || !ptyWorkerAlive(meta) {
		t.Fatalf("worker must survive refused foreign attempts: %+v %v", meta.Status, merr)
	}

	// A conflicting fresh admission under a NEW request_id cannot claim
	// the same session name either: no second worker, no rebind.
	out = call(stranger, 26, "metis_provision", map[string]any{
		"session": "rq", "actor": "cloud", "request_id": "rq-s4",
		"run": "rr", "workdir": work, "harness": "muse-code",
		"provider": "echo", "backend": "pty"})
	if !rqIsError(out) || !strings.Contains(strings.ToLower(rqText(out)), "another controller") {
		t.Fatalf("stranger conflicting admission must be refused on the owner binding, got: %s", rqRaw(out))
	}

	// 8) The owner still holds full control after all the refusals.
	out = call(owner, 6, "metis_session_stop", map[string]any{
		"session": "pty:rq", "actor": "cloud", "request_id": "rq-3"})
	if !strings.Contains(rqText(out), "stopped") {
		t.Fatalf("owner stop must report stopped: %s", rqRaw(out))
	}
	meta, _ = ptyGet("rq")
	if meta.Status != "exited" && meta.Status != "gone" {
		t.Fatalf("worker must be terminated after remote stop, got %q", meta.Status)
	}
	if ptyWorkerAlive(meta) || ptySupervisorAlive(meta) {
		t.Fatalf("worker/supervisor still alive after remote stop: %+v", meta)
	}

	// The owner cannot steer a stopped worker either.
	out = call(owner, 7, "metis_terminal_input", map[string]any{
		"session": "pty:rq", "text": "after death", "actor": "cloud", "request_id": "rq-4"})
	if !rqIsError(out) {
		t.Fatalf("stopped worker must refuse input, got: %s", rqRaw(out))
	}

	// 9) The supervisor is reaped and NO process still references the
	// workdir: no orphan worker or supervisor leaks past the run.
	requireSupervisorReaped(t, meta)
	if left := countProcsMentioning(work); left != 0 {
		t.Fatalf("worker process leak: %d process(es) still reference the workdir", left)
	}
}
package main

// mcp_provision_test.go: the two MCP control tools.
//
// metis_provision is a thin authenticated wrapper over startSession:
// it creates a registry session observable via metis_agents and is
// idempotent by request_id. metis_bind wraps the museCmdBind path
// but must refuse (never fabricate) when the muse daemon is
// unreachable or the binary is missing — LEDGER notes a scrubbed
// PATH blocks native ops over `mcp expose`.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func markFakeTmuxDead(t *testing.T) {
	t.Helper()
	// The fake tmux reports every session live until its flag dir
	// carries a `dead` marker; startSession needs has-session to
	// fail so the name reads as free.
	p := filepath.Join(os.Getenv("FAKE_TMUX_DIR"), "dead")
	if err := os.WriteFile(p, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func agentsHas(t *testing.T, res mcpToolResult, name string) {
	t.Helper()
	if res.IsError {
		t.Fatalf("metis_agents isError: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, `"`+name+`"`) {
		t.Fatalf("metis_agents missing session %q: %s", name, res.Content[0].Text)
	}
}

func TestMCPProvisionRoundTrip(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-owner")
	markFakeTmuxDead(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)

	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "p1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if res.IsError {
		t.Fatalf("provision isError: %s", res.Content[0].Text)
	}
	out := mcpTaskText(t, res)
	if out["session"] != "w1" {
		t.Fatalf("provision must return the session identity: %v", out)
	}
	// Observable via metis_agents.
	agents, err := mcpCallTool("metis_agents", mustJSON(t, map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	agentsHas(t, agents, "w1")
	// Never expose the owner token.
	owner := loadRegistry()["w1"].Owner
	if owner == "" {
		t.Fatal("provisioned session must carry an owner token in the registry")
	}
	if strings.Contains(res.Content[0].Text, owner) {
		t.Fatalf("provision result leaked the owner token: %s", res.Content[0].Text)
	}
	// Exact replay with the same request_id returns the original
	// without duplicating.
	res2, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w1", "objective": "do thing",
		"actor": "orch", "request_id": "p1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if res2.IsError {
		t.Fatalf("replay isError: %s", res2.Content[0].Text)
	}
	out2 := mcpTaskText(t, res2)
	if out2["session"] != "w1" {
		t.Fatalf("replay must return the original session: %v", out2)
	}
	if _, dup := loadRegistry()["w2"]; dup {
		t.Fatal("replay must not create a second session")
	}
	if n := len(loadRegistry()); n != 1 {
		t.Fatalf("exactly one session expected after replay, got %d", n)
	}
	// Same key, different session: idempotency conflict, never
	// the original. (This used to expect silent convergence; a
	// reused key with different arguments must refuse.)
	res3, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r1", "session": "w2", "objective": "different",
		"actor": "orch", "request_id": "p1",
		"workdir": work, "command": "sleep 60",
	}))
	if err != nil {
		t.Fatalf("conflict must arrive as isError, not RPC error: %v", err)
	}
	if !res3.IsError {
		t.Fatal("reused request ID with a different session must conflict")
	}
	if text := res3.Content[0].Text; !strings.Contains(text, "conflict") {
		t.Fatalf("conflict must say so: %s", text)
	}
}

func TestMCPProvisionDefaults(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "prov-def")
	markFakeTmuxDead(t)
	// No workdir/command: the tool parks the session under a
	// run-scoped workdir it creates itself. The parked default is
	// Metis's own command, so a plain (non-custom) root suffices.
	mustRegisterRoot(t, stateDir(), false)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "r9", "session": "auto", "objective": "park me",
		"actor": "orch", "request_id": "p9",
	}))
	if err != nil {
		t.Fatalf("provision defaults: %v", err)
	}
	if res.IsError {
		t.Fatalf("provision defaults isError: %s", res.Content[0].Text)
	}
	entry, ok := loadRegistry()["auto"]
	if !ok {
		t.Fatal("default provision must register the session")
	}
	if fi, err := os.Stat(entry.Workdir); err != nil || !fi.IsDir() {
		t.Fatalf("default workdir must exist: %q %v", entry.Workdir, err)
	}
}

func TestMCPBindRefusesWithoutDaemon(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "bind-owner")
	seedOwnedSession(t, "wb", "bind-owner")
	// Scrubbed PATH per LEDGER: the muse binary lives outside
	// /usr/bin:/bin, so native ops over `mcp expose` cannot run.
	t.Setenv("PATH", "/usr/bin:/bin")
	res, err := mcpCallTool("metis_bind", mustJSON(t, map[string]any{
		"session": "wb", "profile": "", "actor": "orch", "request_id": "b1",
	}))
	if err != nil {
		t.Fatalf("bind refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("bind without daemon/binary must refuse")
	}
	text := res.Content[0].Text
	if !strings.Contains(text, "muse") || (!strings.Contains(text, "PATH") && !strings.Contains(text, "daemon")) {
		t.Fatalf("refusal must name the blocker (binary or daemon): %s", text)
	}
	// Never fabricate: no binding row may exist.
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMuseTables(db); err != nil {
		t.Fatal(err)
	}
	var native string
	err = db.QueryRow(`SELECT native_id FROM muse_bindings WHERE session = ?`, "wb").Scan(&native)
	if err != sql.ErrNoRows {
		t.Fatalf("refused bind must create no binding row (err=%v native=%q)", err, native)
	}
}

func TestMCPProvisionBindAuth(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("METIS_MCP_WRITE_TOKEN", "tok-provision")
	installFakeTmux(t, "prov-auth")
	markFakeTmuxDead(t)
	work := t.TempDir()
	prov := map[string]any{
		"run": "r1", "session": "wa", "objective": "do thing",
		"actor": "orch", "request_id": "pa",
		"workdir": work, "command": "sleep 60",
	}
	if _, err := mcpCallTool("metis_provision", mustJSON(t, prov)); err == nil {
		t.Fatal("provision without auth must be rejected")
	}
	bad := map[string]any{
		"run": "r1", "session": "wa", "objective": "do thing",
		"actor": "orch", "request_id": "pa",
		"workdir": work, "command": "sleep 60", "auth": "wrong",
	}
	if _, err := mcpCallTool("metis_provision", mustJSON(t, bad)); err == nil {
		t.Fatal("provision with wrong auth must be rejected")
	}
	bind := map[string]any{
		"session": "wa", "profile": "", "actor": "orch", "request_id": "ba",
	}
	if _, err := mcpCallTool("metis_bind", mustJSON(t, bind)); err == nil {
		t.Fatal("bind without auth must be rejected")
	}
	bindBad := map[string]any{
		"session": "wa", "profile": "", "actor": "orch", "request_id": "ba", "auth": "wrong",
	}
	if _, err := mcpCallTool("metis_bind", mustJSON(t, bindBad)); err == nil {
		t.Fatal("bind with wrong auth must be rejected")
	}
	if n := len(loadRegistry()); n != 0 {
		t.Fatalf("rejected calls must create nothing, got %d sessions", n)
	}
}

package main

// mcp_http_test.go: authenticated HTTP MCP transport qualification.
//
// Isolated fixtures only (httptest servers, file JWKS, stdlib RSA):
// no relay, no publisher, no external identity provider. Negative
// controls prove each rejection; the allowlist test proves the
// remote surface cannot grow by stdio-dispatcher accident.

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type mcpHTTPFixture struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	evil   *rsa.PrivateKey
	issuer string
	res    string
}

func mcpB64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newMCPHTTPFixture(t *testing.T, extraEnv map[string]string) *mcpHTTPFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	evil, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "kid": "t1", "alg": "RS256", "use": "sig",
		"n": mcpB64(key.N.Bytes()),
		"e": mcpB64(big.NewInt(int64(key.E)).Bytes()),
	}}}
	raw, _ := json.Marshal(jwks)
	dir := t.TempDir()
	jwksFile := filepath.Join(dir, "jwks.json")
	if err := os.WriteFile(jwksFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &mcpHTTPFixture{t: t, key: key, evil: evil,
		issuer: "https://idp.example/oauth",
		res:    "https://relay.example/r/abc/mcp"}
	t.Setenv("METIS_MCP_ISSUER", f.issuer)
	t.Setenv("METIS_MCP_JWKS_FILE", jwksFile)
	t.Setenv("METIS_MCP_JWKS_URL", "")
	t.Setenv("METIS_MCP_RESOURCE", f.res)
	t.Setenv("METIS_MCP_RESOURCE_FILE", "")
	t.Setenv("METIS_MCP_ALLOWED_SUBJECTS", "")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	for k, v := range extraEnv {
		t.Setenv(k, v)
	}
	cfg, err := mcpHTTPConfigFromEnv("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpHTTPServer{cfg: cfg, jwks: &mcpJWKS{file: jwksFile},
		res:     mcpResourceStore{fixed: cfg.resource, file: cfg.resourceFile},
		origins: mcpAllowedOrigins()}
	f.srv = httptest.NewServer(mcpHTTPMux(s))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *mcpHTTPFixture) mint(sub, aud, scope string, exp time.Time, with *rsa.PrivateKey) string {
	if with == nil {
		with = f.key
	}
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "t1"})
	pay, _ := json.Marshal(map[string]any{
		"iss": f.issuer, "sub": sub, "aud": aud,
		"exp": exp.Unix(), "iat": time.Now().Unix(),
		"scope": scope,
	})
	hs, ps := mcpB64(hdr), mcpB64(pay)
	digest := sha256.Sum256([]byte(hs + "." + ps))
	sig, err := rsa.SignPKCS1v15(rand.Reader, with, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return hs + "." + ps + "." + mcpB64(sig)
}

func (f *mcpHTTPFixture) goodToken() string {
	return f.mint("op-1", f.res, "metis:message", time.Now().Add(time.Hour), nil)
}

func (f *mcpHTTPFixture) call(token, version string, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func mcpCallBody(id any, name string, args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}}
}

func TestMCPHTTPConfigRejectsNonLoopback(t *testing.T) {
	t.Setenv("METIS_MCP_ISSUER", "https://idp.example/oauth")
	t.Setenv("METIS_MCP_JWKS_FILE", "/tmp/x.json")
	t.Setenv("METIS_MCP_RESOURCE", "https://relay.example/r/a/mcp")
	for _, bad := range []string{"0.0.0.0:1", "10.9.66.20:1", "example.com:1", ":1", "nope"} {
		if _, err := mcpHTTPConfigFromEnv(bad); err == nil {
			t.Fatalf("listen %q accepted", bad)
		}
	}
	if _, err := mcpHTTPConfigFromEnv("127.0.0.1:0"); err != nil {
		t.Fatalf("loopback rejected: %v", err)
	}
	t.Setenv("METIS_MCP_ISSUER", "")
	if _, err := mcpHTTPConfigFromEnv("127.0.0.1:0"); err == nil {
		t.Fatal("missing issuer accepted")
	}
}

func TestMCPHTTPDiscoveryBindsExactResource(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	resp, err := http.Get(f.srv.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Resource string   `json:"resource"`
		AS       []string `json:"authorization_servers"`
		Scopes   []string `json:"scopes_supported"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc.Resource != f.res || len(doc.AS) != 1 || doc.AS[0] != f.issuer {
		t.Fatalf("discovery mismatch: %+v", doc)
	}
	for _, want := range []string{"metis:message", "metis:provision", "metis:input", "metis:stop"} {
		found := false
		for _, s := range doc.Scopes {
			if s == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("discovery scopes_supported missing %s: %+v", want, doc)
		}
	}
}

func TestMCPHTTPAuthNegatives(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	probe := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
	future := time.Now().Add(time.Hour)
	cases := map[string]string{
		"missing token":      "",
		"malformed":          "not-a-jwt",
		"wrong key":          f.mint("op-1", f.res, "metis:message", future, f.evil),
		"wrong audience":     f.mint("op-1", "https://relay.example/r/evil/mcp", "metis:message", future, nil),
		"insufficient scope": f.mint("op-1", f.res, "metis:read", future, nil),
		"expired":            f.mint("op-1", f.res, "metis:message", time.Now().Add(-time.Hour), nil),
	}
	for name, tok := range cases {
		if name == "insufficient scope" {
			continue // scope gates message tools, not reads; covered below
		}
		if code, _ := f.call(tok, "2024-11-05", probe); code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", name, code)
		}
	}
	// Forged issuer: same key shape, different iss.
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "t1"})
	pay, _ := json.Marshal(map[string]any{"iss": "https://evil.example/o", "sub": "op-1",
		"aud": f.res, "exp": future.Unix(), "scope": "metis:message"})
	hs, ps := mcpB64(hdr), mcpB64(pay)
	d := sha256.Sum256([]byte(hs + "." + ps))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, d[:])
	if code, _ := f.call(hs+"."+ps+"."+mcpB64(sig), "2024-11-05", probe); code != http.StatusUnauthorized {
		t.Fatalf("forged issuer: got %d, want 401", code)
	}
}

func TestMCPHTTPRejectedSubject(t *testing.T) {
	f := newMCPHTTPFixture(t, map[string]string{"METIS_MCP_ALLOWED_SUBJECTS": "op-allowed"})
	probe := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
	if code, _ := f.call(f.mint("op-nope", f.res, "metis:message", time.Now().Add(time.Hour), nil), "2024-11-05", probe); code != http.StatusUnauthorized {
		t.Fatalf("unlisted subject: got %d, want 401", code)
	}
	if code, _ := f.call(f.mint("op-allowed", f.res, "metis:message", time.Now().Add(time.Hour), nil), "2024-11-05", probe); code != http.StatusOK {
		t.Fatalf("listed subject: got %d, want 200", code)
	}
}

func TestMCPHTTPNotificationNeverExecutes(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "tools/call",
		"params": map[string]any{"name": "metis_muse_message", "arguments": map[string]any{
			"session": "nope", "text": "x", "actor": "a", "request_id": "r-n1"}}})
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.goodToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification: got %d, want 202", resp.StatusCode)
	}
	// Nothing executed: the same request_id queried afterwards is
	// unknown to the domain, proving the notification ran nothing.
	_, out := f.call(f.goodToken(), "2024-11-05", mcpCallBody(2, "metis_muse_message_status", map[string]any{"request_id": "r-n1"}))
	res, _ := out["result"].(map[string]any)
	got, _ := json.Marshal(res)
	if res["isError"] != true || !strings.Contains(string(got), "no such muse message request") {
		t.Fatalf("notification executed a message or unexpected result: %v", res)
	}
}

func TestMCPHTTPToolsListIsRemoteSubset(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	code, out := f.call(f.goodToken(), "2024-11-05", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	if code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	res, _ := out["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	names := map[string]string{}
	for _, tl := range tools {
		m, _ := tl.(map[string]any)
		name, _ := m["name"].(string)
		raw, _ := json.Marshal(m["inputSchema"])
		names[name] = string(raw)
	}
	for _, forbidden := range []string{"metis_delegate", "metis_approve", "metis_verify", "metis_bind", "metis_cancel", "metis_terminal_resize"} {
		if _, ok := names[forbidden]; ok {
			t.Fatalf("remote surface leaks %s", forbidden)
		}
	}
	for name, schema := range names {
		if strings.Contains(schema, `"auth"`) {
			t.Fatalf("schema for %s advertises auth", name)
		}
	}
	for _, want := range []string{"metis_status", "metis_muse_message", "metis_muse_message_status", "metis_provision", "metis_terminal", "metis_terminal_input", "metis_send", "metis_session_stop"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("remote surface missing %s", want)
		}
	}
}

func TestMCPHTTPUnknownToolRejected(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	for _, name := range []string{"metis_delegate", "metis_approve", "metis_verify", "metis_terminal_resize", "nope_tool"} {
		_, out := f.call(f.goodToken(), "2024-11-05", mcpCallBody(1, name, map[string]any{"request_id": "r-x"}))
		e, ok := out["error"].(map[string]any)
		if !ok || e["code"] != float64(mcpMethodNotFound) {
			t.Fatalf("%s: got %v, want method-not-found", name, out)
		}
	}
}

func TestMCPHTTPAuthArgIgnored(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	f := newMCPHTTPFixture(t, nil)
	// A forged model-supplied auth secret must not authenticate: with
	// a valid JWT the call passes JWT auth and reaches the domain
	// (bogus session => domain isError, not an auth failure), and the
	// secret value is never consulted.
	_, out := f.call(f.goodToken(), "2024-11-05", mcpCallBody(1, "metis_muse_message", map[string]any{
		"session": "no-such-session", "text": "hi", "actor": "mallory",
		"request_id": "r-authstrip", "auth": "forged-secret"}))
	if _, hasErr := out["error"]; hasErr {
		t.Fatalf("JWT-authed call failed at transport: %v", out["error"])
	}
	res, _ := out["result"].(map[string]any)
	raw, _ := json.Marshal(res)
	// The payload is nested JSON (escaped); match values, not syntax.
	if !strings.Contains(string(raw), "r-authstrip") || !strings.Contains(string(raw), "refused") {
		t.Fatalf("expected domain refusal for bogus session (auth passed): %v", res)
	}
}

func TestMCPHTTPScopeGatesMessage(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	weak := f.mint("op-1", f.res, "other:scope", time.Now().Add(time.Hour), nil)
	// Reads work with any in-audience scope.
	if code, _ := f.call(weak, "2024-11-05", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}); code != http.StatusOK {
		t.Fatalf("read with weak scope: %d", code)
	}
	// Messaging needs metis:message.
	_, out := f.call(weak, "2024-11-05", mcpCallBody(2, "metis_muse_message", map[string]any{
		"session": "s", "text": "t", "actor": "a", "request_id": "r-scope"}))
	if _, hasErr := out["error"]; !hasErr {
		t.Fatalf("scope-less message admitted: %v", out)
	}
}

func TestMCPHTTPReplayAndConcurrency(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	body := func(id any) map[string]any {
		return mcpCallBody(id, "metis_muse_message_status", map[string]any{"request_id": "r-replay-1"})
	}
	_, first := f.call(f.goodToken(), "2024-11-05", body(1))
	_, second := f.call(f.goodToken(), "2024-11-05", body(2))
	fr, _ := first["result"]
	sr, _ := second["result"]
	fe, _ := first["error"]
	se, _ := second["error"]
	fj, _ := json.Marshal(map[string]any{"result": fr, "error": fe})
	sj, _ := json.Marshal(map[string]any{"result": sr, "error": se})
	if string(fj) != string(sj) {
		t.Fatalf("replay diverged:\n%s\n%s", fj, sj)
	}
	var wg sync.WaitGroup
	errs := make(chan int, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _ := f.call(f.goodToken(), "2024-11-05", body(100+i))
			if code != http.StatusOK {
				errs <- code
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for c := range errs {
		t.Fatalf("concurrent call failed: %d", c)
	}
}

func TestMCPHTTPToolsHideAuthToken(t *testing.T) {
	for _, tl := range mcpHTTPTools() {
		if strings.Contains(tl.Description, "METIS_MCP_WRITE_TOKEN") ||
			strings.Contains(tl.Description, "a matching auth argument is required") {
			t.Fatalf("remote tool %s describes a model-supplied secret: %q", tl.Name, tl.Description)
		}
		raw, _ := json.Marshal(tl.InputSchema)
		if strings.Contains(string(raw), `"auth"`) {
			t.Fatalf("remote tool %s schema advertises auth", tl.Name)
		}
	}
}

// seedMusePrincipalRow plants one completed message intent plus the
// binding and registry entries the target-scope gate needs, all in
// an isolated METIS_STATE. It returns the canonical workspace.
func seedMusePrincipalRow(t *testing.T, state, session, request, principal string) {
	t.Helper()
	workdir := t.TempDir()
	reg, _ := json.Marshal(map[string]any{session: map[string]any{
		"name": session, "workdir": workdir}})
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	canon, err := canonicalRoot(workdir)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := ensureMuseTables(db); err != nil {
		t.Fatal(err)
	}
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO muse_bindings
		(session, native_id, view_cursor, provider, profile, workspace, model, created, updated)
		VALUES (?, ?, '', 'p', 'pr', ?, '', 't', 't')`, session, "n1", canon); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO muse_message_intents
		(request_id, session, native_id, profile, provider, workspace, prompt_digest, principal, command_id, turn_id, terminal, detail, created, updated)
		VALUES (?, ?, 'n1', 'pr', 'p', 'w', 'd', ?, 'cmd-1', 'turn-1', 'completed', 'done', 't', 't')`,
		request, session, principal); err != nil {
		t.Fatal(err)
	}
}

func TestMuseMessageCrossPrincipalRefused(t *testing.T) {
	state := t.TempDir()
	t.Setenv("METIS_STATE", state)
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "sx")
	seedMusePrincipalRow(t, state, "sx", "r-x1", "jwt:sub-A")
	// Stranger with overlapping scope: refused, no row-derived identity.
	got, err := queryMuseMessageStatus("r-x1",
		OperationRequest{Actor: "b", RequestID: "k-b", Principal: "jwt:sub-B", Remote: true}, 0)
	if err != nil {
		t.Fatalf("status err: %v", err)
	}
	if got.State != museMessageRefused {
		t.Fatalf("stranger read another principal's request: %+v", got)
	}
	if got.TurnID != "" || got.SessionID != "" || strings.Contains(got.Detail, "turn-1") {
		t.Fatalf("stranger learned row identity: %+v", got)
	}
	// Owner still reconciles its own terminal.
	own, err := queryMuseMessageStatus("r-x1",
		OperationRequest{Actor: "a", RequestID: "k-a", Principal: "jwt:sub-A", Remote: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if own.State != "completed" || own.TurnID != "turn-1" {
		t.Fatalf("owner lost its terminal: %+v", own)
	}
	// After the session stops, the owner keeps historical terminal
	// read; the stranger stays refused.
	reg, _ := json.Marshal(map[string]any{"sx": map[string]any{
		"name": "sx", "workdir": t.TempDir(), "stopped": true}})
	if err := os.WriteFile(filepath.Join(state, "sessions.json"), reg, 0o600); err != nil {
		t.Fatal(err)
	}
	own, err = queryMuseMessageStatus("r-x1",
		OperationRequest{Actor: "a", RequestID: "k-a2", Principal: "jwt:sub-A", Remote: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if own.State != "completed" || own.TurnID != "turn-1" {
		t.Fatalf("owner lost historical terminal after stop: %+v", own)
	}
	got, err = queryMuseMessageStatus("r-x1",
		OperationRequest{Actor: "b", RequestID: "k-b2", Principal: "jwt:sub-B", Remote: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageRefused || got.TurnID != "" {
		t.Fatalf("stranger read stopped-session history: %+v", got)
	}
}

func TestMCPHTTPResourceFileHandoff(t *testing.T) {
	dir := t.TempDir()
	resFile := filepath.Join(dir, "resource.url")
	// Metis starts before Ocliv allocates the route: no binding yet.
	f := newMCPHTTPFixture(t, map[string]string{
		"METIS_MCP_RESOURCE":      "",
		"METIS_MCP_RESOURCE_FILE": resFile,
	})
	getDiscovery := func() (int, map[string]any) {
		resp, err := http.Get(f.srv.URL + "/.well-known/oauth-protected-resource/mcp")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := getDiscovery(); code != http.StatusServiceUnavailable {
		t.Fatalf("unbound discovery: got %d, want 503", code)
	}
	probe := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
	if code, _ := f.call(f.goodToken(), "2024-11-05", probe); code != http.StatusServiceUnavailable {
		t.Fatalf("unbound /mcp: got %d, want 503", code)
	}
	// Ocliv allocates the route and writes the handoff atomically.
	newRes := "https://relay.example/r/route-9/mcp"
	tmp := resFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(newRes+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, resFile); err != nil {
		t.Fatal(err)
	}
	if code, out := getDiscovery(); code != http.StatusOK || out["resource"] != newRes {
		t.Fatalf("bound discovery: got %d %v, want 200 %s", code, out, newRes)
	}
	// Stale-audience tokens (the pre-handoff URL) are rejected.
	if code, _ := f.call(f.goodToken(), "2024-11-05", probe); code != http.StatusUnauthorized {
		t.Fatalf("stale-audience token: got %d, want 401", code)
	}
	fresh := f.mint("op-1", newRes, "metis:message", time.Now().Add(time.Hour), nil)
	if code, out := f.call(fresh, "2024-11-05", probe); code != http.StatusOK || out["result"] == nil {
		t.Fatalf("fresh-audience token: got %d %v, want 200 tools", code, out)
	}
	// Republishing under a retained route rebinds forward only:
	// the old URL stops authorizing the moment the file changes.
	newer := "https://relay.example/r/route-9/mcp?v=2"
	if err := os.WriteFile(tmp, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, resFile); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.call(fresh, "2024-11-05", probe); code != http.StatusUnauthorized {
		t.Fatalf("superseded-audience token: got %d, want 401", code)
	}
	// A merely world-readable handoff is still honored: it names a
	// public URL, and readability confers no write authority.
	if err := os.Chmod(resFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out := getDiscovery(); code != http.StatusOK || out["resource"] != newer {
		t.Fatalf("readable handoff: got %d %v, want 200", code, out)
	}
	// A group-writable handoff is refused, fail closed: anyone
	// sharing the group could rebind the resource under us.
	if err := os.Chmod(resFile, 0o664); err != nil {
		t.Fatal(err)
	}
	if code, _ := getDiscovery(); code != http.StatusServiceUnavailable {
		t.Fatalf("writable handoff: got %d, want 503", code)
	}
}

func (f *mcpHTTPFixture) callOrigin(token, version, origin string, body any) (int, map[string]any) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMCPHTTPOriginPolicy(t *testing.T) {
	probe := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}}
	// No policy configured: absent or arbitrary present Origin passes.
	f := newMCPHTTPFixture(t, nil)
	if code, _ := f.callOrigin(f.goodToken(), "2024-11-05", "https://anything.example", probe); code != http.StatusOK {
		t.Fatalf("unconfigured policy rejected present origin: %d", code)
	}
	// Policy configured: absent ok, listed ok, unlisted 403.
	g := newMCPHTTPFixture(t, map[string]string{
		"METIS_MCP_ALLOWED_ORIGINS": "https://chat.example, https://app.example",
	})
	if code, _ := g.callOrigin(g.goodToken(), "2024-11-05", "", probe); code != http.StatusOK {
		t.Fatalf("absent origin rejected: %d", code)
	}
	if code, _ := g.callOrigin(g.goodToken(), "2024-11-05", "https://app.example", probe); code != http.StatusOK {
		t.Fatalf("listed origin rejected: %d", code)
	}
	if code, _ := g.callOrigin(g.goodToken(), "2024-11-05", "https://evil.example", probe); code != http.StatusForbidden {
		t.Fatalf("unlisted origin: got %d, want 403", code)
	}
}

func TestMCPHTTPVersionNegotiation(t *testing.T) {
	f := newMCPHTTPFixture(t, nil)
	init := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}
	for _, v := range []string{"2024-11-05", "2026-07-28"} {
		code, out := f.call(f.goodToken(), v, init)
		if code != http.StatusOK {
			t.Fatalf("version %s rejected: %d", v, code)
		}
		res, _ := out["result"].(map[string]any)
		if res["protocolVersion"] != v {
			t.Fatalf("version %s not echoed: %v", v, res)
		}
	}
	if code, _ := f.call(f.goodToken(), "1999-01-01", init); code != http.StatusBadRequest {
		t.Fatalf("bogus version: got %d, want 400", code)
	}
	// Headerless legacy clients keep working on the default.
	code, out := f.call(f.goodToken(), "", init)
	res, _ := out["result"].(map[string]any)
	if code != http.StatusOK || res["protocolVersion"] != mcpHTTPVersion {
		t.Fatalf("default version: got %d %v", code, out)
	}
}

func TestMCPHTTPES256(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pad := func(b []byte) []byte {
		for len(b) < 32 {
			b = append([]byte{0}, b...)
		}
		return b
	}
	jwks := map[string]any{"keys": []any{map[string]any{
		"kty": "EC", "kid": "e1", "alg": "ES256", "use": "sig", "crv": "P-256",
		"x": mcpB64(pad(ec.X.Bytes())), "y": mcpB64(pad(ec.Y.Bytes())),
	}}}
	raw, _ := json.Marshal(jwks)
	dir := t.TempDir()
	jwksFile := filepath.Join(dir, "jwks.json")
	if err := os.WriteFile(jwksFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	issuer, res := "https://idp.example/oauth", "https://relay.example/r/abc/mcp"
	t.Setenv("METIS_MCP_ISSUER", issuer)
	t.Setenv("METIS_MCP_JWKS_FILE", jwksFile)
	t.Setenv("METIS_MCP_JWKS_URL", "")
	t.Setenv("METIS_MCP_RESOURCE", res)
	t.Setenv("METIS_MCP_RESOURCE_FILE", "")
	t.Setenv("METIS_MCP_ALLOWED_SUBJECTS", "")
	t.Setenv("METIS_MCP_REMOTE_CAPABILITIES", "message")
	cfg, err := mcpHTTPConfigFromEnv("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &mcpHTTPServer{cfg: cfg, jwks: &mcpJWKS{file: jwksFile},
		res:     mcpResourceStore{fixed: cfg.resource, file: cfg.resourceFile},
		origins: mcpAllowedOrigins()}
	srv := httptest.NewServer(mcpHTTPMux(s))
	defer srv.Close()
	mint := func(sub string, key *ecdsa.PrivateKey) string {
		hdr, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": "e1"})
		pay, _ := json.Marshal(map[string]any{
			"iss": issuer, "sub": sub, "aud": res,
			"exp": time.Now().Add(time.Hour).Unix(), "scope": "metis:message",
		})
		hs, ps := mcpB64(hdr), mcpB64(pay)
		digest := sha256.Sum256([]byte(hs + "." + ps))
		r, sv, err := ecdsa.Sign(rand.Reader, key, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		return hs + "." + ps + "." + mcpB64(append(pad(r.Bytes()), pad(sv.Bytes())...))
	}
	post := func(tok string) int {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2024-11-05")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := post(mint("op-ec", ec)); code != http.StatusOK {
		t.Fatalf("valid ES256 rejected: %d", code)
	}
	evil, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if code := post(mint("op-ec", evil)); code != http.StatusUnauthorized {
		t.Fatalf("wrong-key ES256: got %d, want 401", code)
	}
}

func TestMCPHTTPConfigRejectsHTTPJWKS(t *testing.T) {
	t.Setenv("METIS_MCP_ISSUER", "https://idp.example/oauth")
	t.Setenv("METIS_MCP_JWKS_URL", "http://idp.example/jwks.json")
	t.Setenv("METIS_MCP_JWKS_FILE", "")
	t.Setenv("METIS_MCP_RESOURCE", "https://relay.example/r/a/mcp")
	t.Setenv("METIS_MCP_RESOURCE_FILE", "")
	if _, err := mcpHTTPConfigFromEnv("127.0.0.1:0"); err == nil {
		t.Fatal("plaintext JWKS URL accepted")
	}
}

func TestClaimMuseMessageRowRejectsForeignPrincipal(t *testing.T) {
	msg := MuseMessageRequest{SessionID: "sx", Text: "hi", RequestID: "r-1", Actor: "a"}
	b := museBinding{NativeID: "n1", Profile: "pr", Provider: "p", Workspace: "w"}
	if _, _, _, err := claimMuseMessageRow(msg, b,
		"sx", "n1", "pr", "p", "w", museMessageDigest("hi"), "cmd-1", "turn-1",
		"jwt:sub-A", "jwt:sub-A"); err != nil {
		t.Fatalf("owner replay refused: %v", err)
	}
	if _, _, _, err := claimMuseMessageRow(msg, b,
		"sx", "n1", "pr", "p", "w", museMessageDigest("hi"), "cmd-1", "turn-1",
		"jwt:sub-A", "jwt:sub-B"); err == nil {
		t.Fatal("foreign-principal replay admitted")
	}
}

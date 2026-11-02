package main

// mcp_http.go: authenticated HTTP MCP transport for remote callers.
//
// The stdio adapter (mcp_adapter.go) stays the local-operator path.
// This file adds `metis mcp serve-http`, a loopback-only HTTP endpoint
// for Ocliv `mcp publish --local` publication. No second state
// authority: every tool calls the same controller functions as the
// CLI, TUI, and stdio adapter.
//
// Trust model (fail closed):
//   - Binds numeric loopback only; anything else refuses to start.
//   - Every /mcp request needs Authorization: Bearer <JWT> minted by
//     the configured external issuer (JWKS-verified signature; iss,
//     exact resource aud, exp/nbf, required scope). No authorization
//     server is implemented here.
//   - The principal is the JWT subject, established server-side. The
//     model-supplied `actor` stays an asserted label; the `auth` tool
//     argument is stripped and never honored on this transport, so
//     METIS_MCP_WRITE_TOKEN never appears in remote tool schemas.
//   - Initial remote surface: read-only observation plus scoped Muse
//     messaging. Reads need no capability; message tools need the
//     `metis:message` JWT scope AND the existing scoped env grant
//     (mcpAuthorize). Approve/verify/admin/tasks tools are not
//     reachable here at all.
//   - request_id idempotency is the domain's (same functions as
//     stdio); this layer passes IDs through untouched.

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// mcpHTTPVersion is the one MCP protocol version this transport
// implements: the same envelope the stdio adapter speaks.
const mcpHTTPVersion = "2024-11-05"

// maxMCPHTTPBody bounds one inbound JSON-RPC request.
const maxMCPHTTPBody = 1 << 20

// jwtLeeway tolerates small clock skew on exp/nbf.
const jwtLeeway = 60

// mcpHTTPScopeMessage is the JWT scope gating Muse messaging.
const mcpHTTPScopeMessage = "metis:message"

// mcpHTTPConfig is the resolved remote-transport configuration. All
// values come from explicit operator env; nothing is guessed.
type mcpHTTPConfig struct {
	listen   string
	issuer   string
	jwksURL  string
	jwksFile string
	resource string
	subjects map[string]bool // optional sub allowlist; nil = any sub
}

// mcpHTTPConfigFromEnv resolves and validates the transport config.
// METIS_MCP_RESOURCE_FILE (the publisher --resource-file handoff)
// wins over METIS_MCP_RESOURCE so discovery always names the exact
// public route URL.
func mcpHTTPConfigFromEnv(listen string) (mcpHTTPConfig, error) {
	var c mcpHTTPConfig
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return c, fmt.Errorf("invalid --listen %q: want host:port", listen)
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return c, fmt.Errorf("invalid --listen %q: numeric loopback only", listen)
	}
	c.listen = net.JoinHostPort(ip.String(), port)
	c.issuer = strings.TrimSpace(os.Getenv("METIS_MCP_ISSUER"))
	if c.issuer == "" {
		return c, fmt.Errorf("METIS_MCP_ISSUER is required (reuse the external identity provider; no local issuer)")
	}
	if _, err := parseHTTPSURL(c.issuer, "METIS_MCP_ISSUER"); err != nil {
		return c, err
	}
	c.jwksURL = strings.TrimSpace(os.Getenv("METIS_MCP_JWKS_URL"))
	c.jwksFile = strings.TrimSpace(os.Getenv("METIS_MCP_JWKS_FILE"))
	if c.jwksURL == "" && c.jwksFile == "" {
		return c, fmt.Errorf("METIS_MCP_JWKS_URL or METIS_MCP_JWKS_FILE is required")
	}
	if f := strings.TrimSpace(os.Getenv("METIS_MCP_RESOURCE_FILE")); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			return c, fmt.Errorf("read METIS_MCP_RESOURCE_FILE: %w", err)
		}
		c.resource = strings.TrimSpace(string(raw))
	} else {
		c.resource = strings.TrimSpace(os.Getenv("METIS_MCP_RESOURCE"))
	}
	if c.resource == "" {
		return c, fmt.Errorf("METIS_MCP_RESOURCE or METIS_MCP_RESOURCE_FILE is required (exact public route URL)")
	}
	if _, err := parseHTTPSURL(c.resource, "resource"); err != nil {
		return c, err
	}
	if allow := strings.TrimSpace(os.Getenv("METIS_MCP_ALLOWED_SUBJECTS")); allow != "" {
		c.subjects = map[string]bool{}
		for _, s := range strings.Split(allow, ",") {
			if s = strings.TrimSpace(s); s != "" {
				c.subjects[s] = true
			}
		}
	}
	return c, nil
}

func parseHTTPSURL(raw, what string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s is not a parseable URL: %q", what, raw)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("%s must be https: %q", what, raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s must not carry userinfo: %q", what, raw)
	}
	return raw, nil
}

// mcpJWTClaims is the validated claim set. Scope stays a raw string;
// scopeHas does the space-separated membership test.
type mcpJWTClaims struct {
	Sub   string
	Scope string
}

// mcpJWKS caches the issuer key set with refresh-on-unknown-kid.
type mcpJWKS struct {
	mu      sync.Mutex
	url     string
	file    string
	keys    map[string]any // kid -> *rsa.PublicKey | *ecdsa.PublicKey
	fetched time.Time
}

func (j *mcpJWKS) load() (map[string]any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.keys != nil && time.Since(j.fetched) < 5*time.Minute {
		return j.keys, nil
	}
	keys, err := mcpFetchJWKS(j.url, j.file)
	if err != nil {
		return nil, err
	}
	j.keys = keys
	j.fetched = time.Now()
	return keys, nil
}

func (j *mcpJWKS) refresh() (map[string]any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	keys, err := mcpFetchJWKS(j.url, j.file)
	if err != nil {
		return nil, err
	}
	j.keys = keys
	j.fetched = time.Now()
	return keys, nil
}

// mcpFetchJWKS reads one JWKS document from a URL or a file and
// returns kid-keyed RSA/EC public keys. Only RS256 and ES256 key
// shapes are accepted.
func mcpFetchJWKS(url, file string) (map[string]any, error) {
	var raw []byte
	var err error
	if url != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return nil, err
		}
	} else {
		raw, err = os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		if len(raw) > 1<<20 {
			return nil, fmt.Errorf("jwks file too large")
		}
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Alg string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("jwks decode: %w", err)
	}
	out := map[string]any{}
	for _, k := range doc.Keys {
		if k.Kid == "" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err := base64.RawURLEncoding.DecodeString(k.N)
			if err != nil {
				continue
			}
			e, err := base64.RawURLEncoding.DecodeString(k.E)
			if err != nil {
				continue
			}
			exp := 0
			for _, b := range e {
				exp = exp<<8 + int(b)
			}
			if exp == 0 {
				continue
			}
			out[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}
		case "EC":
			if k.Crv != "P-256" {
				continue
			}
			x, err := base64.RawURLEncoding.DecodeString(k.X)
			if err != nil {
				continue
			}
			y, err := base64.RawURLEncoding.DecodeString(k.Y)
			if err != nil {
				continue
			}
			out[k.Kid] = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jwks holds no usable RSA/EC keys")
	}
	return out, nil
}

// mcpVerifyJWT validates signature (JWKS), issuer, exact resource
// audience, exp/nbf, and subject policy. It never mints, never
// refreshes, never falls back to another credential.
func mcpVerifyJWT(cfg mcpHTTPConfig, jwks *mcpJWKS, token string) (mcpJWTClaims, error) {
	var zero mcpJWTClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return zero, fmt.Errorf("malformed bearer token")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(raw, &hdr) != nil {
		return zero, fmt.Errorf("malformed token header")
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return zero, fmt.Errorf("unsupported token alg %q", hdr.Alg)
	}
	if hdr.Kid == "" {
		return zero, fmt.Errorf("token carries no key id")
	}
	keys, err := jwks.load()
	if err != nil {
		return zero, fmt.Errorf("jwks unavailable: %v", err)
	}
	key, ok := keys[hdr.Kid]
	if !ok {
		keys, err = jwks.refresh()
		if err != nil {
			return zero, fmt.Errorf("jwks unavailable: %v", err)
		}
		key, ok = keys[hdr.Kid]
		if !ok {
			return zero, fmt.Errorf("unknown key id")
		}
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return zero, fmt.Errorf("malformed token signature")
	}
	digest := sha256.Sum256([]byte(signingInput))
	switch k := key.(type) {
	case *rsa.PublicKey:
		if hdr.Alg != "RS256" {
			return zero, fmt.Errorf("alg/key mismatch")
		}
		if err := rsa.VerifyPKCS1v15(k, crypto.SHA256, digest[:], sig); err != nil {
			return zero, fmt.Errorf("bad token signature")
		}
	case *ecdsa.PublicKey:
		if hdr.Alg != "ES256" {
			return zero, fmt.Errorf("alg/key mismatch")
		}
		if len(sig) != 64 {
			return zero, fmt.Errorf("bad token signature")
		}
		if !ecdsa.Verify(k, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
			return zero, fmt.Errorf("bad token signature")
		}
	default:
		return zero, fmt.Errorf("unsupported key type")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return zero, fmt.Errorf("malformed token payload")
	}
	var cl struct {
		Iss   string   `json:"iss"`
		Sub   string   `json:"sub"`
		Aud   any      `json:"aud"`
		Exp   int64    `json:"exp"`
		Nbf   int64    `json:"nbf"`
		Scope string   `json:"scope"`
		Scp   []string `json:"scp"`
	}
	if err := json.Unmarshal(payload, &cl); err != nil {
		return zero, fmt.Errorf("malformed token payload")
	}
	if cl.Iss != cfg.issuer {
		return zero, fmt.Errorf("forged issuer")
	}
	if cl.Sub == "" {
		return zero, fmt.Errorf("token carries no subject")
	}
	if cfg.subjects != nil && !cfg.subjects[cl.Sub] {
		return zero, fmt.Errorf("rejected subject")
	}
	audOK := false
	switch a := cl.Aud.(type) {
	case string:
		audOK = a == cfg.resource
	case []any:
		for _, v := range a {
			if s, _ := v.(string); s == cfg.resource {
				audOK = true
				break
			}
		}
	}
	if !audOK {
		return zero, fmt.Errorf("wrong audience")
	}
	now := time.Now().Unix()
	if cl.Exp == 0 || now >= cl.Exp+jwtLeeway {
		return zero, fmt.Errorf("expired token")
	}
	if cl.Nbf != 0 && now < cl.Nbf-jwtLeeway {
		return zero, fmt.Errorf("token not yet valid")
	}
	scope := cl.Scope
	if scope == "" && len(cl.Scp) > 0 {
		scope = strings.Join(cl.Scp, " ")
	}
	return mcpJWTClaims{Sub: cl.Sub, Scope: scope}, nil
}

// mcpScopeHas tests space-separated scope membership.
func mcpScopeHas(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// mcpHTTPRemoteTools is the complete remote surface: read-only
// observation plus scoped Muse messaging. Anything else is rejected
// before argument parsing, so a future tool cannot leak onto the
// remote transport by being added to the stdio dispatcher.
var mcpHTTPRemoteTools = map[string]bool{
	"metis_status":              true,
	"metis_agents":              true,
	"metis_events":              true,
	"metis_report":              true,
	"metis_reconcile":           true,
	"metis_tasks":               true,
	"metis_task_status":         true,
	"metis_projects":            true,
	"metis_muse_message":        true,
	"metis_muse_message_status": true,
}

// mcpHTTPReadTools need no capability: they are side-effect free by
// the existing read contract. Everything else on the remote surface
// needs the message scope.
var mcpHTTPReadTools = map[string]bool{
	"metis_status":      true,
	"metis_agents":      true,
	"metis_events":      true,
	"metis_report":      true,
	"metis_reconcile":   true,
	"metis_tasks":       true,
	"metis_task_status": true,
	"metis_projects":    true,
}

// mcpHTTPServer owns one serve-http listener.
type mcpHTTPServer struct {
	cfg  mcpHTTPConfig
	jwks *mcpJWKS
}

// mcpServeHTTP validates config, binds numeric loopback, and serves
// until the process ends. It returns only on fatal startup failure.
func mcpServeHTTP(listen string) int {
	cfg, err := mcpHTTPConfigFromEnv(listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp serve-http:", err)
		return 2
	}
	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mcp serve-http: listen:", err)
		return 1
	}
	s := &mcpHTTPServer{cfg: cfg, jwks: &mcpJWKS{url: cfg.jwksURL, file: cfg.jwksFile}}
	srv := &http.Server{
		Handler:           mcpHTTPMux(s),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	fmt.Fprintf(os.Stderr, "metis mcp serve-http on http://%s (loopback only)\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "mcp serve-http:", err)
		return 1
	}
	return 0
}

// mcpHTTPMux wires the transport routes; shared by serve and tests.
func mcpHTTPMux(s *mcpHTTPServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleDiscovery)
	return mux
}

// bearerClaims authenticates one request. Discovery stays public;
// every /mcp call needs a verified JWT.
func (s *mcpHTTPServer) bearerClaims(r *http.Request) (mcpJWTClaims, error) {
	var zero mcpJWTClaims
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, "Bearer ") {
		return zero, fmt.Errorf("missing bearer token")
	}
	return mcpVerifyJWT(s.cfg, s.jwks, strings.TrimPrefix(h, "Bearer "))
}

// handleDiscovery serves public OAuth protected-resource metadata
// naming the exact published resource. No token required: this is
// the document the client uses to learn where to authenticate.
func (s *mcpHTTPServer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 s.cfg.resource,
		"authorization_servers":    []string{s.cfg.issuer},
		"scopes_supported":         []string{mcpHTTPScopeMessage},
		"bearer_methods_supported": []string{"header"},
	})
}

// handleMCP serves Streamable HTTP POST: one JSON-RPC request in,
// one response out; notifications (absent id) get 202 with no body
// and never execute tools.
func (s *mcpHTTPServer) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpInvalidRequest, Message: "content-type must be application/json"}})
		return
	}
	if v := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")); v != "" && v != mcpHTTPVersion {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpInvalidRequest, Message: "unsupported protocol version"}})
		return
	}
	claims, err := s.bearerClaims(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+s.cfg.resource+`", error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxMCPHTTPBody+1))
	if err != nil || len(body) > maxMCPHTTPBody {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpInvalidRequest, Message: "request too large"}})
		return
	}
	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpParseError, Message: "parse error"}})
		return
	}
	id, hasID := mcpID(req.ID)
	if !hasID {
		w.WriteHeader(http.StatusAccepted)
		return // notification: accepted, never executed
	}
	w.Header().Set("MCP-Protocol-Version", mcpHTTPVersion)
	var out []byte
	switch req.Method {
	case "initialize":
		out = mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
			"protocolVersion": mcpHTTPVersion,
			"serverInfo":      map[string]string{"name": mcpServerName, "version": mcpServerVersion},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}})
	case "ping":
		out = mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{}})
	case "tools/list":
		out = mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"tools": mcpHTTPTools()}})
	case "tools/call":
		out = s.handleCall(id, claims, req.Params)
	default:
		out = mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: mcpMethodNotFound, Message: "method not found: " + req.Method}})
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// handleCall dispatches one tool call with the server-established
// principal. The `auth` argument is stripped before dispatch so a
// model-supplied secret can never authenticate; `actor` stays an
// asserted label.
func (s *mcpHTTPServer) handleCall(id any, claims mcpJWTClaims, params json.RawMessage) []byte {
	fail := func(code int, msg string) []byte {
		return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: msg}})
	}
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return fail(mcpInvalidParams, "tools/call requires name and arguments")
	}
	if !mcpHTTPRemoteTools[p.Name] {
		return fail(mcpMethodNotFound, "unknown tool: "+p.Name)
	}
	args := map[string]json.RawMessage{}
	if len(p.Arguments) > 0 {
		if err := json.Unmarshal(p.Arguments, &args); err != nil {
			return fail(mcpInvalidParams, "arguments must be an object")
		}
	}
	delete(args, "auth") // never honored on this transport
	req := OperationRequest{
		Actor:     mcpStr(args, "actor"),
		RequestID: mcpStr(args, "request_id"),
		Principal: "jwt:" + claims.Sub,
		Remote:    true,
	}
	if !mcpHTTPReadTools[p.Name] {
		if !mcpScopeHas(claims.Scope, mcpHTTPScopeMessage) {
			return fail(mcpInvalidParams, "token lacks "+mcpHTTPScopeMessage+" scope")
		}
		if err := mcpAuthorize(p.Name, req); err != nil {
			return fail(mcpInvalidParams, err.Error())
		}
	}
	raw, _ := json.Marshal(args)
	res, cerr := mcpHTTPDispatch(p.Name, raw, req)
	if cerr != nil {
		return fail(mcpInternalError, cerr.Error())
	}
	return mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: res})
}

// mcpHTTPDispatch calls the same domain functions as the stdio
// adapter, with the server-built request. Reads run as-is; the two
// message tools run under the authorized remote principal.
func mcpHTTPDispatch(name string, raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	switch name {
	case "metis_status":
		return mcpToolStatus(raw)
	case "metis_agents":
		return mcpToolAgents(raw)
	case "metis_events":
		return mcpToolEvents(raw)
	case "metis_report":
		return mcpToolReport(raw)
	case "metis_reconcile":
		return mcpToolReconcile(raw)
	case "metis_tasks":
		return mcpToolTasks(raw)
	case "metis_task_status":
		return mcpToolTaskStatus(raw)
	case "metis_projects":
		return mcpToolProjects(raw)
	case "metis_muse_message":
		return mcpHTTPMuseMessage(raw, req)
	case "metis_muse_message_status":
		return mcpHTTPMuseMessageStatus(raw, req)
	default:
		return mcpToolResult{}, &mcpMethodError{name}
	}
}

// mcpHTTPMuseMessage mirrors mcpToolMuseMessage with the
// server-established principal instead of arg-derived auth.
func mcpHTTPMuseMessage(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	msg := MuseMessageRequest{
		SessionID: mcpStr(args, "session"),
		Text:      mcpStr(args, "text"),
		RequestID: mcpStr(args, "request_id"),
		Actor:     mcpStr(args, "actor"),
	}
	res, err := sendMuseMessage(msg, req)
	if err != nil {
		return mcpToolError("muse message: %v", err)
	}
	return mcpTextResult(museMessagePayload(res))
}

// mcpHTTPMuseMessageStatus mirrors mcpToolMuseMessageStatus with the
// server-established principal.
func mcpHTTPMuseMessageStatus(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	res, err := queryMuseMessageStatus(mcpStr(args, "request_id"), req, mcpMessageStatusWait(args))
	if err != nil {
		return mcpToolError("muse message status: %v", err)
	}
	return mcpTextResult(museMessagePayload(res))
}

// mcpHTTPTools advertises the remote subset with schemas minus the
// `auth` argument: there is no model-supplied secret on this
// transport, so none is advertised.
func mcpHTTPTools() []mcpTool {
	var out []mcpTool
	for _, t := range mcpTools() {
		if !mcpHTTPRemoteTools[t.Name] {
			continue
		}
		out = append(out, mcpTool{Name: t.Name, Description: t.Description, InputSchema: mcpStripAuth(t.InputSchema)})
	}
	return out
}

// mcpStripAuth removes the auth property (and its required entry)
// from a tool input schema.
func mcpStripAuth(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	props, _ := m["properties"].(map[string]any)
	if props == nil {
		return schema
	}
	delete(props, "auth")
	if req, ok := m["required"].([]string); ok {
		keep := req[:0]
		for _, r := range req {
			if r != "auth" {
				keep = append(keep, r)
			}
		}
		m["required"] = keep
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

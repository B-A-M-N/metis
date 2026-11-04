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
	"errors"
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
// mcpHTTPScopeProvision is the separately-granted JWT scope gating
// remote worker provisioning. One scope never implies the other.
const mcpHTTPScopeMessage = "metis:message"

const mcpHTTPScopeProvision = "metis:provision"

// mcpHTTPScopeInput gates remote instruction delivery
// (metis_terminal_input, metis_send). A provision-scoped token
// never implies it.
const mcpHTTPScopeInput = "metis:input"

// mcpHTTPScopeStop gates remote session termination
// (metis_session_stop), separate from input and provision.
const mcpHTTPScopeStop = "metis:stop"

// mcpHTTPConfig is the resolved remote-transport configuration. All
// values come from explicit operator env; nothing is guessed.
type mcpHTTPConfig struct {
	listen       string
	issuer       string
	jwksURL      string
	jwksFile     string
	resource     string          // fixed resource, or "" when file-bound
	resourceFile string          // publisher handoff; may be unwritten at startup
	subjects     map[string]bool // optional sub allowlist; nil = any sub
}

// mcpResourceStore resolves the exact public resource per request.
// Fixed mode returns the startup value. File mode re-reads the
// publisher handoff every time so Ocliv can allocate the route after
// Metis starts: one validated snapshot per request is shared by
// discovery, JWT audience checks, and auth challenges. Missing,
// unreadable, invalid, or insecurely-permissioned bindings fail
// closed; stale values are never reused across requests.
type mcpResourceStore struct {
	fixed string
	file  string
}

func (b *mcpResourceStore) get() (string, bool) {
	if b.file == "" {
		return b.fixed, b.fixed != ""
	}
	st, err := os.Lstat(b.file)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	if st.Mode().Perm()&0022 != 0 {
		return "", false // group/other-writable: refuse to honor
	}
	raw, err := os.ReadFile(b.file)
	if err != nil {
		return "", false
	}
	res := strings.TrimSpace(string(raw))
	if res == "" {
		return "", false
	}
	if _, err := parseHTTPSURL(res, "resource"); err != nil {
		return "", false
	}
	return res, true
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
	if c.jwksURL != "" {
		if _, err := parseHTTPSURL(c.jwksURL, "METIS_MCP_JWKS_URL"); err != nil {
			return c, err
		}
	}
	c.resourceFile = strings.TrimSpace(os.Getenv("METIS_MCP_RESOURCE_FILE"))
	c.resource = strings.TrimSpace(os.Getenv("METIS_MCP_RESOURCE"))
	if c.resourceFile == "" {
		if c.resource == "" {
			return c, fmt.Errorf("METIS_MCP_RESOURCE or METIS_MCP_RESOURCE_FILE is required (exact public route URL)")
		}
		if _, err := parseHTTPSURL(c.resource, "resource"); err != nil {
			return c, err
		}
	}
	// File mode starts unbound: Ocliv allocates the route after Metis
	// starts and writes the handoff. Requests fail closed until the
	// first valid binding appears; each request re-resolves.
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

// jwksRefreshCooldown bounds attacker-triggered refetches: distinct
// unknown kids cannot each cause a remote fetch.
const jwksRefreshCooldown = 30 * time.Second

func (j *mcpJWKS) refresh() (map[string]any, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.keys != nil && time.Since(j.fetched) < jwksRefreshCooldown {
		return j.keys, nil // fetched moments ago: no new info, don't hammer
	}
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
		if _, uerr := parseHTTPSURL(url, "jwks url"); uerr != nil {
			return nil, uerr
		}
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
func mcpVerifyJWT(cfg mcpHTTPConfig, jwks *mcpJWKS, resource, token string) (mcpJWTClaims, error) {
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
		audOK = a == resource
	case []any:
		for _, v := range a {
			if s, _ := v.(string); s == resource {
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
	"metis_provision":           true,
	"metis_terminal":            true,
	"metis_terminal_input":      true,
	"metis_send":                true,
	"metis_session_stop":        true,
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
	// Bounded terminal observation stays side-effect free and
	// scopeless like the other reads.
	"metis_terminal": true,
}

// mcpHTTPServer owns one serve-http listener.
type mcpHTTPServer struct {
	cfg     mcpHTTPConfig
	jwks    *mcpJWKS
	res     mcpResourceStore
	origins map[string]bool // nil = any absent Origin ok; present must match
}

// mcpHTTPVersions are the negotiated Streamable HTTP revisions: one
// POST-only JSON engine, honest labels, default for headerless legacy
// clients.
var mcpHTTPVersions = map[string]bool{"2024-11-05": true, "2026-07-28": true}

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
	s := &mcpHTTPServer{cfg: cfg, jwks: &mcpJWKS{url: cfg.jwksURL, file: cfg.jwksFile},
		res:     mcpResourceStore{fixed: cfg.resource, file: cfg.resourceFile},
		origins: mcpAllowedOrigins()}
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
	// No disconnect event on shutdown: HTTP is stateless per
	// request, so shutdown/request-completion/inactivity infer
	// no client's disconnect. Disconnect is observed only via
	// presence-lease expiry (explicit absent state).
	return 0
}

// mcpHTTPMux wires the transport routes; shared by serve and tests.
func mcpHTTPMux(s *mcpHTTPServer) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", s.handleDiscovery)
	return mux
}

// mcpAllowedOrigins parses METIS_MCP_ALLOWED_ORIGINS. A missing
// Origin header (non-browser clients) is always allowed; a present one
// must match exactly.
func mcpAllowedOrigins() map[string]bool {
	raw := strings.TrimSpace(os.Getenv("METIS_MCP_ALLOWED_ORIGINS"))
	if raw == "" {
		return nil
	}
	out := map[string]bool{}
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out[o] = true
		}
	}
	return out
}

// bearerClaims authenticates one request against a single validated
// resource snapshot. Discovery stays public; every /mcp call needs a
// verified JWT.
func (s *mcpHTTPServer) bearerClaims(r *http.Request, resource string) (mcpJWTClaims, error) {
	var zero mcpJWTClaims
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, "Bearer ") {
		return zero, fmt.Errorf("missing bearer token")
	}
	return mcpVerifyJWT(s.cfg, s.jwks, resource, strings.TrimPrefix(h, "Bearer "))
}

// handleDiscovery serves public OAuth protected-resource metadata
// naming the exact published resource. No token required: this is
// the document the client uses to learn where to authenticate.
func (s *mcpHTTPServer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resource, ok := s.res.get()
	if !ok {
		http.Error(w, "resource not configured", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{s.cfg.issuer},
		"scopes_supported":         []string{mcpHTTPScopeMessage, mcpHTTPScopeProvision, mcpHTTPScopeInput, mcpHTTPScopeStop},
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
	resource, ok := s.res.get()
	if !ok {
		http.Error(w, "resource not configured", http.StatusServiceUnavailable)
		return
	}
	if o := strings.TrimSpace(r.Header.Get("Origin")); o != "" && len(s.origins) > 0 && !s.origins[o] {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	negotiated := mcpHTTPVersion
	if v := strings.TrimSpace(r.Header.Get("MCP-Protocol-Version")); v != "" {
		if !mcpHTTPVersions[v] {
			writeJSON(w, http.StatusBadRequest, mcpResponse{JSONRPC: "2.0", Error: &mcpError{Code: mcpInvalidRequest, Message: "unsupported protocol version"}})
			return
		}
		negotiated = v
	}
	claims, err := s.bearerClaims(r, resource)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer resource="`+resource+`", error="invalid_token"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// Advisory controller presence under the verified JWT
	// subject: a working cloud controller must light the Muse tab.
	// A record failure never refuses the call.
	_ = recordMusePresence("jwt:"+claims.Sub, "mcp-http", musePresenceLeaseTTL)
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
	w.Header().Set("MCP-Protocol-Version", negotiated)
	var out []byte
	switch req.Method {
	case "initialize":
		out = mcpMarshal(mcpResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{
			"protocolVersion": negotiated,
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
		want := mcpHTTPScopeMessage
		switch p.Name {
		case "metis_provision":
			want = mcpHTTPScopeProvision
		case "metis_terminal_input", "metis_send":
			want = mcpHTTPScopeInput
		case "metis_session_stop":
			want = mcpHTTPScopeStop
		}
		if !mcpScopeHas(claims.Scope, want) {
			return fail(mcpInvalidParams, "token lacks "+want+" scope")
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
	case "metis_provision":
		return mcpHTTPProvision(raw, req)
	case "metis_terminal":
		return mcpTerminalHTTP(raw, req)
	case "metis_terminal_input":
		return mcpTerminalInputHTTP(raw, req)
	case "metis_send":
		return mcpSendHTTP(raw, req)
	case "metis_session_stop":
		return mcpSessionStopHTTP(raw, req)
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

// mcpHTTPProvision admits a worker session for a run over the remote
// transport. Authentication (JWT scope + admin capability) completes
// before any side effect; execution reuses DefaultProvisionService,
// which owns the durable intent, the workdir, and the launch. Remote
// callers get harness-curated launches only: any explicit
// command/cmd argument is refused here, so a scoped cloud
// controller can never smuggle arbitrary shell execution through
// this transport. Idempotency, conflict refusal, root policy, and
// backend rules are the domain's, identical to local provision.
func mcpHTTPProvision(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if cmd := strings.TrimSpace(mcpStr(args, "command") + mcpStr(args, "cmd")); cmd != "" {
		emitProvisionOutcome(req, ProvisionResult{}, errors.New("provision: remote explicit command refused"))
		return mcpToolError("provision: remote callers use harness-curated launches; explicit commands are refused on this transport")
	}
	// P0-B remote gate: this release qualifies muse-code interactive
	// workers only. An empty harness would admit a parked sleeper
	// (sleep 86400) through the service default, and any other
	// harness lacks a qualified remote launch path; both are
	// refused at this boundary. Local provision behavior is
	// untouched. An explicit workdir is required so the service
	// never falls back to a state-dir path for a remote launch.
	switch harness := strings.TrimSpace(mcpStr(args, "harness")); harness {
	case "muse-code":
	case "":
		emitProvisionOutcome(req, ProvisionResult{}, errors.New("provision: remote requires an explicit harness (this release: muse-code)"))
		return mcpToolError("provision: remote requires an explicit harness (this release: muse-code)")
	default:
		emitProvisionOutcome(req, ProvisionResult{}, errors.New("provision: remote harness not qualified"))
		return mcpToolError("provision: remote harness %q is not qualified on this transport (this release: muse-code)", harness)
	}
	if strings.TrimSpace(mcpStr(args, "workdir")) == "" {
		emitProvisionOutcome(req, ProvisionResult{}, errors.New("provision: remote requires an explicit workdir"))
		return mcpToolError("provision: remote requires an explicit workdir under a registered project root")
	}
	result, perr := DefaultProvisionService().Provision(ProvisionRequest{
		Actor: req.Actor, RequestID: mcpStr(args, "request_id"), Principal: req.Principal, Remote: true,
		Run: mcpStr(args, "run"), Session: mcpStr(args, "session"),
		Objective: mcpStr(args, "objective"), Workdir: mcpStr(args, "workdir"),
		Backend: mcpStr(args, "backend"), Harness: mcpStr(args, "harness"),
		Model:    mcpStr(args, "model"),
		Provider: mcpStr(args, "provider"), Profile: mcpStr(args, "profile"),
		ExplicitCommand: false,
	})
	emitProvisionOutcome(req, result, perr)
	if perr != nil {
		return mcpToolError("%s", perr.Error())
	}
	return provisionResultToMCP(result)
}

// mcpHTTPCheckSessionOwner enforces the principal-scoped
// controller policy on the remote transport: the verified JWT
// subject must be the principal that provisioned the session.
// Unknown sessions (no owner row, including pre-policy sessions)
// and foreign sessions are refused fail-closed before leases,
// generations, or backends are touched. A lease is arbitration,
// never authorization. Local transport bypasses this check.
func mcpHTTPCheckSessionOwner(op, session, principal string) error {
	// Transport-side of the read-model seam: this file must not
	// open the task database itself (boundary_test).
	owner, found, err := provisionOwnerForSession(session)
	if err != nil {
		return fmt.Errorf("%s refused: owner lookup failed: %v", op, err)
	}
	if !found {
		return fmt.Errorf("%s refused: session %q has no recorded remote owner", op, session)
	}
	if owner != principal {
		return fmt.Errorf("%s refused: session %q is owned by another controller", op, session)
	}
	return nil
}

// mcpTerminalHTTP serves bounded session observation over the
// remote transport. Read-only and scopeless like the other reads:
// status reports what is, output returns a capped labeled tail.
// Unknown or dead sessions are refused, never invented. Sessions
// provisioned by another controller are refused: observation is
// principal-scoped, not fleet-public.
func mcpTerminalHTTP(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if req.Remote {
		if err := mcpHTTPCheckSessionOwner("metis_terminal", mcpStr(args, "session"), req.Principal); err != nil {
			return mcpToolError("%s", err.Error())
		}
	}
	return mcpTerminalRun(args)
}

// mcpTerminalInputHTTP delivers one input payload from a verified
// controller through the same lease authority local writers use.
// Scope (metis:input) plus the admin capability gate it; the JWT
// subject becomes the lease owner, so a rival human lease blocks
// instead of interleaving, and request_id converges retries.
func mcpTerminalInputHTTP(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if req.Remote {
		// Broad grant before narrow ownership: the capability
		// refusal keeps its existing contract, then the owner
		// fence, then the lease/generation arbitration inside.
		if err := mcpTerminalGate("metis_terminal_input", req); err != nil {
			return mcpToolError("%s", err.Error())
		}
		if err := mcpHTTPCheckSessionOwner("metis_terminal_input", mcpStr(args, "session"), req.Principal); err != nil {
			return mcpToolError("%s", err.Error())
		}
	}
	return mcpTerminalInputOp(req, args)
}

// mcpSendHTTP files or live-delivers one guidance message from a
// verified controller. Scope (metis:input) plus the tasks capability
// gate it; idempotency and the lease authority are the domain's.
func mcpSendHTTP(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "text", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if err := mcpAuthorize("metis_send", req); err != nil {
		return mcpToolError("%s", err.Error())
	}
	if req.Remote {
		if err := mcpHTTPCheckSessionOwner("metis_send", mcpStr(args, "session"), req.Principal); err != nil {
			return mcpToolError("%s", err.Error())
		}
	}
	return mcpSendOp(req, args)
}

// mcpSessionStopHTTP stops one owned session for a verified
// controller. Scope (metis:stop) plus the admin capability gate it;
// generation fencing and request-ID idempotency are the domain's.
func mcpSessionStopHTTP(raw json.RawMessage, req OperationRequest) (mcpToolResult, error) {
	args, err := mcpNeedArgs(raw, "session", "actor", "request_id")
	if err != nil {
		return mcpToolResult{}, &mcpParamError{err.Error()}
	}
	if req.Remote {
		if err := mcpTerminalGate("metis_session_stop", req); err != nil {
			return mcpToolError("%s", err.Error())
		}
		if err := mcpHTTPCheckSessionOwner("metis_session_stop", mcpStr(args, "session"), req.Principal); err != nil {
			return mcpToolError("%s", err.Error())
		}
	}
	return mcpSessionStopOp(req, args)
}

// mcpHTTPDescriptionOverride replaces local-operator auth
// instructions with transport-accurate ones. The shared
// descriptions tell stdio callers to supply METIS_MCP_WRITE_TOKEN
// as an `auth` argument; on this transport that argument is
// stripped and the JWT bearer token is the only credential.
var mcpHTTPDescriptionOverride = map[string]string{
	"metis_muse_message":        "Send one structured message to a bound Muse-native session as a native turn, without any terminal attached. Authenticates with the connection bearer token (JWT subject becomes the owning principal); do not supply any auth argument — it is stripped. Returns promptly with a durable admission receipt (accepted + turn_id); it never waits for the terminal. Idempotent by request_id: an identical retry reconciles the stored command (same turn), a reused key with different text is a conflict. Poll metis_muse_message_status for the durable terminal outcome.",
	"metis_muse_message_status": "Reconcile one submitted muse message by request_id: returns the durable terminal when observed, accepted/unknown while the turn is still running or unreachable. Only the principal that submitted the request may read it. Bounded wait (timeout_ms, default 5000, max 25000); it never submits a turn. Authenticates with the connection bearer token; do not supply any auth argument — it is stripped.",
	"metis_provision":           "Admit a worker session for a run over this transport. Requires the metis:provision scope plus the admin capability. Harness-curated launches only (e.g. harness=muse-code on pty|tmux with an explicit workdir under a registered project root); explicit command/cmd arguments are refused. Idempotent by request_id: identical replays return the original session identity, a reused key with different arguments is a conflict.",
	"metis_terminal":            "Read-only terminal observation for one session across backends (tmux, managed PTY): status reports backend, liveness, workdir, and native binding; output returns a bounded tail labelled with its source (stream-tail, capture-pane, or pty-log-tail) — a snapshot, never a complete event stream. Unknown or dead sessions are refused. Principal-scoped: only the controller that provisioned the session may observe it. Read-only. Authenticates with the connection Bearer [REDACTED] (JWT subject becomes the owning principal); do not supply any auth argument — it is stripped.",
	"metis_terminal_input":      "Authorized terminal input to one running session (tmux, managed PTY). Requires the metis:input scope plus the admin capability. Delivers through the single-writer input-lease authority under a scoped mcp:<principal>:<actor> lease identity (rival writers conflict, never interleave) on the ownership-bound target (tmux immutable session ID, PTY supervisor identity); an optional generation fences stale PTY requests. Idempotent by request_id: identical replays converge on the recorded typed outcome (queued/confirmed/blocked/rejected), a reused key with different text conflicts. Harnesses without a live-input channel report blocked, never silent success. Authenticates with the connection Bearer [REDACTED]; do not supply any auth argument — it is stripped.",
	"metis_send":                "Send guidance to a session (filed or live-delivered). Requires the metis:input scope plus the tasks capability. Idempotent by request_id: replays return the original row. Authenticates with the connection Bearer [REDACTED] (JWT subject becomes the owning principal); do not supply any auth argument — it is stripped.",
	"metis_session_stop":        "Authorized session termination (tmux, managed PTY). Requires the metis:stop scope plus the admin capability. PTY stops travel the generation-fenced stop path (a session replaced mid-stop refuses instead of signalling the new owner's processes); tmux stops kill the bound session ID and mark the registry only after a confirmed kill. Principal-scoped: only the controller that provisioned the session may stop it. Stop preempts input: session leases are cleared. Idempotent by request_id: identical replays converge, a reused key with a different target conflicts. Authenticates with the connection Bearer [REDACTED]; do not supply any auth argument — it is stripped.",
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
		desc := t.Description
		if o, ok := mcpHTTPDescriptionOverride[t.Name]; ok {
			desc = o
		}
		out = append(out, mcpTool{Name: t.Name, Description: desc, InputSchema: mcpStripAuth(t.InputSchema)})
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

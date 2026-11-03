package main

// ocliv_expose_test.go: Metis↔Ocliv loopback integration.
//
// `ocliv mcp expose` adapts Metis's stdio MCP (`metis mcp serve`)
// to loopback HTTP — the exact first leg of the Ocliv publication
// path (`expose` loopback, then `publish --local` to a public
// route). This test pins that the two real binaries interoperate:
// initialize, tools/list naming metis_muse_message, and one
// tools/call reaching the Metis domain.
//
// Skip-gated: it needs an Ocliv binary, which is an external
// checkout, not a Metis dependency. Set METIS_OCLIV_BIN to run it
// (as the smoke did); otherwise it skips without failing the gate.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

var oclivExposeAddr = regexp.MustCompile(`on http://(127\.0\.0\.1:\d+) \(loopback only\)`)

func TestOclivLoopbackExposeServesMetisTools(t *testing.T) {
	bin := os.Getenv("METIS_OCLIV_BIN")
	if strings.TrimSpace(bin) == "" {
		t.Skip("METIS_OCLIV_BIN not set: Ocliv checkout absent")
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
		t.Skipf("METIS_OCLIV_BIN not executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// The child must be a real metis binary, not this test
	// binary (its TestMain would run the suite instead of serving).
	_, thisFile, _, _ := runtime.Caller(0)
	metisBin := filepath.Join(t.TempDir(), "metis")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", metisBin, ".")
	build.Dir = filepath.Dir(thisFile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build metis: %v\n%s", err, out)
	}
	self := metisBin
	state := t.TempDir()
	cmd := exec.CommandContext(ctx, bin, "mcp", "expose",
		"--dir="+t.TempDir(), "--env=METIS_STATE="+state,
		"--", self, "mcp", "serve")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("ocliv mcp expose: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	addr := ""
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if m := oclivExposeAddr.FindStringSubmatch(lines.Text()); m != nil {
			addr = m[1]
			break
		}
	}
	if addr == "" {
		t.Fatal("ocliv expose never printed its loopback address")
	}
	post := func(id int, method string, params any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s decode: %v", method, err)
		}
		return out
	}
	init := post(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "0"}})
	if fmt.Sprint(init["jsonrpc"]) != "2.0" {
		t.Fatalf("initialize failed: %v", init)
	}
	list := post(2, "tools/list", map[string]any{})
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	found := false
	for _, tl := range tools {
		if m, _ := tl.(map[string]any); fmt.Sprint(m["name"]) == "metis_muse_message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("metis_muse_message missing from %d exposed tools", len(tools))
	}
	call := post(3, "tools/call", map[string]any{"name": "metis_muse_message",
		"arguments": map[string]any{"session": "nope", "text": "hi", "actor": "t", "request_id": "ocl-r1"}})
	text, _ := call["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"refused"`) {
		t.Fatalf("call must reach the Metis domain (refused, unbound), got: %s", text)
	}
}

// callText extracts the human-readable outcome of a tools/call
// response regardless of which side of the result/error boundary
// the transport put it on.
func callText(call map[string]any) string {
	if res, ok := call["result"].(map[string]any); ok {
		if content, ok := res["content"].([]any); ok && len(content) > 0 {
			if first, ok := content[0].(map[string]any); ok {
				if text, ok := first["text"].(string); ok {
					return text
				}
			}
		}
	}
	return fmt.Sprint(call["error"])
}

// TestOclivLoopbackRemoteMessageChain qualifies the remote
// credential chain end to end through the real Ocliv loopback
// leg: token enforcement (missing/wrong token refused before the
// domain), the remote principal + message-capability grant reaching
// a bound session's domain gate, prompt admission timing (the
// round trip stays far under the MCP exchange budget instead of
// hanging on a terminal wait), and terminal retrieval through the
// status op. The submit refuses at the daemon check (no muse
// binary in the fixture), which is exactly the point: the chain,
// not the provider, is under test. Public OAuth ingress stays an
// operator-gated leg (see CAPABILITIES).
func TestOclivLoopbackRemoteMessageChain(t *testing.T) {
	bin := os.Getenv("METIS_OCLIV_BIN")
	if strings.TrimSpace(bin) == "" {
		t.Skip("METIS_OCLIV_BIN not set: Ocliv checkout absent")
	}
	if st, err := os.Stat(bin); err != nil || st.Mode()&0o111 == 0 {
		t.Skipf("METIS_OCLIV_BIN not executable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	state := t.TempDir()
	t.Setenv("METIS_STATE", state)
	// A bound echo session, provisioned and bound through the
	// production paths (stub host stands in for the provider) in
	// the exact state dir the child metis binary will serve.
	_, _, _ = setupMessageSessionIn(t, "ocr")
	_, thisFile, _, _ := runtime.Caller(0)
	metisBin := filepath.Join(t.TempDir(), "metis")
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", metisBin, ".")
	build.Dir = filepath.Dir(thisFile)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build metis: %v\n%s", err, out)
	}
	cmd := exec.CommandContext(ctx, bin, "mcp", "expose",
		"--dir="+t.TempDir(),
		"--env=METIS_STATE="+state,
		"--env=METIS_MCP_REMOTE_INGRESS=1",
		"--env=METIS_MCP_WRITE_TOKEN=tok-e2e",
		"--env=METIS_MCP_REMOTE_CAPABILITIES=message",
		"--env=METIS_MUSE_MESSAGE_TARGETS=ocr",
		"--", metisBin, "mcp", "serve")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("ocliv mcp expose: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	addr := ""
	lines := bufio.NewScanner(stdout)
	for lines.Scan() {
		if m := oclivExposeAddr.FindStringSubmatch(lines.Text()); m != nil {
			addr = m[1]
			break
		}
	}
	if addr == "" {
		t.Fatal("ocliv expose never printed its loopback address")
	}
	post := func(id int, method string, params any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s decode: %v", method, err)
		}
		return out
	}
	init := post(1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "t", "version": "0"}})
	if fmt.Sprint(init["jsonrpc"]) != "2.0" {
		t.Fatalf("initialize failed: %v", init)
	}
	list := post(2, "tools/list", map[string]any{})
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	seen := map[string]bool{}
	for _, tl := range tools {
		if m, _ := tl.(map[string]any); m != nil {
			seen[fmt.Sprint(m["name"])] = true
		}
	}
	if !seen["metis_muse_message"] || !seen["metis_muse_message_status"] {
		t.Fatalf("message ops missing from %d exposed tools", len(tools))
	}
	callArgs := func(extra map[string]any) map[string]any {
		args := map[string]any{"session": "ocr", "text": "chain hi", "actor": "t", "request_id": "ocl-chain-1"}
		for k, v := range extra {
			args[k] = v
		}
		return args
	}
	// Missing and wrong tokens are refused before the domain.
	if text := callText(post(3, "tools/call", map[string]any{"name": "metis_muse_message", "arguments": callArgs(nil)})); !strings.Contains(text, "token") {
		t.Fatalf("missing token must be refused at auth, got: %s", text)
	}
	if text := callText(post(4, "tools/call", map[string]any{"name": "metis_muse_message",
		"arguments": callArgs(map[string]any{"auth": "wrong"})})); !strings.Contains(text, "token") {
		t.Fatalf("wrong token must be refused at auth, got: %s", text)
	}
	// The exact remote chain — token, remote principal, message
	// capability — reaches the bound session's domain gate, and
	// the round trip returns promptly instead of hanging on a
	// terminal wait.
	start := time.Now()
	text := callText(post(5, "tools/call", map[string]any{"name": "metis_muse_message",
		"arguments": callArgs(map[string]any{"auth": "tok-e2e"})}))
	elapsed := time.Since(start)
	if elapsed >= museMessageStatusMaxWait {
		t.Fatalf("remote message round trip must stay far under the exchange budget, took %v: %s", elapsed, text)
	}
	if !strings.Contains(text, `"refused"`) {
		t.Fatalf("bound-session call must reach the domain gate, got: %s", text)
	}
	if !strings.Contains(text, "ocr") {
		t.Fatalf("domain refusal must name the bound session, got: %s", text)
	}
	// Terminal retrieval through the status op: the request was
	// claimed before the daemon refusal, so the status names the
	// unresolved admission instead of inventing a state.
	stText := callText(post(6, "tools/call", map[string]any{"name": "metis_muse_message_status",
		"arguments": map[string]any{"request_id": "ocl-chain-1", "actor": "t", "auth": "tok-e2e"}}))
	if !strings.Contains(stText, `"unknown"`) {
		t.Fatalf("status of the daemon-refused submit must stay unknown, got: %s", stText)
	}
}

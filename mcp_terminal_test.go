package main

// mcp_terminal_test.go: package 3 failing-first.
//
// metis_terminal is the model-facing terminal read surface. It must
// delegate to the existing backend implementations (tmux stream /
// capture-pane, PTY log tail) behind one operation, name its output
// source honestly (snapshot, never a complete event stream), and
// refuse unknown or dead sessions instead of fabricating output.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStreamLines(t *testing.T, name string, lines []string) {
	t.Helper()
	p := streamPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func terminalResult(t *testing.T, res mcpToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("terminal isError: %s", res.Content[0].Text)
	}
	return mcpTaskText(t, res)
}

func TestMCPTerminalStatusUnified(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "term-owner")
	seedOwnedSession(t, "tt", "term-owner")
	seedPtyRow(t, "tp", t.TempDir())

	tm := terminalResult(t, mustTerminal(t, "tt", "status", 0))
	if tm["backend"] != "tmux" || tm["alive"] != true {
		t.Fatalf("tmux status wrong: %v", tm)
	}
	if tm["workdir"] == "" {
		t.Fatalf("tmux status must carry workdir: %v", tm)
	}
	pw := terminalResult(t, mustTerminal(t, "pty:tp", "status", 0))
	if pw["backend"] != "pty" || pw["alive"] != false {
		t.Fatalf("pty status wrong: %v", pw)
	}
	if pw["workdir"] == "" {
		t.Fatalf("pty status must carry workdir: %v", pw)
	}
	// Unknown sessions are refusals, never fabricated snapshots.
	res, err := mcpCallTool("metis_terminal", mustJSON(t, map[string]any{"session": "nope", "op": "status"}))
	if err != nil {
		t.Fatalf("status refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("status of an unknown session must refuse")
	}
}

func TestMCPTerminalOutputBounded(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "term-out")
	seedOwnedSession(t, "to", "term-out")
	writeStreamLines(t, "to", []string{"one", "two", "three", "four", "five"})

	out := terminalResult(t, mustTerminal(t, "to", "output", 3))
	if out["source"] != "stream-tail" {
		t.Fatalf("output must name its source honestly, got %v", out)
	}
	lines, _ := out["lines"].([]any)
	if len(lines) != 3 || lines[0] != "three" || lines[2] != "five" {
		t.Fatalf("output must be the last 3 stream lines, got %v", lines)
	}
	// Oversized requests are capped by available content, never
	// an unbounded read.
	big := terminalResult(t, mustTerminal(t, "to", "output", 100000))
	if n := len(big["lines"].([]any)); n != 5 {
		t.Fatalf("oversized output must return the 5 available lines, got %d", n)
	}
	// Dead sessions refuse instead of returning stale bytes.
	markFakeTmuxDead(t)
	res, err := mcpCallTool("metis_terminal", mustJSON(t, map[string]any{"session": "to", "op": "output"}))
	if err != nil {
		t.Fatalf("output refusal must arrive as isError, not RPC error: %v", err)
	}
	if !res.IsError {
		t.Fatal("output of a dead session must refuse")
	}
}

func mustTerminal(t *testing.T, session, op string, lines int) mcpToolResult {
	t.Helper()
	args := map[string]any{"session": session, "op": op}
	if lines > 0 {
		args["lines"] = lines
	}
	res, err := mcpCallTool("metis_terminal", mustJSON(t, args))
	if err != nil {
		t.Fatalf("terminal %s %s: %v", op, session, err)
	}
	return res
}

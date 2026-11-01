package main

// Boundary enforcement (AST-based): the presentation/transport
// boundary is checked by parsing source, not by substring search,
// so a same-package helper call that opens the DB is caught even
// though it never names an import.
//
// (a) Read/UI adapters must not touch persistence internals:
// files matching mcp_read_*.go, cli_*.go, tui*.go, graph_panel.go
// must not import database/sql, and no transport/UI file matching
// mcp_*.go, cli_*.go, tui*.go, graph_panel.go may call the
// persistence openers (openTasksDB and the ensure*Table/ensure*Column
// schema helpers), resolved as call targets through the AST.
// Write-path MCP tools (provision/idempotency atomic commit) still
// carry DB handles until the write-path follow-up lands, so the
// import ban covers the read/UI surface while the opener-call ban
// covers the whole transport/UI set: a UI/MCP adapter cannot gain
// new DB authority via a same-package helper without failing here.
// The sanctioned read-model seam (task_readmodel.go: taskStatusDetail,
// verdictsForCurrentAttempt, latestRunForAgent, latestRun) matches
// none of the banned callee names by construction.
// (b) Terminal/session domain files must not depend on Bubble Tea:
// terminal_input*.go, input_sender.go, pty_*.go, session_*.go,
// exec_target.go, terminal.go must not import charmbracelet/bubbletea,
// so terminal delivery works without the Tea runtime.
//
// *_test.go fixtures are out of scope: tests open throwaway DBs
// directly and never ship as adapters.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// globSourceFiles expands patterns to non-test Go files. A pattern
// with no match, or one matching only tests, is a setup failure:
// the boundary would be guarding nothing.
func globSourceFiles(t *testing.T, patterns ...string) []string {
	t.Helper()
	var out []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("bad glob %s: %v", pattern, err)
		}
		matched := false
		for _, path := range matches {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			out = append(out, path)
			matched = true
		}
		if !matched {
			t.Fatalf("no source files match %s", pattern)
		}
	}
	return out
}

// parseSource parses one package file for AST inspection.
func parseSource(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// fileImportsPath reports whether f imports a path containing substr,
// resolved from the import specs rather than file text (immune to
// comments and string literals).
func fileImportsPath(f *ast.File, substr string) bool {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, substr) {
			return true
		}
	}
	return false
}

// dbOpenerNames collects the package's persistence-opener helpers:
// openTasksDB plus the ensure*Table / ensure*Column schema family.
// Built from actual FuncDecls so a renamed or added opener is
// picked up automatically. Narrow on purpose: non-DB helpers that
// merely share the ensure prefix (e.g. ensureStreamPipe) match
// neither Table nor Column and stay callable from UI code.
func dbOpenerNames(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	matches, err := filepath.Glob("*.go")
	if err != nil || matches == nil {
		t.Fatalf("no Go files found")
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		_, f := parseSource(t, path)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			name := fn.Name.Name
			if name == "openTasksDB" {
				out[name] = true
				continue
			}
			if strings.HasPrefix(name, "ensure") &&
				(strings.Contains(name, "Table") || strings.Contains(name, "Column")) {
				out[name] = true
			}
		}
	}
	if !out["openTasksDB"] {
		t.Fatalf("openTasksDB not found: opener set is empty")
	}
	return out
}

// fileDBOpenerCalls lists "path:line: callee()" for every call in
// path whose target resolves (Ident or SelectorExpr) to a banned
// opener. Definitions are not calls: declaring ensureMcpDependTable
// in a transport file does not trip this; calling it does.
func fileDBOpenerCalls(t *testing.T, path string, banned map[string]bool) []string {
	t.Helper()
	fset, f := parseSource(t, path)
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			name = fn.Name
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		}
		if banned[name] {
			hits = append(hits, fset.Position(call.Pos()).String()+": "+name+"()")
		}
		return true
	})
	return hits
}

func TestBoundaryReadUIHasNoSQLImport(t *testing.T) {
	for _, path := range globSourceFiles(t, "mcp_read_*.go", "cli_*.go", "tui*.go", "graph_panel.go") {
		_, f := parseSource(t, path)
		if fileImportsPath(f, "database/sql") {
			t.Errorf("%s imports database/sql: read/UI adapters render projections, never query persistence", path)
		}
	}
}

func TestBoundaryTransportHasNoDBOpeners(t *testing.T) {
	banned := dbOpenerNames(t)
	for _, path := range globSourceFiles(t, "mcp_*.go", "cli_*.go", "tui*.go", "graph_panel.go") {
		for _, hit := range fileDBOpenerCalls(t, path, banned) {
			t.Errorf("%s calls DB opener: transport/UI must go through the read model (%s)", hit, path)
		}
	}
}

// (c) The TUI refresh source is instance-scoped (finding D): no
// package-level refreshSource global may exist in UI files, so one
// dashboard can never decide what another dashboard reads. The
// behavioral pin is TestRefreshSourceIsInstanceScoped; this AST
// check rejects the reintroduction of the shared injection point.
func TestBoundaryNoGlobalRefreshSource(t *testing.T) {
	for _, path := range globSourceFiles(t, "tui*.go", "graph_panel.go") {
		_, f := parseSource(t, path)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					if id, ok := vs.Type.(*ast.Ident); ok && strings.Contains(id.Name, "refreshSource") {
						t.Errorf("%s: package-global refreshSource %s: instance-scope the source (finding D)", path, id.Name)
					}
				}
				for _, v := range vs.Values {
					if ce, ok := v.(*ast.CompositeLit); ok {
						if id, ok := ce.Type.(*ast.Ident); ok && strings.Contains(id.Name, "RefreshSource") {
							t.Errorf("%s: package-global refreshSource value: instance-scope the source (finding D)", path)
						}
					}
				}
			}
		}
	}
}

// (d) internal/tui owns UI state only: it may import the standard
// library and the pure termview primitives, never the
// application's concrete persistence, process, or domain
// implementations. The main package depends on tui, never the
// reverse (a main import would not even compile), so this check
// pins the allowed set against future coupling.
func TestBoundaryInternalTUIImportClosure(t *testing.T) {
	for _, path := range globSourceFiles(t, "internal/tui/*.go") {
		_, f := parseSource(t, path)
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: unparseable import: %v", path, err)
			}
			if !strings.Contains(p, ".") {
				continue // standard library
			}
			if p == "tui-dash/internal/termview" {
				continue // pure terminal math, explicitly allowed
			}
			t.Errorf("%s: internal/tui must not import %s: keep domain dependencies behind main-side adapters", path, p)
		}
	}
}

func TestBoundaryTerminalHasNoTeaDependency(t *testing.T) {
	for _, path := range globSourceFiles(t,
		"terminal_input*.go",
		"input_sender.go",
		"pty_*.go",
		"session_*.go",
		"exec_target.go",
		"terminal.go",
	) {
		_, f := parseSource(t, path)
		if fileImportsPath(f, "charmbracelet/bubbletea") {
			t.Errorf("%s imports charmbracelet/bubbletea: terminal delivery must work without Bubble Tea", path)
		}
	}
}

// (c) Attachment outcomes are fenced by the internal/tui Attach
// component (finding G): applyAttachResult must classify through
// Attach.Classify, and no flat attachGen-style generation field
// may return to the main model. A hand-rolled gen comparison in
// the adapter would silently reintroduce the correlation the
// component owns.
func TestBoundaryAttachOutcomesRoutedThroughFence(t *testing.T) {
	_, f := parseSource(t, "tui_attach.go")
	foundApply := false
	classifyCalls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if ok && fn.Name.Name == "applyAttachResult" {
			foundApply = true
			ast.Inspect(fn.Body, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Classify" {
						classifyCalls++
					}
				}
				return true
			})
			return false
		}
		return true
	})
	if !foundApply {
		t.Fatal("tui_attach.go must define applyAttachResult")
	}
	if classifyCalls == 0 {
		t.Error("applyAttachResult must classify through Attach.Classify: no Classify call found")
	}
	for _, path := range globSourceFiles(t, "tui*.go") {
		_, f := parseSource(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "model" {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				for _, name := range field.Names {
					if strings.EqualFold(name.Name, "attachGen") {
						t.Errorf("%s: flat attachGen field bypasses the Attach component: keep generation in internal/tui", path)
					}
					if strings.EqualFold(name.Name, "guidanceBusy") {
						t.Errorf("%s: flat guidanceBusy field bypasses the Guide component: keep serialization in internal/tui", path)
					}
					if name.Name == "input" {
						if ident, ok := field.Type.(*ast.Ident); ok && ident.Name == "string" {
							t.Errorf("%s: flat input buffer bypasses the Guide component: keep editing in internal/tui", path)
						}
					}
				}
			}
			return false
		})
	}
}

// (d) Guidance outcomes and submit serialization are owned by the
// internal/tui Guide component: the guidanceResultMsg case must
// classify through Guide.Complete, and the enter/backspace/type
// paths must transition through Guide rather than editing flat
// fields. A hand-rolled busy check in the adapter would silently
// reintroduce the second-dispatcher risk the component removes.
func TestBoundaryGuidanceRoutedThroughComponent(t *testing.T) {
	_, f := parseSource(t, "tui.go")
	completeCalls := 0
	submitCalls := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			switch sel.Sel.Name {
			case "Complete":
				completeCalls++
			case "Submit":
				submitCalls++
			}
		}
		return true
	})
	if completeCalls == 0 {
		t.Error("guidance outcome must classify through Guide.Complete: no Complete call in tui.go")
	}
	if submitCalls == 0 {
		t.Error("guidance submit must transition through Guide.Submit: no Submit call in tui.go")
	}
}

// (e) The status line is display-only: no control decision may parse
// it. Staleness is owned state (`model.staleMarker`, written only by
// applyRefresh/installStaleMarker); prefix-sniffing the status
// would clobber foreign messages that share the marker's prefix
// (pinned behaviorally by TestRefreshApplyPreservesForeignStatus).
// (f) Navigation owns the UI mode: only its transitions may write
// it. Direct `.mode = ...` assignments scattered across the shell,
// attach, input, and state files let any component silently retarget
// the event loop; transitions (`EnterInput`/`EnterAttach`/`ToNav`)
// keep the retarget explicit and greppable.
func TestBoundaryModeWritesRoutedThroughNav(t *testing.T) {
	for _, path := range globSourceFiles(t, "tui*.go") {
		_, f := parseSource(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				if s, ok := lhs.(*ast.SelectorExpr); ok && (s.Sel.Name == "mode" || s.Sel.Name == "Mode") {
					t.Errorf("%s: direct mode write bypasses navigation transitions: use Nav.EnterInput/EnterAttach/ToNav", path)
				}
			}
			return true
		})
	}
}

// (g) Sel-scoped panes are bound to their session at render time:
// renderers must read entry/live/events/gitLines only through
// boundEntry/boundLines (gated on model.paneSel, recorded by
// applyRefresh). A direct read would reintroduce cross-session
// painting after a selection change (P0, pinned behaviorally by
// TestRenderSelectionChangeHidesPreviousSession).
func TestBoundaryPaneReadsRoutedThroughBinding(t *testing.T) {
	for _, path := range globSourceFiles(t, "tui_render*.go") {
		_, f := parseSource(t, path)
		var enclosing string
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
				return true
			case *ast.CallExpr:
				// Field references passed into the binding
				// helpers are the routed path, not a bypass.
				if sel, ok := node.Fun.(*ast.Ident); ok && (sel.Name == "boundEntry" || sel.Name == "boundLines") {
					return false
				}
				return true
			case *ast.SelectorExpr:
				switch node.Sel.Name {
				case "entry", "live", "gitLines", "events":
					if enclosing != "boundEntry" && enclosing != "boundLines" {
						t.Errorf("%s: direct .%s read in %s bypasses the pane binding: route through boundEntry/boundLines", path, node.Sel.Name, enclosing)
					}
				}
			}
			return true
		})
	}
}

// (h) The pane binding has one writer: applyRefresh records
// model.paneSel alongside the installs it gates. Any other writer
// would fork the binding the renderers trust.
func TestBoundaryPaneBindingSingleWriter(t *testing.T) {
	for _, path := range globSourceFiles(t, "tui*.go") {
		_, f := parseSource(t, path)
		var enclosing string
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				enclosing = node.Name.Name
				return true
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if s, ok := lhs.(*ast.SelectorExpr); ok && s.Sel.Name == "paneSel" {
						if enclosing != "applyRefresh" {
							t.Errorf("%s: .paneSel written in %s: only applyRefresh may record the pane binding", path, enclosing)
						}
					}
				}
			}
			return true
		})
	}
}

func TestBoundaryNoStatusSniffing(t *testing.T) {
	for _, path := range globSourceFiles(t, "tui_state.go", "tui.go") {
		_, f := parseSource(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "HasPrefix" && sel.Sel.Name != "HasSuffix" && sel.Sel.Name != "Contains" {
				return true
			}
			for _, arg := range call.Args {
				if s, ok := arg.(*ast.SelectorExpr); ok && s.Sel.Name == "status" {
					t.Errorf("%s: control decision parses .status with %s: keep control state in owned fields, never in display text", path, sel.Sel.Name)
				}
			}
			return true
		})
	}
}

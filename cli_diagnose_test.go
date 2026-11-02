package main

// cli_diagnose_test.go: one-command bounded investigation bundle.

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func seedDiagnoseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	t.Setenv("METIS_OPS_DIAG", "1")
	opsReset()
	opsSinkMu.Lock()
	opsSinkLines = -1
	opsSinkMu.Unlock()
}

func TestDiagnoseRequiresSession(t *testing.T) {
	seedDiagnoseEnv(t)
	if code := cliDiagnose(nil); code != 2 {
		t.Errorf("diagnose without --session must refuse with exit 2, got %d", code)
	}
}

func TestDiagnoseUnknownSession(t *testing.T) {
	seedDiagnoseEnv(t)
	code, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", "nope"}) })
	if code != 1 {
		t.Fatalf("unknown session must exit 1, got %d:\n%s", code, out)
	}
}

func TestDiagnoseFiltersBeforeLimit(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dfl")
	seedOwnedSession(t, "target", "doc-dfl")
	observeOps(opsPhaseDispatch, "req-target", "task-target", 1, "target", nil)
	for i := 0; i < 30; i++ {
		observeOps(opsPhaseClaim, "req-other", "task-other", i, "other", nil)
	}
	_, out := captureStdout(t, func() int {
		return cliDiagnose([]string{"--session", "target", "--limit", "5", "--json"})
	})
	var bundle diagnoseBundle
	if err := json.Unmarshal([]byte(out), &bundle); err != nil {
		t.Fatalf("decode diagnostic bundle: %v\n%s", err, out)
	}
	if len(bundle.Transitions) != 1 || bundle.Transitions[0].RequestID != "req-target" {
		t.Fatalf("target evidence must survive global later traffic: %+v", bundle.Transitions)
	}
}

func TestDiagnoseRejectsInvalidLimit(t *testing.T) {
	seedDiagnoseEnv(t)
	for _, limit := range []string{"0", "-1", "2001"} {
		if code := cliDiagnose([]string{"--session", "missing", "--limit", limit}); code != 2 {
			t.Errorf("invalid limit %s must be rejected before lookup, got %d", limit, code)
		}
	}
}

func TestDiagnoseNoMatchExplainsRetainedHistory(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dnr")
	seedOwnedSession(t, "target", "doc-dnr")
	observeOps(opsPhaseDispatch, "req-other", "task-other", 1, "other", nil)
	_, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", "target", "--json"}) })
	if !strings.Contains(out, "no matching transitions in retained history") {
		t.Fatalf("nonmatching retained history must be distinguished from an empty sink:\n%s", out)
	}
}

func TestDiagnoseTmuxSessionBundle(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dg")
	seedOwnedSession(t, "w", "doc-dg")
	tk, err := createTask("run-dg", "w", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	observeOps(opsPhaseDispatch, "req-dg", tk.ID, 0, "w", nil)
	observeOps(opsPhaseClaim, "req-dg", tk.ID, 1, "w", errors.New("concurrent dispatch won"))
	code, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", "w"}) })
	if code != 0 {
		t.Fatalf("diagnose must exit 0, got %d:\n%s", code, out)
	}
	for _, want := range []string{"backend=tmux", tk.ID, "req-dg", "conflict"} {
		if !strings.Contains(out, want) {
			t.Errorf("bundle must contain %q:\n%s", want, out)
		}
	}
}

// The JSON bundle must parse and carry the documented sections:
// identity, task, transitions, leases, resources. No transcripts,
// no credentials.
func TestDiagnoseJSONSections(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dj")
	seedOwnedSession(t, "w", "doc-dj")
	tk, err := createTask("run-dj", "w", "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	observeOps(opsPhaseReport, "req-dj", tk.ID, 0, "w", nil)
	_, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", "w", "--json"}) })
	var bundle map[string]any
	if err := json.Unmarshal([]byte(out), &bundle); err != nil {
		t.Fatalf("json bundle must parse: %v:\n%s", err, out)
	}
	for _, section := range []string{"session", "task", "transitions", "leases", "resources"} {
		if _, ok := bundle[section]; !ok {
			t.Errorf("bundle missing section %q: %v", section, bundle)
		}
	}
	id, _ := bundle["session"].(map[string]any)
	if id["backend"] != "tmux" || id["name"] != "w" {
		t.Errorf("identity wrong: %v", id)
	}
}

func TestDiagnoseRedactsSecrets(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dr")
	seedOwnedSession(t, "w", "doc-dr")
	observeOps(opsPhaseClaim, "req-dr", "task-dr", 1, "w", errors.New("claim refused: api_key=hunter2-secret-value"))
	_, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", "w"}) })
	if strings.Contains(out, "hunter2") {
		t.Errorf("bundle leaked credential:\n%s", out)
	}
	if !strings.Contains(out, "<redacted") {
		t.Errorf("bundle must preserve the redaction marker:\n%s", out)
	}
}

func TestDiagnosePtyAndNativeBackends(t *testing.T) {
	seedDiagnoseEnv(t)
	installFakeTmux(t, "doc-dp")
	seedPtyRow(t, "pp", t.TempDir())
	seedLogicalRow(t, "nw")
	for session, backend := range map[string]string{"pty:pp": "pty", "native:nw": "muse-native"} {
		_, out := captureStdout(t, func() int { return cliDiagnose([]string{"--session", session}) })
		if !strings.Contains(out, backend) {
			t.Errorf("bundle for %s must name backend %s:\n%s", session, backend, out)
		}
	}
}

func TestDiagnoseLabelsNativeLifecycleAndObserverMetrics(t *testing.T) {
	seedDiagnoseEnv(t)
	seedLogicalRow(t, "native-diag")
	_, out := captureStdout(t, func() int {
		return cliDiagnose([]string{"--session", "native:native-diag", "--json"})
	})
	var bundle map[string]any
	if err := json.Unmarshal([]byte(out), &bundle); err != nil {
		t.Fatal(err)
	}
	id := bundle["session"].(map[string]any)
	if id["alive"] != true || id["lifecycle"] != "active" || id["observation"] != "not_probed" {
		t.Fatalf("native lifecycle and reachability must remain distinct: %v", id)
	}
	resources := bundle["resources"].(map[string]any)
	if resources["scope"] != string(ScopeObserverProcess) {
		t.Fatalf("observer metrics must carry provenance: %v", resources)
	}
}

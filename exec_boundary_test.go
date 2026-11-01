package main

// P0 execution/security boundary: regression tests. Each test names
// the exact defect it guards; all must FAIL before the fix (undefined
// helpers) and PASS after.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workerEnv must project a closed allowlist plus METIS_* task vars,
// never the ambient controller environment. A controller credential
// in the environment must not reach the child.
func TestWorkerEnvDropsSecrets(t *testing.T) {
	t.Setenv("METIS_TEST_API_KEY", "sk-test-secret-marker")
	t.Setenv("METIS_TEST_RANDOM_UNLISTED", "should-not-propagate")
	t.Setenv("PATH", "/usr/bin:/bin")
	env := workerEnv("task-1", 2, "w", "/work", nil)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "sk-test-secret-marker") {
		t.Fatal("controller secret leaked into worker environment")
	}
	if strings.Contains(joined, "should-not-propagate") {
		t.Fatal("unlisted variable propagated into worker environment")
	}
	has := func(k, v string) bool {
		for _, e := range env {
			if e == k+"="+v {
				return true
			}
		}
		return false
	}
	if !has("PATH", "/usr/bin:/bin") {
		t.Fatal("allowlisted PATH missing from worker environment")
	}
	for k, v := range map[string]string{
		"METIS_TASK_ID": "task-1", "METIS_ATTEMPT": "2",
		"METIS_SESSION": "w", "METIS_EXECUTION_ROOT": "/work",
	} {
		if !has(k, v) {
			t.Fatalf("task binding %s=%s missing", k, v)
		}
	}
}

// recordExecutionStart stores argv; prompt bulk and embedded secrets
// must be redacted at the boundary, with structure and digest kept.
func TestRedactArgvKeepsStructureDropsContent(t *testing.T) {
	prompt := strings.Repeat("sensitive objective text ", 40) // >256B
	argv := []string{"muse", "exec", "--json", prompt, "--api_key=sk-live-marker"}
	red := redactArgv(argv)
	if len(red) != len(argv) {
		t.Fatalf("redaction must preserve argv structure, got %d want %d", len(red), len(argv))
	}
	joined := strings.Join(red, "\n")
	if strings.Contains(joined, "sensitive objective text") {
		t.Fatal("bulk prompt content stored in execution receipt")
	}
	if strings.Contains(joined, "sk-live-marker") {
		t.Fatal("embedded secret stored in execution receipt")
	}
	if red[0] != "muse" || red[1] != "exec" {
		t.Fatalf("invocation structure altered: %q", red[:2])
	}
	if !strings.Contains(joined, "sha256:") {
		t.Fatal("redacted bulk arg must carry a digest for correlation")
	}
}

// Worker output files must refuse symlinks (a prior attempt's worker
// can predict the next attempt's output path) while still allowing
// legitimate same-attempt retries to truncate an owned regular file.
func TestOpenWorkerOutputRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "worker-output-task-1-attempt2.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if f, err := openWorkerOutput(link); err == nil {
		_ = f.Close()
		t.Fatal("symlink output path opened; must be refused")
	}
	if got, _ := os.ReadFile(outside); string(got) != "precious" {
		t.Fatal("symlink target was modified through the refused path")
	}
	// Legitimate path: create, then truncate an owned regular file.
	plain := filepath.Join(dir, "worker-output-task-1-attempt1.log")
	f, err := openWorkerOutput(plain)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, _ = f.WriteString("partial")
	_ = f.Close()
	f2, err := openWorkerOutput(plain)
	if err != nil {
		t.Fatalf("reopen owned regular file (same-attempt retry): %v", err)
	}
	_ = f2.Close()
	if fi, _ := os.Stat(plain); fi.Size() != 0 {
		t.Fatal("reopen must truncate, preserving retry semantics")
	}
}

// Artifact reads must confine BEFORE reading (current code reads
// first), and a swapped-in symlink must be refused, not followed.
func TestReadConfinedFileConfinesFirst(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "evidence.log")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfinedFile(root, link); err == nil {
		t.Fatal("symlink escape read; must be refused")
	}
	// A direct absolute path outside the root is refused without read.
	if _, err := readConfinedFile(root, outside); err == nil {
		t.Fatal("outside-root absolute path read; must be refused")
	}
	// Inside-root regular file reads normally.
	inner := filepath.Join(root, "ok.log")
	if err := os.WriteFile(inner, []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readConfinedFile(root, inner)
	if err != nil || string(data) != "evidence" {
		t.Fatalf("inside-root read failed: %v %q", err, data)
	}
}

// The stored execution receipt must contain the redacted argv, never
// the raw prompt.
func TestExecutionReceiptStoresRedactedArgv(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	prompt := strings.Repeat("receipt prompt bulk ", 40)
	argv := []string{"muse", "exec", prompt}
	red, _ := marshalRedactedArgv(argv)
	if err := recordExecutionStart("task-r", 1, "w", "muse", red, 12345, "test"); err != nil {
		t.Fatalf("record: %v", err)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored string
	if err := db.QueryRow(`SELECT argv FROM execution_receipts WHERE task_id = 'task-r'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "receipt prompt bulk") {
		t.Fatal("raw prompt bulk persisted in execution receipt")
	}
}

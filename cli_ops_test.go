package main

// cli_ops_test.go: the `ops` read surface — bounded, read-only,
// redacted, and honest about the opt-in.

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return code, string(out)
}

func seedOpsSink(t *testing.T, n int) {
	t.Helper()
	seedOpsSinkReq(t, n, "req")
}

func seedOpsSinkReq(t *testing.T, n int, req string) {
	t.Helper()
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("TUI_DASH_OPS_DIAG", "1")
	opsReset()
	opsSinkMu.Lock()
	opsSinkLines = -1
	opsSinkMu.Unlock()
	for i := 0; i < n; i++ {
		observeOps(opsPhaseReport, req, "task", i, "", nil)
	}
}

func TestCliOpsReadSurface(t *testing.T) {
	seedOpsSink(t, 5)
	code, out := captureStdout(t, func() int { return cliOps(nil) })
	if code != 0 {
		t.Fatalf("ops read must exit 0, got %d", code)
	}
	if got := strings.Count(out, "\n"); got != 5 {
		t.Fatalf("want 5 rows, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "report") || !strings.Contains(out, "task=task") {
		t.Fatalf("rows must carry phase and task identity:\n%s", out)
	}
}

func TestCliOpsHonestAboutMissingIdentity(t *testing.T) {
	seedOpsSinkReq(t, 1, "")
	_, out := captureStdout(t, func() int { return cliOps(nil) })
	if !strings.Contains(out, "req=-") || !strings.Contains(out, "sess=-") {
		t.Fatalf("empty identity must print as missing, not drift columns:\n%s", out)
	}
}

func TestCliOpsLimit(t *testing.T) {
	seedOpsSink(t, 10)
	_, out := captureStdout(t, func() int { return cliOps([]string{"--limit", "3"}) })
	if got := strings.Count(out, "\n"); got != 3 {
		t.Fatalf("limit must bound rows, got %d:\n%s", got, out)
	}
	if !strings.Contains(out, "attempt=9") || strings.Contains(out, "attempt=6") {
		t.Fatalf("limit must keep newest oldest-first:\n%s", out)
	}
}

func TestCliOpsJSON(t *testing.T) {
	seedOpsSink(t, 2)
	code, out := captureStdout(t, func() int { return cliOps([]string{"--json"}) })
	if code != 0 {
		t.Fatalf("json read must exit 0, got %d", code)
	}
	if !strings.HasPrefix(strings.TrimSpace(out), "[") {
		t.Fatalf("json mode must emit an array:\n%s", out)
	}
	if !strings.Contains(out, `"phase": "report"`) {
		t.Fatalf("json rows must carry phase:\n%s", out)
	}
}

func TestCliOpsMissingSink(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("TUI_DASH_OPS_DIAG", "")
	if _, err := os.Stat(opsSinkPath()); !os.IsNotExist(err) {
		t.Skip("sink exists in this state dir; nothing to prove")
	}
	if code := cliOps(nil); code != 0 {
		t.Fatalf("missing sink is an empty reading, not a failure: got %d", code)
	}
}

func TestCliOpsReadOnly(t *testing.T) {
	seedOpsSink(t, 3)
	before, err := os.ReadFile(opsSinkPath())
	if err != nil {
		t.Fatal(err)
	}
	sumBefore := sha256.Sum256(before)
	captureStdout(t, func() int { return cliOps([]string{"--limit", "2"}) })
	captureStdout(t, func() int { return cliOps([]string{"--json"}) })
	after, err := os.ReadFile(opsSinkPath())
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(after) != sumBefore {
		t.Fatal("ops reads must never mutate the sink")
	}
}

// Identifier filters select correlated evidence through the real
// command: one request's events across phases, one task, one
// session, one phase, one error class, errors only. Filters apply
// within the bounded read; --limit still bounds the read itself.
func TestCliOpsFilters(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("TUI_DASH_OPS_DIAG", "1")
	opsReset()
	opsSinkMu.Lock()
	opsSinkLines = -1
	opsSinkMu.Unlock()
	observeOps(opsPhaseDispatch, "req-a", "task-1", 0, "sess-1", nil)
	observeOps(opsPhaseClaim, "req-a", "task-1", 1, "sess-1", errors.New("concurrent dispatch won"))
	observeOps(opsPhaseExecution, "req-b", "task-2", 0, "sess-2", errors.New("no such session: gone"))
	observeOps(opsPhaseReport, "req-b", "task-2", 0, "sess-2", nil)
	cases := []struct {
		args []string
		want int
	}{
		{[]string{"--req", "req-a"}, 2},
		{[]string{"--task", "task-2"}, 2},
		{[]string{"--session", "sess-1"}, 2},
		{[]string{"--phase", "claim"}, 1},
		{[]string{"--class", "not_found"}, 1},
		{[]string{"--errors-only"}, 2},
		{[]string{"--attempt", "0"}, 3},
		{[]string{"--req", "req-a", "--errors-only"}, 1},
	}
	for _, c := range cases {
		_, out := captureStdout(t, func() int { return cliOps(c.args) })
		if got := strings.Count(out, "\n"); got != c.want {
			t.Errorf("cliOps(%v) = %d rows, want %d:\n%s", c.args, got, c.want, out)
		}
	}
	_, out := captureStdout(t, func() int { return cliOps([]string{"--req", "req-a", "--json"}) })
	if strings.Count(out, "req-a") != 2 {
		t.Errorf("json mode must honor filters, got:\n%s", out)
	}
	if code := cliOps([]string{"--phase", "bogus"}); code != 2 {
		t.Errorf("unknown phase must refuse with exit 2, got %d", code)
	}
	if code := cliOps([]string{"--class", "bogus"}); code != 2 {
		t.Errorf("unknown class must refuse with exit 2, got %d", code)
	}
}

func TestCliOpsOutputStaysRedacted(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	t.Setenv("TUI_DASH_OPS_DIAG", "1")
	opsReset()
	opsSinkMu.Lock()
	opsSinkLines = -1
	opsSinkMu.Unlock()
	observeOps(opsPhaseClaim, "req-r", "task-r", 1, "sess-r",
		errors.New("claim refused: password=s3cr3t-p@ss"))
	_, out := captureStdout(t, func() int { return cliOps(nil) })
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("ops output leaked credential: %q", out)
	}
	if !strings.Contains(out, "<redacted") {
		t.Fatalf("ops output must carry the redaction marker: %q", out)
	}
}

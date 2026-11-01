package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// installFakeTmux places a stub tmux at the front of PATH. The stub emulates
// a live session for has-session/show-environment/display-message and logs
// destructive calls. Env controls:
//
//	FAKE_TMUX_ID    - session id from display-message (default "$99")
//	FAKE_TMUX_OWNER - owner value from show-environment
//	FAKE_TMUX_KILL_RC - exit code for kill-session (default 0)
//	FAKE_TMUX_DIR   - scratch dir holding the dead flag
//	FAKE_TMUX_LOG   - file receiving one line per send-keys/kill-session call
//	FAKE_TMUX_LIST  - space-separated session names for list-sessions
func installFakeTmux(t *testing.T, owner string) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	runDir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
case "$1" in
  has-session)
    echo "has-session" >> "$FAKE_TMUX_LOG"
    [ -f "$FAKE_TMUX_DIR/dead" ] && exit 1 || exit 0 ;;
  list-sessions)
    for s in $FAKE_TMUX_LIST; do echo "$s"; done
    exit 0 ;;
  display-message)
    echo "${FAKE_TMUX_ID:-\$99}"; exit 0 ;;
  show-environment)
    echo "TUI_DASH_OWNER=$FAKE_TMUX_OWNER"; exit 0 ;;
  kill-session)
    echo "kill-session $@" >> "$FAKE_TMUX_LOG"
    touch "$FAKE_TMUX_DIR/dead"
    exit "${FAKE_TMUX_KILL_RC:-0}" ;;
  send-keys|pipe-pane|set-environment|capture-pane)
    echo "$1 $@" >> "$FAKE_TMUX_LOG"
    exit 0 ;;
  *)
    exit 0 ;;
esac
`
	p := filepath.Join(dir, "tmux")
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_TMUX_OWNER", owner)
	t.Setenv("FAKE_TMUX_DIR", runDir)
	t.Setenv("FAKE_TMUX_LOG", logPath)
	os.Unsetenv("FAKE_TMUX_ID")
	os.Unsetenv("FAKE_TMUX_KILL_RC")
	os.Unsetenv("FAKE_TMUX_LIST")
	return logPath
}

func seedOwnedSession(t *testing.T, name, owner string) {
	t.Helper()
	reg := loadRegistry()
	reg[name] = Session{Name: name, Tmux: tmuxName(name), Workdir: t.TempDir(), Owner: owner}
	if err := saveRegistry(reg); err != nil {
		t.Fatal(err)
	}
}

// A corrupt registry must report failure instead of an empty list:
// sessionsRound's trust flag lets the installer keep last-known
// sessions rather than wiping the panes (P1).
func TestSessionsRoundCorruptRegistryReportsFailure(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	if err := os.WriteFile(registryPath(), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, listOK := sessionsRound()
	if listOK {
		t.Fatal("corrupt registry must report list failure, got ok")
	}
}

func TestResolveOwnedTargetBindsID(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "tok-1")
	seedOwnedSession(t, "s", "tok-1")
	_, target, err := resolveOwnedTarget(loadRegistry(), "s")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if target != "$99" {
		t.Fatalf("expected immutable session id $99, got %q", target)
	}
}

func TestSendInputAndStopUseSessionID(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "tok-2")
	seedOwnedSession(t, "s", "tok-2")
	if err := sendTerminalInput("s", "hello", "Enter"); err != nil {
		t.Fatalf("send: %v", err)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "send-keys -t $99") {
		t.Fatalf("send-keys did not address the session id:\n%s", data)
	}
	if strings.Contains(string(data), "tui-s") {
		t.Fatalf("destructive call used the replaceable name:\n%s", data)
	}
	if err := stopSession("s"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	data, _ = os.ReadFile(logPath)
	if !strings.Contains(string(data), "kill-session -t $99") {
		t.Fatalf("kill did not address the session id:\n%s", data)
	}
	if got := loadRegistry()["s"]; !got.Stopped {
		t.Fatal("stopped session not marked stopped after confirmed kill")
	}
}

// A failed kill must never leave a live session recorded as stopped.
func TestStopKillFailureDoesNotMarkStopped(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "tok-3")
	t.Setenv("FAKE_TMUX_KILL_RC", "1")
	seedOwnedSession(t, "s", "tok-3")
	if err := stopSession("s"); err == nil {
		t.Fatal("expected kill failure to surface as an error")
	}
	if got := loadRegistry()["s"]; got.Stopped {
		t.Fatal("session marked stopped even though the kill failed")
	}
}

// P0-B: startSession must not orphan a live tmux session when the
// registry confirm fails. Failing-first: before the compensating
// cleanup this observes a startSession error with no kill-session
// call in the tmux log.
func TestStartSessionRegistryFailureCleansUpTmux(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	logPath := installFakeTmux(t, "tok-cleanup")
	markFakeTmuxDead(t) // has-session fails so the name reads as free
	seedOwnedSession(t, "s", "tok-existing")
	if err := startSession("s", t.TempDir(), "sleep 60", "", ""); err == nil {
		t.Fatal("expected registry conflict error for duplicate name")
	} else if !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected duplicate-registration error, got: %v", err)
	}
	data, _ := os.ReadFile(logPath)
	if !strings.Contains(string(data), "kill-session") {
		t.Fatalf("registry failure orphaned the tmux session (no kill-session):\n%s", data)
	}
	if got := loadRegistry()["s"]; got.Owner != "tok-existing" {
		t.Fatalf("registry entry mutated by failed start: %+v", got)
	}
}

func TestRegistryLockConcurrentWrites(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "c" + string(rune('a'+i))
			_ = withRegistryLock(func() error {
				locked := loadRegistry()
				locked[name] = Session{Name: name, Owner: "o"}
				return saveRegistry(locked)
			})
		}(i)
	}
	wg.Wait()
	reg := loadRegistry()
	for i := 0; i < 16; i++ {
		name := "c" + string(rune('a'+i))
		if _, ok := reg[name]; !ok {
			t.Fatalf("concurrent registry update lost entry %q", name)
		}
	}
}

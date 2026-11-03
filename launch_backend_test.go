package main

// launch_backend_test.go: explicit --backend selection for
// `metis launch` (task A).
//
// `metis launch` used to hardcode the provision backend, so a tmux
// backend (the only one with F5 full-terminal attach) was
// unreachable from the launch path even though the provision
// service supports it. The flag forwards to ProvisionService with
// the PTY default preserved; F5 stays tmux-only.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestResolveLaunchBackend(t *testing.T) {
	for _, tc := range []struct {
		flag string
		want string
	}{
		{"", provisionBackendPTY},
		{"pty", provisionBackendPTY},
		{"tmux", provisionBackendTMUX},
		{"  tmux  ", provisionBackendTMUX},
	} {
		got, err := resolveLaunchBackend(tc.flag)
		if err != nil || got != tc.want {
			t.Fatalf("resolveLaunchBackend(%q) = %q, %v; want %q", tc.flag, got, err, tc.want)
		}
	}
	for _, bad := range []string{"bogus", "muse-native", "TMUX", "PTY"} {
		if _, err := resolveLaunchBackend(bad); err == nil {
			t.Fatalf("resolveLaunchBackend(%q) must refuse", bad)
		}
	}
}

// TestCliLaunchForwardsBackend pins the flag-to-service wiring: the
// default launch records a PTY provision, an explicit tmux records a
// tmux provision, and a bad backend refuses before the provision
// service is touched. The stub launcher records instead of
// spawning, so no session starts.
func TestCliLaunchForwardsBackend(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	stub := &stubProvisionLauncher{}
	prev := cliProvisionService
	cliProvisionService = func() *ProvisionService { return testProvisionService(stub, false, true) }
	defer func() { cliProvisionService = prev }()
	if rc := cliLaunch([]string{"--name", "lp", "--workdir", work}); rc != 0 {
		t.Fatalf("default launch must succeed, got exit %d", rc)
	}
	if stub.last == nil || stub.last.Backend != provisionBackendPTY {
		t.Fatalf("default launch must record PTY backend, got %+v", stub.last)
	}
	stub.last = nil
	if rc := cliLaunch([]string{"--name", "lt", "--workdir", work, "--backend", "tmux"}); rc != 0 {
		t.Fatalf("tmux launch must succeed, got exit %d", rc)
	}
	if stub.last == nil || stub.last.Backend != provisionBackendTMUX {
		t.Fatalf("explicit tmux launch must record tmux backend, got %+v", stub.last)
	}
	stub.last = nil
	if rc := cliLaunch([]string{"--name", "lb", "--workdir", work, "--backend", "bogus"}); rc == 0 {
		t.Fatal("bogus backend must refuse")
	}
	if stub.last != nil {
		t.Fatalf("refused launch must not touch the provision service, got %+v", stub.last)
	}
}

// TestF5KeepsTmuxAttachWhenExplicitlySelected: F5 on a running tmux
// session still enters the external-attach path, while F5 on a PTY
// session explains the snapshot-only limit instead of attaching.
func TestF5KeepsTmuxAttachWhenExplicitlySelected(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeTmux(t, "f5launch")
	seedOwnedSession(t, "w", "f5launch")
	m := newModel("")
	m.nav.OnMuse = false
	m.names = []string{"w", "pty:pw"}
	m.running = map[string]bool{"w": true, "pty:pw": true}
	m.nav.Selected = 0
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyF5})
	got := updated.(model)
	if cmd == nil || !strings.Contains(got.status, "checking owned tmux session w") {
		t.Fatalf("F5 on tmux must enter attach path: status=%q cmd=%v", got.status, cmd != nil)
	}
	m.nav.Selected = 1
	updated, cmd = m.Update(tea.KeyMsg{Type: tea.KeyF5})
	got = updated.(model)
	if cmd != nil || !strings.Contains(got.status, "available for tmux sessions only") {
		t.Fatalf("F5 on PTY must explain the limit without attaching: status=%q cmd=%v", got.status, cmd != nil)
	}
}

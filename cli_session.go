package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cli_session.go: session lifecycle commands (up/run/start/stop/adopt/list/snapshot). Extracted from main.go; behavior unchanged.

// cliUp handles the 'up' command.
func cliUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	session := fs.String("session", "demo", "session name")
	workdir := fs.String("workdir", "", "workdir (default ~/metis-work; reuses existing ~/tui-dash-work)")
	cmd := fs.String("cmd", "", "worker command (default: this binary's demo worker)")
	_ = fs.Parse(args)
	wd := *workdir
	if wd == "" {
		home, _ := os.UserHomeDir()
		legacy := filepath.Join(home, "tui-dash-work")
		wd = filepath.Join(home, "metis-work")
		if _, err := os.Stat(legacy); err == nil || !os.IsNotExist(err) {
			wd = legacy
		}
	}
	if err := os.MkdirAll(wd, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "workdir:", err)
		return 1
	}
	if !sessionRunning(*session) {
		worker := *cmd
		if worker == "" {
			exe, err := os.Executable()
			if err != nil {
				fmt.Fprintln(os.Stderr, "executable:", err)
				return 1
			}
			worker = fmt.Sprintf("%q demo --workdir %q --steps 120 --delay 1", exe, wd)
		}
		if err := startSession(*session, wd, worker, "demo", ""); err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			return 1
		}
		fmt.Printf("started %s in %s\n", *session, wd)
	}
	if err := runTUI(*session); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 1
	}
	return 0
}

// cliRun handles the 'run' command.
func cliRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	session := fs.String("session", "", "session to select")
	_ = fs.Parse(args)
	if err := runTUI(*session); err != nil {
		fmt.Fprintln(os.Stderr, "tui:", err)
		return 1
	}
	return 0
}

// cliStart handles the 'start' command.
func cliStart(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	workdir := fs.String("workdir", "", "workdir")
	cmd := fs.String("cmd", "", "worker command (explicit; wins over --harness)")
	harness := fs.String("harness", "", "worker harness (e.g. demo, muse, codex, hermes, claude)")
	prompt := fs.String("prompt", "", "task prompt (required with --harness)")
	model := fs.String("model", "", "loaded model name, if known")
	_ = fs.Parse(args)
	if *name == "" || *workdir == "" {
		fmt.Fprintln(os.Stderr, "start requires --name, --workdir, and --cmd or --harness with --prompt")
		return 2
	}
	worker, err := resolveWorkerCommand(*harness, *cmd, *workdir, *prompt, *model)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	label := *harness
	if label == "" {
		label = "custom"
	}
	if err := startSession(*name, *workdir, worker, label, *model); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("started %s\n", *name)
	return 0
}

// cliLaunch provisions the MVP worker shape: one interactive Muse Code
// process in one owned tmux session. The command is built from argv values,
// then shell-quoted only at the tmux lifecycle boundary.
func cliLaunch(args []string) int {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	name := fs.String("name", "", "Metis session name")
	workdir := fs.String("workdir", "", "existing project directory")
	provider := fs.String("provider", "meta", "Muse Code provider: meta, local, or echo")
	backend := fs.String("backend", "pty", "session backend: pty or tmux (pty default; tmux keeps F5 full-terminal attach)")
	model := fs.String("model", "", "Muse Code model ID (uses Muse Code default when omitted)")
	profile := fs.String("profile", "", "Muse Code permission profile ID")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if !nameRe.MatchString(*name) || strings.TrimSpace(*workdir) == "" {
		fmt.Fprintln(os.Stderr, "launch requires --name NAME and --workdir DIR; name must match [A-Za-z0-9_-]{1,64}")
		return 2
	}
	if *provider != "meta" && *provider != "local" && *provider != "echo" {
		fmt.Fprintln(os.Stderr, "--provider must be meta, local, or echo")
		return 2
	}
	if strings.ContainsAny(*model+*profile, "\x00\n\r") {
		fmt.Fprintln(os.Stderr, "model and profile cannot contain control characters")
		return 2
	}
	work, err := filepath.Abs(*workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "workdir:", err)
		return 2
	}
	if info, err := os.Stat(work); err != nil || !info.IsDir() {
		fmt.Fprintln(os.Stderr, "workdir must be an existing directory:", work)
		return 2
	}
	be, err := resolveLaunchBackend(*backend)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	result, err := cliProvisionService().Provision(ProvisionRequest{
		Actor: "local-cli", RequestID: newRequestID(), Session: *name,
		Workdir: work, Backend: be, Harness: "muse-code",
		Model: *model, Provider: *provider, Profile: *profile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("Muse Code running: session=%s backend=%s provider=%s model=%s workdir=%s\n", result.SessionID, result.Backend, *provider, orDash(*model), work)
	if result.Backend == provisionBackendPTY {
		fmt.Printf("Watch output: metis watch (TUI text snapshot) or MCP metis_terminal for %s\n", result.SessionID)
		return 0
	}
	entry := loadRegistry()[result.SessionID]
	target := entry.Tmux
	if target == "" {
		target = tmuxName(result.SessionID)
	}
	fmt.Printf("External terminal: tmux attach -t %s\n", target)
	return 0
}

// cliProvisionService owns the ProvisionService used by CLI session
// commands. A package variable (not a parameter thread) so focused
// CLI tests can substitute the stub launcher without spawning
// processes; production always uses DefaultProvisionService.
var cliProvisionService = DefaultProvisionService

// resolveLaunchBackend maps the launch --backend flag to the
// provision backend. Empty means the PTY default; pty and tmux pass
// through; anything else (including muse-native, which has no shell
// launch path) is refused before any provisioning side effect.
func resolveLaunchBackend(flag string) (string, error) {
	switch strings.TrimSpace(flag) {
	case "", provisionBackendPTY:
		return provisionBackendPTY, nil
	case provisionBackendTMUX:
		return provisionBackendTMUX, nil
	default:
		return "", fmt.Errorf("launch --backend must be pty or tmux, got %q", flag)
	}
}

// cliStop handles the 'stop' command.
func cliStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	_ = fs.Parse(args)
	if err := stopSession(*name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("stopped %s\n", *name)
	return 0
}

// cliAdopt handles the 'adopt' command.
func cliAdopt(args []string) int {
	fs := flag.NewFlagSet("adopt", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	_ = fs.Parse(args)
	if _, err := adoptSession(*name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("adopted %s\n", *name)
	return 0
}

// publicRegistrySession projects a registry (tmux) session with
// observed — not merely recorded — liveness. The stored Stopped
// flag says what the last control action did; sessionRunning asks
// the multiplexer what exists now. A row never marked stopped
// whose tmux session is gone reports "gone", never "running":
// recorded history must not masquerade as a live worker. A row
// marked stopped stays "stopped" — stopped is stopped, whether or
// not a stray live session lingers under the name.
func publicRegistrySession(s Session) PublicSession {
	out := s.Public()
	if !s.Stopped && !sessionRunning(s.Name) {
		out.Status = "gone"
	}
	return out
}

// publicPTYSession projects a PTY-backed session into the same
// machine-readable shape as a registry session. The name carries
// the pty: prefix so it matches the text list and the TUI
// selection keys, and it round-trips through stopSession's
// backend-qualified identities.
func publicPTYSession(m ptyMeta) PublicSession {
	return PublicSession{
		Name: ptyDisplay(m.Name), Backend: "pty", Status: ptyClassify(m),
		Workdir: m.Workdir, Command: m.Command,
		Harness: m.Harness, Model: m.Model,
	}
}

// cliList handles the 'list' command.
func cliList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	reg := loadRegistry()
	names := sortedNames(reg)
	metas, err := ptyListAll()
	if err != nil {
		// An unreadable PTY registry is unavailable, not empty:
		// listing a partial universe as complete would hide
		// workers from the operator. Fail loudly instead.
		fmt.Fprintf(os.Stderr, "list: PTY registry unavailable: %v\n", err)
		return 1
	}
	if *asJSON {
		out := make([]PublicSession, 0, len(names)+len(metas))
		for _, n := range names {
			out = append(out, publicRegistrySession(reg[n]))
		}
		for _, mt := range metas {
			out = append(out, publicPTYSession(mt))
		}
		emitJSON(out)
		return 0
	}
	if len(reg) == 0 && len(metas) == 0 {
		fmt.Println("(no sessions)")
		return 0
	}
	for _, n := range names {
		e := reg[n]
		fmt.Printf("%s  harness=%s  model=%s  tmux=%s  running=%v  workdir=%s\n",
			n, orDash(e.Harness), orDash(e.Model), e.Tmux, sessionRunning(n), e.Workdir)
	}
	for _, mt := range metas {
		fmt.Printf("%s  backend=pty  status=%s  workdir=%s\n",
			ptyDisplay(mt.Name), ptyClassify(mt), mt.Workdir)
	}
	return 0
}

// cliSnapshot handles the 'snapshot' command.
func cliSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	_ = fs.Parse(args)
	fmt.Print(snapshotText(*name))
	return 0
}

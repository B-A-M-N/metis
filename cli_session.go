package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// cli_session.go: session lifecycle commands (up/run/start/stop/adopt/list/snapshot). Extracted from main.go; behavior unchanged.

// cliUp handles the 'up' command.
func cliUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	session := fs.String("session", "demo", "session name")
	workdir := fs.String("workdir", "", "workdir (default ~/tui-dash-work)")
	cmd := fs.String("cmd", "", "worker command (default: this binary's demo worker)")
	_ = fs.Parse(args)
	wd := *workdir
	if wd == "" {
		home, _ := os.UserHomeDir()
		wd = filepath.Join(home, "tui-dash-work")
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

// cliList handles the 'list' command.
func cliList(args []string) int {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	reg := loadRegistry()
	names := sortedNames(reg)
	if *asJSON {
		out := make([]PublicSession, 0, len(names))
		for _, n := range names {
			out = append(out, reg[n].Public())
		}
		emitJSON(out)
		return 0
	}
	if len(reg) == 0 {
		fmt.Println("(no sessions)")
		return 0
	}
	for _, n := range names {
		e := reg[n]
		fmt.Printf("%s  harness=%s  model=%s  tmux=%s  running=%v  workdir=%s\n",
			n, orDash(e.Harness), orDash(e.Model), e.Tmux, sessionRunning(n), e.Workdir)
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

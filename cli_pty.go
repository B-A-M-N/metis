package main

import (
	"flag"
	"fmt"
	"os"
)

// cli_pty.go: managed PTY commands. Extracted from main.go; behavior unchanged.

// cliPtyStart handles the 'pty-start' command.
func cliPtyStart(args []string) int {
	fs := flag.NewFlagSet("pty-start", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	workdir := fs.String("workdir", "", "workdir")
	cmd := fs.String("cmd", "", "worker command")
	harness := fs.String("harness", "", "worker harness (e.g. demo, muse, codex)")
	model := fs.String("model", "", "loaded model name, if known")
	_ = fs.Parse(args)
	if *name == "" || *workdir == "" || *cmd == "" {
		fmt.Fprintln(os.Stderr, "pty-start requires --name, --workdir, --cmd")
		return 2
	}
	if err := ptyStart(*name, *workdir, *cmd, *harness, *model); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("pty session %s running\n", *name)
	return 0
}

// cliPtySupervise handles the 'pty-supervise' command.
func cliPtySupervise(args []string) int {
	fs := flag.NewFlagSet("pty-supervise", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	_ = fs.Parse(args)
	if err := ptySupervise(*name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// cliPtyLog handles the 'pty-log' command.
func cliPtyLog(args []string) int {
	fs := flag.NewFlagSet("pty-log", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	tail := fs.Int("bytes", 4000, "tail bytes")
	_ = fs.Parse(args)
	data, err := ptyLog(*name, *tail)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Print(string(data))
	return 0
}

// cliPtyInput handles the 'pty-input' command.
func cliPtyInput(args []string) int {
	fs := flag.NewFlagSet("pty-input", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	owner := fs.String("owner", "", "input owner (default: this process)")
	text := fs.String("text", "", "text to send")
	_ = fs.Parse(args)
	who := *owner
	if who == "" {
		who = controllerID()
	}
	id, err := ptyInput(*name, who, *text)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// Queued to the supervisor fifo; use pty-receipt to learn
	// whether it reached the PTY (written_to_pty) or failed.
	fmt.Printf("queued %s to %s\n", id, *name)
	return 0
}

// cliPtyReceipt handles the 'pty-receipt' command.
func cliPtyReceipt(args []string) int {
	fs := flag.NewFlagSet("pty-receipt", flag.ExitOnError)
	id := fs.String("id", "", "input command id from pty-input")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	status, detail, err := ptyInputStatus(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(map[string]string{"id": *id, "status": status, "detail": detail})
		return 0
	}
	if detail != "" {
		fmt.Printf("%s: %s (%s)\n", *id, status, detail)
	} else {
		fmt.Printf("%s: %s\n", *id, status)
	}
	return 0
}

// cliPtyResize handles the 'pty-resize' command.
func cliPtyResize(args []string) int {
	fs := flag.NewFlagSet("pty-resize", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	rows := fs.Int("rows", 24, "rows")
	cols := fs.Int("cols", 80, "cols")
	_ = fs.Parse(args)
	if err := ptyResize(*name, *rows, *cols); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// cliPtyStatus handles the 'pty-status' command.
func cliPtyStatus(args []string) int {
	fs := flag.NewFlagSet("pty-status", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	m, state, err := ptyStatus(*name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(ptyView{Session: m.publicPty(), State: state})
		return 0
	}
	fmt.Printf("name: %s  state: %s  status: %s  exit: %d  pts: %s  gen: %d\n",
		m.Name, state, m.Status, m.ExitCode, orDash(m.Pts), m.Generation)
	return 0
}

// cliPtyStop handles the 'pty-stop' command.
func cliPtyStop(args []string) int {
	fs := flag.NewFlagSet("pty-stop", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	_ = fs.Parse(args)
	if err := ptyStop(*name); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("stopped %s\n", *name)
	return 0
}

// cliPtyList handles the 'pty-list' command.
func cliPtyList(args []string) int {
	fs := flag.NewFlagSet("pty-list", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	sessions, err := ptyListAll()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		views, err := ptyViewAll()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if views == nil {
			views = []ptyView{}
		}
		emitJSON(views)
		return 0
	}
	if len(sessions) == 0 {
		fmt.Println("(no PTY sessions)")
		return 0
	}
	for _, m := range sessions {
		_, state, _ := ptyStatus(m.Name)
		fmt.Printf("%s  state=%s  worker=%d  cmd=%s\n", m.Name, state, m.WorkerPid, m.Command)
	}
	return 0
}

package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// cli_misc.go: diagnostics, demo, graph, and MCP entry points. Extracted from main.go; behavior unchanged.

// cliDoctor handles the 'doctor' command.
func cliDoctor(args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	// No model inference, no writes: doctorReport opens the
	// database query_only and never migrates or receipts.
	rep, err := doctorReport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(rep)
		return 0
	}
	for _, c := range rep.Conditions {
		fmt.Printf("[%s] %s: %s (source: %s)\n", c.Status, c.Name, c.Detail, c.Source)
	}
	return 0
}

// cliDemo handles the 'demo' command.
func cliDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	workdir := fs.String("workdir", ".", "workdir")
	steps := fs.Int("steps", 60, "steps to run")
	delay := fs.Float64("delay", 1, "seconds per step")
	_ = fs.Parse(args)
	if err := runDemo(*workdir, *steps, time.Duration(*delay*float64(time.Second))); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// cliGraph handles the 'graph' command.
func cliGraph(args []string) int {
	// Read-only deterministic projections over a run's lineage.
	// No TUI, no daemon, no execution: pure SQLite reads.
	if (len(args) + 2) < 4 {
		fmt.Fprintln(os.Stderr, "usage: metis graph walk|blockers|ready|coverage --run RUN")
		return 2
	}
	if err := graphCmd(args[0], args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// cliMcp handles the 'mcp' command.
func cliMcp(args []string) int {
	// Thin stdio adapter for model clients (local Muse Code
	// directly; remote models through Ocliv's `mcp expose`,
	// which hosts this stdio interface).
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: metis mcp serve | metis mcp serve-http --listen 127.0.0.1:PORT")
		return 2
	}
	switch args[0] {
	case "serve":
		mcpServeStdio()
		return 0
	case "serve-http":
		// Authenticated HTTP transport for Ocliv `mcp publish
		// --local` publication. Binds numeric loopback only; every
		// call needs a JWT from the configured external issuer.
		listen := "127.0.0.1:0"
		rest := args[1:]
		for i := 0; i < len(rest); i++ {
			a := rest[i]
			if v, ok := strings.CutPrefix(a, "--listen="); ok {
				listen = v
			} else if a == "--listen" && i+1 < len(rest) {
				listen = rest[i+1]
				i++
			} else {
				fmt.Fprintln(os.Stderr, "usage: metis mcp serve-http --listen 127.0.0.1:PORT")
				return 2
			}
		}
		return mcpServeHTTP(listen)
	default:
		fmt.Fprintln(os.Stderr, "usage: metis mcp serve | metis mcp serve-http --listen 127.0.0.1:PORT")
		return 2
	}
}

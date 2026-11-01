package main

import (
	"flag"
	"fmt"
)

// cli_agents.go: agent listing. Extracted from main.go; behavior unchanged.

// cliAgents handles the 'agents' command.
func cliAgents(args []string) int {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	views := adapterViews()
	if *asJSON {
		emitJSON(views)
		return 0
	}
	for _, a := range views {
		fmt.Printf("%s  one_shot=%v  interactive=%v  live_input=%v\n",
			a.Harness, a.OneShot, a.Interactive, a.LiveInput)
	}
	return 0
}

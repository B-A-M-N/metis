package main

// cli_graph.go: `tui-dash graph <op> --run RUN` dispatch. Read-only
// deterministic projections over a run's lineage: no TUI, no
// daemon, no execution — pure SQLite reads. (The `graph`
// subcommand entry point cliGraph lives with the other CLI entry
// points; this file owns the op dispatch it calls.)

import (
	"encoding/json"
	"fmt"
)

// graphCmd implements `tui-dash graph <op> --run RUN`.
func graphCmd(op string, args []string) error {
	var run string
	for i := 0; i < len(args); i++ {
		if args[i] == "--run" && i+1 < len(args) {
			run = args[i+1]
			i++
		}
	}
	if run == "" {
		return fmt.Errorf("graph %s requires --run RUN", op)
	}
	var (
		v   any
		err error
	)
	switch op {
	case "walk":
		v, err = buildRunGraph(run)
	case "blockers":
		v, err = graphBlockers(run)
	case "ready":
		v, err = graphReady(run)
	case "coverage":
		v, err = graphCoverage(run)
	case "project":
		v, err = buildCoverageGraph(run)
	default:
		return fmt.Errorf("unknown graph op %q: walk|blockers|ready|coverage|project", op)
	}
	if err != nil {
		return err
	}
	out, merr := json.MarshalIndent(v, "", "  ")
	if merr != nil {
		return merr
	}
	fmt.Println(string(out))
	return nil
}

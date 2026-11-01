package main

import (
	"flag"
	"fmt"
	"os"
)

// cli_attention.go: operator attention commands.
//
// attention list --run R [--all]
// attention open --run R --task T --kind K --reason R [--session S --evidence E]
// attention resolve --id ID --rev N --outcome O
// attention dismiss --id ID --rev N --outcome O
//
// Resolve/dismiss quote the revision they saw: a stale revision is
// refused, so two racing operators cannot both win silently.

func cliAttention(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: attention <list|open|resolve|dismiss> [flags]")
		return 1
	}
	switch args[0] {
	case "list":
		return cliAttentionList(args[1:])
	case "open":
		return cliAttentionOpen(args[1:])
	case "resolve", "dismiss":
		return cliAttentionSettle(args[0], args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown attention subcommand %q\n", args[0])
		return 1
	}
}

func cliAttentionList(args []string) int {
	fs := flag.NewFlagSet("attention list", flag.ExitOnError)
	run := fs.String("run", "", "run id (empty: all)")
	all := fs.Bool("all", false, "include resolved and dismissed rows")
	_ = fs.Parse(args)
	attns, err := listAttention(*run, !*all)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, a := range attns {
		fmt.Printf("%s rev=%d %s %s task=%s reason=%s\n", a.ID, a.Revision, a.Status, a.Kind, a.TaskID, a.Reason)
	}
	return 0
}

func cliAttentionOpen(args []string) int {
	fs := flag.NewFlagSet("attention open", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	task := fs.String("task", "", "task id")
	session := fs.String("session", "", "session name")
	kind := fs.String("kind", "", "one of: uncertain_outcome repeated_failure conflicting_findings missing_approval replan signoff")
	reason := fs.String("reason", "", "why escalation is needed")
	evidence := fs.String("evidence", "[]", "evidence references (JSON array)")
	actor := fs.String("actor", "local-cli", "who opens the escalation")
	_ = fs.Parse(args)
	req := OperationRequest{Actor: *actor, RequestID: newRequestID()}
	id, err := openAttention(req, *run, *task, *session, *kind, *reason, *evidence)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("opened %s\n", id)
	return 0
}

func cliAttentionSettle(action string, args []string) int {
	fs := flag.NewFlagSet("attention "+action, flag.ExitOnError)
	id := fs.String("id", "", "escalation id")
	rev := fs.Int("rev", -1, "expected revision (from list)")
	outcome := fs.String("outcome", "", "decision outcome")
	actor := fs.String("actor", "local-cli", "who decides")
	_ = fs.Parse(args)
	if *id == "" || *rev < 0 || *outcome == "" {
		fmt.Fprintln(os.Stderr, "attention "+action+" requires --id, --rev, and --outcome")
		return 1
	}
	by := *actor + " (local-cli)"
	var err error
	if action == "resolve" {
		err = resolveAttention(*id, *rev, by, *outcome)
	} else {
		err = dismissAttention(*id, *rev, by, *outcome)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("%s %s at rev %d\n", action+"d", *id, *rev)
	return 0
}

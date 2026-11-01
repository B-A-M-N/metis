package main

import (
	"flag"
	"fmt"
	"os"
)

// cli_guidance.go: guidance queue commands. Extracted from main.go; behavior unchanged.

// cliInject handles the 'inject' command.
func cliInject(args []string) int {
	fs := flag.NewFlagSet("inject", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	text := fs.String("text", "", "guidance text")
	direct := fs.Bool("direct", false, "also send-keys to the tmux pane")
	_ = fs.Parse(args)
	if *name == "" || *text == "" {
		fmt.Fprintln(os.Stderr, "inject requires --name and --text")
		return 2
	}
	gid, err := submitGuidance(*name, *text, *direct, OperationRequest{Actor: "local-cli", RequestID: newRequestID()})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if g, gerr := guidanceStatus(gid); gerr == nil {
		fmt.Printf("guidance %s to %s (%s)\n", gid, *name, g.State)
	} else {
		fmt.Printf("guidance %s to %s\n", gid, *name)
	}
	return 0
}

// cliGuidanceList handles the 'guidance-list' command.
func cliGuidanceList(args []string) int {
	fs := flag.NewFlagSet("guidance-list", flag.ExitOnError)
	name := fs.String("name", "", "session name")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	list, err := listGuidance(*name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if list == nil {
			list = []Guidance{}
		}
		emitJSON(list)
		return 0
	}
	if len(list) == 0 {
		fmt.Println("(no guidance)")
		return 0
	}
	for _, g := range list {
		fmt.Printf("%s session=%s mode=%s state=%s actor=%s req=%s created=%s\n  text: %s\n  outcome: %s\n",
			g.ID, g.Session, g.Mode, g.State, g.Actor, orDash(g.RequestID), g.Created, g.Text, orDash(g.Detail))
	}
	return 0
}

// cliGuidanceStatus handles the 'guidance-status' command.
func cliGuidanceStatus(args []string) int {
	fs := flag.NewFlagSet("guidance-status", flag.ExitOnError)
	id := fs.String("id", "", "guidance id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	g, err := guidanceStatus(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(g)
		return 0
	}
	fmt.Printf("id: %s\nsession: %s\nmode: %s\nstate: %s\nactor: %s\nrequest: %s\ncreated: %s\nupdated: %s\ntext: %s\noutcome: %s\n",
		g.ID, g.Session, g.Mode, g.State, g.Actor, orDash(g.RequestID), g.Created, g.Updated, g.Text, orDash(g.Detail))
	return 0
}

// cliGuidanceOutstanding handles the 'guidance-outstanding' command.
func cliGuidanceOutstanding(args []string) int {
	fs := flag.NewFlagSet("guidance-outstanding", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	out, err := outstandingGuidance()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if out == nil {
			out = []Guidance{}
		}
		emitJSON(out)
		return 0
	}
	if len(out) == 0 {
		fmt.Println("(none outstanding)")
		return 0
	}
	for _, g := range out {
		fmt.Printf("%s session=%s mode=%s actor=%s req=%s created=%s text: %s\n",
			g.ID, g.Session, g.Mode, g.Actor, orDash(g.RequestID), g.Created, g.Text)
	}
	return 0
}

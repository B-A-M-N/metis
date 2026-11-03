package main

// cli_m2m.go: operator-controlled M2M inbox intake.
//
// `metis m2m receive` is the production M2M workflow: fetch the
// configured Python client's verified inbox, dispatch each item to
// one bound session through the single dispatch choke point, and
// acknowledge what Metis durably decided. It runs bounded (ctx
// timeout), sequentially (replay converges on the dispatch claim),
// and exits nonzero when anything needs a later run.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"
)

func cliM2M(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: metis m2m receive --session NAME [--wait SECS] [--timeout SECS] [--json]")
		return 2
	}
	switch args[0] {
	case "receive":
		return cliM2MReceive(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown m2m subcommand %q: want receive\n", args[0])
		return 2
	}
}

func cliM2MReceive(args []string) int {
	fs := flag.NewFlagSet("m2m receive", flag.ContinueOnError)
	session := fs.String("session", "", "bound target session for fetched inbox items")
	wait := fs.Int("wait", 30, "seconds to observe each turn to its terminal")
	timeout := fs.Int("timeout", 180, "seconds bounding the whole run")
	asJSON := fs.Bool("json", false, "machine-readable report")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: metis m2m receive --session NAME [--wait SECS] [--timeout SECS] [--json]")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Fetch the M2M client's verified inbox (METIS_M2M_CLIENT),")
		fmt.Fprintln(os.Stderr, "dispatch each item to the bound session, and acknowledge")
		fmt.Fprintln(os.Stderr, "durable decisions back through the client. Policy-refused")
		fmt.Fprintln(os.Stderr, "items ack REFUSED; dispatch errors stay unacknowledged for")
		fmt.Fprintln(os.Stderr, "a later run. Exits nonzero when anything needs retry.")
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	target := *session
	if target == "" {
		fmt.Fprintln(os.Stderr, "m2m receive needs --session NAME")
		fs.Usage()
		return 2
	}
	if *wait < 0 || *wait > 300 {
		fmt.Fprintln(os.Stderr, "m2m receive --wait must be between 0 and 300 seconds")
		return 2
	}
	if *timeout < 1 || *timeout > 1800 {
		fmt.Fprintln(os.Stderr, "m2m receive --timeout must be between 1 and 1800 seconds")
		return 2
	}
	b, err := getMuseBinding(target)
	if err != nil || b.NativeID == "" {
		fmt.Fprintf(os.Stderr, "m2m receive: no muse binding for session %s; bind it first\n", target)
		return 1
	}
	c, err := resolveMuseCommander(b.Profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "m2m receive:", err)
		return 1
	}
	if err := museRequireDaemonFor(c); err != nil {
		fmt.Fprintln(os.Stderr, "m2m receive:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeout)*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, b.Provider, nil, "--join")
	if err != nil {
		fmt.Fprintln(os.Stderr, "m2m receive: host:", err)
		return 1
	}
	defer h.Close()
	rep := m2mReceiveDispatch(ctx, h, target, time.Duration(*wait)*time.Second)
	if *asJSON {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(raw))
	} else {
		fmt.Printf("m2m receive -> session %s: %d received\n", rep.Session, rep.Received)
		for _, it := range rep.Outcomes {
			fmt.Printf("  %s: %s %s\n", it.MessageID, it.Outcome, it.Detail)
		}
		for _, m := range rep.Malformed {
			fmt.Printf("  malformed: %s\n", m)
		}
		if rep.ClientFailed != "" {
			fmt.Printf("  client failed: %s\n", rep.ClientFailed)
		}
	}
	if rep.ClientFailed != "" || rep.Unacked > 0 || len(rep.Malformed) > 0 {
		return 1
	}
	return 0
}

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"
)

// cli_muse.go: native Muse session commands (sub-dispatched). Extracted from main.go; behavior unchanged.

// cliMuse handles the 'muse' command.
func cliMuse(args []string) int {
	// Native Muse sessions over MSP. The provider is always
	// explicit (no silent default); each invocation spawns its
	// own short-lived host and resumes the durable native
	// session, so no daemon is required for correctness.
	if (len(args) + 2) < 3 {
		fmt.Fprintln(os.Stderr, "usage: metis muse daemon|bind|turn|status|approvals|approve ...")
		return 2
	}
	switch args[0] {
	case "daemon":
		fs := flag.NewFlagSet("muse daemon", flag.ExitOnError)
		provider := fs.String("provider", "", "muse provider to serve (required)")
		profile := fs.String("profile", "", "commander profile: \"\" for muse, \"musefi\" for the separate FI launcher")
		_ = fs.Parse(args[1:])
		if *provider == "" {
			fmt.Fprintln(os.Stderr, "muse daemon requires --provider")
			return 2
		}
		if err := museCmdDaemon(*provider, *profile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "bind":
		fs := flag.NewFlagSet("muse bind", flag.ExitOnError)
		session := fs.String("session", "", "metis worker session name")
		provider := fs.String("provider", "", "muse provider (e.g. echo, meta; required)")
		profile := fs.String("profile", "", "commander profile: \"\" for muse, \"musefi\" for the separate FI launcher")
		title := fs.String("title", "", "native session title (default: metis session name)")
		actor := fs.String("actor", "local-cli", "attributed actor")
		_ = fs.Parse(args[1:])
		if *session == "" || *provider == "" {
			fmt.Fprintln(os.Stderr, "muse bind requires --session and --provider")
			return 2
		}
		if *title == "" {
			*title = "metis:" + *session
		}
		if err := museCmdBind(*session, *provider, *profile, *title, *actor); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "turn":
		fs := flag.NewFlagSet("muse turn", flag.ExitOnError)
		session := fs.String("session", "", "metis worker session name")
		profile := fs.String("profile", "", "commander profile (must match bind)")
		text := fs.String("text", "", "input text for the turn")
		provider := fs.String("provider", "", "muse provider (required; must match bind)")
		timeout := fs.String("timeout", "5m", "how long to wait for turn/completed")
		_ = fs.Parse(args[1:])
		if *session == "" || *text == "" || *provider == "" {
			fmt.Fprintln(os.Stderr, "muse turn requires --session, --text and --provider")
			return 2
		}
		d, err := time.ParseDuration(*timeout)
		if err != nil || d <= 0 {
			fmt.Fprintln(os.Stderr, "muse turn needs a positive --timeout")
			return 2
		}
		if err := museCmdTurn(*session, *provider, *profile, *text, d); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "status":
		fs := flag.NewFlagSet("muse status", flag.ExitOnError)
		session := fs.String("session", "", "metis worker session name")
		provider := fs.String("provider", "", "muse provider (required)")
		profile := fs.String("profile", "", "commander profile (must match bind)")
		_ = fs.Parse(args[1:])
		if *session == "" || *provider == "" {
			fmt.Fprintln(os.Stderr, "muse status requires --session and --provider")
			return 2
		}
		if err := museCmdStatus(*session, *provider, *profile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "approvals":
		fs := flag.NewFlagSet("muse approvals", flag.ExitOnError)
		session := fs.String("session", "", "metis worker session name")
		provider := fs.String("provider", "", "muse provider (required; must match bind)")
		profile := fs.String("profile", "", "commander profile (must match bind)")
		_ = fs.Parse(args[1:])
		if *session == "" || *provider == "" {
			fmt.Fprintln(os.Stderr, "muse approvals requires --session and --provider")
			return 2
		}
		if err := museCmdApprovals(*session, *provider, *profile); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	case "approve":
		fs := flag.NewFlagSet("muse approve", flag.ExitOnError)
		session := fs.String("session", "", "metis worker session name")
		provider := fs.String("provider", "", "muse provider (required; must match bind)")
		profile := fs.String("profile", "", "commander profile (must match bind)")
		id := fs.String("id", "", "approval id from muse approvals (required)")
		choice := fs.String("choice", "", "offered choiceId to send (required)")
		feedback := fs.String("feedback", "", "optional reviewer note")
		actor := fs.String("actor", "", "deciding identity for the receipt (required)")
		_ = fs.Parse(args[1:])
		if *session == "" || *provider == "" || *id == "" || *choice == "" || *actor == "" {
			fmt.Fprintln(os.Stderr, "muse approve requires --session --provider --id --choice --actor")
			return 2
		}
		if err := museCmdApprove(*session, *provider, *profile, *id, *choice, *feedback, *actor); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	default:
		fmt.Fprintln(os.Stderr, "usage: metis muse bind|turn|status ...")
		return 2
	}
	return 0
}

// museCmdApprove decides one pending approval the durable row
// already knows. The choice must be offered, the requirement guard
// rides along, and the decision is receipted with actor identity.
func museCmdApprove(session, provider, profile, approvalID, choiceID, feedback, actor string) error {
	req := OperationRequest{Actor: actor, RequestID: newRequestID()}
	if err := museApproveViaBinding(session, provider, profile, approvalID, choiceID, feedback, req); err != nil {
		return err
	}
	fmt.Printf("approval %s decided %s (session %s)\n", approvalID, choiceID, session)
	return nil
}

// museCmdApprovals prints the server fold plus the durable rows for
// a bound Metis session. Read-only: no lease, no decision.
func museCmdApprovals(session, provider, profile string) error {
	c, err := resolveMuseCommander(profile)
	if err != nil {
		return err
	}
	b, err := getMuseBinding(session)
	if err != nil {
		return err
	}
	if err := checkMuseBindingScope(b, provider, profile); err != nil {
		return err
	}
	if err := museRequireDaemonFor(c); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, c.Binary, provider, nil, "--join")
	if err != nil {
		return err
	}
	defer h.Close()
	fold, err := museListPendingApprovals(ctx, h, b.NativeID)
	if err != nil {
		return err
	}
	rows, err := listMuseApprovals(session, false)
	if err != nil {
		return err
	}
	byID := map[string]string{}
	for _, r := range rows {
		byID[r.ApprovalID] = r.Status
	}
	if len(fold.Approvals) == 0 && len(fold.UserInputs) == 0 {
		fmt.Println("(no pending approvals or prompts)")
		return nil
	}
	for _, a := range fold.Approvals {
		id, _ := a["approvalId"].(string)
		fmt.Printf("approval %s durable=%s\n", id, orDash(byID[id]))
	}
	for _, u := range fold.UserInputs {
		fmt.Printf("user-input %v\n", u["promptId"])
	}
	return nil
}

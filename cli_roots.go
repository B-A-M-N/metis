package main

import (
	"flag"
	"fmt"
	"os"
)

// cli_roots.go: authorized project-root registry commands
// (P1-2). Registration is a local-operator action by design: it is
// the trust anchor for model-facing provisioning, so no MCP tool
// exposes it.

// cliProjectRootRegister handles 'project-root-register'.
func cliProjectRootRegister(args []string) int {
	fs := flag.NewFlagSet("project-root-register", flag.ExitOnError)
	root := fs.String("root", "", "authorized project root directory")
	custom := fs.Bool("custom", false, "allow explicit custom commands under this root")
	_ = fs.Parse(args)
	if *root == "" {
		fmt.Fprintln(os.Stderr, "project-root-register requires --root")
		return 2
	}
	canon, err := registerProjectRoot(*root, *custom)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("registered %s custom=%v\n", canon, *custom)
	return 0
}

// cliProjectRootRevoke handles 'project-root-revoke'.
func cliProjectRootRevoke(args []string) int {
	fs := flag.NewFlagSet("project-root-revoke", flag.ExitOnError)
	root := fs.String("root", "", "project root directory to revoke")
	_ = fs.Parse(args)
	if *root == "" {
		fmt.Fprintln(os.Stderr, "project-root-revoke requires --root")
		return 2
	}
	if err := revokeProjectRoot(*root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("revoked %s\n", *root)
	return 0
}

// cliProjectRoots handles 'project-roots'.
func cliProjectRoots(args []string) int {
	fs := flag.NewFlagSet("project-roots", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	roots, err := listProjectRoots()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(roots)
		return 0
	}
	if len(roots) == 0 {
		fmt.Println("(no project roots registered)")
		return 0
	}
	for _, r := range roots {
		fmt.Printf("%s  custom=%v\n", r.Root, r.AllowCustom)
	}
	return 0
}

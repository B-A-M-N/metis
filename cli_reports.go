package main

import (
	"flag"
	"fmt"
	"os"
)

// cli_reports.go: report inspection commands. Extracted from main.go; behavior unchanged.

// cliReportArtifact handles the 'report-artifact' command.
func cliReportArtifact(args []string) int {
	fs := flag.NewFlagSet("report-artifact", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	path := fs.String("path", "", "evidence file path")
	_ = fs.Parse(args)
	a, err := attachArtifact(*id, *path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("attached %s %s\n", a.Path, a.Digest)
	return 0
}

// cliReportGet handles the 'report-get' command.
func cliReportGet(args []string) int {
	fs := flag.NewFlagSet("report-get", flag.ExitOnError)
	id := fs.String("task", "", "task id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	fr, err := getFullReport(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		emitJSON(fr)
		return 0
	}
	fmt.Printf("task: %s  agent: %s  status: %s  attempt: %d\nreport: %s\n",
		fr.Task.ID, fr.Task.Agent, fr.Task.Status, fr.Task.Attempt, orDash(fr.Task.Report))
	for _, a := range fr.Artifacts {
		ok, verr := verifyArtifact(a)
		mark := "OK"
		if verr != nil {
			mark = "UNREADABLE"
		} else if !ok {
			mark = "MODIFIED"
		}
		fmt.Printf("artifact: %s  %s  [%s]\n", a.Path, a.Digest, mark)
	}
	if len(fr.Artifacts) == 0 {
		fmt.Println("(no artifacts attached)")
	}
	for _, r := range fr.Receipts {
		fmt.Printf("receipt: %s  %s  %s\n", r.Kind, r.Detail, r.Created)
	}
	if len(fr.Receipts) == 0 {
		fmt.Println("(no receipts recorded)")
	}
	return 0
}

// cliCompareReports handles the 'compare-reports' command.
func cliCompareReports(args []string) int {
	fs := flag.NewFlagSet("compare-reports", flag.ExitOnError)
	run := fs.String("run", "", "run id")
	asJSON := fs.Bool("json", false, "machine-readable output")
	_ = fs.Parse(args)
	tasks, err := compareReports(*run)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *asJSON {
		if tasks == nil {
			tasks = []Task{}
		}
		emitJSON(tasks)
		return 0
	}
	if len(tasks) == 0 {
		fmt.Println("(no reported tasks in run)")
		return 0
	}
	for _, t := range tasks {
		fmt.Printf("--- %s  agent=%s  status=%s ---\n%s\n", t.ID, t.Agent, t.Status, t.Report)
	}
	return 0
}

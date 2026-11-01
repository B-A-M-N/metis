package main

// tui-dash: a Go Bubble Tea dashboard for a visible coding agent.
//
// Usage (friendly forms; old single-word names still work):
//   tui-dash watch [--session NAME]      open the dashboard
//   tui-dash up ...                      start session if needed, then open it
//   tui-dash agent start|stop|list|adopt|snapshot|work ...
//   tui-dash send --name NAME --text TEXT
//   tui-dash tasks | task create|status|dispatch|report|verify|cancel|followup|depend|...
//   tui-dash guides | guide list|status|pending
//   tui-dash report get|compare|attach ...
//   tui-dash run finalize|limit ... | graph ... | muse ... | mcp serve
//   tui-dash demo --workdir DIR [--steps N] [--delay SECONDS]
//       The simulated worker used by `up` by default.
//
// This binary does NOT show Muse's own chat work. It shows a worker
// agent running in a tmux session (tui-<name>), which is what the
// user watches and guides. Guidance defaults to file mode.

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"syscall"
)

// emitJSON writes v as indented JSON for machine consumers.
// Human-readable text stays the default; --json opts in per command.
func emitJSON(v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(data))
}

func usage() {
	fmt.Println(`tui-dash - live TUI for visible coding agents

  tui-dash watch [--session NAME]
      Open the dashboard (same as: run).
  tui-dash up [--session NAME] [--workdir DIR] [--cmd CMD]
      Start the session if it is not running, then open the dashboard.
  tui-dash agent start --name NAME --workdir DIR [--cmd CMD | --harness H --prompt TEXT] [--model M]
  tui-dash agent list [--json]  (also: agents, workers, sessions)
  tui-dash agent stop --name NAME | agent adopt --name NAME
  tui-dash agent snapshot [--name NAME] | agent work --session NAME [--once]
  tui-dash send --name NAME --text TEXT [--direct]
      Message a worker (same as: inject).
  tui-dash tasks [--run RUN]  (also: task list)
  tui-dash task create --run RUN --agent AGENT --objective TEXT [--acceptance TEXT] [--workdir DIR]
  tui-dash task status --task ID | task dispatch --task ID --session NAME
  tui-dash task report --task ID --report TEXT|@FILE
  tui-dash task verify --task ID [--pass] [--approve WHO]
  tui-dash task cancel --task ID | task followup --task ID --objective TEXT
  tui-dash task depend --task ID --on DEP-ID | task reconcile --task ID
  tui-dash task verdict --task ID --criterion C1 --verdict PASS --reason TEXT
  tui-dash task share --task ID | task set-root --task ID --workdir DIR
  tui-dash run finalize --run RUN [--json] | run limit --run RUN --max-live N
  tui-dash guide list --name NAME [--json] | guide status --id ID
  tui-dash guides  (needs-attention guidance; same as: guide pending)
  tui-dash report get --task ID | report compare --run RUN | report attach --task ID --path FILE
  tui-dash graph walk|blockers|ready|coverage --run RUN
  tui-dash muse daemon|bind|turn|status ...
  tui-dash mcp serve
      Local stdio MCP adapter for model clients (also hosted by
      Ocliv mcp expose for remote access). No HTTP listener.
  tui-dash doctor [--json]
  tui-dash demo --workdir DIR [--steps N] [--delay SECONDS]
  tui-dash pty-start --name NAME --workdir DIR --cmd CMD [--harness H] [--model M]
  tui-dash pty-list [--json] | pty-log | pty-input | pty-receipt | pty-resize | pty-status | pty-stop

Old single-word names (task-list, guidance-outstanding, inject,
run, ...) still work. Flags are unchanged.
Append --json to agent list, tasks, task status, report get,
report compare, pty-list, or pty-status for JSON output.

State: $TUI_DASH_STATE, default ~/.local/share/tui-dash/sessions.json
Docs: README.md in this folder.`)
}

// cliGroups maps `noun verb` spellings onto canonical commands:
// `task list --run R` reaches `task-list --run R` with identical
// flags. Nouns with no bare command of their own (`task`, `guide`,
// `report`, `agent`) print their verbs when given alone; `run`
// keeps its legacy meaning (open the TUI) unless followed by one of
// its verbs.
var cliGroups = map[string]map[string]string{
	"task": {
		"create": "task-create", "list": "task-list", "status": "task-status",
		"dispatch": "task-dispatch", "report": "task-report", "verify": "task-verify",
		"cancel": "task-cancel", "followup": "task-followup", "depend": "task-depend",
		"reconcile": "task-reconcile", "verdict": "task-verdict", "share": "task-share",
		"set-root": "task-set-root",
	},
	"run": {
		"finalize": "run-finalize", "limit": "run-limit",
	},
	"guide": {
		"list": "guidance-list", "status": "guidance-status", "pending": "guidance-outstanding",
	},
	"report": {
		"get": "report-get", "compare": "compare-reports", "attach": "report-artifact",
	},
	"agent": {
		"start": "start", "stop": "stop", "list": "agents", "adopt": "adopt",
		"snapshot": "snapshot", "work": "worker-loop",
	},
}

// cliFlat maps single memorable words onto canonical commands.
var cliFlat = map[string]string{
	"tasks": "task-list", "guides": "guidance-outstanding",
	"workers": "agents", "sessions": "list",
	"send": "inject", "watch": "run",
}

// aliasArgs rewrites friendly spellings to canonical commands and
// returns the arg list. Unknown words pass through untouched, so a
// typo still reaches the "unknown command" error, never a wrong
// command.
func aliasArgs(args []string) []string {
	if len(args) < 2 {
		return args
	}
	if len(args) >= 3 {
		if verbs, ok := cliGroups[args[1]]; ok {
			if canon, ok := verbs[args[2]]; ok {
				out := make([]string, 0, len(args)-1)
				out = append(out, args[0], canon)
				return append(out, args[3:]...)
			}
			return args
		}
	}
	if canon, ok := cliFlat[args[1]]; ok {
		out := make([]string, 0, len(args))
		out = append(out, args[0], canon)
		return append(out, args[2:]...)
	}
	return args
}

// groupHelp returns a verb listing for a bare noun (`tui-dash task`)
// with no verb. It reports false for real commands (including bare
// `run`, which opens the TUI) so they dispatch normally.
func groupHelp(word string) (string, bool) {
	verbs, ok := cliGroups[word]
	if !ok || word == "run" {
		return "", false
	}
	names := make([]string, 0, len(verbs))
	for v := range verbs {
		names = append(names, v)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "tui-dash %s <verb> [flags] — verbs:\n", word)
	for _, v := range names {
		fmt.Fprintf(&b, "  %s %s\n", word, v)
	}
	fmt.Fprintf(&b, "Old single-word names (e.g. %s) still work.\n", verbs[names[0]])
	return b.String(), true
}

func main() {
	// Owner-only posture for everything this tool writes (state,
	// streams, guidance, task DB). Workers inherit it too.
	syscall.Umask(0o077)
	hardenStatePerms()
	// Friendly spellings (`task list`, `send`, `watch`) normalize to
	// the canonical words before dispatch, so every handler below
	// keeps working unchanged. Canonical names keep working too.
	os.Args = aliasArgs(os.Args)
	if len(os.Args) == 2 {
		if help, ok := groupHelp(os.Args[1]); ok {
			fmt.Print(help)
			os.Exit(2)
		}
	}
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	os.Exit(dispatchCLI(os.Args[1], os.Args[2:]))
}

// dispatchCLI routes one command to its family handler and
// returns the process exit code. Extracted from main; the
// preamble above still runs first.
func dispatchCLI(cmd string, args []string) int {
	switch cmd {
	case "up":
		return cliUp(args)
	case "run":
		return cliRun(args)
	case "start":
		return cliStart(args)
	case "stop":
		return cliStop(args)
	case "adopt":
		return cliAdopt(args)
	case "list":
		return cliList(args)
	case "snapshot":
		return cliSnapshot(args)
	case "inject":
		return cliInject(args)
	case "guidance-list":
		return cliGuidanceList(args)
	case "guidance-status":
		return cliGuidanceStatus(args)
	case "guidance-outstanding":
		return cliGuidanceOutstanding(args)
	case "agents":
		return cliAgents(args)
	case "project-root-register":
		return cliProjectRootRegister(args)
	case "project-root-revoke":
		return cliProjectRootRevoke(args)
	case "project-roots":
		return cliProjectRoots(args)
	case "task-create":
		return cliTaskCreate(args)
	case "task-list":
		return cliTaskList(args)
	case "task-status":
		return cliTaskStatus(args)
	case "task-dispatch":
		return cliTaskDispatch(args)
	case "worker-loop":
		return cliWorkerLoop(args)
	case "task-report":
		return cliTaskReport(args)
	case "task-verdict":
		return cliTaskVerdict(args)
	case "doctor":
		return cliDoctor(args)
	case "task-reconcile":
		return cliTaskReconcile(args)
	case "task-verify":
		return cliTaskVerify(args)
	case "task-cancel":
		return cliTaskCancel(args)
	case "task-followup":
		return cliTaskFollowup(args)
	case "task-depend":
		return cliTaskDepend(args)
	case "run-finalize":
		return cliRunFinalize(args)
	case "run-limit":
		return cliRunLimit(args)
	case "task-set-root":
		return cliTaskSetRoot(args)
	case "task-share":
		return cliTaskShare(args)
	case "report-artifact":
		return cliReportArtifact(args)
	case "report-get":
		return cliReportGet(args)
	case "compare-reports":
		return cliCompareReports(args)
	case "pty-start":
		return cliPtyStart(args)
	case "pty-supervise":
		return cliPtySupervise(args)
	case "pty-log":
		return cliPtyLog(args)
	case "pty-input":
		return cliPtyInput(args)
	case "pty-receipt":
		return cliPtyReceipt(args)
	case "pty-resize":
		return cliPtyResize(args)
	case "pty-status":
		return cliPtyStatus(args)
	case "pty-stop":
		return cliPtyStop(args)
	case "pty-list":
		return cliPtyList(args)
	case "attention":
		return cliAttention(args)
	case "demo":
		return cliDemo(args)
	case "graph":
		return cliGraph(args)
	case "mcp":
		return cliMcp(args)
	case "muse":
		return cliMuse(args)
	case "ops":
		return cliOps(args)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		usage()
		return 2
	}
}

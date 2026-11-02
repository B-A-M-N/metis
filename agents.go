package main

// agents.go: capability-aware worker adapters.
//
// Each harness backend declares exactly what it can do. An
// unsupported operation returns an explicit error; it never silently
// queues input and claims the agent was redirected.
//
// Terminal attach (typing into a pane) is a separate concern from
// agent interactivity (the worker understanding follow-up turns).
// One-shot exec workers can be watched and typed at, but they exit
// after one assignment and offer no conversation continuity.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Adapter describes one worker harness backend.
type Adapter struct {
	// Harness is the registry key, e.g. "muse", "codex", "demo".
	Harness string `json:"harness"`
	// OneShot runs a bounded exec and exits after one assignment.
	OneShot bool `json:"one_shot"`
	// Interactive keeps a persistent session that understands
	// follow-up turns in the same conversation.
	Interactive bool `json:"interactive"`
	// AcceptsLiveInput means the agent consumes live stdin/guidance
	// turns while running (not just its launch prompt).
	AcceptsLiveInput bool `json:"live_input"`
	// BuildCommand renders the launch command for a workdir + prompt.
	BuildCommand func(workdir, prompt, model string) string
	// ManagedArgv renders the same invocation as an argument vector
	// for managed execution: no shell, no quoting games, the prompt
	// is one trailing element. Nil means the harness cannot be
	// launched by the worker loop.
	ManagedArgv func(workdir, prompt, model string) ([]string, error)
	// ManagedTurn runs one attempt as a native turn on the session's
	// bound native session. Nil means the harness has no native
	// execution path; the worker loop uses ManagedArgv. A turn that
	// cannot observe a terminal outcome returns an error and no
	// outcome: unknown is never coerced. commandID is the
	// caller-persisted idempotency key: retries of the same attempt
	// must pass the same value so the server dedups instead of
	// executing twice. Empty means the adapter mints one (no
	// cross-crash identity).
	ManagedTurn func(ctx context.Context, session, prompt string, timeout time.Duration, commandID string) (museTurnOutcome, error)
}

// museTurnOutcome is the observed result of one native turn: the
// server-authoritative terminal value plus the final answer text.
// Terminal is empty exactly when no terminal outcome was observed
// (always paired with a non-nil error from ManagedTurn).
type museTurnOutcome struct {
	TurnID   string
	Terminal string
	Text     string
}

// SupportsContinuation reports whether the harness offers a genuine
// persistent conversation: a session that survives turns AND
// consumes live input. Interactive alone is not enough — a session
// that cannot take follow-up input is not continuable (TD-053).
func (a Adapter) SupportsContinuation() bool {
	return a.Interactive && a.AcceptsLiveInput
}

func demoAdapter() Adapter {
	return Adapter{
		Harness:          "demo",
		OneShot:          true,
		AcceptsLiveInput: false,
		BuildCommand: func(workdir, prompt, model string) string {
			return fmt.Sprintf("%s demo --workdir %s --steps 120 --delay 1", shQuote(currentExecutable()), shQuote(workdir))
		},
		ManagedArgv: func(workdir, prompt, model string) ([]string, error) {
			return []string{currentExecutable(), "demo", "--workdir", workdir, "--steps", "120", "--delay", "1"}, nil
		},
	}
}

func currentExecutable() string {
	exe, err := os.Executable()
	if err != nil {
		return "metis"
	}
	return exe
}

// oneShotExecAdapter builds a bounded CLI exec invocation.
// The prompt is passed as a single trailing argument; model is
// included only when the CLI supports naming it.
func oneShotExecAdapter(harness, prog string, extraArgs []string, modelFlag string) Adapter {
	return Adapter{
		Harness:          harness,
		OneShot:          true,
		AcceptsLiveInput: false,
		BuildCommand: func(workdir, prompt, model string) string {
			var b strings.Builder
			b.WriteString(shQuote(prog))
			for _, a := range extraArgs {
				b.WriteString(" " + shQuote(a))
			}
			if model != "" && modelFlag != "" {
				b.WriteString(" " + shQuote(modelFlag) + " " + shQuote(model))
			}
			b.WriteString(" " + shQuote(prompt))
			_ = workdir
			return b.String()
		},
		// Structured twin of BuildCommand: identical arguments as a
		// vector so managed execution never passes through a shell.
		// The prompt stays one trailing element; a hostile prompt
		// cannot inject flags or commands.
		ManagedArgv: func(workdir, prompt, model string) ([]string, error) {
			if strings.TrimSpace(prog) == "" {
				return nil, fmt.Errorf("harness %q names no executable", harness)
			}
			argv := append([]string{prog}, extraArgs...)
			if model != "" && modelFlag != "" {
				argv = append(argv, modelFlag, model)
			}
			argv = append(argv, prompt)
			_ = workdir
			return argv, nil
		},
	}
}

// adapterRegistry is the known harness set. Local exec CLIs are
// launched in the session workdir by the caller.
func adapterRegistry() map[string]Adapter {
	reg := map[string]Adapter{}
	demo := demoAdapter()
	reg[demo.Harness] = demo
	// Bounded one-shot exec adapters. Flags follow each CLI's
	// documented non-interactive mode; interactive sessions for
	// these providers are not yet qualified, so Interactive stays
	// false rather than promising continuity we cannot deliver.
	for _, a := range []Adapter{
		oneShotExecAdapter("codex", "codex", []string{"exec", "--skip-git-repo-check"}, "--model"),
		oneShotExecAdapter("claude", "claude", []string{"-p"}, "--model"),
		oneShotExecAdapter("muse", "muse", []string{"exec"}, ""),
		oneShotExecAdapter("hermes", "hermes", []string{"chat", "--oneshot"}, ""),
		museNativeAdapter(),
	} {
		reg[a.Harness] = a
	}
	return reg
}

// museNativeAdapter declares the persistent native Muse path: no
// child process, no shell argv — one MSP turn on the session's bound
// native session. Interactive + live input (turns are the input
// channel), so SupportsContinuation holds once a binding exists.
func museNativeAdapter() Adapter {
	return Adapter{
		Harness:          "muse-native",
		Interactive:      true,
		AcceptsLiveInput: true,
		ManagedTurn:      museManagedTurn,
	}
}

// adapterJSON is the machine-readable capability projection.
// BuildCommand is code and cannot cross the CLI boundary.
type adapterJSON struct {
	Harness     string `json:"harness"`
	OneShot     bool   `json:"one_shot"`
	Interactive bool   `json:"interactive"`
	LiveInput   bool   `json:"live_input"`
}

func adapterViews() []adapterJSON {
	names := []string{"demo", "codex", "claude", "muse", "muse-native", "hermes"}
	out := make([]adapterJSON, 0, len(names))
	for _, n := range names {
		if a, err := lookupAdapter(n); err == nil {
			out = append(out, adapterJSON{a.Harness, a.OneShot, a.Interactive, a.AcceptsLiveInput})
		}
	}
	return out
}

func lookupAdapter(harness string) (Adapter, error) {
	if a, ok := adapterRegistry()[harness]; ok {
		return a, nil
	}
	names := []string{}
	for n := range adapterRegistry() {
		names = append(names, n)
	}
	sort.Strings(names)
	return Adapter{}, fmt.Errorf("unknown harness %q (known: %s)", harness, strings.Join(names, ", "))
}

// resolveWorkerCommand maps start flags to a launch command. An
// explicit --cmd always wins (custom worker); otherwise --harness +
// --prompt build the command from the adapter.
func resolveWorkerCommand(harness, cmd, workdir, prompt, model string) (string, error) {
	if strings.TrimSpace(cmd) != "" {
		return cmd, nil
	}
	if strings.TrimSpace(harness) == "" {
		return "", fmt.Errorf("start requires --cmd or --harness with --prompt")
	}
	a, err := lookupAdapter(harness)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("harness %q requires --prompt (or pass --cmd directly)", harness)
	}
	if !a.OneShot && !a.Interactive {
		return "", fmt.Errorf("harness %q declares no execution mode", harness)
	}
	if a.BuildCommand == nil {
		// Typed launch-mode refusal: native harnesses (muse-native)
		// provision through their session/binding path, never through
		// a shell command. Calling a nil builder would panic; refuse
		// explicitly instead.
		return "", fmt.Errorf("harness %q has no shell launch command: it provisions through its native session path (bind a native session, do not resolve a command)", harness)
	}
	return a.BuildCommand(workdir, prompt, model), nil
}

// museCodeCommand returns the curated interactive Muse Code invocation. The
// session lifecycle runs it in workdir under tmux; no prompt is supplied, so
// exactly one interactive Muse Code process starts and waits for its user.
func museCodeCommand(workdir, provider, model, profile string) (string, error) {
	argv, err := museCodeArgv(workdir, provider, model, profile)
	if err != nil {
		return "", err
	}
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = shQuote(arg)
	}
	return "exec " + strings.Join(parts, " "), nil
}

func museCodeArgv(workdir, provider, model, profile string) ([]string, error) {
	if provider != "meta" && provider != "local" && provider != "echo" {
		return nil, fmt.Errorf("provider must be meta, local, or echo")
	}
	if strings.ContainsAny(provider+model+profile+workdir, "\x00\n\r") {
		return nil, fmt.Errorf("provider, model, profile, and workdir cannot contain control characters")
	}
	program, err := exec.LookPath("muse")
	if err != nil {
		return nil, fmt.Errorf("muse executable is not on PATH")
	}
	argv := []string{program, "--provider", provider, "--workspace", workdir}
	if model != "" {
		argv = append(argv, "--model", model)
	}
	if profile != "" {
		argv = append(argv, "--permission-profile", profile)
	}
	return argv, nil
}

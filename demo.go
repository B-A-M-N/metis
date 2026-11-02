package main

// Demo worker: `metis demo`.
// A stand-in worker, not a model. It exists so `metis up` can be
// run and verified without an API key, a Hermes gateway, or a model
// endpoint. Behaviour matches the Python demo_agent.py:
//   - prints a step line per step and writes step-NNN.txt per step
//   - polls guidance.md for unseen lines, prints GUIDANCE RECEIVED,
//     and appends to guidance_ack.md
// A real worker replaces the --cmd in `metis start` / `up`.
// Hermes --oneshot workers show process output only, not a live
// tool-call trace; a live view needs an interactive/streaming worker.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func runDemo(workdir string, steps int, delay time.Duration) error {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	guidance := filepath.Join(abs, "guidance.md")
	seen := 0
	if data, err := os.ReadFile(guidance); err == nil {
		seen = len(strings.Split(strings.TrimRight(string(data), "\n"), "\n"))
		// Empty file case: TrimRight("") gives [""], count 1 is fine;
		// it only suppresses re-acking pre-existing boilerplate.
	}
	fmt.Printf("[demo-agent] workdir=%s steps=%d\n", abs, steps)
	for i := 1; i <= steps; i++ {
		name := fmt.Sprintf("step-%03d.txt", i)
		_ = os.WriteFile(filepath.Join(abs, name), []byte(fmt.Sprintf("step %d done\n", i)), 0o644)
		fmt.Printf("[demo-agent] step %d/%d: wrote %s\n", i, steps, name)
		if data, err := os.ReadFile(guidance); err == nil {
			lines := strings.Split(string(data), "\n")
			// Count lines the same way as the initial count for
			// lines beyond `seen`, ignoring headers and blanks.
			if len(lines) > seen {
				for _, line := range lines[seen:] {
					line = strings.TrimSpace(line)
					if line == "" || strings.HasPrefix(line, "#") {
						continue
					}
					fmt.Printf("[demo-agent] GUIDANCE RECEIVED: %s\n", line)
					f, err := os.OpenFile(filepath.Join(abs, "guidance_ack.md"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
					if err == nil {
						_, _ = f.WriteString(line + "\n")
						_ = f.Close()
					}
				}
			}
			seen = len(lines)
		}
		time.Sleep(delay)
	}
	fmt.Println("[demo-agent] done")
	return nil
}

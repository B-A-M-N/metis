package main

// task_idgen_crossprocess_test.go: P0-A — ID generators must be
// collision-resistant across independent OS processes, not just
// goroutines. The pre-fix generators combined a second-resolution
// timestamp with a process-local counter starting at zero, so every
// freshly started process minted the same first IDs within a second.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// runIDGenProbe executes in a re-execed child test binary (see
// TestMain): it prints count fresh IDs from the named generator, one
// per line, then the parent asserts global uniqueness.
func runIDGenProbe(kind string) {
	gen := idGenByKind(kind)
	if gen == nil {
		fmt.Fprintf(os.Stderr, "unknown idgen probe %q\n", kind)
		os.Exit(2)
	}
	for i := 0; i < 25; i++ {
		fmt.Println(gen())
	}
}

func idGenByKind(kind string) func() string {
	switch kind {
	case "task":
		return newTaskID
	case "req":
		return newRequestID
	case "atm":
		return newAttemptID
	case "dlv":
		return newDeliveryID
	case "ob":
		return newObligationID
	case "judg":
		return newJudgmentID
	default:
		return nil
	}
}

// TestConcurrentIDGeneratorsUnique pins the in-process property:
// concurrent goroutines must never observe a duplicate ID from any
// generator. This held pre-fix (shared atomic counter) and must keep
// holding after the fix.
func TestConcurrentIDGeneratorsUnique(t *testing.T) {
	gens := map[string]func() string{
		"task": newTaskID, "req": newRequestID,
		"atm": newAttemptID, "dlv": newDeliveryID,
		"ob": newObligationID, "judg": newJudgmentID,
	}
	for kind, gen := range gens {
		seen := map[string]bool{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		for g := 0; g < 32; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					id := gen()
					if !strings.HasPrefix(id, kind+"-") {
						t.Errorf("generator %q lost prefix: %q", kind, id)
						return
					}
					mu.Lock()
					if seen[id] {
						t.Errorf("generator %q duplicated ID %q", kind, id)
					}
					seen[id] = true
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}
}

// TestCrossProcessIDGeneratorsUnique re-executes the test binary as
// 8 independent processes per generator kind and requires every
// minted ID to be globally unique. Pre-fix this fails: each fresh
// process restarts its counter at zero, so same-second processes
// mint byte-identical IDs.
func TestCrossProcessIDGeneratorsUnique(t *testing.T) {
	if os.Getenv("METIS_IDGEN_PROBE") != "" {
		t.Skip("probe child does not run the parent test")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	for _, kind := range []string{"task", "req", "atm", "dlv"} {
		seen := map[string]string{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		// Buffer the worst case: every ID of every proc colliding.
		errs := make(chan string, 8*25+8)
		for p := 0; p < 8; p++ {
			wg.Add(1)
			go func(proc int) {
				defer wg.Done()
				cmd := exec.Command(self)
				cmd.Env = append(os.Environ(), "METIS_IDGEN_PROBE="+kind)
				out, err := cmd.Output()
				if err != nil {
					errs <- fmt.Sprintf("kind %s proc %d: %v", kind, proc, err)
					return
				}
				for _, id := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					id = strings.TrimSpace(id)
					if id == "" {
						continue
					}
					mu.Lock()
					if prev, dup := seen[id]; dup {
						errs <- fmt.Sprintf("kind %s: ID %q minted by both %s and proc %d", kind, id, prev, proc)
					}
					seen[id] = fmt.Sprintf("proc %d", proc)
					mu.Unlock()
				}
			}(p)
		}
		wg.Wait()
		close(errs)
		var dupes []string
		for e := range errs {
			dupes = append(dupes, e)
		}
		if len(dupes) > 0 {
			t.Fatalf("kind %s: %d cross-process collisions (showing up to 5):\n%s",
				kind, len(dupes), strings.Join(dupes[:min(5, len(dupes))], "\n"))
		}
		if len(seen) != 8*25 {
			t.Fatalf("kind %s: expected %d IDs, collected %d", kind, 8*25, len(seen))
		}
	}
}

// min is the Go builtin (go >= 1.21); no local helper needed.

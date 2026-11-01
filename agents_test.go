package main

import (
	"strings"
	"testing"
)

func TestLookupAdapter(t *testing.T) {
	for _, h := range []string{"demo", "codex", "claude", "muse", "hermes"} {
		a, err := lookupAdapter(h)
		if err != nil {
			t.Fatalf("known harness %q: %v", h, err)
		}
		if a.Harness != h {
			t.Fatalf("identity mismatch: %+v", a)
		}
		if !a.OneShot {
			t.Fatalf("%q should be one-shot until interactive is qualified", h)
		}
		if a.Interactive || a.AcceptsLiveInput {
			t.Fatalf("%q must not promise unqualified interactivity", h)
		}
	}
	if _, err := lookupAdapter("nope"); err == nil {
		t.Fatal("unknown harness should fail with known list")
	} else if !strings.Contains(err.Error(), "demo") {
		t.Fatalf("error should name known harnesses: %v", err)
	}
}

func TestResolveWorkerCommand(t *testing.T) {
	// Explicit command always wins.
	got, err := resolveWorkerCommand("", "echo hi", "/w", "", "")
	if err != nil || got != "echo hi" {
		t.Fatalf("explicit cmd: %q %v", got, err)
	}
	// Harness requires a prompt.
	if _, err := resolveWorkerCommand("codex", "", "/w", "", ""); err == nil {
		t.Fatal("harness without prompt should fail")
	}
	// Unknown harness fails.
	if _, err := resolveWorkerCommand("nope", "", "/w", "do it", ""); err == nil {
		t.Fatal("unknown harness should fail")
	}
	// Adapter embeds the prompt; model flag only when supported.
	got, err = resolveWorkerCommand("codex", "", "/w", "audit auth", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "audit auth") || !strings.Contains(got, "codex") {
		t.Fatalf("codex command missing prompt/prog: %q", got)
	}
	got, err = resolveWorkerCommand("muse", "", "/w", "do it", "spark")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "do it") {
		t.Fatalf("muse command missing prompt: %q", got)
	}
}

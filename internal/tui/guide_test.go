package tui

import (
	"errors"
	"testing"
)

var errGuideTest = errors.New("boom")

// Pure guidance tests: editing, submit serialization, and outcome
// status need no tmux, registry, DB, or event loop. The shell
// adapter (main package) owns mode transitions and command
// construction; behavior pins live in phase4_test.go and
// tui_decompose_test.go through the real Update path.

func TestGuideTypeAppendsRunes(t *testing.T) {
	var g Guide
	g = g.Type([]rune("abé👍"))
	if g.Buf != "abé👍" {
		t.Fatalf("Type must append runes, got %q", g.Buf)
	}
}

func TestGuideBackspaceIsRuneAware(t *testing.T) {
	g := Guide{Buf: "abé👍"}
	g = g.Backspace()
	if g.Buf != "abé" {
		t.Fatalf("Backspace must drop one rune, got %q", g.Buf)
	}
	g = Guide{Buf: "é"}.Backspace()
	if g.Buf != "" {
		t.Fatalf("Backspace must clear a lone rune, got %q", g.Buf)
	}
	g = Guide{}.Backspace()
	if g.Buf != "" {
		t.Fatalf("Backspace on empty must stay empty, got %q", g.Buf)
	}
}

func TestGuideSubmitSendsTrimmedText(t *testing.T) {
	g := Guide{Buf: "  review the diff  "}
	next, intent := g.Submit()
	if intent.Kind != GuideSend || intent.Text != "review the diff" {
		t.Fatalf("Submit must send trimmed text, got %+v", intent)
	}
	if !next.Busy || next.Buf != "" {
		t.Fatalf("Send must clear the buffer and mark busy, got %+v", next)
	}
}

func TestGuideSubmitDropsBlank(t *testing.T) {
	g := Guide{Buf: "   "}
	next, intent := g.Submit()
	if intent.Kind != GuideDrop {
		t.Fatalf("blank buffer must drop, got %+v", intent)
	}
	if next.Busy || next.Buf != "" {
		t.Fatalf("Drop must clear without marking busy, got %+v", next)
	}
}

func TestGuideSubmitHoldsWhileBusy(t *testing.T) {
	g := Guide{Busy: true, Buf: "second"}
	next, intent := g.Submit()
	if intent.Kind != GuideHold || intent.Text != "second" {
		t.Fatalf("busy submit must hold with text kept, got %+v", intent)
	}
	if !next.Busy || next.Buf != "second" {
		t.Fatalf("Hold must change nothing, got %+v", next)
	}
}

func TestGuideCompleteClearsBusyAndNamesOrigin(t *testing.T) {
	g := Guide{Busy: true}
	next, status := g.Complete(GuideOutcome{Session: "a", GID: "g1"})
	if next.Busy {
		t.Fatalf("Complete must clear busy, got %+v", next)
	}
	if status != "guidance g1 sent (file) → a" {
		t.Fatalf("status must name origin session, got %q", status)
	}
	next, status = g.Complete(GuideOutcome{Session: "b", GID: "g2", Direct: true})
	if next.Busy {
		t.Fatalf("direct Complete must clear busy, got %+v", next)
	}
	if status != "guidance g2 sent (direct) → b" {
		t.Fatalf("direct status wrong, got %q", status)
	}
}

func TestGuideCompleteReportsError(t *testing.T) {
	next, status := Guide{Busy: true}.Complete(GuideOutcome{Session: "a", Err: errGuideTest})
	if next.Busy {
		t.Fatalf("error Complete must clear busy, got %+v", next)
	}
	if status != "boom" {
		t.Fatalf("error status must carry the message, got %q", status)
	}
}

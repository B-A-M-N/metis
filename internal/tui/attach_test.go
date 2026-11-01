package tui

import "testing"

// Pure fence tests: no tmux, registry, DB, or event loop. The shell
// adapter (main package) supplies mode/liveness and applies the
// status/mode effects; TestAttachStaleRefusalCannotClobberNewerAttach
// pins the integrated path.

func TestAttachRequestBumpsGeneration(t *testing.T) {
	var a Attach
	var gen int
	a, gen = a.Request("w")
	if gen != 1 || a.Gen != 1 || a.Session != "w" {
		t.Fatalf("request must start gen 1 at w, got %+v gen %d", a, gen)
	}
	a, gen = a.Request("w")
	if gen != 2 || a.Gen != 2 {
		t.Fatalf("second attempt must bump to gen 2, got %+v gen %d", a, gen)
	}
}

func TestAttachClassifyConfirmed(t *testing.T) {
	a, gen := Attach{}.Request("w")
	if got := a.Classify(ModeAttach, gen, "w", true, "w", true); got != AttachConfirmed {
		t.Fatalf("live matching success must confirm, got %v", got)
	}
}

func TestAttachClassifyIgnores(t *testing.T) {
	a, gen := Attach{}.Request("w")
	cases := map[string]struct {
		mode             Mode
		res, live        string
		attached, liveOK bool
		resGen           int
	}{
		"detached mode":      {mode: ModeNav, res: "w", live: "w", attached: true, liveOK: true, resGen: gen},
		"stale generation":   {mode: ModeAttach, res: "w", live: "w", attached: true, liveOK: true, resGen: gen - 1},
		"future generation":  {mode: ModeAttach, res: "w", live: "w", attached: false, liveOK: true, resGen: gen + 1},
		"other session":      {mode: ModeAttach, res: "x", live: "w", attached: false, liveOK: true, resGen: gen},
		"target not live":    {mode: ModeAttach, res: "w", live: "w", attached: true, liveOK: false, resGen: gen},
		"target switched":    {mode: ModeAttach, res: "w", live: "z", attached: true, liveOK: true, resGen: gen},
		"refused other sess": {mode: ModeAttach, res: "x", live: "w", attached: false, liveOK: true, resGen: gen},
	}
	for name, c := range cases {
		if got := a.Classify(c.mode, c.resGen, c.res, c.attached, c.live, c.liveOK); got != AttachIgnored {
			t.Fatalf("%s must be ignored, got %v", name, got)
		}
	}
}

func TestAttachClassifyRefused(t *testing.T) {
	a, gen := Attach{}.Request("w")
	if got := a.Classify(ModeAttach, gen, "w", false, "w", true); got != AttachRefused {
		t.Fatalf("live matching refusal must refuse, got %v", got)
	}
}

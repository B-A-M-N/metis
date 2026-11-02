package main

// muse_turn_intent_test.go: the turn commandId is persisted before
// submit and reused on retry. An ambiguous submit error keeps the
// task working with the intent submitted — never a refund — while
// a proven pre-transmission failure still refunds.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The recovery proof rests on a server property: resubmitting the
// identical commandId+payload admits the SAME turn instead of
// executing twice. Kill Metis after accept but before the ack, and
// the recovered intent names a command the server already knows.
func TestTurnResubmitDedups(t *testing.T) {
	if testing.Short() {
		t.Skip("integration gate (daemon/host turns); run without -short")
	}
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	h, err := startMuseHost(ctx, "muse", "echo", nil, "--join")
	if err != nil {
		t.Skipf("no ambient muse daemon: %v", err)
	}
	defer h.Close()
	sid, _, err := museStartSession(ctx, h, "metis-qual-dedup-pin", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := museResumeSession(ctx, h, sid, ""); err != nil {
		t.Fatal(err)
	}
	cmdID, err := newMuseCommandID()
	if err != nil {
		t.Fatal(err)
	}
	a1, err := museSubmitTurnWithID(ctx, h, sid, "dedup pin", "", cmdID)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := museSubmitTurnWithID(ctx, h, sid, "dedup pin", "", cmdID)
	if err != nil {
		t.Fatal(err)
	}
	if a1.TurnID == "" || a1.TurnID != a2.TurnID {
		t.Fatalf("identical resubmission must admit the same turn, got %q then %q", a1.TurnID, a2.TurnID)
	}
}

func seedBoundTask(t *testing.T, session, run string) Task {
	t.Helper()
	tk, err := createTask(run, session, "obj", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchTaskWithRequest(tk.ID, session, testReq("orch")); err != nil {
		t.Fatal(err)
	}
	got, _ := getTask(tk.ID)
	if err := markTaskWorking(tk.ID, got.Attempt, testReq("worker-loop/"+session)); err != nil {
		t.Fatal(err)
	}
	got, _ = getTask(tk.ID)
	return got
}

func TestTurnIntentReusedOnRetry(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	req := testReq("loop")
	first, err := ensureTurnIntent("t1", 0, "w", "n1", "", "echo", "/repo-a", "do x", req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureTurnIntent("t1", 0, "w", "n1", "", "echo", "/repo-a", "do x", req)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || first != second {
		t.Fatalf("retry must reuse the command, got %q then %q", first, second)
	}
	// P0-2: a changed payload on the same attempt is a conflict,
	// not a silent replacement identity.
	if _, err := ensureTurnIntent("t1", 0, "w", "n1", "", "echo", "/repo-a", "do y", req); err == nil {
		t.Fatal("changed payload must conflict, not mint a new identity")
	} else if !strings.Contains(err.Error(), "turn_intent_conflict") {
		t.Fatalf("conflict must be explicit, got: %v", err)
	}
	in, err := getTurnIntent("t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if in.CommandID != first || in.State != "submitted" {
		t.Fatalf("frozen intent row must be untouched: %+v", in)
	}
	if err := recordTurnAck(first, "turn-1"); err != nil {
		t.Fatal(err)
	}
	in, _ = getTurnIntent("t1", 0)
	if in.State != "acknowledged" || in.TurnID != "turn-1" {
		t.Fatalf("ack not recorded: %+v", in)
	}
}

func nativeFakeAdapter(turnErr error, term string) Adapter {
	return Adapter{
		Harness: "muse-native",
		ManagedTurn: func(ctx context.Context, session, prompt string, timeout time.Duration, commandID string) (museTurnOutcome, error) {
			if turnErr != nil {
				return museTurnOutcome{}, turnErr
			}
			if commandID == "" {
				return museTurnOutcome{}, errors.New("fake adapter requires a command identity")
			}
			if err := recordTurnAck(commandID, "turn-fake"); err != nil {
				return museTurnOutcome{}, err
			}
			return museTurnOutcome{TurnID: "turn-fake", Terminal: term, Text: "ok"}, nil
		},
	}
}

func TestUncertainSubmitKeepsWorking(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "niu")
	seedOwnedSession(t, "w", "niu")
	if err := bindMuseSession("w", "n1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatal(err)
	}
	tk := seedBoundTask(t, "w", "run-niu")
	a := nativeFakeAdapter(errors.New(`turn/start cmd-1: write: broken pipe (submission uncertain; intent cmd-1 retained, no refund)`), "")
	ok, err := runNativeTurnOnce("w", a, tk, "do x", t.TempDir(), testReq("loop"))
	if err == nil || ok {
		t.Fatalf("ambiguous submit must fail without success: %v %v", ok, err)
	}
	if got, _ := getTask(tk.ID); got.Status != TaskWorking {
		t.Fatalf("uncertain submit must keep working, got %s", got.Status)
	}
	in, err := getTurnIntent(tk.ID, tk.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if in.State != "submitted" {
		t.Fatalf("intent must stay submitted for reconcile, got %s", in.State)
	}
	open, err := listAttention("run-niu", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "uncertain_outcome" || open[0].TaskID != tk.ID {
		t.Fatalf("uncertain submit must raise one attention, got %+v", open)
	}
}

func TestPreSubmitStillRefunds(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "nipr")
	seedOwnedSession(t, "w", "nipr")
	if err := bindMuseSession("w", "n1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatal(err)
	}
	tk := seedBoundTask(t, "w", "run-nipr")
	a := nativeFakeAdapter(musePreSubmit(errors.New("resume: no daemon")), "")
	if _, err := runNativeTurnOnce("w", a, tk, "do x", t.TempDir(), testReq("loop")); err == nil {
		t.Fatal("pre-submit failure must error")
	}
	if got, _ := getTask(tk.ID); got.Status != TaskDispatched {
		t.Fatalf("proven pre-submit must refund to dispatched, got %s", got.Status)
	}
}

// P0-2: once an attempt owns a submitted command identity, the
// payload and execution scope are frozen. Submit, lose the ack,
// change the prompt, retry: the retry must be refused as a
// conflict and must not submit a second turn. The original
// prompt must still reconcile through the SAME command id.
func TestChangedPayloadAfterUncertainSubmitRefused(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	installFakeTmux(t, "nip")
	seedOwnedSession(t, "w", "nip")
	if err := bindMuseSession("w", "n1", "c0", "echo", "", testReq("op")); err != nil {
		t.Fatal(err)
	}
	tk := seedBoundTask(t, "w", "run-nip")
	submits := 0
	var seen []string
	a := Adapter{
		Harness: "muse-native",
		ManagedTurn: func(ctx context.Context, session, prompt string, timeout time.Duration, commandID string) (museTurnOutcome, error) {
			submits++
			seen = append(seen, commandID)
			if submits == 1 {
				return museTurnOutcome{}, errors.New("turn/start: broken pipe (submission uncertain)")
			}
			if err := recordTurnAck(commandID, "turn-fake"); err != nil {
				return museTurnOutcome{}, err
			}
			return museTurnOutcome{TurnID: "turn-fake", Terminal: "completed", Text: "ok"}, nil
		},
	}
	root := t.TempDir()
	if _, err := runNativeTurnOnce("w", a, tk, "do x", root, testReq("loop")); err == nil {
		t.Fatal("ambiguous first submit must fail")
	}
	if submits != 1 {
		t.Fatalf("first pass must submit once, got %d", submits)
	}
	in, err := getTurnIntent(tk.ID, tk.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	firstID := in.CommandID
	if firstID == "" || in.State != "submitted" {
		t.Fatalf("intent must stay submitted, got %+v", in)
	}
	// Same attempt, changed prompt, ack still lost: conflict, and
	// crucially no second turn may be submitted.
	if _, err := runNativeTurnOnce("w", a, tk, "do Y-CHANGED", root, testReq("loop")); err == nil {
		t.Fatal("changed payload on an uncertain attempt must be refused")
	} else if !strings.Contains(err.Error(), "turn_intent_conflict") {
		t.Fatalf("refusal must name the intent conflict, got: %v", err)
	}
	if submits != 1 {
		t.Fatalf("conflicted retry must not submit a second turn, submits=%d", submits)
	}
	in, err = getTurnIntent(tk.ID, tk.Attempt)
	if err != nil {
		t.Fatal(err)
	}
	if in.CommandID != firstID || in.PromptDigest != turnPromptDigest("do x") {
		t.Fatalf("frozen identity must be untouched, got %+v", in)
	}
	// The frozen identity survives: the same attempt+payload still
	// resolves to the original command, so a later reconciliation
	// retries the SAME turn instead of forking a duplicate.
	b, err := getMuseBinding("w")
	if err != nil {
		t.Fatal(err)
	}
	reused, err := ensureTurnIntent(tk.ID, tk.Attempt, "w", "n1", b.Profile, b.Provider, b.Workspace, "do x", testReq("loop"))
	if err != nil {
		t.Fatalf("frozen identity must be reusable: %v", err)
	}
	if reused != firstID {
		t.Fatalf("reconcile must reuse the frozen command, got %q want %q", reused, firstID)
	}
	// And the execution gate still refuses a duplicate launch while
	// the first outcome is unknown: no second turn is submitted.
	if _, err := runNativeTurnOnce("w", a, tk, "do x", root, testReq("loop")); err == nil {
		t.Fatal("duplicate launch while outcome unknown must be refused")
	} else if !strings.Contains(err.Error(), "already executed") {
		t.Fatalf("refusal must name the duplicate guard, got: %v", err)
	}
	if submits != 1 {
		t.Fatalf("no second turn may be submitted, submits=%d seen=%q", submits, seen)
	}
}

// P0-2: reuse validates the whole stored identity, not just the
// digest. Same payload on a different session, native session,
// profile, provider, or workspace is a conflict.
func TestTurnIntentRejectsIdentityMismatch(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	req := testReq("loop")
	if _, err := ensureTurnIntent("t9", 0, "w", "n1", "", "echo", "/repo-a", "do x", req); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string][6]string{
		"session":   {"w-other", "n1", "", "echo", "/repo-a", "do x"},
		"native":    {"w", "n2", "", "echo", "/repo-a", "do x"},
		"profile":   {"w", "n1", "fi", "echo", "/repo-a", "do x"},
		"provider":  {"w", "n1", "", "meta", "/repo-a", "do x"},
		"workspace": {"w", "n1", "", "echo", "/repo-b", "do x"},
	} {
		if _, err := ensureTurnIntent("t9", 0, v[0], v[1], v[2], v[3], v[4], v[5], req); err == nil {
			t.Fatalf("%s mismatch must conflict", name)
		} else if !strings.Contains(err.Error(), "turn_intent_conflict") {
			t.Fatalf("%s refusal must name the intent conflict, got: %v", name, err)
		}
	}
}

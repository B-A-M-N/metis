package main

// muse_message_service_test.go: P0 direct-Muse-message acceptance.
//
// A bound Muse-native session receives one structured message as a
// native MSP turn with no terminal attached. All tests drive the
// production submit/wait path against the scripted stub host and
// assert the shell stores stay empty throughout.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// setupMessageSession provisions a native-only logical session and
// binds it through the production claim path on a stub host. It
// returns the stub, the host, and the bound native session ID. No
// tmux row and no PTY supervisor ever exist behind the name.
func setupMessageSession(t *testing.T, name string) (*nativeStub, *museHost, string) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	return setupMessageSessionIn(t, name)
}

// setupMessageSessionIn is setupMessageSession against the
// caller's METIS_STATE: the loopback E2E test provisions and binds
// in the same state dir it later exposes, so the child metis
// binary sees the binding.
func setupMessageSessionIn(t *testing.T, name string) (*nativeStub, *museHost, string) {
	t.Helper()
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rmsg", "session": name, "objective": "receive messages",
		"actor": "orch", "request_id": name + "-prov-1", "backend": "muse-native",
		"workdir": work, "provider": "echo",
	}))
	if err != nil {
		t.Fatalf("native provision: %v", err)
	}
	if res.IsError {
		t.Fatalf("native provision isError: %s", res.Content[0].Text)
	}
	st := newNativeStub()
	h := newNativeStubHost(t, st)
	wsRoot, err := canonicalRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	model, err := resolveSessionModel(name)
	if err != nil {
		t.Fatalf("model must resolve from the logical record: %v", err)
	}
	req := OperationRequest{Actor: "orch", RequestID: name + "-bind-1", Principal: "tester"}
	cmdID, err := claimBindIntent(req, name, "echo", "", "metis:"+name, wsRoot, model)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	sid, _, err := bindMuseNativeSessionOnHost(ctx, h, name, "echo", "", "metis:"+name, wsRoot, model, cmdID, req)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if sid == "" {
		t.Fatal("bind must return a native session id")
	}
	return st, h, sid
}

func messageAuth(reqID string) OperationRequest {
	return OperationRequest{Actor: "orch", RequestID: reqID, Principal: "tester"}
}

// TestMuseMessageWithoutTerminal sends one message with no tmux row
// and no PTY supervisor behind the name and observes the terminal.
func TestMuseMessageWithoutTerminal(t *testing.T) {
	st, h, sid := setupMessageSession(t, "mm1")
	_ = st
	assertNoShellBacking(t, "message setup")
	b, err := getMuseBinding("mm1")
	if err != nil {
		t.Fatal(err)
	}
	if b.NativeID != sid {
		t.Fatalf("binding must pin the stub native session: %+v vs %q", b, sid)
	}
	msg := MuseMessageRequest{SessionID: "mm1", Text: "hello muse", RequestID: "mm1-m1", Actor: "orch"}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := sendMuseMessageOnHost(ctx, h, msg, b, mustClaimMessage(t, msg, b), "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" {
		t.Fatalf("message must complete, got %+v", got)
	}
	if got.TurnID == "" {
		t.Fatal("completed message must name its turn")
	}
	assertNoShellBacking(t, "message submit")
}

func mustClaimMessage(t *testing.T, msg MuseMessageRequest, b museBinding) string {
	t.Helper()
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatalf("claim message intent: %v", err)
	}
	return cmd
}

// TestMuseMessageExactSession proves the turn lands on the named
// session only: the sibling binding keeps its cursor and gains no
// message intent.
func TestMuseMessageExactSession(t *testing.T) {
	_, h, _ := setupMessageSession(t, "mma")
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := mcpCallTool("metis_provision", mustJSON(t, map[string]any{
		"run": "rmsg", "session": "mmb", "objective": "sibling",
		"actor": "orch", "request_id": "mmb-prov-1", "backend": "muse-native",
		"workdir": work, "provider": "echo",
	}))
	if err != nil || res.IsError {
		t.Fatalf("sibling provision: %v isError=%v", err, res.IsError)
	}
	wsRoot, err := canonicalRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	model, _ := resolveSessionModel("mmb")
	req := OperationRequest{Actor: "orch", RequestID: "mmb-bind-1", Principal: "tester"}
	cmdID, err := claimBindIntent(req, "mmb", "echo", "", "metis:mmb", wsRoot, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bindMuseNativeSessionOnHost(ctx, h, "mmb", "echo", "", "metis:mmb", wsRoot, model, cmdID, req); err != nil {
		t.Fatalf("sibling bind: %v", err)
	}
	before, err := getMuseBinding("mmb")
	if err != nil {
		t.Fatal(err)
	}
	a, err := getMuseBinding("mma")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mma", Text: "for A only", RequestID: "mma-exact-1", Actor: "orch"}
	got, err := sendMuseMessageOnHost(ctx, h, msg, a, mustClaimMessage(t, msg, a), "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" {
		t.Fatalf("message must complete, got %+v", got)
	}
	after, err := getMuseBinding("mmb")
	if err != nil {
		t.Fatal(err)
	}
	if after.Cursor != before.Cursor {
		t.Fatalf("sibling cursor must not move: %q -> %q", before.Cursor, after.Cursor)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM muse_message_intents WHERE session = 'mmb'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("sibling must gain no message intent, got %d rows", n)
	}
}

// TestDuplicateMessageSubmitsOnce replays the same request ID and
// proves the server sees one execution: the second call reconciles
// the stored command without a second turn/start.
func TestDuplicateMessageSubmitsOnce(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmd")
	b, err := getMuseBinding("mmd")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg := MuseMessageRequest{SessionID: "mmd", Text: "once only", RequestID: "mmd-dup-1", Actor: "orch"}
	cmd1, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd1, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "completed" || first.TurnID == "" {
		t.Fatalf("first submit must complete, got %+v", first)
	}
	submitsAfterFirst := st.submits.Load()
	cmd2, priorTurn, replay, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	if !replay || cmd2 != cmd1 || priorTurn != first.TurnID {
		t.Fatalf("retry must recover the stored command and turn (replay=%v cmd %q vs %q turn %q vs %q)",
			replay, cmd2, cmd1, priorTurn, first.TurnID)
	}
	// The completed request replays its durable terminal without
	// touching the host: no resubmit, no new wait, no terminal
	// notification needed.
	second, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd2, priorTurn, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "completed" {
		t.Fatalf("reconciled duplicate must report its terminal, got %+v", second)
	}
	if !strings.Contains(second.Detail, "durable terminal") {
		t.Fatalf("replay must come from the durable row, got %q", second.Detail)
	}
	if second.TurnID != first.TurnID {
		t.Fatalf("duplicate must reconcile the same turn: %q vs %q", second.TurnID, first.TurnID)
	}
	if got := st.submits.Load(); got != submitsAfterFirst {
		t.Fatalf("duplicate must submit no second turn: submits %d -> %d", submitsAfterFirst, got)
	}
	// A reused key with different text is a conflict, never a second
	// execution.
	changed := MuseMessageRequest{SessionID: "mmd", Text: "different text", RequestID: "mmd-dup-1", Actor: "orch"}
	if _, _, _, err := claimMuseMessageIntent(changed, b, messageAuth(changed.RequestID).Principal); err == nil {
		t.Fatal("changed-payload replay must conflict")
	}
}

// TestConcurrentIdenticalClaimsSubmitOnce proves N concurrent
// identical submissions converge on one command and one domain
// turn: exactly one claim wins, every loser replays the winner,
// and the single submit admits exactly one turn.
func TestConcurrentIdenticalClaimsSubmitOnce(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmc")
	b, err := getMuseBinding("mmc")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmc", Text: "race me", RequestID: "mmc-race-1", Actor: "orch"}
	const racers = 16
	type outcome struct {
		cmd    string
		replay bool
	}
	won := make(chan outcome, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd, _, replay, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
			if err != nil {
				t.Errorf("concurrent claim: %v", err)
				return
			}
			won <- outcome{cmd, replay}
		}()
	}
	wg.Wait()
	close(won)
	var cmd string
	winners := 0
	n := 0
	for o := range won {
		n++
		if cmd == "" {
			cmd = o.cmd
		} else if o.cmd != cmd {
			t.Fatalf("concurrent claims diverged: %q vs %q", o.cmd, cmd)
		}
		if !o.replay {
			winners++
		}
	}
	if n != racers {
		t.Fatalf("lost %d racers", racers-n)
	}
	if winners != 1 {
		t.Fatalf("exactly one claim must win, got %d", winners)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := st.submits.Load()
	got, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" {
		t.Fatalf("converged submit must complete, got %+v", got)
	}
	if n := st.submits.Load() - before; n != 1 {
		t.Fatalf("converged submit admitted %d turns, want 1", n)
	}
}

// TestStoppedSessionRefused proves a stopped session refuses before
// any submit: no turn ID, refused state naming the stop.
func TestStoppedSessionRefused(t *testing.T) {
	_, _, _ = setupMessageSession(t, "mms")
	if err := stopLogicalSession("mms"); err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mms", Text: "wake up", RequestID: "mms-s1", Actor: "orch"}
	got, err := sendMuseMessage(msg, messageAuth("mms-s1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageRefused {
		t.Fatalf("stopped session must refuse, got %+v", got)
	}
	if !strings.Contains(strings.ToLower(got.Detail), "stopped") {
		t.Fatalf("refusal must name the stop, got %q", got.Detail)
	}
	if got.TurnID != "" {
		t.Fatalf("refused message must submit nothing, got turn %q", got.TurnID)
	}
}

// TestDisconnectAfterSubmissionReconciles proves crash recovery: the
// first attempt admits the turn but loses the terminal (unknown,
// turn retained); the retry with the same request ID reconciles the
// same turn to its observed terminal without a second submit.
func TestDisconnectAfterSubmissionReconciles(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmr")
	st.autoComplete.Store(false)
	b, err := getMuseBinding("mmr")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmr", Text: "reconcile me", RequestID: "mmr-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	short, shortCancel := context.WithTimeout(ctx, 0)
	defer shortCancel()
	_ = short
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != museMessageUnknown {
		t.Fatalf("lost terminal must report unknown, got %+v", first)
	}
	if first.TurnID == "" {
		t.Fatal("unknown attempt must retain its turn for reconciliation")
	}
	// Reconnect: the same turn completes on the live stream. Push
	// the terminal event directly and reconcile by waiting for the
	// exact known turn — no second submit.
	st.autoComplete.Store(true)
	go func() {
		time.Sleep(100 * time.Millisecond)
		raw, _ := json.Marshal(map[string]any{
			"sessionId": b.NativeID, "turnId": first.TurnID,
			"terminal": "completed", "viewCursor": "c-reconciled",
		})
		h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	}()
	second, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, first.TurnID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.TurnID != first.TurnID {
		t.Fatalf("reconcile must wait the original turn: %q vs %q", second.TurnID, first.TurnID)
	}
	if second.State != "completed" {
		t.Fatalf("reconciled turn must report its terminal, got %+v", second)
	}
}

// TestConcurrentMessageClaimsSingleCommand fires simultaneous
// identical claims and proves they converge on one command: every
// claim succeeds, all name the same command, exactly one is the
// first writer, and a single submit executes one turn.
func TestConcurrentMessageClaimsSingleCommand(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmc")
	b, err := getMuseBinding("mmc")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmc", Text: "race me", RequestID: "mmc-r1", Actor: "orch"}
	const racers = 16
	type claim struct {
		cmd    string
		turn   string
		replay bool
		err    error
	}
	out := make([]claim, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd, turn, replay, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
			out[i] = claim{cmd, turn, replay, err}
		}(i)
	}
	wg.Wait()
	first := 0
	for i, c := range out {
		if c.err != nil {
			t.Fatalf("racer %d: identical claim must converge, got %v", i, c.err)
		}
		if c.cmd != out[0].cmd {
			t.Fatalf("racer %d: command %q diverges from winner %q", i, c.cmd, out[0].cmd)
		}
		if !c.replay {
			first++
		}
	}
	if first != 1 {
		t.Fatalf("exactly one racer must be the first writer, got %d", first)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := sendMuseMessageOnHost(ctx, h, msg, b, out[0].cmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "completed" || got.TurnID == "" {
		t.Fatalf("single submit must complete, got %+v", got)
	}
	if n := st.submits.Load(); n != 1 {
		t.Fatalf("concurrent claims must execute exactly one turn, got %d submits", n)
	}
}

// TestCompletedReplayNeedsNoHost proves a completed request
// replays its durable terminal without touching the host: the
// replay succeeds even after the connection is closed.
func TestCompletedReplayNeedsNoHost(t *testing.T) {
	_, h, _ := setupMessageSession(t, "mmn")
	b, err := getMuseBinding("mmn")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmn", Text: "no host needed", RequestID: "mmn-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "completed" {
		t.Fatalf("submit must complete, got %+v", first)
	}
	h.Close()
	second, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, first.TurnID, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "completed" || second.TurnID != first.TurnID {
		t.Fatalf("closed-host replay must return the durable terminal, got %+v", second)
	}
	if !strings.Contains(second.Detail, "durable terminal") {
		t.Fatalf("replay must come from the durable row, got %q", second.Detail)
	}
}

// TestStatusReconcilesUnknownTurn proves the status path recovers
// an unresolved submission: an admitted-but-unobserved turn starts
// unknown, then reconciles to its observed terminal without a
// second submit, and the row records the terminal durably.
func TestStatusReconcilesUnknownTurn(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmsr")
	st.autoComplete.Store(false)
	b, err := getMuseBinding("mmsr")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmsr", Text: "status me", RequestID: "mmsr-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != museMessageUnknown || first.TurnID == "" {
		t.Fatalf("unobserved submit must stay unknown with its turn, got %+v", first)
	}
	if terminal, _, _, terr := musePersistedTerminal(cmd); terr != nil || terminal != "" {
		t.Fatalf("unresolved request must persist no terminal, got %q", terminal)
	}
	st.autoComplete.Store(true)
	raw, _ := json.Marshal(map[string]any{
		"sessionId": b.NativeID, "turnId": first.TurnID,
		"terminal": "completed", "viewCursor": "c-status",
	})
	h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	out, err := loadMuseMessageOutcome("mmsr-r1")
	if err != nil {
		t.Fatal(err)
	}
	reconciled := reconcileMuseMessageTurn(ctx, h, b, out, 10*time.Second)
	if reconciled.State != "completed" || reconciled.TurnID != first.TurnID {
		t.Fatalf("status must reconcile the admitted turn, got %+v", reconciled)
	}
	if terminal, _, _, terr := musePersistedTerminal(cmd); terr != nil || terminal != "completed" {
		t.Fatalf("reconcile must persist the terminal, got %q", terminal)
	}
	if n := st.submits.Load(); n != 1 {
		t.Fatalf("reconcile must submit no second turn, got %d", n)
	}
}

// TestMessageStatusWaitClamp pins the poll budget: the default
// keeps a reconcile responsive and the cap keeps every poll under
// the MCP exchange budget.
func TestMessageStatusWaitClamp(t *testing.T) {
	if got := clampMessageStatusWait(0); got != messageStatusDefaultWait {
		t.Fatalf("zero wait must default, got %v", got)
	}
	if got := clampMessageStatusWait(-time.Second); got != messageStatusDefaultWait {
		t.Fatalf("negative wait must default, got %v", got)
	}
	if got := clampMessageStatusWait(2 * time.Second); got != 2*time.Second {
		t.Fatalf("short wait must pass through, got %v", got)
	}
	if got := clampMessageStatusWait(60 * time.Second); got != museMessageStatusMaxWait {
		t.Fatalf("long wait must cap at %v, got %v", museMessageStatusMaxWait, got)
	}
	if museMessageStatusMaxWait >= 30*time.Second {
		t.Fatalf("status cap %v must stay under the 30s MCP exchange budget", museMessageStatusMaxWait)
	}
}

// messageActivityLines reads the session workdir's events tail,
// the same file the TUI activity view renders.
func messageActivityLines(t *testing.T, workdir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(workdir, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var lines []string
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.Contains(ln, `"muse_message"`) {
			lines = append(lines, ln)
		}
	}
	return lines
}

// TestMessageTraceLandsInSessionActivity proves the submit path
// surfaces the admission receipt (request ID + turn ID) in the
// session's own activity tail, which the refresh path already
// renders with no new polling.
func TestMessageTraceLandsInSessionActivity(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmt")
	_ = st
	b, err := getMuseBinding("mmt")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmt", Text: "trace me", RequestID: "mmt-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageAccepted || got.TurnID == "" {
		t.Fatalf("submit must admit promptly, got %+v", got)
	}
	lines := messageActivityLines(t, b.Workspace)
	if len(lines) != 1 {
		t.Fatalf("admission must trace exactly one activity line, got %d", len(lines))
	}
	if !strings.Contains(lines[0], "mmt-r1") || !strings.Contains(lines[0], got.TurnID) {
		t.Fatalf("activity line must name request and turn, got: %s", lines[0])
	}
}

// TestStatusPollDoesNotSpamActivity proves the status path is
// observation-only: repeated unknown polls add no activity lines,
// while the terminal transition traces exactly once.
func TestStatusPollDoesNotSpamActivity(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmsp")
	st.autoComplete.Store(false)
	b, err := getMuseBinding("mmsp")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmsp", Text: "quiet polls", RequestID: "mmsp-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, messageAuth(msg.RequestID).Principal)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != museMessageAccepted {
		t.Fatalf("submit must admit, got %+v", first)
	}
	out, err := loadMuseMessageOutcome("mmsp-r1")
	if err != nil {
		t.Fatal(err)
	}
	// Two unknown polls: no new activity.
	for i := 0; i < 2; i++ {
		got := reconcileMuseMessageTurn(ctx, h, b, out, 300*time.Millisecond)
		if got.State != museMessageUnknown {
			t.Fatalf("unobserved turn must stay unknown, got %+v", got)
		}
	}
	if lines := messageActivityLines(t, b.Workspace); len(lines) != 1 {
		t.Fatalf("status polls must not spam the activity tail, got %d lines", len(lines))
	}
	// The terminal transition traces exactly once, then replays
	// stay silent.
	raw, _ := json.Marshal(map[string]any{
		"sessionId": b.NativeID, "turnId": first.TurnID,
		"terminal": "completed", "viewCursor": "c-spam",
	})
	h.pushNotif(museNotification{Method: "turn/completed", Params: raw})
	got := reconcileMuseMessageTurn(ctx, h, b, out, 10*time.Second)
	if got.State != "completed" {
		t.Fatalf("reconcile must complete, got %+v", got)
	}
	lines := messageActivityLines(t, b.Workspace)
	if len(lines) != 2 {
		t.Fatalf("terminal must trace exactly once more, got %d lines", len(lines))
	}
	if !strings.Contains(lines[1], "terminal=completed") || !strings.Contains(lines[1], first.TurnID) {
		t.Fatalf("terminal line must name turn and disposition, got: %s", lines[1])
	}
}

// TestNativeMessageSummaryStates pins the activity-view summary
// for direct messages: none recorded on a fresh store,
// admitted-but-unresolved with the recovery pointer, and the
// durable terminal once observed.
func TestNativeMessageSummaryStates(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	if got := nativeMessageSummary("ghost"); got != "none recorded" {
		t.Fatalf("fresh store must read as none recorded, got %q", got)
	}
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO muse_message_intents
		(request_id, session, native_id, profile, provider, workspace, prompt_digest, command_id, turn_id, terminal, detail, created, updated)
		VALUES ('sum-r1', 'ns1', 'nl:1', 'p', 'echo', '/w', 'd', 'cmd-1', 'turn-9', '', '', ?, ?)`,
		nowUTC(), nowUTC()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got := nativeMessageSummary("ns1")
	if !strings.Contains(got, "sum-r1") || !strings.Contains(got, "turn-9") || !strings.Contains(got, "unresolved") {
		t.Fatalf("unresolved message must name request, turn, and recovery, got %q", got)
	}
	if !strings.Contains(got, "metis_muse_message_status") {
		t.Fatalf("unresolved summary must point at the status op, got %q", got)
	}
	db, err = openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE muse_message_intents SET terminal = 'completed' WHERE request_id = 'sum-r1'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	got = nativeMessageSummary("ns1")
	if !strings.Contains(got, "terminal=completed") || !strings.Contains(got, "turn-9") {
		t.Fatalf("resolved message must name turn and terminal, got %q", got)
	}
}

// TestNativeMessageSummaryStorageFailure pins the P1-D
// boundary: a store that exists but cannot answer (message table
// dropped out from under a live database) reads as unavailable,
// never as an invented none-recorded/completed state.
func TestNativeMessageSummaryStorageFailure(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureMuseMessageTable(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE muse_message_intents`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if got := nativeMessageSummary("ns1"); got != "unavailable" {
		t.Fatalf("unreadable message store must read unavailable, got %q", got)
	}
}

// statusAuth builds a status-poll transport identity for an
// arbitrary principal.
func statusAuth(principal string) OperationRequest {
	return OperationRequest{Actor: "op-" + principal, RequestID: "poll-" + principal, Principal: principal}
}

// TestStatusPrincipalIsolation proves the P0-B boundary: once the
// session leaves scope, a stranger learns nothing (not even the
// cached terminal), while the initiating principal still reads
// its own completed result without contacting Muse.
func TestStatusPrincipalIsolation(t *testing.T) {
	_, h, _ := setupMessageSession(t, "mmp")
	b, err := getMuseBinding("mmp")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmp", Text: "owned history", RequestID: "mmp-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(msg, b, "owner-p")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "completed" {
		t.Fatalf("fixture submit must complete, got %+v", first)
	}
	// The session leaves scope: the allowlist no longer names it.
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "some-other-session")
	stranger, err := queryMuseMessageStatus("mmp-r1", statusAuth("stranger"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if stranger.State != museMessageRefused {
		t.Fatalf("stranger must be refused, got %+v", stranger)
	}
	if strings.Contains(stranger.Detail, "completed") {
		t.Fatalf("refusal must not leak the cached terminal, got %q", stranger.Detail)
	}
	owner, err := queryMuseMessageStatus("mmp-r1", statusAuth("owner-p"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if owner.State != "completed" || owner.TurnID != first.TurnID {
		t.Fatalf("initiating principal must read its own completed result, got %+v", owner)
	}
	if !strings.Contains(owner.Detail, "historical read") {
		t.Fatalf("lapsed-scope read must say so, got %q", owner.Detail)
	}
}

// TestMessageLifecycleReachesTimeline proves controller visibility:
// a refused submit records message.sent/refused and an observed
// terminal polled through the service entry records
// message.delivered — both under the server-established principal,
// never the asserted actor label.
func TestMessageLifecycleReachesTimeline(t *testing.T) {
	// One shared state dir: each setupMessageSession would reset
	// METIS_STATE and strand the other session's feed rows.
	t.Setenv("METIS_STATE", t.TempDir())
	_, _, _ = setupMessageSessionIn(t, "mmx")
	if err := stopLogicalSession("mmx"); err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmx", Text: "wake up", RequestID: "mmx-s1", Actor: "orch"}
	if got, err := sendMuseMessage(msg, messageAuth("mmx-s1")); err != nil || got.State != museMessageRefused {
		t.Fatalf("fixture refusal: %+v %v", got, err)
	}
	_, h, _ := setupMessageSessionIn(t, "mmt")
	b, err := getMuseBinding("mmt")
	if err != nil {
		t.Fatal(err)
	}
	m2 := MuseMessageRequest{SessionID: "mmt", Text: "owned history", RequestID: "mmt-r1", Actor: "orch"}
	cmd, _, _, err := claimMuseMessageIntent(m2, b, "owner-p")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if first, err := sendMuseMessageOnHost(ctx, h, m2, b, cmd, "", 10*time.Second); err != nil || first.State != "completed" {
		t.Fatalf("fixture submit: %+v %v", first, err)
	}
	owner, err := queryMuseMessageStatus("mmt-r1", statusAuth("owner-p"), 0)
	if err != nil || owner.State != "completed" {
		t.Fatalf("fixture status: %+v %v", owner, err)
	}
	feed, ferr := listLatestOrchEvents(500)
	if ferr != nil {
		t.Fatalf("feed read: %v", ferr)
	}
	var sent, delivered string
	for _, e := range feed {
		switch {
		case e.EventType == OrchMessageSent && e.RequestID == "mmx-s1":
			sent = e.OriginPrincipal + "|" + e.TargetAgent + "|" + e.Disposition
		case e.EventType == OrchMessageDelivered && e.RequestID == "mmt-r1":
			delivered = e.OriginPrincipal + "|" + e.TargetAgent + "|" + e.Disposition
		}
	}
	if sent != "tester|mmx|refused" {
		t.Fatalf("refused submit timeline entry wrong: %q", sent)
	}
	if delivered != "owner-p|mmt|completed" {
		t.Fatalf("terminal timeline entry wrong: %q", delivered)
	}
}

// TestInboxNamespaceReserved proves ordinary claimants cannot
// mint request IDs in the M2M dispatch namespace.
func TestInboxNamespaceReserved(t *testing.T) {
	_, _, _ = setupMessageSession(t, "mmns")
	b, err := getMuseBinding("mmns")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmns", Text: "spoof", RequestID: "inbox:evil", Actor: "orch"}
	if _, _, _, err := claimMuseMessageIntent(msg, b, "tester"); err == nil {
		t.Fatal("inbox:-prefixed request IDs must be refused on the ordinary claim path")
	} else if !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("refusal must name the reservation, got: %v", err)
	}
}

// TestTerminalLookupFailureIsUnknownNotSubmit is the P1-A core:
// a storage fault during the replay check must yield unknown
// (retry to reconcile), never a second submission of a possibly
// completed turn.
func TestTerminalLookupFailureIsUnknownNotSubmit(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmf")
	b, err := getMuseBinding("mmf")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmf", Text: "maybe sent", RequestID: "mmf-r1", Actor: "orch"}
	cmd := mustClaimMessage(t, msg, b)
	// Break storage after the claim: the DB path is not openable.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_STATE", blocker)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	got, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != museMessageUnknown {
		t.Fatalf("storage fault must be unknown, got %+v", got)
	}
	if n := st.submits.Load(); n != 0 {
		t.Fatalf("storage fault must submit nothing, submitted %d", n)
	}
}

// TestLateNoteCannotClobberTerminal pins the recordMuseMessageNote
// guard: a note racing a landed terminal leaves the stored
// terminal and its detail exactly as observed.
func TestLateNoteCannotClobberTerminal(t *testing.T) {
	st, h, _ := setupMessageSession(t, "mmn")
	_ = st
	b, err := getMuseBinding("mmn")
	if err != nil {
		t.Fatal(err)
	}
	msg := MuseMessageRequest{SessionID: "mmn", Text: "noted", RequestID: "mmn-r1", Actor: "orch"}
	cmd := mustClaimMessage(t, msg, b)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sendMuseMessageOnHost(ctx, h, msg, b, cmd, "", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "completed" {
		t.Fatalf("fixture submit must complete, got %+v", first)
	}
	before, err := loadMuseMessageByCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordMuseMessageNote(cmd, "late racing note"); err != nil {
		t.Fatalf("note write must not fail, got: %v", err)
	}
	after, err := loadMuseMessageByCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if after.Terminal != before.Terminal || after.Detail != before.Detail {
		t.Fatalf("late note must not touch a settled row: before=%+v after=%+v", before, after)
	}
}

// TestMessageTargetAllowlist pins the operator allowlist: a set
// allowlist refuses non-listed sessions and admits listed ones
// through the shared gate.
func TestMessageTargetAllowlist(t *testing.T) {
	_, _, _ = setupMessageSession(t, "mma")
	auth := messageAuth("mma-r1")
	msg := MuseMessageRequest{SessionID: "mma", Text: "scoped", RequestID: "mma-r1", Actor: "orch"}
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "other-session")
	if _, refusal := authorizeMuseMessageTarget(msg, auth, true); !strings.Contains(refusal, "not an authorized muse message target") {
		t.Fatalf("non-listed session must refuse, got %q", refusal)
	}
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "other-session,mma")
	if _, refusal := authorizeMuseMessageTarget(msg, auth, true); refusal != "" {
		t.Fatalf("listed session must pass the gate, got %q", refusal)
	}
	t.Setenv("METIS_MUSE_MESSAGE_TARGETS", "")
	if _, refusal := authorizeMuseMessageTarget(msg, auth, true); refusal != "" {
		t.Fatalf("empty allowlist must admit in-scope targets, got %q", refusal)
	}
}

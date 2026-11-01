package main

// attention_test.go: failing-first proof for the durable
// attention lifecycle. Attention requests are Metis-level
// escalation (uncertain outcomes, conflicting findings, missing
// signoff) — distinct from Muse-native tool approvals, which
// belong to Muse and live in muse_approvals.go.

import (
	"testing"
)

func TestAttentionLifecycle(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	req := testReq("lead-01")

	// Open: returns a stable id.
	id, err := openAttention(req, "run-1", "task-9", "w",
		"uncertain_outcome", "turn ack lost; outcome unknown", `["ev-1"]`)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if id == "" {
		t.Fatal("open must return an id")
	}

	// Idempotent reopen with the same request id returns the
	// same row instead of duplicating it.
	id2, err := openAttention(req, "run-1", "task-9", "w",
		"uncertain_outcome", "turn ack lost; outcome unknown", `["ev-1"]`)
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	if id2 != id {
		t.Fatalf("idempotent reopen must return %s, got %s", id, id2)
	}

	// Same request id with a different payload is a conflict,
	// never a silent overwrite.
	if _, err := openAttention(req, "run-1", "task-9", "w",
		"signoff", "different reason", `[]`); err == nil {
		t.Fatal("reused request id with different payload must fail")
	}

	// Wrong revision guard: resolve must present the revision
	// the resolver actually saw.
	if err := resolveAttention(id, 999, "lead-01", "retried with fresh command id"); err == nil {
		t.Fatal("resolve with stale revision must fail")
	}

	// Correct resolve closes the request and bumps the revision.
	a, err := getAttention(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := resolveAttention(id, a.Revision, "lead-01", "retried with fresh command id"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Resolving a closed request fails: no silent double-close.
	if err := resolveAttention(id, a.Revision+1, "lead-01", "again"); err == nil {
		t.Fatal("double resolve must fail")
	}

	// Dismissing a resolved request fails too.
	if err := dismissAttention(id, a.Revision+1, "lead-01", "moot"); err == nil {
		t.Fatal("dismiss of resolved request must fail")
	}

	// Open set no longer lists it; history still does.
	open, err := listAttention("run-1", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range open {
		if r.ID == id {
			t.Fatal("resolved request must leave the open set")
		}
	}
	all, err := listAttention("run-1", false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range all {
		if r.ID == id && r.Status == "resolved" {
			found = true
		}
	}
	if !found {
		t.Fatal("resolved request must remain in history")
	}
}

// A requested native approval files one bounded missing_approval
// escalation; re-observed folds reuse it; resolution closes it.
// Approvals are must-answer tool requests — without this producer
// they sit silently in the approval rows.
func TestApprovalAttentionLifecycle(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	fold := []byte(`{"approvalId":"ap-att","itemId":"it-1",
		"availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", fold); err != nil {
		t.Fatal(err)
	}
	open, err := listAttention("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Kind != "missing_approval" || open[0].Status != "open" {
		t.Fatalf("requested approval must file one open escalation, got %+v", open)
	}
	// Re-observed fold (updated choices) reuses the row, never a second.
	updated := []byte(`{"approvalId":"ap-att","itemId":"it-1",
		"availableChoices":[{"choiceId":"allow","label":"Allow"},{"choiceId":"deny","label":"Deny"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", updated); err != nil {
		t.Fatal(err)
	}
	open, err = listAttention("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("re-observed fold must reuse the escalation, got %+v", open)
	}
	// A second approval files its own escalation.
	other := []byte(`{"approvalId":"ap-other",
		"availableChoices":[{"choiceId":"allow","label":"Allow"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", other); err != nil {
		t.Fatal(err)
	}
	// Server resolution closes only its own escalation.
	if err := recordMuseApprovalResolved("w", "ap-att",
		[]byte(`{"approvalId":"ap-att","decision":"allow"}`)); err != nil {
		t.Fatal(err)
	}
	open, err = listAttention("", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].Reason != "approval ap-other for session w needs a decision" {
		t.Fatalf("resolution must close only its own escalation, got %+v", open)
	}
}

// The escalation binds to the dispatched task when the session has
// one, so the operator sees which work the approval blocks.
func TestApprovalAttentionBindsTask(t *testing.T) {
	t.Setenv("TUI_DASH_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureLifecycleTables(db); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	tk, err := createTask("run-a", "w", "objective", "accept")
	if err != nil {
		t.Fatal(err)
	}
	if err := transitionTask(tk.ID, []string{"pending"}, "dispatched", ""); err != nil {
		t.Fatal(err)
	}
	tasks, err := listTasks("run-a")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("seed task: %+v %v", tasks, err)
	}
	fold := []byte(`{"approvalId":"ap-t","availableChoices":[{"choiceId":"allow","label":"A"}]}`)
	if err := recordMuseApprovalRequested("w", "native-1", fold); err != nil {
		t.Fatal(err)
	}
	open, err := listAttention("run-a", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].TaskID != tasks[0].ID {
		t.Fatalf("escalation must bind the dispatched task, got %+v", open)
	}
}

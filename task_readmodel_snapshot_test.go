package main

// task_readmodel_snapshot_test.go: revision-pinning tests for the
// task-status read snapshot (task_readmodel.go). taskStatusDetail
// must return ONE consistent revision even when a retry/transition
// advances the task mid-read, and its MCP fields must stay
// byte-identical to the separate-ops projection it replaced.

import (
	"reflect"
	"testing"
)

// seedSnapshotRows plants one artifact and one verdict row at the
// task's current attempt via SQL. Tests open throwaway DBs directly;
// production verdict/artifact filing keeps its own gates.
func seedSnapshotRows(t *testing.T, taskID string, attempt int) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB: %v", err)
	}
	defer db.Close()
	if err := ensureArtifactsTable(db); err != nil {
		t.Fatalf("ensureArtifactsTable: %v", err)
	}
	if err := ensureVerdictsTable(db); err != nil {
		t.Fatalf("ensureVerdictsTable: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO artifacts (task_id, path, digest, attempt_no, created)
		VALUES (?, ?, ?, ?, ?)`, taskID, "/ev/seed.txt", "seed-digest", attempt, nowUTC()); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO task_verdicts
		(task_id, attempt_no, criterion_id, verdict, reviewer, evidence, reason, request_id, created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, attempt, "c1", VerdictPass, "reviewer-1", `["/ev/seed.txt"]`, "seed reason", "req-seed", nowUTC()); err != nil {
		t.Fatalf("seed verdict: %v", err)
	}
}

// TestSnapshotParityWithSeparateReads pins the MCP fields
// byte-identical: with no race in flight, the snapshot projection
// must equal the legacy separate-ops tuple field for field.
func TestSnapshotParityWithSeparateReads(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	task, err := createTask("run-snap", "agent-1", "objective", "accept!")
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	dep, err := createTask("run-snap", "agent-2", "dep objective", "accept!")
	if err != nil {
		t.Fatalf("createTask dep: %v", err)
	}
	if err := addTaskDep(task.ID, dep.ID, testReq("snap")); err != nil {
		t.Fatalf("addTaskDep: %v", err)
	}
	seedSnapshotRows(t, task.ID, 0)

	tk, waiting, verdicts, count, err := taskStatusDetail(task.ID)
	if err != nil {
		t.Fatalf("taskStatusDetail: %v", err)
	}
	wantTask, err := getTask(task.ID)
	if err != nil {
		t.Fatalf("getTask: %v", err)
	}
	if !reflect.DeepEqual(tk, wantTask) {
		t.Errorf("task mismatch:\n got %+v,\nwant %+v", tk, wantTask)
	}
	wantWaiting, err := unsatisfiedDeps(task.ID)
	if err != nil {
		t.Fatalf("unsatisfiedDeps: %v", err)
	}
	if !reflect.DeepEqual(waiting, wantWaiting) {
		t.Errorf("waiting = %q, want %q", waiting, wantWaiting)
	}
	wantVerdicts, err := verdictsForCurrentAttempt(wantTask)
	if err != nil {
		t.Fatalf("verdictsForCurrentAttempt: %v", err)
	}
	if !reflect.DeepEqual(verdicts, wantVerdicts) {
		t.Errorf("verdicts = %+v, want %+v", verdicts, wantVerdicts)
	}
	wantArts, err := currentAttemptArtifacts(task.ID, wantTask.Attempt)
	if err != nil {
		t.Fatalf("currentAttemptArtifacts: %v", err)
	}
	if count != len(wantArts) {
		t.Errorf("artifactCount = %d, want %d", count, len(wantArts))
	}
}

// TestSnapshotPinnedAcrossConcurrentTransition drives a real
// concurrent transition (status + attempt advance, new-revision
// verdict/artifact rows, dep satisfaction) during the projection
// phase and requires the returned bundle to match ONE revision —
// the pre-transition one.
func TestSnapshotPinnedAcrossConcurrentTransition(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	task, err := createTask("run-snap", "agent-1", "objective", "accept!")
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	dep, err := createTask("run-snap", "agent-2", "dep objective", "accept!")
	if err != nil {
		t.Fatalf("createTask dep: %v", err)
	}
	if err := addTaskDep(task.ID, dep.ID, testReq("snap")); err != nil {
		t.Fatalf("addTaskDep: %v", err)
	}
	seedSnapshotRows(t, task.ID, 0)

	wantWaiting := []string{dep.ID + " (pending)"}

	// Concurrent transition driver: runs on its own goroutine and
	// its own DB handle while taskStatusDetail sits in projection.
	release := make(chan struct{})
	done := make(chan struct{})
	taskStatusDetailProbe = func(Task) {
		close(release)
		<-done
	}
	defer func() { taskStatusDetailProbe = nil }()
	go func() {
		defer close(done)
		<-release
		db, err := openTasksDB()
		if err != nil {
			t.Errorf("driver openTasksDB: %v", err)
			return
		}
		defer db.Close()
		if _, err := db.Exec(`UPDATE tasks SET status = ?, attempt = ?, updated = ? WHERE id = ?`,
			TaskWorking, 1, nowUTC(), task.ID); err != nil {
			t.Errorf("driver advance task: %v", err)
			return
		}
		if _, err := db.Exec(`INSERT INTO artifacts (task_id, path, digest, attempt_no, created)
			VALUES (?, ?, ?, ?, ?)`, task.ID, "/ev/next.txt", "next-digest", 1, nowUTC()); err != nil {
			t.Errorf("driver insert artifact: %v", err)
			return
		}
		if _, err := db.Exec(`INSERT INTO task_verdicts
			(task_id, attempt_no, criterion_id, verdict, reviewer, evidence, reason, request_id, created)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			task.ID, 1, "c2", VerdictFail, "reviewer-2", `["/ev/next.txt"]`, "next reason", "req-next", nowUTC()); err != nil {
			t.Errorf("driver insert verdict: %v", err)
			return
		}
		if _, err := db.Exec(`UPDATE tasks SET status = ?, updated = ? WHERE id = ?`,
			TaskVerified, nowUTC(), dep.ID); err != nil {
			t.Errorf("driver verify dep: %v", err)
			return
		}
	}()

	tk, waiting, verdicts, count, err := taskStatusDetail(task.ID)
	if err != nil {
		t.Fatalf("taskStatusDetail: %v", err)
	}
	// The driver must have landed before the call returned (the
	// probe waited on it), so any mixed revision would show here.
	after, err := getTask(task.ID)
	if err != nil {
		t.Fatalf("getTask after: %v", err)
	}
	if after.Attempt != 1 || after.Status != TaskWorking {
		t.Fatalf("driver did not advance task: %+v", after)
	}
	if tk.Attempt != 0 || tk.Status != TaskPending {
		t.Errorf("task revision mixed: got attempt=%d status=%s, want attempt=0 status=pending",
			tk.Attempt, tk.Status)
	}
	if !reflect.DeepEqual(waiting, wantWaiting) {
		t.Errorf("waiting = %q, want pinned %q (dep verified only after the snapshot)", waiting, wantWaiting)
	}
	if count != 1 {
		t.Errorf("artifactCount = %d, want 1 (attempt-1 row must not leak in)", count)
	}
	if len(verdicts) != 1 {
		t.Fatalf("len(verdicts) = %d, want 1 pinned verdict", len(verdicts))
	}
	got, ok := verdicts["c1"]
	if !ok {
		t.Fatalf("verdicts keys = %v, want only [c1]", reflect.ValueOf(verdicts).MapKeys())
	}
	if got.Attempt != tk.Attempt {
		t.Errorf("verdict attempt=%d does not match task attempt=%d: mixed revisions", got.Attempt, tk.Attempt)
	}
	if _, leaked := verdicts["c2"]; leaked {
		t.Errorf("attempt-1 verdict c2 leaked into an attempt-0 bundle")
	}
}

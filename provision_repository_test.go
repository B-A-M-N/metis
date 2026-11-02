package main

// provision_repository_test.go: persistence split for the provision
// domain. The repository owns the intent rows and the
// generation-fenced writes; the service keeps admission, replay,
// launch, and convergence as one cohesive lifecycle on top.

import (
	"testing"
)

func openProvisionRepoDB(t *testing.T) func() {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	repo := NewProvisionRepository(db)
	if err := repo.Ensure(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Ensure is idempotent: a second call must not disturb rows.
	if err := repo.Ensure(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_ = repo
	return func() {}
}

func TestProvisionRepositoryInsertLoadRoundTrip(t *testing.T) {
	openProvisionRepoDB(t)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	if err := repo.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := mcpProvisionRecord{
		Session: "s1", Run: "r1", Objective: "do",
		Workdir: "/tmp/w1", Backend: "tmux",
		Harness: "", Command: "sleep 60", Model: "",
	}
	if _, err := repo.InsertReserved("repo1", want); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, gen, found := repo.Load("repo1")
	if !found {
		t.Fatal("load must find inserted intent")
	}
	if gen != 0 || got.Generation != 0 {
		t.Fatalf("fresh intent generation = %d, want 0", gen)
	}
	if got.State != provisionReserved {
		t.Fatalf("fresh intent state = %q, want reserved", got.State)
	}
	if !got.samePayload(want) {
		t.Fatalf("round-trip payload mismatch: %+v", got)
	}
	if _, _, found := repo.Load("missing-key"); found {
		t.Fatal("load of missing key must report false")
	}
}

func TestProvisionRepositoryDuplicateInsertLeavesWinner(t *testing.T) {
	openProvisionRepoDB(t)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	if err := repo.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := mcpProvisionRecord{Session: "s1", Run: "r1", Backend: "tmux", Command: "sleep 60"}
	if _, err := repo.InsertReserved("repodup", want); err != nil {
		t.Fatal(err)
	}
	rival := mcpProvisionRecord{Session: "s2", Run: "r1", Backend: "tmux", Command: "sleep 60"}
	if _, err := repo.InsertReserved("repodup", rival); err == nil {
		t.Fatal("duplicate request_id must fail")
	}
	got, _, found := repo.Load("repodup")
	if !found || got.Session != "s1" {
		t.Fatalf("winner must survive duplicate insert, got %+v", got)
	}
}

func TestProvisionRepositoryCompareAndSetFencing(t *testing.T) {
	openProvisionRepoDB(t)
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	if err := repo.Ensure(); err != nil {
		t.Fatal(err)
	}
	want := mcpProvisionRecord{Session: "s1", Run: "r1", Backend: "tmux", Command: "sleep 60"}
	if _, err := repo.InsertReserved("repofence", want); err != nil {
		t.Fatal(err)
	}
	// Illegal lifecycle edge never commits.
	if ok, err := repo.CompareAndSet("repofence", provisionReserved, 0, provisionFailed); err != nil || ok {
		t.Fatalf("illegal edge must refuse: ok=%v err=%v", ok, err)
	}
	// Legal claim bumps the generation.
	ok, err := repo.CompareAndSet("repofence", provisionReserved, 0, provisionStarting)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if _, gen, _ := repo.Load("repofence"); gen != 1 {
		t.Fatalf("claim must bump generation to 1, got %d", gen)
	}
	// Stale generation never commits.
	if ok, err := repo.CompareAndSet("repofence", provisionReserved, 0, provisionStarting); err != nil || ok {
		t.Fatalf("stale fence must refuse: ok=%v err=%v", ok, err)
	}
	if _, gen, _ := repo.Load("repofence"); gen != 1 {
		t.Fatalf("refused write must not bump generation, got %d", gen)
	}
	// Confirm, then running is terminal.
	if ok, err := repo.CompareAndSet("repofence", provisionStarting, 1, provisionRunning); err != nil || !ok {
		t.Fatalf("confirm: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.CompareAndSet("repofence", provisionRunning, 2, provisionStarting); err != nil || ok {
		t.Fatalf("running must be terminal: ok=%v err=%v", ok, err)
	}
}

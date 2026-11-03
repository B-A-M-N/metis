package main

// provision_owner_binding_test.go: pre-launch owner binding proofs.
//
// A successful provision is contingent on a durable, correct owner
// binding recorded BEFORE the worker launches, on the intent's own
// handle. These tests fail the binding, retry through it, and
// attack across principals — with a stub launcher that records
// whether a worker would have started.

import (
	"errors"
	"os"
	"strings"
	"testing"
)

type countingLauncher struct {
	inner *stubProvisionLauncher
	calls int
}

func (c *countingLauncher) LaunchProvisionSession(want mcpProvisionRecord) error {
	c.calls++
	return c.inner.LaunchProvisionSession(want)
}

// bindingFixture wires a service with a counting stub launcher.
// liveness controls the backend aliveness answers; bind fails the
// owner binding exactly failBind times before delegating to the
// real repository claim.
func bindingFixture(t *testing.T, live bool, failBind int) (*ProvisionService, *countingLauncher, string) {
	t.Helper()
	t.Setenv("METIS_STATE", t.TempDir())
	installFakeMuse(t)
	work := t.TempDir()
	mustRegisterRoot(t, work, true)
	inner := &stubProvisionLauncher{}
	cl := &countingLauncher{inner: inner}
	svc := &ProvisionService{
		OpenDB:   openTasksDB,
		Launcher: cl,
		Liveness: stubProvisionLiveness{live: live, verified: true},
		MkdirAll: os.MkdirAll,
	}
	if failBind > 0 {
		calls := 0
		svc.BindOwner = func(store OwnerStore, session, principal string) (bool, error) {
			calls++
			if calls <= failBind {
				return false, errors.New("simulated owner store outage")
			}
			return claimProvisionOwner(store, session, principal)
		}
	}
	return svc, cl, work
}

func bindingReq(principal, reqID, session, work string, remote bool) ProvisionRequest {
	return ProvisionRequest{
		Actor: "cloud", RequestID: reqID, Principal: principal, Remote: remote,
		Run: "r1", Session: session, Objective: "binding proof",
		Workdir: work, Backend: provisionBackendPTY,
		Harness: "muse-code", Provider: "echo", Model: "spark",
	}
}

func ownerOf(t *testing.T, session string) (string, bool) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner, found, err := lookupProvisionOwner(db, session)
	if err != nil {
		t.Fatal(err)
	}
	return owner, found
}

func requireConflict(t *testing.T, err error, what string) {
	t.Helper()
	var pe *ProvisionError
	if !errors.As(err, &pe) || !pe.Conflict {
		t.Fatalf("%s must conflict, got: %v", what, err)
	}
}

// Proof 1: a fresh remote provision persists the verified JWT principal.
func TestProvisionOwnerBindingFreshRemote(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	res, err := svc.Provision(bindingReq("jwt:op-1", "bind-1", "bs1", work, true))
	if err != nil {
		t.Fatalf("fresh remote provision: %v", err)
	}
	if res.Disposition != ProvisionDispositionRunning || res.SessionID != "bs1" {
		t.Fatalf("fresh provision must report running with the session id, got %+v", res)
	}
	if cl.calls != 1 {
		t.Fatalf("fresh provision must launch exactly once, got %d", cl.calls)
	}
	if owner, found := ownerOf(t, "bs1"); !found || owner != "jwt:op-1" {
		t.Fatalf("binding must persist the JWT principal, got %q found=%v", owner, found)
	}
}

// Proof 2: an owner persistence failure reports failure — never a
// false success — and starts no worker.
func TestProvisionOwnerBindingFailureFailsProvision(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 1)
	_, err := svc.Provision(bindingReq("jwt:op-1", "bind-2", "bs2", work, true))
	if err == nil || !strings.Contains(err.Error(), "owner binding failed") {
		t.Fatalf("binding failure must fail the provision loudly, got: %v", err)
	}
	if cl.calls != 0 {
		t.Fatalf("failed binding must start no worker, got %d launches", cl.calls)
	}
	db, derr := openTasksDB()
	if derr != nil {
		t.Fatal(derr)
	}
	defer db.Close()
	if _, _, found := NewProvisionRepository(db).Load("bind-2"); found {
		t.Fatal("failed binding must compensate the untouched reservation")
	}
	if _, found := ownerOf(t, "bs2"); found {
		t.Fatal("failed binding must leave no owner row")
	}
}

// Proof 3: retry after a partial failure converges safely — one
// worker, correct owner, no duplicate.
func TestProvisionOwnerBindingRetryConverges(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 1)
	req := bindingReq("jwt:op-1", "bind-3", "bs3", work, true)
	if _, err := svc.Provision(req); err == nil {
		t.Fatal("first attempt must fail on the injected outage")
	}
	res, err := svc.Provision(req)
	if err != nil {
		t.Fatalf("retry must converge: %v", err)
	}
	if res.Disposition != ProvisionDispositionRunning {
		t.Fatalf("retry must report running, got %+v", res)
	}
	if cl.calls != 1 {
		t.Fatalf("retry must launch exactly once total, got %d", cl.calls)
	}
	if owner, found := ownerOf(t, "bs3"); !found || owner != "jwt:op-1" {
		t.Fatalf("retry must bind the original principal, got %q found=%v", owner, found)
	}
}

// Proof 4: replaying the same request preserves its original owner
// and never rewrites the binding.
func TestProvisionOwnerBindingReplayPreservesOwner(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	req := bindingReq("jwt:op-1", "bind-4", "bs4", work, true)
	if _, err := svc.Provision(req); err != nil {
		t.Fatalf("admit: %v", err)
	}
	svc.BindOwner = func(store OwnerStore, session, principal string) (bool, error) {
		t.Error("replay must not touch the owner binding")
		return false, nil
	}
	res, err := svc.Provision(req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !res.Replay {
		t.Fatalf("identical retry must converge as replay, got %+v", res)
	}
	if cl.calls != 1 {
		t.Fatalf("replay must not relaunch, got %d launches", cl.calls)
	}
	if owner, found := ownerOf(t, "bs4"); !found || owner != "jwt:op-1" {
		t.Fatalf("replay must preserve the original owner, got %q found=%v", owner, found)
	}
}

// Proof 5: replaying another principal's key cannot claim the worker.
func TestProvisionOwnerBindingCrossPrincipalReplayRefused(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	if _, err := svc.Provision(bindingReq("jwt:op-1", "bind-5", "bs5", work, true)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	_, err := svc.Provision(bindingReq("jwt:op-2", "bind-5", "bs5", work, true))
	requireConflict(t, err, "cross-principal replay")
	if cl.calls != 1 {
		t.Fatalf("refused replay must not relaunch, got %d launches", cl.calls)
	}
	if owner, found := ownerOf(t, "bs5"); !found || owner != "jwt:op-1" {
		t.Fatalf("refused replay must not move ownership, got %q found=%v", owner, found)
	}
}

// Proof 6: a conflicting fresh admission cannot rebind an existing worker.
func TestProvisionOwnerBindingConflictingAdmissionRefused(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	if _, err := svc.Provision(bindingReq("jwt:op-1", "bind-6a", "bs6", work, true)); err != nil {
		t.Fatalf("admit: %v", err)
	}
	_, err := svc.Provision(bindingReq("jwt:op-2", "bind-6b", "bs6", work, true))
	requireConflict(t, err, "conflicting fresh admission")
	if cl.calls != 1 {
		t.Fatalf("conflicting admission must not launch, got %d launches", cl.calls)
	}
	if owner, found := ownerOf(t, "bs6"); !found || owner != "jwt:op-1" {
		t.Fatalf("conflicting admission must not rebind, got %q found=%v", owner, found)
	}
}

// A remote provision must not adopt a live-but-unowned session:
// only the machine operator may dispose of those, locally.
func TestProvisionOwnerBindingLiveUnownedRemoteRefused(t *testing.T) {
	svc, cl, work := bindingFixture(t, true, 0)
	_, err := svc.Provision(bindingReq("jwt:op-2", "bind-7", "bs7", work, true))
	requireConflict(t, err, "remote adopt of unowned live session")
	if cl.calls != 0 {
		t.Fatalf("refused adopt must not launch, got %d launches", cl.calls)
	}
}

// A crash (or rollback) after the reservation insert but before
// the owner write commits must leave nothing behind: no intent
// row, no owner row, no worker. The original principal retries
// cleanly; another principal cannot claim the same key.
func TestProvisionOwnerBindingCrashBetweenReserveAndCommit(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	svc.BindOwner = func(store OwnerStore, session, principal string) (bool, error) {
		return false, errors.New("simulated crash before commit")
	}
	req := bindingReq("jwt:op-1", "bind-9", "bs9", work, true)
	if _, err := svc.Provision(req); err == nil {
		t.Fatal("mid-unit failure must fail the provision")
	}
	if cl.calls != 0 {
		t.Fatalf("rolled-back unit must start no worker, got %d launches", cl.calls)
	}
	db, derr := openTasksDB()
	if derr != nil {
		t.Fatal(derr)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	if _, _, found := repo.Load("bind-9"); found {
		t.Fatal("rolled-back unit must persist no intent row")
	}
	if _, found := ownerOf(t, "bs9"); found {
		t.Fatal("rolled-back unit must persist no owner row")
	}
	// Original principal retries from zero and converges.
	svc.BindOwner = nil
	res, err := svc.Provision(req)
	if err != nil {
		t.Fatalf("retry after rollback must converge: %v", err)
	}
	if res.Disposition != ProvisionDispositionRunning {
		t.Fatalf("retry must report running, got %+v", res)
	}
	if cl.calls != 1 {
		t.Fatalf("retry must launch exactly once total, got %d", cl.calls)
	}
	// Another principal cannot claim the same key.
	if _, err := svc.Provision(bindingReq("jwt:op-2", "bind-9", "bs9", work, true)); err == nil {
		t.Fatal("cross-principal claim after commit must conflict")
	} else {
		requireConflict(t, err, "cross-principal claim after commit")
	}
	if cl.calls != 1 {
		t.Fatalf("refused claim must not relaunch, got %d launches", cl.calls)
	}
	if owner, found := ownerOf(t, "bs9"); !found || owner != "jwt:op-1" {
		t.Fatalf("refused claim must not move ownership, got %q found=%v", owner, found)
	}
}

// A crash after the reservation commits but before the launch
// starts must reconcile without duplicate execution: the retry
// converges on the committed unit and launches exactly once.
func TestProvisionOwnerBindingCommitWithoutLaunchReconciles(t *testing.T) {
	svc, cl, work := bindingFixture(t, false, 0)
	db, derr := openTasksDB()
	if derr != nil {
		t.Fatal(derr)
	}
	defer db.Close()
	repo := NewProvisionRepository(db)
	// Drive the unit exactly as the service does, then stop
	// before launch: this is the committed-but-unlaunched state
	// a pre-launch crash leaves behind.
	resv, err := repo.BeginReserve()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	// Build the intent through the service's own admission so
	// the replayed payload compares identical field-for-field.
	breq := bindingReq("jwt:op-1", "bind-10", "bs10", work, true)
	want, _, aerr := svc.admit(breq)
	if aerr != nil {
		t.Fatalf("admit: %v", aerr)
	}
	if err := resv.InsertReserved("bind-10", want); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if other, err := claimProvisionOwner(resv, "bs10", "jwt:op-1"); err != nil || other {
		t.Fatalf("claim: other=%v err=%v", other, err)
	}
	if err := resv.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// The retry finds the committed unit and launches once.
	res, err := svc.Provision(bindingReq("jwt:op-1", "bind-10", "bs10", work, true))
	if err != nil {
		t.Fatalf("reconciling retry must succeed: %v", err)
	}
	if res.Disposition != ProvisionDispositionRunning {
		t.Fatalf("reconciling retry must report running, got %+v", res)
	}
	if cl.calls != 1 {
		t.Fatalf("committed unit must launch exactly once, got %d", cl.calls)
	}
	if owner, found := ownerOf(t, "bs10"); !found || owner != "jwt:op-1" {
		t.Fatalf("reconcile must preserve the committed owner, got %q found=%v", owner, found)
	}
}

// The reservation transaction itself: uncommitted intent and
// owner rows are invisible outside the unit, and a rollback
// leaves neither behind. This is the boundary the atomicity
// claim rests on — not handle sharing.
func TestProvisionReservationTransactionBoundary(t *testing.T) {
	t.Setenv("METIS_STATE", t.TempDir())
	// Two handles: the unit holds dbA's single connection for the
	// whole transaction (MaxOpenConns(1)), so outside reads use
	// dbB — WAL serves the pre-unit snapshot without blocking.
	dbA, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer dbA.Close()
	dbB, err := openTasksDB()
	if err != nil {
		t.Fatal(err)
	}
	defer dbB.Close()
	repo := NewProvisionRepository(dbA)
	outside := NewProvisionRepository(dbB)
	// Pre-ensure both tables outside any transaction so the
	// outside reads below exercise visibility, not DDL: a
	// CREATE colliding with the open write unit would burn the
	// 5s busy timeout instead of proving anything.
	if err := repo.Ensure(); err != nil {
		t.Fatalf("ensure intent: %v", err)
	}
	if err := ensureProvisionOwnerTable(dbB); err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	want := mcpProvisionRecord{Session: "txs", Run: "r"}
	resv, err := repo.BeginReserve()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := resv.InsertReserved("tx-1", want); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if other, err := claimProvisionOwner(resv, "txs", "jwt:tx"); err != nil || other {
		t.Fatalf("claim: other=%v err=%v", other, err)
	}
	// Uncommitted: invisible to outside readers. Errors are
	// asserted, not ignored: a failed ensure must not masquerade
	// as invisibility.
	if _, _, found := outside.Load("tx-1"); found {
		t.Fatal("uncommitted intent must be invisible outside the unit")
	}
	if _, found, err := lookupProvisionOwner(dbB, "txs"); err != nil || found {
		t.Fatalf("uncommitted owner must be invisible outside the unit, got found=%v err=%v", found, err)
	}
	if err := resv.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, _, found := outside.Load("tx-1"); found {
		t.Fatal("rolled-back intent must persist nothing")
	}
	if _, found, _ := lookupProvisionOwner(dbB, "txs"); found {
		t.Fatal("rolled-back owner must persist nothing")
	}
	// Redo and commit: both durable together.
	resv2, err := repo.BeginReserve()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := resv2.InsertReserved("tx-1", want); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if other, err := claimProvisionOwner(resv2, "txs", "jwt:tx"); err != nil || other {
		t.Fatalf("claim: other=%v err=%v", other, err)
	}
	if err := resv2.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if _, _, found := outside.Load("tx-1"); !found {
		t.Fatal("committed intent must persist")
	}
	if owner, found, _ := lookupProvisionOwner(dbB, "txs"); !found || owner != "jwt:tx" {
		t.Fatalf("committed owner must persist, got %q found=%v", owner, found)
	}
}

// Local behavior is unchanged: the operator adopts a live session
// and the binding records the local principal.
func TestProvisionOwnerBindingLocalAdoptGrandfathered(t *testing.T) {
	svc, cl, work := bindingFixture(t, true, 0)
	res, err := svc.Provision(bindingReq("local-operator", "bind-8", "bs8", work, false))
	if err != nil {
		t.Fatalf("local adopt must stay permitted: %v", err)
	}
	if !res.Reconciled {
		t.Fatalf("local adopt of a live session must reconcile, got %+v", res)
	}
	if cl.calls != 0 {
		t.Fatalf("adopt must not launch a duplicate, got %d launches", cl.calls)
	}
	if owner, found := ownerOf(t, "bs8"); !found || owner != "local-operator" {
		t.Fatalf("local adopt must bind the local principal, got %q found=%v", owner, found)
	}
}

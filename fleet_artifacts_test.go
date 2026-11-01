package main

import (
	"os"
	"testing"
	"time"
)

// backdateFleetArtifact forces a stale manifest without sleeping.
func backdateFleetArtifact(t *testing.T, manifestID string, ns int64) {
	t.Helper()
	db, err := openTasksDB()
	if err != nil {
		t.Fatalf("openTasksDB: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE fleet_artifacts SET created_ns = ? WHERE manifest_id = ?`, ns, manifestID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// A manifest binds one digest to one node and one attempt: the
// same bytes from another node are a different manifest row, so
// cross-node confusion is impossible by construction.
func TestFleetArtifactsBindNodeAndAttempt(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	digest := "sha256:abc123"
	m1, err := recordFleetArtifact(id, "task-a", "atm-1", "/out/report.md", digest, 42)
	if err != nil {
		t.Fatalf("recordFleetArtifact: %v", err)
	}
	m2, err := recordFleetArtifact("node-elsewhere", "task-a", "atm-1", "/out/report.md", digest, 42)
	if err != nil {
		t.Fatalf("recordFleetArtifact (other node): %v", err)
	}
	if m1 == m2 {
		t.Fatalf("cross-node manifests share identity %q", m1)
	}
	got, err := getFleetArtifact(m1)
	if err != nil {
		t.Fatalf("getFleetArtifact: %v", err)
	}
	if got.NodeID != id || got.TaskID != "task-a" || got.Digest != digest {
		t.Fatalf("manifest mismatch: %+v", got)
	}
}

// The same row re-recorded is the same manifest: retries and
// duplicate reports converge instead of multiplying rows.
func TestFleetArtifactsIdempotentRecord(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	a, err := recordFleetArtifact(id, "task-b", "atm-1", "/out/x.md", "sha256:d", 7)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	b, err := recordFleetArtifact(id, "task-b", "atm-1", "/out/x.md", "sha256:d", 7)
	if err != nil {
		t.Fatalf("re-record: %v", err)
	}
	if a != b {
		t.Fatalf("re-record minted %q, want %q", b, a)
	}
}

// Unknown manifests are refused: retrieval of a never-recorded
// identity is an error, never empty bytes presented as content.
func TestFleetArtifactsUnknownRefused(t *testing.T) {
	if _, err := getFleetArtifact("fart-missing"); err == nil {
		t.Fatal("retrieved unknown manifest, want refusal")
	}
}

// GC removes only manifests older than the horizon that no live
// execution still references: fresh rows and rows bound to a
// claimed attempt survive; ancient unreferenced rows go.
func TestFleetArtifactsGCRespectsLive(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	id, err := enrollFleetNode(host, `{}`)
	if err != nil {
		t.Fatalf("enrollFleetNode: %v", err)
	}
	if err := heartbeatFleetNode(id); err != nil {
		t.Fatalf("heartbeatFleetNode: %v", err)
	}
	txLive := uniqueTaskKey("task-gc")
	txStale := uniqueTaskKey("task-old")
	req := OperationRequest{Actor: "test", RequestID: newRequestID()}
	if err := claimFleetExecution(id, txLive, "atm-1", req); err != nil {
		t.Fatalf("claim: %v", err)
	}
	live, err := recordFleetArtifact(id, txLive, "atm-1", "/out/live.md", "sha256:live", 1)
	if err != nil {
		t.Fatalf("record live: %v", err)
	}
	stale, err := recordFleetArtifact(id, txStale, "atm-9", "/out/old.md", "sha256:old", 1)
	if err != nil {
		t.Fatalf("record stale: %v", err)
	}
	backdateFleetArtifact(t, stale, time.Now().Add(-48*time.Hour).UnixNano())
	n, err := gcFleetArtifacts(24 * time.Hour)
	if err != nil {
		t.Fatalf("gcFleetArtifacts: %v", err)
	}
	if n != 1 {
		t.Fatalf("gc removed %d manifests, want 1", n)
	}
	if _, err := getFleetArtifact(live); err != nil {
		t.Fatalf("live manifest collected: %v", err)
	}
	if _, err := getFleetArtifact(stale); err == nil {
		t.Fatal("stale manifest survived GC, want removal")
	}
}

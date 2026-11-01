package main

import (
	"strings"
	"testing"
)

// Policy revisions are append-only: publishing v2 must not rewrite
// v1, and a run pinned to v1 must keep resolving v1 after v2 lands.
func TestTeamPolicyRevisionHistory(t *testing.T) {
	project := uniqueTaskKey("proj-pol")
	v1 := `{"workers":[{"id":"w-a","harness":"muse-native","profile":"muse","role":"implementation"}],
		"limits":{"max_active_workers":1},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	rev1, err := setTeamPolicy(project, v1, OperationRequest{Actor: "test", RequestID: newRequestID()})
	if err != nil {
		t.Fatalf("set v1: %v", err)
	}
	if rev1 != 1 {
		t.Fatalf("first revision = %d, want 1", rev1)
	}
	v2 := `{"workers":[{"id":"w-a","harness":"muse-native","profile":"muse","role":"implementation"},
		{"id":"w-b","harness":"muse-native","profile":"musefi","role":"verification"}],
		"limits":{"max_active_workers":2},
		"policy":{"require_evidence":true,"require_independent_review":true,"shared_write_workspace":false}}`
	rev2, err := setTeamPolicy(project, v2, OperationRequest{Actor: "test", RequestID: newRequestID()})
	if err != nil {
		t.Fatalf("set v2: %v", err)
	}
	if rev2 != 2 {
		t.Fatalf("second revision = %d, want 2", rev2)
	}
	got, rev, err := getTeamPolicy(project, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if rev != 1 || !strings.Contains(got, "w-a") || strings.Contains(got, "w-b") {
		t.Fatalf("v1 mutated: rev=%d policy=%s", rev, got)
	}
	if _, rev, err := getTeamPolicy(project, 0); err != nil || rev != 2 {
		t.Fatalf("latest = rev %d, err %v; want rev 2", rev, err)
	}
}

// A run pinned to a revision keeps that revision's constraints even
// after the project policy advances: in-flight attempts are never
// reinterpreted by a later edit.
func TestTeamPolicyPinStable(t *testing.T) {
	project := uniqueTaskKey("proj-pin")
	v1 := `{"workers":[{"id":"w-a","harness":"muse-native","profile":"muse","role":"implementation"}],
		"limits":{"max_active_workers":1},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	if _, err := setTeamPolicy(project, v1, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set v1: %v", err)
	}
	run := uniqueTaskKey("run-pin")
	if err := pinTeamPolicy(run, project); err != nil {
		t.Fatalf("pin: %v", err)
	}
	v2 := `{"workers":[{"id":"w-a","harness":"demo","profile":"","role":"implementation"}],
		"limits":{"max_active_workers":1},
		"policy":{"require_evidence":true,"require_independent_review":false,"shared_write_workspace":false}}`
	if _, err := setTeamPolicy(project, v2, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set v2: %v", err)
	}
	pinned, err := pinnedTeamPolicy(run)
	if err != nil {
		t.Fatalf("pinnedTeamPolicy: %v", err)
	}
	if pinned.Revision != 1 {
		t.Fatalf("pinned revision = %d, want 1", pinned.Revision)
	}
	if err := checkTeamSlot(pinned, "muse-native", "muse", 0); err != nil {
		t.Fatalf("pinned v1 should admit muse-native/muse: %v", err)
	}
	if err := checkTeamSlot(pinned, "demo", "", 0); err == nil {
		t.Fatal("pinned v1 admitted demo harness from v2, want refusal")
	}
}

// Admission checks the pinned policy, not good intentions: unknown
// harness, unapproved profile, and over-limit dispatch are refused
// with explicit reasons.
func TestTeamPolicyAdmissionGates(t *testing.T) {
	project := uniqueTaskKey("proj-adm")
	pol := `{"workers":[{"id":"w-a","harness":"muse-native","profile":"muse","role":"implementation"}],
		"limits":{"max_active_workers":1},
		"policy":{"require_evidence":true,"require_independent_review":true,"shared_write_workspace":false}}`
	if _, err := setTeamPolicy(project, pol, OperationRequest{Actor: "test", RequestID: newRequestID()}); err != nil {
		t.Fatalf("set: %v", err)
	}
	pinned, err := latestTeamPolicy(project)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if err := checkTeamSlot(pinned, "muse-native", "muse", 0); err != nil {
		t.Fatalf("eligible slot refused: %v", err)
	}
	if err := checkTeamSlot(pinned, "codex", "muse", 0); err == nil {
		t.Fatal("unknown harness admitted, want refusal")
	}
	if err := checkTeamSlot(pinned, "muse-native", "musefi", 0); err == nil {
		t.Fatal("unapproved profile admitted, want refusal")
	}
	if err := checkTeamSlot(pinned, "muse-native", "muse", 1); err == nil {
		t.Fatal("over-limit dispatch admitted, want refusal")
	}
}

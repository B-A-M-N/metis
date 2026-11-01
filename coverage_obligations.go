package main

// coverage_obligations.go: delegation-coverage obligation ledger (OCO-B).
//
// This file is now only the map. The ledger was split by
// responsibility; behavior is unchanged:
//
//   - coverage_store.go: obligation records, schema, queries,
//     compare-and-set transitions, revision identity.
//   - coverage_policy.go: PURE evaluation of pinned coverage
//     policies against immutable evidence snapshots.
//   - coverage_service.go: revision supersession + idempotent
//     obligation generation through the repository.
//   - coverage_gate.go: READ-ONLY assessment of unresolved
//     obligations for an exact (task, attempt, evidence revision).

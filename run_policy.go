package main

// run_policy.go: run-level scheduling policy contracts.
//
// Thin policy surface only: dependency gates, live-task budgets,
// and run close-out. Admission decisions, not execution.
//
// Implementation lives in the split files:
//   - task_dependencies.go: edge admission (shared transaction-scoped
//     admitDependEdgeTx + cycle validation), idempotent insertion,
//     unsatisfiedDeps status.
//   - run_capacity.go: concurrency limits, live-task accounting,
//     capacity admission (never finalizes, never mutates graphs;
//     the claim transaction rechecks atomically at dispatch time).
//   - run_finalization.go: closed-run state, completion checks, the
//     finalization transaction — the ONLY authority that closes a run.

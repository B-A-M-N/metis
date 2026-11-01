package main

// task_recovery.go: retry, cancellation, uncertain settlement —
// module map.
//
// Reconciling uncertain attempts, replacement dispatch, task
// cancellation with owned-execution settlement. Uncertain outcomes
// are preserved, never silently replaced.
//
// The recovery pipeline is split by responsibility:
//
//	assurance_cancel.go     cancellation coordination + descendant
//	                        accounting (correction-lineage BFS,
//	                        attributable transitions, skipped-terminal
//	                        reporting)
//	assurance_terminate.go  execution-specific terminators: OS
//	                        processes (PID-incarnation-checked
//	                        signals) and native turns (MSP, never
//	                        signalled); capability routing;
//	                        worker settlement accounting
//	assurance_reconcile.go  attempt reconciliation: live-attempt
//	                        refusal, atomic supersede-and-bump,
//	                        history never rewritten
//	assurance_retry.go      retry policy (attempt budget, bound
//	                        workspace root) and replacement
//	                        dispatch
//
// Invariants kept by every layer: a sent SIGTERM is requested,
// never confirmed — only a reaped, gone, or never-bound target
// counts as terminated. Unknown termination retains the
// workspace claim (termination_unknown), never reads as
// cancelled.

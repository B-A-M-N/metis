package main

// muse_approvals.go: native approval pipeline — module map.
//
// Provider approvals announce twice: the must-answer
// `approval/request` and the `approval/requested` fold. Both land on
// the host notification channel and feed the same pending row keyed
// by approvalId — the stable identity the schema mandates (never the
// JSON-RPC id, which belongs to one connection). `approval/resolved`
// closes the row with the server's terminal decision.
// `approval/listPending` is the lease-free server fold read.
//
// The pipeline is split by responsibility; nothing here implements
// it, so muse_host.go stays intact:
//
//	approval_observer.go   records provider notifications into
//	                       pending projections; answers must-answer
//	                       requests once recorded (receipt means
//	                       recorded, never decided)
//	approval_store.go      durable rows: pending requests plus the
//	                       server's terminal decision
//	approval_decision.go   decision service: validates actor
//	                       authority, pending identity, offered
//	                       choices, and idempotency before any byte
//	                       moves
//	approval_transport.go  MSP transport adapter: carries the
//	                       validated decision only
//
// Provider approval is provider approval. It is never task
// acceptance: deciding a provider prompt judges no acceptance
// criterion, files no verdict, and satisfies no coverage duty. The
// verification authority (verification.go, verdict.go,
// coverage_obligations.go) never merges the two.

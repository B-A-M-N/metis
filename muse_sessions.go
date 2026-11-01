package main

// muse_sessions.go: protocol overview for native (MSP) session and
// turn operations over a museHost.
//
// The implementation lives in three protocol-concern files; this
// file keeps only the shared contract summary so the wire rules
// stay visible in one place:
//
//   - muse_session_rpc.go — session lifecycle: create (idempotent),
//     read, resume, list, and config checks, plus the client-minted
//     UUIDv7 commandId primitive backing every idempotency handle.
//   - muse_turn_rpc.go — turn admission: submit and cancel. The
//     turn/start ack (turnId + disposition) admits work; it never
//     completes it.
//   - muse_observation.go — cursor subscription and terminal-event
//     observation. Only a turn/completed (or turn/unqueued)
//     notification carrying the authoritative `terminal` value ends
//     a turn; every other outcome stays unknown with the last
//     observed cursor for replay.
//
// Durability for the lost-ack-never-duplicates-session guarantee
// lives in muse_session_store.go: the intent row is persisted
// before transmitting, so a retry with the same caller key returns
// the already-created session instead of minting another.

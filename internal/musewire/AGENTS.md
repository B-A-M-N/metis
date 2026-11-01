# musewire — DOX contract

## Purpose
Muse MSP wire framing and codecs (encode calls/notifies/results, classify frames).

## Ownership
Byte shapes, frame classification, cursor passthrough, protocol constants.

## Local Contracts
- Stdlib only (`encoding/json`, `fmt`, `time`). Never import package main.
- Wire bytes are byte-identical to the pre-extraction protocol; changes here are protocol changes and need byte-exact tests.
- No session orchestration, no task ledger, no process supervision (those stay in package main).

## Forbidden
Command-ID minting policy, turn semantics, ack/replay decisions, daemon management.

## Verification
`go test ./internal/musewire/` — byte-exactness, classification, round-trip.

## Child DOX Index
None.

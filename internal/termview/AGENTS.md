# termview — DOX contract

## Purpose
Pure terminal measurement and sanitization (width, ANSI sequences, truncation).

## Ownership
Rune/sequence width, sanitization, padding. No rendering layout, no I/O.

## Local Contracts
- Stdlib only (`regexp`, `strings`, `unicode/utf8`). Never import package main, DB, processes, tmux, or transports.
- Pure functions: no queries, no blocking, no clock.

## Forbidden
Anything beyond terminal math. Layout lives in `tui_layout.go` (package main).

## Verification
`go test ./internal/termview/` — width/trunc/pad/sanitize tables.

## Child DOX Index
None.

package main

// input_sender.go: terminal input leaves the Bubble Tea update
// path (P0-B).
//
// This file is now only a pointer: the delivery boundary
// (queueing, execution, per-session serialization, receipt
// handling, and the typed {session, opID, disposition} result)
// lives tea-free in terminal_input_queue.go, and the Bubble Tea
// presentation adapters (waitInputResultCmd, applyInputResult)
// live in tui_input.go. It stays so the P0-B history and
// references keep a stable home.

package main

// session_lifecycle_unified.go: unified lifecycle projection (P0).
//
// tmux, managed PTY, and muse-native each name liveness in their own
// store vocabulary (registry stopped flag, pty_sessions status,
// logical_sessions state). Transport surfaces collapsed those to an
// alive bool, conflating stopped with failed with unknown. This
// projection maps each backend's observed state to one vocabulary,
// and — critically — keeps two facts separate:
//
//   unified_lifecycle (process/harness readiness):
//     running  process converged; interactive harness readiness
//              not observed (one-shot, unknown, or bare harness)
//     ready    running AND the recorded harness is observed
//              interactive-capable (SupportsContinuation, or the
//              curated muse-code launcher, or a bound muse-native
//              session). Readiness is a harness-capability fact,
//              NEVER inferred from task occupancy: an idle worker
//              with a one-shot harness is running, not ready.
//     stopped  recorded terminal stop (clean)
//     failed   supervisor-confirmed failure (worker-gone / non-zero exit)
//     unknown  indeterminate (store unreadable, starting, orphaned,
//              dead but not recorded as stopped, binding-only)
//
//   task_occupancy (ledger fact, reported alongside, never folded
//   into readiness):
//     busy     an open dispatched/working task is bound to the
//              session's dispatch-canonical agent spelling
//     idle     none open (store readable)
//     unknown  task store unreadable
//
// No state machine changes: occupancy comes from the task ledger
// read (task_readmodel), liveness/readiness from the backend
// stores plus the recorded harness capability. Store failure
// yields unknown, never a fabricated ready/running.

import (
	"strings"
)

// Unified lifecycle vocabulary (stable for MCP payloads).
const (
	UnifiedRunning = "running"
	UnifiedReady   = "ready"
	UnifiedStopped = "stopped"
	UnifiedFailed  = "failed"
	UnifiedUnknown = "unknown"
)

// Task occupancy vocabulary (reported beside the lifecycle).
const (
	OccupancyBusy   = "busy"
	OccupancyIdle   = "idle"
	OccupancyUnknown = "unknown"
)

// harnessInteractive reports observed interactive readiness from
// the recorded harness: either the curated interactive muse-code
// launcher (provision-confirmed process), or an adapter that
// genuinely continues a conversation (Interactive + live input).
// One-shot, unknown, and empty harnesses are never ready — a
// process being alive does not prove it can take a follow-up turn.
func harnessInteractive(harness string) bool {
	if harness == "muse-code" {
		return true
	}
	a, err := lookupAdapter(harness)
	return err == nil && a.SupportsContinuation()
}

// runningLifecycle maps a converged process to running vs ready
// based solely on observed harness readiness.
func runningLifecycle(interactive bool) (string, bool) {
	if interactive {
		return UnifiedReady, true
	}
	return UnifiedRunning, true
}

// lifecycleForPty maps one PTY classified state to the unified
// vocabulary. classified is ptyClassify output; exitCode is the worker's
// recorded code (-1 = none recorded).
func lifecycleForPty(classified string, exitCode int, interactive bool) (string, bool) {
	switch classified {
	case "running":
		return runningLifecycle(interactive)
	case "exited":
		if exitCode != 0 && exitCode != -1 {
			return UnifiedFailed, false
		}
		return UnifiedStopped, false
	case "gone":
		return UnifiedStopped, false
	case "worker-gone":
		return UnifiedFailed, false
	default:
		// starting, orphaned, or anything unrecognized: indeterminate.
		return UnifiedUnknown, false
	}
}

// lifecycleForTmux maps registry stopped + probed running to the
// unified vocabulary. A recorded stop wins over liveness: markStopped
// only lands after a confirmed kill, so stopped means terminal.
func lifecycleForTmux(stopped, running bool, interactive bool) (string, bool) {
	if stopped {
		return UnifiedStopped, false
	}
	if running {
		return runningLifecycle(interactive)
	}
	return UnifiedUnknown, false
}

// lifecycleForNative maps the logical record state to the unified
// vocabulary. Recorded state only (no daemon dial), matching the
// diagnose/listing contract: binding readiness stays in exec_target.
// interactive must already incorporate the binding check (a bound
// native ID); an unbound record is running, never ready.
func lifecycleForNative(state string, interactive bool) (string, bool) {
	switch state {
	case logicalStopped:
		return UnifiedStopped, false
	case logicalActive:
		return runningLifecycle(interactive)
	default:
		return UnifiedUnknown, false
	}
}

// sessionOccupancyFor reports task occupancy for one display name
// using ONLY its dispatch-canonical agent spelling, so backends
// sharing a bare name never cross-attribute work:
//   - PTY sessions dispatch as "pty:<bare>" (dispatchSessionScope
//     requires the qualified display), so only that spelling counts.
//   - Native sessions dispatch as the bare logical name.
//   - Tmux sessions dispatch as the bare registry name.
//
// Matching every spelling (the old behavior) let a dispatched task
// for tmux "w" mark pty:w and native:w busy — a reproducible
// identity collision. known=false means the map could not be built.
func sessionOccupancyFor(display string, busyByAgent map[string]bool, known bool) string {
	if !known {
		return OccupancyUnknown
	}
	spelling := display
	if strings.HasPrefix(display, nativePrefix) {
		spelling = strings.TrimPrefix(display, nativePrefix)
	}
	if busyByAgent[spelling] {
		return OccupancyBusy
	}
	return OccupancyIdle
}

// unifiedWithOccupancy resolves one display name against a
// preloaded busy map: backend probe first, then the pure backend
// mapping with the harness-readiness fact. Store failure yields
// unknown, never an error — a diagnostic projection must survive
// broken stores.
func unifiedWithOccupancy(display string, busyByAgent map[string]bool, busyKnown bool) (string, bool, string) {
	occ := sessionOccupancyFor(display, busyByAgent, busyKnown)
	if display == "" {
		return UnifiedUnknown, false, occ
	}
	if isPtySession(display) {
		m, err := ptyGet(ptyBare(display))
		if err != nil {
			return UnifiedUnknown, false, occ
		}
		life, alive := lifecycleForPty(ptyClassify(m), m.ExitCode, harnessInteractive(m.Harness))
		return life, alive, occ
	}
	if strings.HasPrefix(display, nativePrefix) {
		ls, found, err := loadLogicalSession(strings.TrimPrefix(display, nativePrefix))
		if err != nil || !found {
			return UnifiedUnknown, false, occ
		}
		interactive := ls.NativeID != "" && harnessInteractive(ls.Harness)
		life, alive := lifecycleForNative(ls.State, interactive)
		return life, alive, occ
	}
	reg := loadRegistry()
	if e, ok := reg[display]; ok {
		life, alive := lifecycleForTmux(e.Stopped, sessionRunning(display), harnessInteractive(e.Harness))
		return life, alive, occ
	}
	if ls, found, err := loadLogicalSession(display); err == nil && found && ls.Backend == TargetMuseNative {
		interactive := ls.NativeID != "" && harnessInteractive(ls.Harness)
		life, alive := lifecycleForNative(ls.State, interactive)
		return life, alive, occ
	}
	// Binding-only (no logical record): a muse binding proves a
	// native session was bound once, not its current lifecycle.
	// Reporting it active would fabricate liveness from durable
	// state alone — unknown is the honest projection.
	if b, err := getMuseBinding(display); err == nil && b.NativeID != "" {
		return UnifiedUnknown, false, occ
	}
	return UnifiedUnknown, false, occ
}

// sessionUnifiedLifecycle is the single-session entry point (terminal
// status). One busy-map load, one backend probe.
func sessionUnifiedLifecycle(display string) (string, bool, string) {
	busyByAgent, busyKnown := sessionBusyAll()
	return unifiedWithOccupancy(display, busyByAgent, busyKnown)
}

// sessionUnifiedLifecycleBulk preloads the busy map once for a listing.
func sessionUnifiedLifecycleBulk(display string, busyByAgent map[string]bool, busyKnown bool) (string, bool, string) {
	return unifiedWithOccupancy(display, busyByAgent, busyKnown)
}

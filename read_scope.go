package main

// read_scope.go: principal-scoped remote read authorization.
//
// Remote MCP reads share the domain read functions with local
// stdio. Without a scope, any authenticated JWT subject could
// read every controller's sessions, tasks, reports, events, and
// projects — a cross-controller information disclosure. ReadScope
// is established at the transport boundary from the verified JWT
// principal (plus the explicit metis:fleet grant for fleet-wide
// observation) and enforced inside the read paths BEFORE
// pagination, using the existing provision-owner metadata.
// Local stdio passes a nil scope (unscoped, behavior unchanged).

import "strings"

// eventsScanBudgetPages bounds per-request forward/backward
// event scans for scoped readers: at most this many database
// pages (maxOrchPage rows each) per request, with an advancing
// continuation cursor — never an unbounded history scan.
const eventsScanBudgetPages = 10

// ReadScope authorizes one remote reader's view of shared read
// models. FleetWide requires the explicit metis:fleet JWT scope —
// fleet observation is an authorization decision, never a
// consequence of authenticated read access.
type ReadScope struct {
	Principal string
	FleetWide bool
}

// bareSession strips backend display prefixes for owner lookup.
func bareSession(name string) string {
	name = strings.TrimPrefix(name, ptyPrefix)
	name = strings.TrimPrefix(name, nativePrefix)
	return name
}

// scopeAllowsSession reports whether scope may observe session.
// Unknown/unowned sessions are hidden remotely (fail closed).
func scopeAllowsSession(scope *ReadScope, session string) bool {
	if scope == nil || scope.FleetWide {
		return true
	}
	owner, found, err := provisionOwnerForSession(session)
	if err != nil || !found {
		return false
	}
	return owner == scope.Principal
}

// scopeAllowsTask reports whether scope may observe task tk:
// its session must be owned by the principal (or fleet-wide).
func scopeAllowsTask(scope *ReadScope, tk Task) bool {
	if scope == nil || scope.FleetWide {
		return true
	}
	return scopeAllowsSession(scope, tk.Agent)
}

// scopeAllowsEvent reports whether scope may observe event e:
// its own events, events targeting its sessions, and worker
// events originating from its sessions (reports remain visible
// to their owner). Other controllers' events stay hidden.
func scopeAllowsEvent(scope *ReadScope, e OrchEvent) bool {
	if scope == nil || scope.FleetWide {
		return true
	}
	if e.OriginPrincipal == scope.Principal {
		return true
	}
	if e.TargetAgent != "" && scopeAllowsSession(scope, e.TargetAgent) {
		return true
	}
	if e.OriginPrincipal != "" && scopeAllowsSession(scope, e.OriginPrincipal) {
		return true
	}
	if e.MuseSession != "" && scopeAllowsSession(scope, e.MuseSession) {
		return true
	}
	return false
}

// filterScopedEvents applies scope before cursor pagination.
func filterScopedEvents(scope *ReadScope, events []OrchEvent) []OrchEvent {
	if scope == nil || scope.FleetWide {
		return events
	}
	out := make([]OrchEvent, 0, len(events))
	for _, e := range events {
		if scopeAllowsEvent(scope, e) {
			out = append(out, e)
		}
	}
	return out
}

// filterScopedTasks applies scope before pagination.
func filterScopedTasks(scope *ReadScope, tasks []Task) []Task {
	if scope == nil || scope.FleetWide {
		return tasks
	}
	out := make([]Task, 0, len(tasks))
	for _, tk := range tasks {
		if scopeAllowsTask(scope, tk) {
			out = append(out, tk)
		}
	}
	return out
}

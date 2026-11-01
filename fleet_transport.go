package main

import (
	"fmt"
	"os"
	"time"
)

// fleet_transport.go: authenticated node request routing.
//
// This is the second fleet module. It routes a (node, session) pair
// to the local execution backend when — and only when — the node is
// an enrolled, live, LOCAL node. Remote nodes are refused with an
// explicit error naming the missing leg: there is no cross-machine
// transport yet, and this module must not fake one by executing
// remote work locally. Session readiness itself stays with
// resolveExecTarget, which this module delegates to, never
// reimplements.

// fleetRouteSession resolves nodeID to a live enrolled node, proves
// it is this machine, then delegates session readiness to the local
// backend. Unknown nodes, stale heartbeats, and remote hosts are
// errors, never fallbacks.
func fleetRouteSession(nodeID, session string, ttl time.Duration) (execTarget, error) {
	nodes, err := listFleetNodes(ttl)
	if err != nil {
		return execTarget{}, err
	}
	var found *fleetNode
	for i, n := range nodes {
		if n.NodeID == nodeID {
			found = &nodes[i]
			break
		}
	}
	if found == nil {
		return execTarget{}, fmt.Errorf("fleet: unknown node %q: enroll before routing", nodeID)
	}
	if !found.Alive {
		return execTarget{}, fmt.Errorf("fleet: node %q (host %q) is stale: heartbeat lapsed, not routable", nodeID, found.Host)
	}
	local, err := os.Hostname()
	if err != nil {
		return execTarget{}, fmt.Errorf("fleet: cannot prove local host: %w", err)
	}
	if found.Host != local {
		return execTarget{}, fmt.Errorf("fleet: node %q is remote (host %q): no remote transport yet", nodeID, found.Host)
	}
	return resolveExecTarget(session)
}

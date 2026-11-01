package main

import (
	"database/sql"
	"fmt"
	"time"
)

// fleet_node.go: enrolled machine identity for the Hermes Fleet
// node layer. This is the first of six fleet modules; it has no
// dependencies beyond the task database.
//
// A node is a machine (or runtime) eligible to execute work. The
// Metis controller remains the only task authority: enrollment
// records identity and liveness, never task state. Reads project
// liveness (Alive = heartbeat within ttl); stale nodes stay listed
// but never read as ready. Heartbeats for unknown nodes are refused
// rather than auto-registered: identity comes from enrollment,
// never from first contact.

type fleetNode struct {
	NodeID        string
	Host          string
	Capabilities  string
	Enrolled      int64
	LastHeartbeat int64
	Alive         bool
}

func ensureFleetNodeTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS fleet_nodes (
		node_id          TEXT PRIMARY KEY,
		host             TEXT NOT NULL DEFAULT '',
		capabilities     TEXT NOT NULL DEFAULT '{}',
		enrolled_ns      INTEGER NOT NULL,
		last_heartbeat_ns INTEGER NOT NULL
	)`)
	return err
}

// enrollFleetNode mints a fresh node identity for host. Every call
// mints a distinct identity: re-enrollment never merges with, or
// resurrects, a prior node row.
func enrollFleetNode(host, capabilities string) (string, error) {
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureFleetNodeTable(db); err != nil {
		return "", err
	}
	if capabilities == "" {
		capabilities = "{}"
	}
	id := newUniqueID("node")
	now := time.Now().UnixNano()
	if _, err := db.Exec(`INSERT INTO fleet_nodes (node_id, host, capabilities, enrolled_ns, last_heartbeat_ns)
		VALUES (?, ?, ?, ?, ?)`, id, host, capabilities, now, 0); err != nil {
		return "", err
	}
	return id, nil
}

// heartbeatFleetNode records liveness for an enrolled node. Unknown
// nodes fail closed: the caller must enroll first.
func heartbeatFleetNode(nodeID string) error {
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureFleetNodeTable(db); err != nil {
		return err
	}
	res, err := db.Exec(`UPDATE fleet_nodes SET last_heartbeat_ns = ? WHERE node_id = ?`,
		time.Now().UnixNano(), nodeID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("unknown fleet node %q: enroll before heartbeat", nodeID)
	}
	return nil
}

// listFleetNodes returns all enrolled nodes with liveness projected
// against ttl. Stale nodes are included with Alive=false so callers
// can distinguish "known but down" from "never enrolled".
func listFleetNodes(ttl time.Duration) ([]fleetNode, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureFleetNodeTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT node_id, host, capabilities, enrolled_ns, last_heartbeat_ns
		FROM fleet_nodes ORDER BY enrolled_ns ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UnixNano()
	var out []fleetNode
	for rows.Next() {
		var n fleetNode
		if err := rows.Scan(&n.NodeID, &n.Host, &n.Capabilities, &n.Enrolled, &n.LastHeartbeat); err != nil {
			return nil, err
		}
		n.Alive = n.LastHeartbeat > 0 && now-n.LastHeartbeat <= int64(ttl)
		out = append(out, n)
	}
	return out, rows.Err()
}

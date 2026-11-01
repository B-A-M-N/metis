package main

// project_roots.go: authorized project-root registry (P1-2).
//
// A model caller must not gain arbitrary command execution by
// passing a workdir and a token. metis_provision admits only
// workdirs at or under a locally registered root, and an explicit
// custom command additionally requires that root's custom-command
// capability. Adapter-built commands (harness + objective) are
// curated invocations, not custom commands.
//
// Registration is CLI-only on purpose: no MCP tool can authorize
// its own target. Roots are matched by longest canonical prefix;
// symlinks are resolved at registration so a linked path cannot
// smuggle a workdir outside its root.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// projectRoot is one authorized execution root.
type projectRoot struct {
	Root        string `json:"root"`
	AllowCustom bool   `json:"allow_custom"`
	Created     string `json:"created"`
}

func ensureProjectRootsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS project_roots (
		root TEXT PRIMARY KEY,
		allow_custom INTEGER NOT NULL DEFAULT 0,
		created TEXT NOT NULL
	)`)
	return err
}

// canonicalProvisionDir resolves a workdir the way the launchers
// consume it: absolute, cleaned, symlinks resolved. Registration
// and authorization share it, so neither side can see a different
// directory through the same string.
func canonicalProvisionDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Clean(abs), nil
		}
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func registerProjectRoot(dir string, allowCustom bool) (string, error) {
	canon, err := canonicalProvisionDir(dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(canon); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("project root is not a directory: %s", dir)
	}
	db, err := openTasksDB()
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := ensureProjectRootsTable(db); err != nil {
		return "", err
	}
	custom := 0
	if allowCustom {
		custom = 1
	}
	if _, err := db.Exec(`INSERT INTO project_roots (root, allow_custom, created) VALUES (?, ?, ?)
		ON CONFLICT(root) DO UPDATE SET allow_custom = excluded.allow_custom`,
		canon, custom, nowUTC()); err != nil {
		return "", err
	}
	return canon, nil
}

func revokeProjectRoot(dir string) error {
	canon, err := canonicalProvisionDir(dir)
	if err != nil {
		return err
	}
	db, err := openTasksDB()
	if err != nil {
		return err
	}
	defer db.Close()
	if err := ensureProjectRootsTable(db); err != nil {
		return err
	}
	res, err := db.Exec(`DELETE FROM project_roots WHERE root = ?`, canon)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no such project root: %s", dir)
	}
	return nil
}

func listProjectRoots() ([]projectRoot, error) {
	db, err := openTasksDB()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := ensureProjectRootsTable(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT root, allow_custom, created FROM project_roots ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []projectRoot
	for rows.Next() {
		var r projectRoot
		var custom int
		if err := rows.Scan(&r.Root, &custom, &r.Created); err != nil {
			return nil, err
		}
		r.AllowCustom = custom != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// findProjectRoot returns the longest registered root containing
// dir. An error means no registered root covers it.
func findProjectRoot(dir string) (projectRoot, error) {
	canon, err := canonicalProvisionDir(dir)
	if err != nil {
		return projectRoot{}, err
	}
	roots, err := listProjectRoots()
	if err != nil {
		return projectRoot{}, err
	}
	best := -1
	for i, r := range roots {
		if canon == r.Root || strings.HasPrefix(canon, r.Root+string(os.PathSeparator)) {
			if best == -1 || len(r.Root) > len(roots[best].Root) {
				best = i
			}
		}
	}
	if best == -1 {
		return projectRoot{}, fmt.Errorf("workdir %s is not under a registered project root; register it with project-root-register before provisioning", dir)
	}
	return roots[best], nil
}

// authorizeProvisionTarget enforces the P1-2 gates for one
// model-facing provision: registered root containing the workdir,
// plus the root's custom-command capability when the caller
// supplies its own command string.
func authorizeProvisionTarget(workdir string, explicitCommand bool) error {
	root, err := findProjectRoot(workdir)
	if err != nil {
		return err
	}
	if explicitCommand && !root.AllowCustom {
		return fmt.Errorf("custom commands need the custom-command capability on project root %s; register it with --custom or use a harness-built command", root.Root)
	}
	return nil
}

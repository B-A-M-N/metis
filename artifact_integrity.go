package main

// artifact_integrity.go: workspace confinement and content hashing
// for task evidence.
//
// Evidence is only meaningful when it provably came from the
// worker's authorized workspace and provably has not changed since.
// confinement (artifactRoot, confineToRoot) answers the first;
// sha256Digest answers the second. The store (artifact_store.go)
// calls both before recording anything.

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
)

// sha256Digest renders the content address stored for evidence:
// a "sha256:<hex>" digest of the exact bytes read.
func sha256Digest(data []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(data))
}

// artifactRoot resolves the only directory a task's evidence may come
// from: the workdir of the worker session the task is assigned to.
func artifactRoot(t Task) (string, error) {
	root, err := sessionWorkdir(t.Agent)
	if err != nil {
		return "", fmt.Errorf("artifact refused: %w", err)
	}
	return root, nil
}

// confineToRoot ensures abs stays inside root after symlink
// resolution, so `..` segments and symlink escapes cannot smuggle in
// files from outside the worker's authorized workspace.
func confineToRoot(root, abs string) error {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the workspace")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("workspace unreadable: %w", err)
	}
	realAbs, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("artifact unreadable: %w", err)
	}
	rel, err = filepath.Rel(realRoot, realAbs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the workspace")
	}
	return nil
}

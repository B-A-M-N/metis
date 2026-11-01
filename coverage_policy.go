package main

// coverage_policy.go: PURE evaluation of pinned coverage policies
// against immutable evidence snapshots.
//
// evaluateCoveragePolicy maps a run's pinned coverage rules and the
// filed evidence for one attempt to the required obligation specs:
// what duties are owed, before anything is persisted. It performs
// NO writes, NO reads, and touches NO clock: same policy plus same
// evidence snapshot always yields the same specs.
//
// The caller supplies the evidence as an immutable snapshot; which
// attempt is current, and whether a row already exists, are service
// concerns, never policy ones.

import (
	"encoding/json"
	"path"
)

// coverageObligationSpec is one required duty derived from policy
// and evidence, before persistence. EvidenceRefs carries the filed
// artifact digests the duty reviews, JSON-encoded.
type coverageObligationSpec struct {
	Kind         string
	EvidenceRefs string
}

// coverageRuleApplies reports whether a rule's path globs match any
// filed artifact. An empty pattern list applies to every report.
// Both the base name and the full path are tried, so `*auth*`
// matches `pkg/auth/server.go` as well as a top-level `auth.go`.
func coverageRuleApplies(rule coverageRule, artifacts []Artifact) bool {
	if len(rule.Paths) == 0 {
		return true
	}
	for _, a := range artifacts {
		base := path.Base(a.Path)
		for _, pat := range rule.Paths {
			if ok, _ := path.Match(pat, base); ok {
				return true
			}
			if ok, _ := path.Match(pat, a.Path); ok {
				return true
			}
		}
	}
	return false
}

// evaluateCoveragePolicy returns the obligation specs owed for one
// evidence snapshot under a pinned policy: one spec per rule whose
// path globs match the filed artifacts. Rules that match nothing
// produce nothing; a policy without coverage produces nothing.
func evaluateCoveragePolicy(pol teamPolicy, artifacts []Artifact) ([]coverageObligationSpec, error) {
	digests := make([]string, 0, len(artifacts))
	for _, a := range artifacts {
		digests = append(digests, a.Digest)
	}
	evidence, err := json.Marshal(digests)
	if err != nil {
		return nil, err
	}
	var out []coverageObligationSpec
	for _, rule := range pol.Coverage {
		if !coverageRuleApplies(rule, artifacts) {
			continue
		}
		out = append(out, coverageObligationSpec{Kind: rule.Kind, EvidenceRefs: string(evidence)})
	}
	if out == nil {
		out = []coverageObligationSpec{}
	}
	return out, nil
}

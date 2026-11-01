package main

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

// team_policy.go: Team/role contract definition + pure evaluation
// (OCO-A).
//
// A project declares which worker roles may execute: eligible
// harnesses, approved account profiles, concurrency budgets, and
// verification requirements. Revisions are append-only; a run pins
// the revision in force at admission, so later policy edits never
// reinterpret an in-flight attempt. Roles are eligibility
// assignments, not instructions to run every worker continuously.
//
// This file is definition + pure evaluation ONLY: it parses policy
// documents and judges (harness, profile, active) triples against an
// immutable snapshot. It performs no I/O. Persistence, revision
// lookup, and run pinning live in team_policy_store.go; live
// worker-slot accounting and admission against a pinned snapshot
// live in placement_capacity.go.

type teamWorker struct {
	ID      string `json:"id"`
	Harness string `json:"harness"`
	Profile string `json:"profile"`
	Role    string `json:"role"`
}

type teamLimits struct {
	MaxActiveWorkers int `json:"max_active_workers"`
}

type teamPolicyFlags struct {
	RequireEvidence          bool `json:"require_evidence"`
	RequireIndependentReview bool `json:"require_independent_review"`
	SharedWriteWorkspace     bool `json:"shared_write_workspace"`
}

// coverageRule is one deterministic delegation-coverage duty: filed
// evidence matching Paths requires a review of Kind at the
// enforcement checkpoints (report, verify, finalize). An empty
// Paths list applies to every report. Independent defaults to true:
// the reviewer must differ from the worker that produced the
// attempt; a rule may explicitly relax it (e.g. worker-run
// integration testing under a separate review duty).
//
// ASSURANCE-CONTRACT SHIM: this type is logically owned by the
// assurance domain — coverage_obligations.go consumes it
// (coverageRuleApplies, requireCoverageObligations) and
// obligationKinds (defined there) validates rule kinds at parse
// time. The definition lives here, not there, because moving it
// into the coverage files requires editing files outside this
// split's write grant; coordination was done by reading the
// coverage files only, with zero edits to them. When the package
// extraction lands, move this type into the assurance contract
// alongside obligationKinds and leave a type alias here.
type coverageRule struct {
	Kind        string   `json:"kind"`
	Paths       []string `json:"paths"`
	Independent *bool    `json:"independent"`
}

// requiresIndependent reports the rule's independence constraint,
// defaulting to true when the policy does not say otherwise.
func (r coverageRule) requiresIndependent() bool {
	return r.Independent == nil || *r.Independent
}

type teamPolicyDoc struct {
	Workers  []teamWorker    `json:"workers"`
	Limits   teamLimits      `json:"limits"`
	Policy   teamPolicyFlags `json:"policy"`
	Coverage []coverageRule  `json:"coverage"`
}

// teamPolicy is one parsed revision of a project's contract. Treat
// values as immutable snapshots: admission (placement_capacity.go)
// and the claim transaction (dispatch_commit.go) judge a pinned copy,
// never a live row.
type teamPolicy struct {
	Project  string
	Revision int
	Workers  []teamWorker
	Limits   teamLimits
	Flags    teamPolicyFlags
	Coverage []coverageRule
	Raw      string
}

func parseTeamPolicy(project string, revision int, raw string) (teamPolicy, error) {
	var doc teamPolicyDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return teamPolicy{}, fmt.Errorf("team policy: invalid JSON: %w", err)
	}
	if len(doc.Workers) == 0 {
		return teamPolicy{}, fmt.Errorf("team policy: at least one worker role required")
	}
	for i, w := range doc.Workers {
		if strings.TrimSpace(w.ID) == "" || strings.TrimSpace(w.Harness) == "" || strings.TrimSpace(w.Role) == "" {
			return teamPolicy{}, fmt.Errorf("team policy: worker %d needs id, harness, and role", i)
		}
	}
	if doc.Limits.MaxActiveWorkers < 1 {
		return teamPolicy{}, fmt.Errorf("team policy: max_active_workers must be >= 1")
	}
	for i, r := range doc.Coverage {
		if !obligationKinds[r.Kind] {
			return teamPolicy{}, fmt.Errorf("team policy: coverage rule %d has unknown kind %q", i, r.Kind)
		}
		for _, pat := range r.Paths {
			if _, err := path.Match(pat, ""); err != nil {
				return teamPolicy{}, fmt.Errorf("team policy: coverage rule %d has invalid glob %q: %v", i, pat, err)
			}
		}
	}
	return teamPolicy{Project: project, Revision: revision, Workers: doc.Workers, Limits: doc.Limits, Flags: doc.Policy, Coverage: doc.Coverage, Raw: raw}, nil
}

// checkTeamSlot admits one (harness, profile) pair against the
// pinned contract: the pair must be an eligible worker and the live
// count must be under the concurrency budget. Pure: the caller
// supplies the snapshot and the count; nothing is read here.
func checkTeamSlot(pol teamPolicy, harness, profile string, active int) error {
	eligible := false
	for _, w := range pol.Workers {
		if w.Harness == harness && w.Profile == profile {
			eligible = true
			break
		}
	}
	if !eligible {
		return fmt.Errorf("team policy (rev %d): harness %q profile %q is not an eligible worker",
			pol.Revision, harness, profile)
	}
	if active >= pol.Limits.MaxActiveWorkers {
		return fmt.Errorf("team policy (rev %d): %d active workers at limit %d",
			pol.Revision, active, pol.Limits.MaxActiveWorkers)
	}
	return nil
}

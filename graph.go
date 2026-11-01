package main

// graph.go: read-only deterministic projections over one run's
// existing lineage. This file is now only the package note; the
// implementation lives in the split files:
//
//	graph_lineage.go — lineage construction (buildRunGraph)
//	graph_readiness.go — dependency readiness (graphBlockers, graphReady)
//	graph_project_coverage.go — run/project/session coverage projections
//	cli_graph.go — `tui-dash graph <op> --run RUN` dispatch (graphCmd)
//
// Observation vs owed duty stay separate: these projections never
// merge with the authoritative coverage-obligation service
// (coverage_*), which tracks what is owed.

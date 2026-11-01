package main

// mcp_read_tools.go: read-only MCP tools (no execution side
// effects). Extracted from mcp_tools.go; behavior unchanged.
//
// Per-domain implementations (pure code navigation, no authority
// changes); this file keeps the name so dispatcher references stay
// stable:
//   - sessions (metis_status, metis_agents): mcp_read_sessions.go
//   - tasks (metis_tasks, metis_task_status): mcp_read_tasks.go
//   - graph and events (metis_graph, metis_events): mcp_read_graph.go
//   - projects (metis_projects): mcp_read_projects.go
//   - approvals (metis_approvals): mcp_read_approvals.go

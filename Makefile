# Metis test gates.
#
# Gate ladder (cheapest first):
#   test-quick       ordinary edit loop: internal packages + critical
#                    boundaries and refresh/session invariants (~seconds).
#   test-fast        broad smoke qualification: -short mode (skips the
#                    measured integration set: daemon/host turns, fleet
#                    scale, perf characterization, timing/race stress,
#                    fresh-binary test) + all internal packages.
#   test-domain      one area after a coherent fix: AREA=<name> selects
#                    a -run pattern (see area map below).
#   test-integration affected workflows: builds a fresh binary from the
#                    exact tree first (TestIntegrationTmux staleness
#                    fix), then runs the full suite without -short.
#   test-full        integration checkpoints and release: full uncached
#                    suite, then the race subset.
#   test             legacy alias of test-full (release gate).
#
# Slow-test inventory: measured with
#   go test -json -count=1 -buildvcs=false ./...
#   (`go test -json` Elapsed per Test) and recorded in
#   test-inventory.json (test -> {owner file, seconds, deps}).
#   Regenerate after major suite changes. Tests measuring >=1.5s
#   that spin daemons/hosts/PTY/fleets, characterize perf, or are
#   timing/race stress carry `if testing.Short()` skips. Pure-logic
#   slow tests stay in every gate (their cost is fixture overhead, a
#   future optimization target, not an integration dependency).
# Never add t.Parallel: many tests share env and daemon state.
.PHONY: build test clean test-quick test-fast test-domain test-integration test-full test-inventory-check test-inventory-refresh

# -buildvcs=false is required on pop-os: plain `go build` fails there
# with "error obtaining VCS status: exit status 128", because Go
# tries to stamp VCS info and git reports the folder is not a git
# repository (exit 128). Disabling stamping changes nothing else
# about the binary; it just skips that stamp.
build:
	GOCACHE=/tmp/td-gocache go build -buildvcs=false -o metis .
	ln -sf metis tui-dash

test:
	$(MAKE) test-full

clean:
	rm -f metis tui-dash

test-quick:
	GOCACHE=/tmp/td-gocache go test -count=1 -buildvcs=false ./internal/tui ./internal/termview ./internal/musewire
	GOCACHE=/tmp/td-gocache go test -count=1 -buildvcs=false -run '^(TestBoundary.*|TestRefresh.*|TestRenderSelectionChangeHidesPreviousSession|TestMCPAgentsListsUnifiedSessions)$$' .

test-fast:
	GOCACHE=/tmp/td-gocache go test -short -count=1 -buildvcs=false ./...

# check is source-only and suitable for ordinary edits; refresh runs
# the full uncached suite to update measured durations.
test-inventory-check:
	python3 scripts/test_inventory.py check

test-inventory-refresh:
	python3 scripts/test_inventory.py refresh

# AREA selects one domain's -run pattern. Examples:
#   make test-domain AREA=sessions | provision | dispatch | tui |
#   muse | assurance | workspace | fleet | transport | terminal
test-domain:
	@: $${AREA:?usage: make test-domain AREA=<sessions|provision|dispatch|tui|muse|assurance|workspace|fleet|transport|terminal>}
	case "$(AREA)" in \
	  sessions)  PAT='Session|Registry|Pty|Attach|SendInput' ;; \
	  provision) PAT='Provision|BindRetry|MCPBind' ;; \
	  dispatch)  PAT='Dispatch|BlockedWorker|AdvDispatch|FinalizeRaces|ConcurrentDep' ;; \
	  tui)       PAT='Tui|TUI|Model|View|Render|Graph|Refresh|Boundary|Nav|Guide|Feed' ;; \
	  muse)      PAT='Muse|Turn|Steer|GuidanceNative|CmdStatus|Native' ;; \
	  assurance) PAT='Assur|Verdict|Verif|Coverage|Report|Judg|Receipt|Reconcile' ;; \
	  workspace) PAT='Workspace|ProjectRoot|Reserve|Workdir' ;; \
	  fleet)     PAT='Fleet|Worker|SixWorkers|TwelveWorker' ;; \
	  transport) PAT='MCP|Cli|Ops|Terminal|Doctor|Diagnose' ;; \
	  terminal)  PAT='Terminal|Pty|PTY|InputSender|Supervisor|Lease' ;; \
	  *) echo "unknown AREA $(AREA)"; exit 2 ;; \
	esac; \
	GOCACHE=/tmp/td-gocache go test -count=1 -buildvcs=false -run "$$PAT" .

# Fresh binary first: TestIntegrationTmux exercises ./metis, and a
# stale binary would silently qualify an older tree. Build the legacy
# command symlink too for existing scripts.
test-integration: build
	GOCACHE=/tmp/td-gocache go test -count=1 -buildvcs=false ./...

test-full: test-integration
	GOCACHE=/tmp/td-gocache go test -race -count=1 -buildvcs=false -run 'TestDispatchConcurrentCrossSessionOneWinnerNoOrphan|TestDispatchLateAdmissionRefusedBeforePrepare|TestMCPDepend|TestMCPProvision|TestCoverageRevisedEvidenceReopensGate' .

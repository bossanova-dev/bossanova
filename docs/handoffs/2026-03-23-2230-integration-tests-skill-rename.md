## Handoff: Flight Leg 9 — Integration Testing + Skill Renaming

**Date:** 2026-03-23 22:30
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed

- bossanova-ithj: Add integration tests for HostService workflow/attempt RPCs
- bossanova-wkux: Add WorkflowStore integration tests
- bossanova-snaz: Rename skills to boss- prefix and add Makefile build target
- bossanova-tqwr: [HANDOFF] Review Flight Leg 9

### Files Changed

- `services/bossd/internal/plugin/host_service_test.go` — Added 9 new tests: mockClaudeRunner, setupWorkflowTestServer helper (real SQLite + mock runner), CreateWorkflow, GetWorkflow, UpdateWorkflow, ListWorkflows (with status filter), WorkflowNilStore, CreateAttempt, GetAttemptStatus (running + completed), CreateAttemptNilRunner, CreateAttemptStartError, GetAttemptStatusUnknownSession
- `services/bossd/internal/db/workflow_store_test.go` — Added 3 new tests: FullLifecycle (pending→running→implement→resume→verify→land→completed with ListByStatus verification), ConcurrentAccess (20 goroutines doing parallel Get/List), FailureWithError (running→failed with last_error + list verification)
- `.claude/skills/` — Renamed 7 skill directories: pre-flight-checks→boss-plan, take-off→boss-implement, handoff-task→boss-handoff, resume-handoff→boss-resume, post-flight-checks→boss-verify, land-the-plane→boss-land, file-a-flight-plan→boss-flight-plan
- `.claude/skills/boss-*/SKILL.md` — Updated all cross-references from old skill names to new boss- prefixed names
- `Makefile:1-4,48-49,57-58,104-115` — Added build-autopilot phony target, added bossd-plugin-autopilot to build target, added $(BIN_DIR)/bossd-plugin-autopilot recipe and per-module build-autopilot target

### Learnings & Notes

- The HostService workflow tests use real in-memory SQLite with migrations (not mocks) for full integration coverage — the setupWorkflowTestServer helper creates repo + session for FK satisfaction
- The mockClaudeRunner implements the claude.ClaudeRunner interface with configurable start error and session tracking — useful pattern for testing workflow orchestration without real Claude processes
- Skill renaming was clean: no Go code references old skill names (they're configurable), only SKILL.md cross-references needed updating
- The autopilot plugin binary is ~18MB and builds in <2 seconds
- `wg.Go()` is the modern Go pattern replacing `wg.Add(1) + go func() { defer wg.Done()... }()`

### Issues Encountered

- None — all implementation followed existing patterns

### Next Steps (Flight Leg 10: Final Verification + End-to-End)

- bossanova-hrfy: Run full test suite, linter, and build all binaries
- bossanova-w7g6: Manual end-to-end smoke test of autopilot commands
- bossanova-0hgm: [HANDOFF] Final review

### Resume Command

To continue this work:

1. Run `bd ready` — should show bossanova-hrfy
2. Run `make test` to verify full suite
3. Run `make lint` to verify linting
4. Run `make build` to verify all binaries including autopilot plugin
5. Manual smoke test: build boss/bossd, run `boss autopilot --help`, verify subcommands

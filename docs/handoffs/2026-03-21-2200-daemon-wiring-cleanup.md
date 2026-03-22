## Handoff: Flight Leg 5 — Daemon Wiring + Cleanup

**Date:** 2026-03-21 22:00
**Branch:** task-source-plugin
**Flight ID:** fp-2026-03-21-phase3-dependabot-task-source-plugin
**Planning Doc:** docs/plans/2026-03-21-phase3-dependabot-task-source-plugin.md

### Tasks Completed This Flight Leg

- bossanova-pj85: Update MergePR to accept merge strategy parameter
- bossanova-0jkj: Wire orchestrator into daemon startup/shutdown in main.go
- bossanova-w88i: Remove in-daemon dependabot code (poller + dispatcher + events)

### Files Changed

- `lib/bossalib/vcs/provider.go:31-33` — MergePR interface now takes `strategy string` parameter
- `lib/bossalib/vcs/events.go` — Removed DependabotReady event type and its vcsEvent() marker
- `services/bossd/internal/vcs/github/provider.go:254-282` — MergePR implementation maps strategy ("rebase"/"squash"/"merge") to gh CLI flags, defaults to --rebase
- `services/bossd/cmd/main.go:81,118-124,176-177,216-219` — Added TaskMappingStore, SessionCreator, Orchestrator creation; starts with pollerCtx; shutdown cancels pollerCtx before pluginHost.Stop()
- `services/bossd/internal/taskorchestrator/orchestrator.go:127-131,397-398,498-502` — repoInfo now carries mergeStrategy; handleAutoMerge passes it to MergePR
- `services/bossd/internal/session/poller.go` — Removed DependabotAuthor constant, checkDependabotPRs() function, and its call in poll()
- `services/bossd/internal/session/dispatcher.go` — Removed DependabotReady dispatch case and handleDependabotReady() handler
- `services/bossd/internal/session/poller_test.go` — Removed 4 dependabot tests (TestPollerEmitsDependabotReady, SkipsDependabotWhenFlagDisabled, SkipsDependabotWithFailingChecks, SkipsNonDependabotPRs)
- `services/bossd/internal/session/dispatcher_test.go` — Removed 2 dependabot tests (DependabotReadyAutoMergeEnabled, DependabotReadyAutoMergeDisabled)
- `services/bossd/internal/testharness/mock_vcs.go:109` — Updated MergePR mock signature
- `services/bossd/internal/session/lifecycle_test.go:354` — Updated MergePR mock signature
- `services/bossd/internal/taskorchestrator/orchestrator_test.go:178` — Updated MergePR mock signature

### Implementation Notes

- **MergePR strategy mapping**: GitHub provider uses a switch on the strategy string to select `--rebase`, `--squash`, or `--merge` flag. Empty string defaults to `--rebase` for backward compatibility.
- **Orchestrator wiring order**: SessionCreator and Orchestrator are created after plugin host starts (needs dispensed interfaces). pollerCtx is cancelled before pluginHost.Stop() during shutdown to prevent the orchestrator from calling into dead plugin processes.
- **Dependabot code removal**: 329 lines deleted across 5 files. The `CanAutoMergeDependabot` repo flag is kept — the orchestrator uses it to filter eligible repos.
- **go mod tidy caveat**: Still does NOT work for this project. Use `go work sync` from workspace root.

### Current Status

- Tests: ALL PASS (22 taskorchestrator + 27 plugin + rest of daemon = all green)
- Lint: PASS (go vet)
- Build: PASS (all modules including plugin binary)

### Next Flight Leg (Flight Leg 6: Additional Tests)

- bossanova-3msg: Write plugin server_test.go (12 PollTasks classification tests)
- bossanova-d94c: Write task_mapping_store_test.go (5 CRUD tests)
- bossanova-6ti1: Write host_service_test.go (4 VCS proxy tests)
- bossanova-q8u6: Write host_test.go GetTaskSources tests (3 tests)
- bossanova-hody: [HANDOFF] Review Flight Leg 6

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-21-phase3-dependabot-task-source-plugin"` to see available tasks
2. Review files: `services/bossd/internal/plugin/host.go`, `services/bossd/internal/plugin/host_service.go`, `services/bossd/internal/db/task_mapping_store.go`, `plugins/bossd-plugin-dependabot/server.go`

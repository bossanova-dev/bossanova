## Handoff: Flight Leg 4 — SessionCreator + Task Orchestrator

**Date:** 2026-03-21 21:00
**Branch:** task-source-plugin
**Flight ID:** fp-2026-03-21-phase3-dependabot-task-source-plugin
**Planning Doc:** docs/plans/2026-03-21-phase3-dependabot-task-source-plugin.md

### Tasks Completed This Flight Leg

- bossanova-btlb: Create SessionCreator interface + implementation
- bossanova-c9ag: Create Task Orchestrator core struct + poll loop
- bossanova-at2a: Implement orchestrator task routing (AUTO_MERGE, CREATE_SESSION, NOTIFY_USER)
- bossanova-0xik: Implement per-repo FIFO queue + event bus completion callback

### Files Changed

- `services/bossd/internal/taskorchestrator/session_creator.go` — NEW: SessionCreator interface + lifecycleSessionCreator (wraps db.SessionStore.Create + Lifecycle.StartSession). Uses SessionStarter interface for testability.
- `services/bossd/internal/taskorchestrator/session_creator_test.go` — NEW: 4 tests (success, no-head-branch, create error, start error)
- `services/bossd/internal/taskorchestrator/orchestrator.go` — NEW: Task Orchestrator with staggered poll loop, task routing (AUTO_MERGE/CREATE_SESSION/NOTIFY_USER), per-repo FIFO queue, HandleSessionCompleted callback, RetryPendingUpdates. Uses TaskSourceProvider, SessionCreator, vcs.Provider interfaces.
- `services/bossd/internal/taskorchestrator/orchestrator_test.go` — NEW: 18 tests covering poll filtering, dedup, all routing actions, queue ordering, completion callback, pending update storage
- `services/bossd/internal/db/store.go` — Added GetBySessionID to TaskMappingStore interface
- `services/bossd/internal/db/task_mapping_store.go` — Added GetBySessionID SQLite implementation

### Implementation Notes

- **SessionStarter interface**: Created to allow testing lifecycleSessionCreator without a real session.Lifecycle. The real `*session.Lifecycle` satisfies this interface implicitly.
- **TaskSourceProvider interface**: Wraps `plugin.Host.GetTaskSources()` for testability. The real Host satisfies this interface.
- **Per-repo FIFO queue**: Uses sync.Mutex-protected maps (`queues` and `active`). AUTO_MERGE and NOTIFY_USER dequeue synchronously (via `defer o.dequeueNext`). CREATE_SESSION dequeues asynchronously via `HandleSessionCompleted`.
- **Completion callback**: `HandleSessionCompleted(ctx, sessionID, outcome)` looks up task mapping by session ID, calls `plugin.UpdateTaskStatus`, stores pending update on failure. `RetryPendingUpdates` runs at poll start.
- **PR number extraction**: `parsePRNumberFromExternalID` parses the last colon-delimited segment as an int. Works for dependabot's `dependabot:pr:<repoURL>:<prNumber>` format.
- **UNSPECIFIED action**: Treated as CREATE_SESSION per proto spec.
- **go mod tidy caveat**: Still does NOT work for this project. Use `go work sync` from workspace root.

### Current Status

- Tests: ALL PASS (22 taskorchestrator + 27 plugin + rest of daemon = all green)
- Lint: PASS (go vet)
- Build: PASS (all modules)

### Next Flight Leg

- bossanova-0jkj: Wire orchestrator into daemon startup/shutdown in main.go
- bossanova-pj85: Update MergePR to accept merge strategy parameter
- bossanova-w88i: Remove in-daemon dependabot code (poller + dispatcher + events)
- bossanova-cvdw: [HANDOFF] Review Flight Leg 5

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-21-phase3-dependabot-task-source-plugin"` to see available tasks
2. Review files: `services/bossd/internal/taskorchestrator/orchestrator.go`, `services/bossd/cmd/main.go`, `services/bossd/internal/session/poller.go`

# Phase 3: Dependabot Task Source Plugin — Implementation Plan

**Design Doc:** `~/.gstack/projects/recurser-bossanova/dave-task-source-plugin-design-20260321-120000.md`
**Eng Review:** 2026-03-21, CLEARED (9 issues resolved, 1 critical gap noted)
**Branch:** task-source-plugin

## Overview

Build the first real plugin binary (dependabot task source) that proves the end-to-end plugin architecture. The plugin detects dependabot PRs via the daemon's HostService, classifies them (auto-merge vs fix-required vs previously-rejected), and the daemon's Task Orchestrator routes them to the appropriate action.

This is the "Holy Shit Demo": dependabot bumps a dependency → tests break → Claude fixes it → PR goes green → auto-merges. All while you sleep.

## Architecture Diagram

```
                    PLUGIN BINARY (subprocess)
                    bossd-plugin-dependabot
  ┌──────────────────────────────────────────────────┐
  │                                                    │
  │  TaskSourceService gRPC Server                     │
  │  ├─ GetInfo()                                      │
  │  ├─ PollTasks(repo)                                │
  │  │   ├─ HostService.ListOpenPRs(repo)  ──┐        │
  │  │   ├─ HostService.GetCheckResults()  ──┤ call    │
  │  │   ├─ HostService.GetPRStatus()      ──┤ back    │
  │  │   ├─ History check (closed PRs)       │        │
  │  │   └─ Classify → TaskAction enum       │        │
  │  └─ UpdateTaskStatus(id, status)         │        │
  │                                          │        │
  └──────────────────────────────────────────┼────────┘
                                             │ gRPC (bidirectional
                                             │ via go-plugin broker)
  ┌──────────────────────────────────────────┼────────┐
  │  BOSSD DAEMON                            │        │
  │                                          ▼        │
  │  ┌───────────────────┐    ┌──────────────────┐   │
  │  │ Plugin Host        │    │ HostService      │   │
  │  │ (enhanced)         │    │ (NEW gRPC server)│   │
  │  │ GetTaskSources()   │    │ ListOpenPRs()    │   │
  │  │ cached interfaces  │    │ GetCheckResults()│   │
  │  └────────┬──────────┘    │ GetPRStatus()    │   │
  │           │               └──────────────────┘   │
  │           │                                       │
  │  ┌────────▼──────────────────────────────────┐   │
  │  │ Task Orchestrator (NEW)                    │   │
  │  │                                            │   │
  │  │ Poll loop (staggered per repo):            │   │
  │  │   for each repo (spread across interval):  │   │
  │  │     tasks = plugin.PollTasks(repo)          │   │
  │  │     for each task:                          │   │
  │  │       dedup via task_mappings table         │   │
  │  │       enqueue to per-repo FIFO              │   │
  │  │                                            │   │
  │  │ Per-repo queue processor:                  │   │
  │  │   ┌──────────┬──────────┬────────────┐    │   │
  │  │   │AUTO_MERGE│CREATE_   │NOTIFY_USER │    │   │
  │  │   │          │SESSION   │            │    │   │
  │  │   ▼          ▼          ▼            │    │   │
  │  │ MergePR   SessionCreator  Log/skip   │    │   │
  │  │ (strategy) (Lifecycle)               │    │   │
  │  │   │          │                       │    │   │
  │  │   │          ▼                       │    │   │
  │  │   │   Existing pipeline:             │    │   │
  │  │   │   Poller→Dispatcher→FixLoop      │    │   │
  │  │   │          │                       │    │   │
  │  │   └──────────┼───────────────────────┘    │   │
  │  │              │                             │   │
  │  │ Event bus callback (session completion):   │   │
  │  │   Merged  → UpdateTaskStatus(COMPLETED)    │   │
  │  │   Blocked → UpdateTaskStatus(FAILED)       │   │
  │  │   Failed  → store pending, retry on next   │   │
  │  └───────────────────────────────────────────┘   │
  │                                                   │
  │  ┌────────────────────────┐                      │
  │  │ task_mappings (SQLite)  │                      │
  │  │ external_id → session  │                      │
  │  │ dedup + pending status │                      │
  │  └────────────────────────┘                      │
  └───────────────────────────────────────────────────┘
```

## Affected Areas

### Proto Changes

- [ ] `proto/bossanova/v1/plugin.proto` — Add `TaskAction` enum, add `action` field to `TaskItem`
- [ ] `proto/bossanova/v1/host_service.proto` — NEW: HostService with ListOpenPRs, GetCheckResults, GetPRStatus
- [ ] `lib/bossalib/gen/` — Regenerated protobuf code

### Shared Library (bossalib)

- [ ] `lib/bossalib/plugin/shared.go` — NEW: Move handshake config + PluginMap from bossd/internal/plugin
- [ ] `lib/bossalib/models/models.go` — Add `MergeStrategy` field to Repo

### Plugin Binary (NEW Go module)

- [ ] `plugins/bossd-plugin-dependabot/go.mod` — NEW module
- [ ] `plugins/bossd-plugin-dependabot/main.go` — Plugin entry point, go-plugin.Serve()
- [ ] `plugins/bossd-plugin-dependabot/server.go` — TaskSourceService gRPC server implementation
- [ ] `plugins/bossd-plugin-dependabot/github.go` — HostService client calls for GitHub data
- [ ] `plugins/bossd-plugin-dependabot/history.go` — Previously-rejected PR detection
- [ ] `go.work` — Add `./plugins/bossd-plugin-dependabot` to workspace

### Daemon (bossd)

- [ ] `services/bossd/internal/plugin/host.go` — Add GetTaskSources() with cached dispensed interfaces
- [ ] `services/bossd/internal/plugin/shared.go` — Import handshake from bossalib (remove local copy)
- [ ] `services/bossd/internal/plugin/host_service.go` — NEW: HostService gRPC server wrapping vcs.Provider
- [ ] `services/bossd/internal/taskorchestrator/orchestrator.go` — NEW: Task orchestrator (poll + route + queue)
- [ ] `services/bossd/internal/taskorchestrator/session_creator.go` — NEW: SessionCreator interface
- [ ] `services/bossd/internal/db/task_mapping_store.go` — NEW: TaskMappingStore
- [ ] `services/bossd/migrations/20260321170000_task_mappings.sql` — NEW migration
- [ ] `services/bossd/migrations/20260321170001_repo_merge_strategy.sql` — NEW migration
- [ ] `services/bossd/internal/vcs/github/provider.go` — MergePR() accepts strategy parameter
- [ ] `services/bossd/cmd/main.go` — Wire orchestrator into daemon startup/shutdown

### Removals

- [ ] `services/bossd/internal/session/poller.go` — Remove `checkDependabotPRs()` and its call in `poll()`
- [ ] `services/bossd/internal/session/dispatcher.go` — Remove `handleDependabotReady()`
- [ ] `lib/bossalib/vcs/events.go` — Remove `DependabotReady` event type
- [ ] `services/bossd/internal/session/poller.go` — Remove `DependabotAuthor` constant

### Tests (~49 total)

- [ ] `plugins/bossd-plugin-dependabot/server_test.go` — 12 tests: PollTasks classification for all PR states
- [ ] `services/bossd/internal/taskorchestrator/orchestrator_test.go` — 20 tests: routing, dedup, queue, retry
- [ ] `services/bossd/internal/db/task_mapping_store_test.go` — 5 tests: CRUD + unique constraint
- [ ] `services/bossd/internal/taskorchestrator/session_creator_test.go` — 4 tests: create + error cases
- [ ] `services/bossd/internal/plugin/host_test.go` — 3 additional tests: GetTaskSources
- [ ] `services/bossd/internal/plugin/host_service_test.go` — 4 tests: VCS proxy
- [ ] Integration test: launch plugin binary, call PollTasks over gRPC — 1 test

## Design References

- Plugin host: `services/bossd/internal/plugin/host.go`
- gRPC bridges: `services/bossd/internal/plugin/grpc_plugins.go`
- Event bus: `services/bossd/internal/plugin/eventbus/eventbus.go`
- Session lifecycle: `services/bossd/internal/session/lifecycle.go` (StartSession with existingBranch)
- Existing dependabot code: `services/bossd/internal/session/poller.go:179-237`
- VCS provider: `services/bossd/internal/vcs/github/provider.go`
- Config: `lib/bossalib/config/config.go`
- Daemon wiring: `services/bossd/cmd/main.go`
- go-plugin bidirectional: https://github.com/hashicorp/go-plugin/tree/main/examples/bidirectional

---

## Resolved Engineering Decisions

| #   | Decision                    | Choice                                         | Rationale                                                                    |
| --- | --------------------------- | ---------------------------------------------- | ---------------------------------------------------------------------------- |
| 1   | Host plugin type retrieval  | Cache dispensed interfaces at startup          | Avoid gRPC connection churn per poll; go-plugin best practice                |
| 2   | Polling architecture        | Two loops + event bus for completion callbacks | Validates event bus; reactive UpdateTaskStatus without polling session state |
| 3   | Session creation API        | New SessionCreator interface                   | Prepares for remote-control orchestrator (future)                            |
| 4   | GitHub access from plugin   | Daemon proxy via HostService                   | Multi-provider abstraction from day 1 (GitLab, Bitbucket later)              |
| 5   | Module layout for handshake | Move to bossalib                               | DRY — handshake is the contract; single source of truth                      |
| 6   | Plugin crash handling       | Retry with backoff + store pending             | Prevents data loss on plugin crash; critical for Linear/Jira future          |
| 7   | Task routing mechanism      | TaskAction proto enum                          | Type-safe routing; explicit > stringly-typed labels                          |
| 8   | Merge strategy              | Repo-level field, default rebase               | Different repos may need different strategies regardless of plugin           |
| 9   | Poll scheduling             | Stagger per repo across interval               | Reduces GitHub API burst; 5 repos = poll one every 12s                       |

## Proto Changes Detail

### TaskAction enum (add to plugin.proto)

```protobuf
enum TaskAction {
  TASK_ACTION_UNSPECIFIED = 0;    // Daemon treats as CREATE_SESSION
  TASK_ACTION_AUTO_MERGE = 1;     // Merge PR directly (no Claude session)
  TASK_ACTION_CREATE_SESSION = 2; // Create Claude Code session with plan
  TASK_ACTION_NOTIFY_USER = 3;    // Skip action, notify user (e.g. previously-rejected)
}

message TaskItem {
  // ... existing fields ...
  TaskAction action = 8;  // NEW: what the daemon should do with this task
}
```

### HostService (new proto file)

```protobuf
service HostService {
  rpc ListOpenPRs(ListOpenPRsRequest) returns (ListOpenPRsResponse);
  rpc GetCheckResults(GetCheckResultsRequest) returns (GetCheckResultsResponse);
  rpc GetPRStatus(GetPRStatusRequest) returns (GetPRStatusResponse);
}
```

## Task Mapping Schema

```sql
-- +goose Up
CREATE TABLE task_mappings (
    id            TEXT PRIMARY KEY,
    external_id   TEXT NOT NULL UNIQUE,
    plugin_name   TEXT NOT NULL,
    session_id    TEXT,
    repo_id       TEXT NOT NULL,
    status        INTEGER NOT NULL DEFAULT 0,
    pending_update_status  INTEGER,
    pending_update_details TEXT,
    created_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at    TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    FOREIGN KEY (session_id) REFERENCES sessions(id),
    FOREIGN KEY (repo_id) REFERENCES repos(id)
);

-- +goose Down
DROP TABLE task_mappings;
```

`pending_update_status` and `pending_update_details` are non-null when a status update failed (plugin crash) and needs retry.

## Repo Merge Strategy Schema

```sql
-- +goose Up
ALTER TABLE repos ADD COLUMN merge_strategy TEXT NOT NULL DEFAULT 'rebase';

-- +goose Down
-- SQLite doesn't support DROP COLUMN in older versions; this is a no-op for safety.
```

## Implementation Order

1. **Proto changes** — TaskAction enum, HostService proto, regenerate
2. **Shared library** — Move handshake to bossalib, add MergeStrategy to Repo model
3. **Task mapping store** — SQLite migration + CRUD
4. **HostService server** — gRPC server wrapping vcs.Provider
5. **Plugin host enhancement** — GetTaskSources() with cached interfaces
6. **Plugin binary** — main.go, server.go, github.go, history.go
7. **SessionCreator** — Interface wrapping sessions.Create + Lifecycle.StartSession
8. **Task orchestrator** — Poll loop, routing, queue, event bus callback, retry
9. **Daemon wiring** — Wire orchestrator into main.go startup/shutdown
10. **MergePR strategy** — Update provider to accept strategy parameter
11. **Remove in-daemon dependabot code** — Clean up poller/dispatcher/events
12. **Tests** — All 49 tests across plugin, orchestrator, stores, host

## In Scope (Phase 3)

- Plugin binary implementing TaskSourceService
- HostService for VCS proxy (3 RPCs)
- Task Orchestrator with poll/route/queue/retry
- Task mapping store for dedup
- SessionCreator interface
- Dependabot history intelligence (previously-rejected detection)
- Per-repo merge strategy
- Full test coverage (49 tests)
- Remove in-daemon dependabot code

## NOT In Scope (deferred to TODOS.md)

- TUI notification/toast system (daemon logs for now)
- License gating for paid plugins
- Dependabot blacklist UX (.github/dependabot.yml editing)
- HostService expansion beyond VCS reads (CreateSession, ListRepos, etc.)
- Dynamic plugin hot-reload
- EventSource streaming integration

## Success Criteria

1. Plugin binary launches via go-plugin handshake and passes health checks
2. PollTasks classifies dependabot PRs correctly (auto-merge / fix-required / rejected)
3. HostService proxies VCS calls from plugin to daemon's GitHub provider
4. Green dependabot PRs auto-merge with configurable strategy
5. Failing dependabot PRs create Claude Code sessions with fix plans
6. Task dedup prevents duplicate sessions
7. UpdateTaskStatus reports outcomes; pending updates retry on plugin recovery
8. Previously-rejected libraries detected and skipped
9. In-daemon dependabot code removed
10. 49 tests passing

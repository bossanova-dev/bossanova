## Handoff: Flight Leg 1 — Proto Changes & Shared Library

**Date:** 2026-03-21 13:00
**Branch:** task-source-plugin
**Flight ID:** fp-2026-03-21-phase3-dependabot-task-source-plugin
**Planning Doc:** docs/plans/2026-03-21-phase3-dependabot-task-source-plugin.md

### Tasks Completed This Flight Leg

- bossanova-y46v: Add TaskAction enum and action field to TaskItem in plugin.proto
- bossanova-ogbe: Create host_service.proto with HostService RPCs
- bossanova-px7n: Regenerate protobuf Go code
- bossanova-2g7x: Move handshake config + PluginMap to lib/bossalib/plugin/shared.go
- bossanova-u0kh: Add MergeStrategy field to Repo model + migration

### Files Changed

- `proto/bossanova/v1/plugin.proto` — Added TaskAction enum (AUTO_MERGE, CREATE_SESSION, NOTIFY_USER), action field (8) and existing_branch field (9) to TaskItem
- `proto/bossanova/v1/host_service.proto` — NEW: HostService with ListOpenPRs, GetCheckResults, GetPRStatus RPCs
- `proto/bossanova/v1/models.proto` — Added author field (5) to PRSummary message
- `lib/bossalib/gen/bossanova/v1/plugin.pb.go` — Regenerated
- `lib/bossalib/gen/bossanova/v1/models.pb.go` — Regenerated
- `lib/bossalib/gen/bossanova/v1/host_service.pb.go` — NEW: Generated Go code for HostService
- `lib/bossalib/gen/bossanova/v1/bossanovav1connect/host_service.connect.go` — NEW: Connect-Go client/handler
- `lib/bossalib/plugin/shared.go` — NEW: Handshake constants + plugin type name constants (no go-plugin dependency)
- `lib/bossalib/models/models.go` — Added MergeStrategy type (rebase/merge/squash) and field to Repo
- `services/bossd/internal/plugin/shared.go` — Now imports handshake values from bossalib/plugin
- `services/bossd/internal/db/repo_store.go` — Updated all queries + scanRepo for merge_strategy column
- `services/bossd/internal/db/store.go` — Added MergeStrategy to UpdateRepoParams
- `services/bossd/migrations/20260321170001_repo_merge_strategy.sql` — NEW: Adds merge_strategy column

### Implementation Notes

- Shared plugin constants in bossalib intentionally avoid importing go-plugin (would add heavy transitive deps). Instead, raw constants are exported and each side constructs their own HandshakeConfig.
- PRSummary proto now has `author` field (5) — needed by dependabot plugin to filter by `dependabot[bot]` author.
- TaskItem got `existing_branch` field (9) for dependabot PR branches that already exist.
- MergeStrategy defaults to "rebase" in both migration and Create method.

### Current Status

- Tests: ALL PASS (12 test packages)
- Lint: PASS (buf lint + go vet)
- Build: PASS (bossalib + bossd)

### Next Flight Leg

- bossanova-ej6q: Create task_mappings migration + TaskMappingStore interface
- bossanova-o4aw: Implement SQLiteTaskMappingStore
- bossanova-naf1: Create HostService gRPC server wrapping vcs.Provider
- bossanova-i9bu: Add GetTaskSources() to plugin Host with cached interfaces
- bossanova-9bny: [HANDOFF] Review Flight Leg 2

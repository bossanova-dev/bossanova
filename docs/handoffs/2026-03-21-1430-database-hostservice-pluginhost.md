## Handoff: Flight Leg 2 — Database + HostService + Plugin Host

**Date:** 2026-03-21 14:30
**Branch:** task-source-plugin
**Flight ID:** fp-2026-03-21-phase3-dependabot-task-source-plugin
**Planning Doc:** docs/plans/2026-03-21-phase3-dependabot-task-source-plugin.md

### Tasks Completed This Flight Leg

- bossanova-ej6q: Create task_mappings migration + TaskMappingStore interface
- bossanova-o4aw: Implement SQLiteTaskMappingStore
- bossanova-naf1: Create HostService gRPC server wrapping vcs.Provider
- bossanova-i9bu: Add GetTaskSources() to plugin Host with cached interfaces

### Files Changed

- `lib/bossalib/models/models.go` — Added TaskMappingStatus enum and TaskMapping model struct
- `services/bossd/internal/db/store.go` — Added CreateTaskMappingParams, UpdateTaskMappingParams, and TaskMappingStore interface
- `services/bossd/internal/db/task_mapping_store.go` — NEW: SQLiteTaskMappingStore with Create, GetByExternalID, Update, ListPending
- `services/bossd/internal/plugin/host_service.go` — NEW: HostServiceServer wrapping vcs.Provider with manual gRPC service descriptor
- `services/bossd/internal/plugin/host.go` — Added taskSource field to managedPlugin, dispense at startup, GetTaskSources() method
- `services/bossd/migrations/20260321170000_task_mappings.sql` — NEW: task_mappings table with external_id UNIQUE, FK to sessions and repos

### Implementation Notes

- HostServiceServer uses a manually-built `grpc.ServiceDesc` because the project generates connect-go code (not protoc-gen-go-grpc). The service descriptor defines handlers for ListOpenPRs, GetCheckResults, GetPRStatus that decode protobuf requests and delegate to the server methods.
- TaskMappingStore uses double-pointer pattern (`**string`, `**models.TaskMappingStatus`) for nullable fields in UpdateTaskMappingParams, matching the existing SessionStore/RepoStore convention.
- GetTaskSources() dispenses TaskSource interfaces once at startup and caches them on `managedPlugin.taskSource`. Non-TaskSource plugins silently skip dispensing.
- VCS domain type → proto enum conversion uses explicit switch statements rather than numeric casting, keeping the mapping clear and maintainable.
- TaskMappingStatus enum: Pending(0), InProgress(1), Completed(2), Failed(3), Skipped(4).

### Current Status

- Tests: ALL PASS (12 test packages in bossd, 7 in bossalib)
- Lint: PASS (go vet)
- Build: PASS (bossalib + bossd)

### Next Flight Leg

- bossanova-hldq: Create plugin module + main.go entry point
- bossanova-rkod: Implement plugin github.go (HostService client wrapper)
- bossanova-9e34: Implement plugin history.go (previously-rejected PR detection)
- bossanova-vgyp: Implement plugin server.go (TaskSourceService gRPC server)
- bossanova-q165: [HANDOFF] Review Flight Leg 3

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-21-phase3-dependabot-task-source-plugin"` to see available tasks
2. Review files: `services/bossd/internal/plugin/host_service.go`, `services/bossd/internal/plugin/grpc_plugins.go`, `lib/bossalib/plugin/shared.go`

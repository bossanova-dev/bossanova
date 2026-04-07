## Handoff: Flight Leg 1 - Proto Definitions + Code Generation

**Date:** 2026-04-07 12:36 JST
**Branch:** add-a-plugin-for-integrating-with-linear
**Flight ID:** fp-2026-04-07-linear-integration-plugin
**Planning Doc:** docs/plans/2026-04-07-linear-integration-plugin.md
**bd Issues Completed:** None (plan created, no bd tasks closed yet)

### Tasks Completed

- ✅ Created comprehensive Linear integration plugin plan with 6 flight legs
- ✅ Added `TrackerIssue` message to `proto/bossanova/v1/models.proto`
- ✅ Added `linear_api_key` and `linear_team_key` fields to `Repo` message
- ✅ Added `ListAvailableIssues` RPC to `TaskSourceService` in `plugin.proto`
- ✅ Added `ListTrackerIssues` RPC to `daemon.proto`
- ✅ Added `branch_name` field to `CreateSessionRequest` in `daemon.proto`
- ✅ Ran `make generate` to regenerate Go code from protos

### Files Changed

- `docs/plans/2026-04-07-linear-integration-plugin.md:1-582` - Created comprehensive flight plan with 6 legs
- `proto/bossanova/v1/models.proto:233-242` - Added TrackerIssue message (fields 1-8)
- `proto/bossanova/v1/models.proto:219-220` - Added linear_api_key (field 15) and linear_team_key (field 16) to Repo
- `proto/bossanova/v1/plugin.proto:21` - Added ListAvailableIssues RPC to TaskSourceService
- `proto/bossanova/v1/plugin.proto:133-141` - Added ListAvailableIssuesRequest/Response messages
- `proto/bossanova/v1/daemon.proto` - Added ListTrackerIssues RPC, updated UpdateRepoRequest with fields 9-10, added branch_name to CreateSessionRequest
- `lib/bossalib/gen/bossanova/v1/*.pb.go` - Regenerated protobuf Go code
- `lib/bossalib/gen/bossanova/v1/bossanovav1connect/*.connect.go` - Regenerated connect-go handlers
- `services/boss/internal/tuitest/mock_daemon.go` - Updated mock to include new RPC methods
- `TODOS.md` - Added Linear plugin TODO entry

### Learnings & Notes

- **Proto field numbers locked**: Repo fields 15-16, UpdateRepoRequest fields 9-10, TrackerIssue fields 1-8, CreateSessionRequest field 8
- **TrackerIssue design**: Includes both suggested branch name and existing PR/branch detection fields for reconciliation
- **Auth format**: Linear uses `Authorization: <api_key>` (no "Bearer" prefix for personal API keys)
- **Plugin pattern**: Following existing `bossd-plugin-dependabot` structure with eager host client dial
- **PR matching strategy**: Primary match on branch name, fallback to `[ENG-123]` title tag
- **Session creation**: Using Linear's suggested branch name via new `branch_name` field in CreateSessionRequest
- **Code generation**: `make generate` successfully regenerated all protobuf code with new messages and RPCs

### Issues Encountered

- None - proto definitions and code generation completed cleanly

### Next Steps (Flight Leg 2: Database, Store, Daemon Handler, Plugin Interface)

Per the plan, the next flight leg involves:

1. Add SQLite migration for `linear_api_key` and `linear_team_key` columns
2. Add fields to `models.Repo` struct in Go
3. Update `repo_store.go` (scan, create, update)
4. Update daemon `UpdateRepo` handler to pass through new fields
5. Extend `TaskSource` interface with `ListAvailableIssues` method
6. Add `ListTrackerIssues` daemon handler
7. Add `ListTrackerIssues` to `BossClient` interface and `LocalClient`

Critical files to review:

- `services/bossd/migrations/` - for migration pattern
- `lib/bossalib/models/models.go` - Repo struct definition
- `services/bossd/internal/db/repo_store.go` - store implementation
- `services/bossd/internal/server/server.go` - daemon handlers
- `services/bossd/internal/plugin/grpc_plugins.go` - TaskSource interface
- `services/boss/internal/client/client.go` and `local.go` - client interface

### Resume Command

To continue this work:

1. Run `bd ready` to see available tasks for this flight (or create tasks with `/boss-create-tasks`)
2. Review proto changes: `proto/bossanova/v1/models.proto`, `proto/bossanova/v1/daemon.proto`, `proto/bossanova/v1/plugin.proto`
3. Review generated code: `lib/bossalib/gen/bossanova/v1/models.pb.go`, `lib/bossalib/gen/bossanova/v1/daemon.pb.go`
4. Review plan for Flight Leg 2: `docs/plans/2026-04-07-linear-integration-plugin.md` (lines 124-209)

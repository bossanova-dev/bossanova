## Handoff: Flight Leg 1 - Proto Definitions + Code Generation

**Date:** 2026-04-07 12:49 UTC
**Branch:** add-a-plugin-for-integrating-with-linear
**Flight ID:** fp-2026-04-07-linear-integration-plugin
**Planning Doc:** docs/plans/2026-04-07-linear-integration-plugin.md
**bd Issues Completed:** (see tasks below)

### Tasks Completed

This flight leg completed all proto definition tasks from the plan:

- Added `TrackerIssue` message to `proto/bossanova/v1/models.proto`
- Added `linear_api_key` and `linear_team_key` fields to `Repo` message (fields 15-16)
- Added `ListAvailableIssues` RPC to `TaskSourceService` in `plugin.proto`
- Added `ListTrackerIssues` RPC to `DaemonService` in `daemon.proto`
- Added `linear_api_key` and `linear_team_key` to `UpdateRepoRequest` (fields 9-10)
- Added optional `branch_name` field to `CreateSessionRequest` (field 8)
- Ran `make generate` to regenerate Go code from protos

### Files Changed

- `proto/bossanova/v1/models.proto:232-247` - Added TrackerIssue message (8 fields)
- `proto/bossanova/v1/models.proto:63-64` - Added linear_api_key and linear_team_key to Repo (fields 15-16)
- `proto/bossanova/v1/plugin.proto:20-21` - Added ListAvailableIssues RPC to TaskSourceService
- `proto/bossanova/v1/plugin.proto:132-141` - Added ListAvailableIssuesRequest/Response messages
- `proto/bossanova/v1/daemon.proto:45-46` - Added ListTrackerIssues RPC to DaemonService
- `proto/bossanova/v1/daemon.proto:9-10` - Added linear_api_key/linear_team_key to UpdateRepoRequest
- `proto/bossanova/v1/daemon.proto:8` - Added optional branch_name to CreateSessionRequest
- `proto/bossanova/v1/daemon.proto:313-320` - Added ListTrackerIssuesRequest/Response messages
- `lib/bossalib/gen/bossanova/v1/*.pb.go` - Regenerated Go code (all proto changes reflected)
- `lib/bossalib/gen/bossanova/v1/bossanovav1connect/*.connect.go` - Regenerated Connect RPC handlers
- `services/bossd/internal/plugin/grpc_plugins.go:84-97` - Extended TaskSource interface with ListAvailableIssues
- `services/bossd/internal/plugin/grpc_plugins.go:136-146` - Added gRPC client implementation for ListAvailableIssues
- `services/bossd/internal/plugin/integration_test.go:43` - Added ListAvailableIssues to taskSourceGRPCClientWrapper
- `services/bossd/internal/server/server.go:236-267` - Added ListTrackerIssues daemon handler
- `services/boss/internal/client/client.go:23` - Added ListTrackerIssues to BossClient interface
- `services/boss/internal/client/local.go:117-124` - Implemented ListTrackerIssues in LocalClient
- `services/boss/internal/client/remote.go:115-122` - Implemented ListTrackerIssues in RemoteClient
- `services/boss/internal/views/newsession_test.go:29` - Added ListTrackerIssues to stubClient

### Learnings & Notes

- Proto field numbers locked: Repo 15-16, UpdateRepoRequest 9-10, CreateSessionRequest 8, TrackerIssue 1-8
- TrackerIssue includes `pr_number` and `existing_branch` for matching existing PRs to Linear tickets
- ListTrackerIssues handler validates LinearAPIKey is set before calling plugin
- TaskSource interface extension maintains backward compatibility (new method only)
- All generated code follows existing patterns from buf generate

### Issues Encountered

None - proto definitions and code generation completed cleanly.

### Post-Flight Verification

✅ **Quality Gates:**

- `make format`: PASSED (no changes)
- `make test`: PASSED (all tests green)

✅ **Spec Verification:**

- `buf lint`: PASSED
- TrackerIssue message generated with all 8 fields
- ListAvailableIssues RPC added to plugin.proto
- ListTrackerIssues RPC added to daemon.proto
- Repo has LinearApiKey/LinearTeamKey fields
- All modules compile (bossalib, bossd, boss)
- No regressions in existing tests

### Next Steps (Flight Leg 2: Database, Store, Daemon Handler, Plugin Interface)

The next flight leg will implement:

- SQLite migration for linear_api_key and linear_team_key columns
- Update RepoStore to scan/create/update new columns
- Update daemon UpdateRepo handler
- Extend TaskSource plugin interface
- Add ListTrackerIssues daemon handler
- Add ListTrackerIssues to BossClient interface

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-04-07-linear-integration-plugin"` - should show next available task
2. Review files: `proto/bossanova/v1/*.proto`, `lib/bossalib/gen/bossanova/v1/*.pb.go`

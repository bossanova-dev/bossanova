## Handoff: Flight Leg 1 — Proto Definitions + Code Generation

**Date:** 2026-03-23 15:30
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed This Flight Leg

- bossanova-q0iu: Add PluginTypeWorkflow constant to shared.go
- bossanova-hwzi: Add WorkflowService to plugin.proto
- bossanova-80w7: Add workflow/attempt RPCs to host_service.proto
- bossanova-t3iw: Add autopilot RPCs to daemon.proto and models to models.proto
- bossanova-qb6z: Run make generate and verify proto compilation

### Files Changed

- `lib/bossalib/plugin/shared.go:17` — Added `PluginTypeWorkflow = "workflow"` constant
- `proto/bossanova/v1/plugin.proto:248-340` — New `WorkflowService` with 6 RPCs + request/response messages + `WorkflowStatusInfo`
- `proto/bossanova/v1/host_service.proto:25-135` — 7 new RPCs on `HostService` (CreateWorkflow, UpdateWorkflow, GetWorkflow, ListWorkflows, CreateAttempt, GetAttemptStatus, StreamAttemptOutput) + messages + `Workflow` model + `AttemptRunStatus` enum
- `proto/bossanova/v1/daemon.proto:55-142` — 7 new RPCs on `DaemonService` (Start/Pause/Resume/Cancel/GetStatus/List/StreamOutput) + messages + `AutopilotWorkflow` model
- `proto/bossanova/v1/models.proto:90-110` — `WorkflowStatus` enum (7 values) + `WorkflowStep` enum (7 values)
- `lib/bossalib/gen/bossanova/v1/` — All generated Go + ConnectRPC code updated
- `docs/designs/autopilot-plugin.md` — Design doc committed
- `docs/plans/2026-03-23-1514-autopilot-plugin.md` — Implementation plan committed

### Implementation Notes

- `plugin.proto` needed an explicit `import "bossanova/v1/models.proto"` to reference `WorkflowStatus` and `WorkflowStep` enums (was missing on first generate attempt)
- `Workflow` message in host_service.proto uses string types for status/step/created_at/updated_at (matching the DB layer pattern where new models use string types, not enums)
- `AutopilotWorkflow` message in daemon.proto uses proper enums (`WorkflowStatus`, `WorkflowStep`) since it's the API-facing model
- `StreamAttemptOutput` is a server-streaming RPC from host to plugin (different from ConnectRPC streaming used for CLI-to-daemon)
- `StreamAutopilotOutput` response uses a oneof for output_line vs status_update events

### Current Status

- Tests: pass (all existing tests pass with updated protos)
- Lint: pass (`buf lint` passes)
- Build: pass (`make generate` succeeds)

### Next Flight Leg

- bossanova-i81z: Create workflows table migration (20260323170000_workflows.sql)
- bossanova-soff: Add Workflow model to lib/bossalib/models/
- bossanova-rb13: Create WorkflowStore with CRUD operations
- bossanova-9xmw: Add WorkflowStore tests
- bossanova-8ziq: [HANDOFF] Review Flight Leg 2

## Handoff: Flight Leg 4 — Host-Side Plugin Infrastructure

**Date:** 2026-03-23 17:30
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed

- bossanova-vwp5: Add WorkflowService interface and GRPCPlugin to grpc_plugins.go
- bossanova-zlk5: Add PluginTypeWorkflow to NewPluginMap in shared.go
- bossanova-ufdr: Extend plugin Host to dispense and cache WorkflowService
- bossanova-gy1i: Add workflow and attempt RPCs to HostServiceServer
- bossanova-ul60: [HANDOFF] Review Flight Leg 4

### Files Changed

- `services/bossd/internal/plugin/grpc_plugins.go:17-42` — Added `WorkflowService` interface with 6 methods (GetInfo, StartWorkflow, PauseWorkflow, ResumeWorkflow, CancelWorkflow, GetWorkflowStatus)
- `services/bossd/internal/plugin/grpc_plugins.go:46-70` — Added `WorkflowServiceGRPCPlugin` with broker ID 1 HostService registration (same pattern as TaskSourceGRPCPlugin)
- `services/bossd/internal/plugin/grpc_plugins.go:195-271` — Added `workflowServiceGRPCClient` struct implementing all 6 WorkflowService methods via `conn.Invoke()`
- `services/bossd/internal/plugin/grpc_plugins.go:274` — Added compile-time interface check `var _ WorkflowService = (*workflowServiceGRPCClient)(nil)`
- `services/bossd/internal/plugin/shared.go:25` — Added `PluginTypeWorkflow` entry to `NewPluginMap()` with HostService injection
- `services/bossd/internal/plugin/host.go:38` — Added `workflowService WorkflowService` field to `managedPlugin`
- `services/bossd/internal/plugin/host.go:122-131` — Added WorkflowService dispensing in `Start()` after TaskSource
- `services/bossd/internal/plugin/host.go:197-210` — Added `GetWorkflowServices()` accessor method
- `services/bossd/internal/plugin/host_service.go:14-40` — Expanded `HostServiceServer` struct with `workflowStore` and `claude` dependencies, added `SetWorkflowDeps()` for deferred injection
- `services/bossd/internal/plugin/host_service.go:54-98` — Added 7 new methods and 1 stream to `hostServiceDesc` (CreateWorkflow, UpdateWorkflow, GetWorkflow, ListWorkflows, CreateAttempt, GetAttemptStatus, StreamAttemptOutput)
- `services/bossd/internal/plugin/host_service.go:105-115` — Extended `hostServiceHandler` interface with 7 new methods
- `services/bossd/internal/plugin/host_service.go:125-186` — Added gRPC handler stubs for all 7 new methods including streaming handler
- `services/bossd/internal/plugin/host_service.go:190-345` — Added workflow CRUD implementations (CreateWorkflow, UpdateWorkflow, GetWorkflow, ListWorkflows), attempt implementations (CreateAttempt, GetAttemptStatus, StreamAttemptOutput), and `workflowToProto` converter

### Learnings & Notes

- `SetWorkflowDeps()` pattern used for backwards-compatible dependency injection — existing constructor `NewHostServiceServer(provider)` unchanged, new deps added via setter. This avoids changing 4+ call sites until the full wiring is done in Flight Leg 7
- Broker ID 1 reuse is safe because go-plugin creates a separate broker per plugin process — each plugin gets its own broker namespace
- `UpdateWorkflowParams.LastError` uses `**string` (double pointer): `nil` = don't update, `*nil` = clear, `*&str` = set to value
- The streaming handler pattern differs from unary: uses `stream.RecvMsg()` for request and `stream.SendMsg()` for responses
- `hostServiceHandler` interface must match exactly what `hostServiceDesc` references in handler type assertion or build fails
- The `claude.ClaudeRunner` interface's `Start()` returns a sessionID which serves as the attempt ID
- `models.Workflow` uses typed string enums (`WorkflowStatus`, `WorkflowStep`) that map directly to proto string fields

### Issues Encountered

- None — implementation followed existing patterns cleanly

### Next Steps (Flight Leg 5: Autopilot Plugin Binary)

- bossanova-rigb: Create plugin module, main.go, and go.work entry
- bossanova-y1jh: Create plugin.go with GRPCPlugin and manual service descriptor
- bossanova-6pbu: Create host.go with lazy host client and HostService RPC methods
- bossanova-313e: Create server.go with orchestration loop and smart retry
- bossanova-a4l8: Create handoff.go with directory scanning logic
- bossanova-p697: [HANDOFF] Review Flight Leg 5

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-23-1514-autopilot-plugin"` — should show bossanova-rigb
2. Review files: `plugins/bossd-plugin-dependabot/` (reference pattern for plugin structure), `services/bossd/internal/plugin/grpc_plugins.go` (WorkflowService interface), `services/bossd/internal/plugin/host_service.go` (HostService RPC signatures the plugin will call)

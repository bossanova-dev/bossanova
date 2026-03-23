## Handoff: Flight Leg 5 — Autopilot Plugin Binary

**Date:** 2026-03-23 18:15
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed

- bossanova-rigb: Create plugin module, main.go, and go.work entry
- bossanova-y1jh: Create plugin.go with GRPCPlugin and manual service descriptor
- bossanova-6pbu: Create host.go with lazy host client and HostService RPC methods
- bossanova-313e: Create server.go with orchestration loop and smart retry
- bossanova-a4l8: Create handoff.go with directory scanning logic
- bossanova-p697: [HANDOFF] Review Flight Leg 5

### Files Changed

- `plugins/bossd-plugin-autopilot/main.go:1-34` — Plugin entry point, registers as PluginTypeWorkflow with go-plugin ServeConfig
- `plugins/bossd-plugin-autopilot/plugin.go:1-131` — workflowPlugin GRPCPlugin implementation with manual grpc.ServiceDesc for WorkflowService (6 methods: GetInfo, StartWorkflow, PauseWorkflow, ResumeWorkflow, CancelWorkflow, GetWorkflowStatus)
- `plugins/bossd-plugin-autopilot/host.go:1-210` — hostClient interface + lazy broker connection pattern. Provides CreateWorkflow, UpdateWorkflow, GetWorkflow, CreateAttempt, GetAttemptStatus, StreamAttemptOutput RPCs. StreamAttemptOutput uses grpc.NewStream for server-streaming from host
- `plugins/bossd-plugin-autopilot/server.go:1-473` — Core orchestrator: plan→implement→handoff/resume loop→verify→land. Includes workflowConfig parsing from JSON, poll-based attempt monitoring, single smart retry on failure, confirm-land pause, max-legs guard, and proto enum converters
- `plugins/bossd-plugin-autopilot/handoff.go:1-52` — scanHandoffDir reads directory, filters by mtime after a given timestamp, returns newest file path
- `plugins/bossd-plugin-autopilot/go.mod:1-29` — Module definition with go-plugin, zerolog, grpc dependencies
- `go.work:6` — Added `./plugins/bossd-plugin-autopilot` to workspace

### Learnings & Notes

- The plugin mirrors the config.AutopilotConfig struct locally (workflowConfig) to avoid importing the config package — the config is passed as JSON in CreateWorkflowRequest.ConfigJson and parsed on the plugin side
- StartWorkflow kicks off the orchestration loop in a goroutine via `go o.runWorkflow(...)` and returns immediately, so the caller can stream output or poll status. Uses `context.WithoutCancel(ctx)` to prevent cancellation when the RPC returns
- The smart retry builds a context-aware prompt: if the error is actionable, it includes the error message; if non-actionable (timeout, crash, signal, killed), it uses a generic retry prompt
- scanHandoffDir returns empty string (not error) when no new files found — this signals the orchestration loop to proceed to verify instead of resuming
- isStoppedOrDone checks workflow status from the DB on each loop iteration to support external pause/cancel while the loop is running
- The go.mod required `go work sync` to resolve bossalib (private repo) — standalone `go mod tidy` fails but workspace resolution works
- Binary size: 18MB (under 20MB limit from spec)

### Issues Encountered

- `go mod tidy` fails on standalone module due to private bossalib repo — resolved by relying on `go work sync` for workspace-level dependency resolution (same as dependabot plugin)

### Next Steps (Flight Leg 6: Plugin Tests)

- bossanova-8vpa: Create server_test.go with 16+ table-driven orchestration tests
- bossanova-zs1h: Create handoff_test.go with directory scanning tests
- bossanova-b7id: [HANDOFF] Review Flight Leg 6

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-23-1514-autopilot-plugin"` — should show bossanova-8vpa
2. Review files: `plugins/bossd-plugin-autopilot/server.go` (orchestrator logic to test), `plugins/bossd-plugin-autopilot/handoff.go` (scanHandoffDir to test), `plugins/bossd-plugin-dependabot/server_test.go` (reference test pattern)

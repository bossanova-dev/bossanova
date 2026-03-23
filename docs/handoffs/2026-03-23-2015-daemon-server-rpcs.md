## Handoff: Flight Leg 7 — Daemon Server RPCs + Wiring

**Date:** 2026-03-23 20:15
**Branch:** implement-the-autopilot-plugin
**Flight ID:** fp-2026-03-23-1514-autopilot-plugin
**Planning Doc:** docs/plans/2026-03-23-1514-autopilot-plugin.md

### Tasks Completed

- bossanova-bq8u: Add autopilot RPCs to daemon server (start/pause/resume/cancel/status/list)
- bossanova-j1nf: Add StreamAutopilotOutput streaming RPC to server
- bossanova-j6uf: Wire WorkflowStore and PluginHost into Server and main.go
- bossanova-ovwh: [HANDOFF] Review Flight Leg 7

### Files Changed

- `services/bossd/internal/server/server.go:44-57` — Added `workflows db.WorkflowStore` and `pluginHost *plugin.Host` fields to Server struct and Config
- `services/bossd/internal/server/server.go:95-100` — Wired new fields in `New()` constructor
- `services/bossd/internal/server/server.go:1085-1296` — Added 6 unary autopilot RPCs (StartAutopilot, PauseAutopilot, ResumeAutopilot, CancelAutopilot, GetAutopilotStatus, ListAutopilotWorkflows) plus StreamAutopilotOutput streaming RPC, plus resolveAutopilotContext and getWorkflowService helpers
- `services/bossd/internal/server/convert.go:115-178` — Added `autopilotWorkflowToProto`, `workflowStatusToProto`, `workflowStepToProto` converters (models.Workflow → pb.AutopilotWorkflow)
- `services/bossd/internal/plugin/host.go:209-215` — Added `SetWorkflowDeps` method on Host to inject workflow/attempt deps into HostServiceServer
- `services/bossd/cmd/main.go:82` — Created WorkflowStore from database
- `services/bossd/cmd/main.go:113` — Called `pluginHost.SetWorkflowDeps(workflows, claudeRunner)` before plugin start
- `services/bossd/cmd/main.go:137-148` — Added `Workflows` and `PluginHost` to server.Config

### Learnings & Notes

- The server already follows a clean pattern: all RPCs in `server.go`, converters in `convert.go`, and tests in `convert_test.go`
- `StartAutopilot` uses `resolveAutopilotContext` to resolve a working directory to repo ID + session ID — this mirrors the existing `ResolveContext` RPC pattern
- `getWorkflowService()` returns the first available workflow service from the plugin host — there's only ever one autopilot plugin
- `ListAutopilotWorkflows` with `IncludeAll=false` queries three statuses separately (running, paused, pending) since the store doesn't have a "list active" method — could be optimized with a composite query later
- `StreamAutopilotOutput` follows AttachSession's exact pattern: initial status → history burst → subscribe → stream → final status
- The streaming RPC uses the session's ClaudeSessionID to find the active Claude process — this works for the current architecture where autopilot creates attempts that get tracked through sessions
- `SetWorkflowDeps` on Host delegates to HostServiceServer — this preserves the existing plugin.New() constructor signature for backwards compatibility
- `go build ./cmd/main.go` works for building the daemon binary (not `go build ./cmd/...` which conflicts with the `cmd` directory name)

### Issues Encountered

- None — implementation followed existing patterns cleanly

### Next Steps (Flight Leg 8: CLI Commands)

- bossanova-20kh: Create autopilot.go Cobra subcommand group (start/status/list/pause/resume/cancel)
- bossanova-y7uo: Add table formatting for autopilot list and status output
- bossanova-pchx: [HANDOFF] Review Flight Leg 8

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-23-1514-autopilot-plugin"` — should show bossanova-20kh
2. Review files: `services/boss/cmd/main.go` (CLI main, existing Cobra setup), `services/boss/cmd/` (existing subcommand files for pattern), `services/bossd/internal/server/server.go:1085-1296` (autopilot RPCs the CLI will call), `lib/bossalib/gen/bossanova/v1/bossanovav1connect/daemon.connect.go` (client methods for RPC calls)

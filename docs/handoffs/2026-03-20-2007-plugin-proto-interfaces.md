## Handoff: Plugin Proto Interfaces (Flight Leg 1)

**Date:** 2026-03-20 20:07
**Branch:** implement-a-gstack-plan
**Flight ID:** fp-2026-03-20-1510-plugin-host-infrastructure
**Planning Doc:** docs/plans/2026-03-20-1510-plugin-host-infrastructure.md

### Tasks Completed This Flight Leg

- bossanova-cqko: Create plugin.proto with TaskSource, EventSource, Scheduler service definitions
- bossanova-wyfl: Run make generate and verify plugin proto codegen
- bossanova-c5pl: Verify buf lint and tests pass with new proto

### Files Changed

- `proto/bossanova/v1/plugin.proto` (new) — Three plugin service definitions with all request/response messages
- `lib/bossalib/gen/bossanova/v1/plugin.pb.go` (generated) — Go protobuf types
- `lib/bossalib/gen/bossanova/v1/bossanovav1connect/plugin.connect.go` (generated) — ConnectRPC client/server stubs

### Implementation Notes

- Services named with `Service` suffix per buf STANDARD lint rules: `TaskSourceService`, `EventSourceService`, `SchedulerService`
- Each service has its own `GetInfo` request/response types (e.g. `TaskSourceServiceGetInfoRequest`) since buf lint requires unique request/response types across RPCs
- `StreamEvents` returns `stream StreamEventsResponse` wrapping `EventNotification` (buf lint requires response type name matching the RPC)
- `EventNotification` uses `oneof event` for: `TaskReadyEvent`, `TaskUpdatedEvent`, `ExternalCheckEvent`, `CustomEvent`
- `JobAction` uses `oneof action` for: `CreateSessionAction`, `NoOpAction`
- Shared `PluginInfo` message (name, version, capabilities) used by all three services
- `TaskItemStatus` enum for task lifecycle tracking (unspecified, in_progress, completed, failed)

### Current Status

- Proto lint: pass (`buf lint` — no errors)
- Code generation: pass (both `.pb.go` and `.connect.go` files generated)
- Build: pass (`make build-bossd` compiles)
- Tests: pass (`make test-bossalib && make test-bossd` all green)

### Next Flight Leg

**Flight Leg 2: Event Bus**

- bossanova-v25y: Create eventbus package with Bus, Publish, Subscribe, Close
- bossanova-f2cc: Add eventbus tests for pub/sub, multi-subscriber, close
- bossanova-5q7s: Verify make test-bossd passes with eventbus
- bossanova-2sa2: [HANDOFF] Review Flight Leg 2

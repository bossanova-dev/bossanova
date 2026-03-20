# Plugin Host Infrastructure Implementation Plan

**Flight ID:** fp-2026-03-20-1510-plugin-host-infrastructure
**Planning Doc:** docs/plans/open-core-plugin-architecture.md (Phase 2)

## Overview

Build the plugin host infrastructure in bossd using hashicorp/go-plugin. This adds plugin proto interfaces, a plugin host with discovery/lifecycle/health, an internal event bus for plugin-core communication, and plugin configuration in settings.json.

## Affected Areas

- [ ] `proto/bossanova/v1/` — New `plugin.proto` with TaskSource, EventSource, Scheduler interfaces
- [ ] `lib/bossalib/gen/` — Regenerated protobuf code
- [ ] `lib/bossalib/config/` — Plugin configuration in Settings struct
- [ ] `services/bossd/internal/plugin/` — New package: plugin host (discovery, lifecycle, health)
- [ ] `services/bossd/internal/plugin/eventbus/` — New package: internal pub/sub event bus
- [ ] `services/bossd/cmd/main.go` — Wire plugin host into daemon startup/shutdown

## Design References

- VCS Provider interface pattern: `lib/bossalib/vcs/provider.go`
- Event type pattern: `lib/bossalib/vcs/events.go`
- Dispatcher event consumption: `services/bossd/internal/session/dispatcher.go`
- Config loading: `lib/bossalib/config/config.go`
- Daemon wiring: `services/bossd/cmd/main.go`
- Mock pattern: `services/bossd/internal/testharness/mock_vcs.go`
- Buf generate pipeline: `buf.gen.yaml`, `Makefile` (generate target)

---

## Flight Leg 1: Plugin Proto Interfaces

Define the plugin service interfaces in protobuf, matching the plan's four plugin types.

### Tasks

- [ ] Create `proto/bossanova/v1/plugin.proto` with `TaskSourcePlugin`, `EventSourcePlugin`, `SchedulerPlugin` service definitions and all request/response/notification messages
  - Files: `proto/bossanova/v1/plugin.proto` (new)
  - Pattern: Follow `daemon.proto` service definition style, `models.proto` message style
  - Include `PluginInfo` message for plugin self-identification (name, version, capabilities)
  - Use `oneof` for `EventNotification` and `JobAction` per the plan spec
- [ ] Run `make generate` to produce Go code from new proto
  - Verify: `lib/bossalib/gen/bossanova/v1/plugin.pb.go` and `plugin_connect.pb.go` exist
- [ ] Verify `buf lint` passes with the new proto file
- [ ] Verify `make test-bossalib && make test-bossd` still pass (no regressions)

### Post-Flight Checks for Flight Leg 1

- [ ] **Proto lint:** `buf lint` — no errors
- [ ] **Code generation:** Files exist at `lib/bossalib/gen/bossanova/v1/plugin.pb.go` and `plugin.connect.go`
- [ ] **Build:** `make build-bossd` — compiles successfully
- [ ] **Tests:** `make test-bossalib && make test-bossd` — all pass

### [HANDOFF] Review Flight Leg 1

Human reviews: Proto interface design — are the services, messages, and field types correct? Is the event notification oneof complete?

---

## Flight Leg 2: Event Bus

Build the internal pub/sub event bus that plugins will use to communicate with the core dispatcher.

### Tasks

- [ ] Create `services/bossd/internal/plugin/eventbus/eventbus.go` — `Bus` struct with `Publish()`, `Subscribe()`, `Unsubscribe()`, `Close()` methods
  - Generic event type using the proto `EventNotification` message
  - Buffered channels per subscriber (size 64, matching poller pattern)
  - Thread-safe with mutex (matching dispatcher pattern)
  - Context-aware subscribe that auto-unsubscribes on context cancellation
- [ ] Create `services/bossd/internal/plugin/eventbus/eventbus_test.go` — tests for publish/subscribe, multiple subscribers, unsubscribe, close, buffer overflow (dropped messages with log warning)
- [ ] Verify `make test-bossd` passes with new tests

### Post-Flight Checks for Flight Leg 2

- [ ] **Format:** `gofmt -l services/bossd/internal/plugin/` — no output (already formatted)
- [ ] **Tests:** `go test ./services/bossd/internal/plugin/eventbus/...` — all pass
- [ ] **Build:** `make build-bossd` — compiles successfully

### [HANDOFF] Review Flight Leg 2

Human reviews: Event bus API design — is the pub/sub interface clean? Are the concurrency patterns correct?

---

## Flight Leg 3: Plugin Host Core

Build the plugin host that manages plugin discovery, lifecycle, and health monitoring using hashicorp/go-plugin.

### Tasks

- [ ] Add `hashicorp/go-plugin` dependency to `services/bossd/go.mod`
  - Run `cd services/bossd && go get github.com/hashicorp/go-plugin`
- [ ] Add `Plugins []PluginConfig` to `lib/bossalib/config/Settings` struct
  - `PluginConfig` struct: `Name`, `Path`, `Enabled`, `Config map[string]string`
  - Pattern: Follow existing Settings JSON serialization pattern
- [ ] Create `services/bossd/internal/plugin/host.go` — `Host` struct
  - `New(config []config.PluginConfig, eventBus *eventbus.Bus, logger zerolog.Logger) *Host`
  - `Start(ctx context.Context) error` — discovers and launches enabled plugins
  - `Stop() error` — gracefully kills all plugin processes
  - `Plugins() []PluginStatus` — returns status of each plugin (name, running, pid, uptime)
  - Uses go-plugin `plugin.ClientConfig` for process management
  - Implements health-check goroutine that pings plugins on interval and restarts on failure
- [ ] Create `services/bossd/internal/plugin/shared.go` — shared go-plugin handshake config and plugin map
  - Define `Handshake` (magic cookie), `PluginMap` for the three plugin types
  - GRPCPlugin implementations that bridge go-plugin to the generated proto services

### Post-Flight Checks for Flight Leg 3

- [ ] **Format:** `gofmt -l services/bossd/internal/plugin/` — no output
- [ ] **Build:** `make build-bossd` — compiles (host compiles against go-plugin)
- [ ] **Config test:** `go test ./lib/bossalib/config/...` — settings with plugins marshal/unmarshal correctly

### [HANDOFF] Review Flight Leg 3

Human reviews: Plugin host design — is the go-plugin integration clean? Are the GRPCPlugin bridges correct? Is the health-check strategy reasonable?

---

## Flight Leg 4: Plugin Host Tests & Daemon Wiring

Wire the plugin host into the daemon startup sequence and add comprehensive tests.

### Tasks

- [ ] Create `services/bossd/internal/plugin/host_test.go` — unit tests
  - Test: Host starts with empty plugin list (no-op)
  - Test: Host with disabled plugin skips it
  - Test: Host.Stop() is idempotent
  - Test: PluginStatus reports correctly
- [ ] Wire plugin host into `services/bossd/cmd/main.go`
  - Load plugin config from settings
  - Create event bus
  - Create and start plugin host after stores, before server
  - Stop plugin host in shutdown sequence (before poller cancel)
- [ ] Verify full build and test pass
  - `make build-bossd && make test-bossd`
  - `make build-boss && make test-bossalib`

### Post-Flight Checks for Flight Leg 4

- [ ] **Build:** `make build` — all three binaries compile
- [ ] **Tests:** `make test` — all module tests pass
- [ ] **Format:** `make format` passes without changes (or only formatting changes)
- [ ] **Daemon starts:** `make build-bossd && ./bin/bossd --version` — prints version without error

### [HANDOFF] Review Flight Leg 4

Human reviews: Complete Phase 2 — daemon wiring, test coverage, overall architecture. Ready to merge?

---

## Rollback Plan

- All changes are additive — new files, new proto, new config field
- No existing behavior changes; plugin host is a no-op when no plugins configured
- Revert: `git revert` the commits from this branch

## Notes

- The plugin host is intentionally a no-op when `settings.plugins` is empty or absent — zero impact on existing users
- go-plugin uses gRPC under the hood, matching the existing ConnectRPC pattern used for daemon ↔ CLI communication
- The event bus is in-memory only per the plan spec; no persistence or replay
- Plugin proto interfaces are defined but no plugin implementations exist yet — those come in Phases 3-5
- The host health-checks but does NOT auto-restart in the first implementation; just logs and marks unhealthy. Auto-restart can be added when we have real plugins to test with

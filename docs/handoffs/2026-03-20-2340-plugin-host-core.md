## Handoff: Plugin Host Core (Flight Leg 3)

**Date:** 2026-03-20 23:40
**Branch:** implement-a-gstack-plan
**Flight ID:** fp-2026-03-20-1510-plugin-host-infrastructure
**Planning Doc:** docs/plans/2026-03-20-1510-plugin-host-infrastructure.md

### Tasks Completed This Flight Leg

- bossanova-2mdj: Add hashicorp/go-plugin dep and PluginConfig to Settings
- bossanova-ge6b: Create plugin host.go with Start, Stop, Plugins, health-check
- bossanova-18k6: Create plugin shared.go with handshake and GRPCPlugin bridges

### Files Changed

- `services/bossd/go.mod` — Added `hashicorp/go-plugin v1.7.0` and transitive deps (go-hclog, yamux, grpc, etc.)
- `services/bossd/go.sum` — Updated checksums
- `go.work.sum` — Updated workspace checksums
- `lib/bossalib/config/config.go:13-19` — New `PluginConfig` struct (Name, Path, Enabled, Config map); added `Plugins []PluginConfig` field to Settings with `omitempty`
- `lib/bossalib/config/config_test.go:129-191` — `TestPluginsRoundTrip` (save/load with two plugins, verify all fields) and `TestPluginsOmittedWhenEmpty` (omitempty check)
- `services/bossd/internal/plugin/host.go` (new) — `Host` struct with `New()`, `Start(ctx, cfgs)`, `Stop()`, `Plugins()` methods and background health-check loop
- `services/bossd/internal/plugin/shared.go` (new) — `Handshake` config (magic cookie BOSSANOVA_PLUGIN), `PluginMap` with three plugin type keys
- `services/bossd/internal/plugin/grpc_plugins.go` (new) — Three `GRPCPlugin` implementations (TaskSource, EventSource, Scheduler) with gRPC client wrappers using `conn.Invoke()`; host-side Go interfaces with compile-time checks
- `services/bossd/internal/plugin/hclog_adapter.go` (new) — `hclogAdapter` bridging hashicorp/go-hclog Logger interface to zerolog

### Implementation Notes

- **go-plugin ↔ ConnectRPC tension**: The project uses `protoc-gen-connect-go` (not `protoc-gen-go-grpc`), so there are no standard `_grpc.pb.go` files. The GRPCPlugin bridges use `conn.Invoke()` with raw proto method paths (e.g., `/bossanova.v1.TaskSourceService/GetInfo`) instead of generated gRPC client stubs.
- **Host-side interfaces**: Defined `TaskSource`, `EventSource`, `Scheduler` Go interfaces in `grpc_plugins.go` that abstract the gRPC clients. These are what the rest of bossd will use.
- **hclog adapter**: go-plugin requires `hclog.Logger`; the adapter bridges all levels to zerolog. Uses `go-hclog@v1.6.3` interface (still uses `interface{}`/`any` varargs).
- **Health check**: Pings plugins every 30s via `ClientProtocol.Ping()`. Logs warnings but does NOT auto-restart (per plan: "auto-restart can be added when we have real plugins").
- **Stop() ordering**: Cancels health-check context first, waits for goroutine exit, then kills plugin processes to avoid races.
- **WaitGroup.Go()**: Used Go 1.25's `sync.WaitGroup.Go()` convenience method for the health-check goroutine.
- Import alias: `goplugin "github.com/hashicorp/go-plugin"` to avoid conflict with the `plugin` package name.

### Current Status

- Format: pass (`gofmt -l` — no output)
- Build: pass (`make build-bossd` — compiles)
- Config tests: pass (`go test ./lib/bossalib/config/...` — 10 tests, including 2 new plugin tests)
- Full suite: pass (`make test-bossd` — all modules green)

### Next Flight Leg

**Flight Leg 4: Plugin Host Tests & Daemon Wiring**

- bossanova-1xm9: Add plugin host unit tests
- bossanova-56oy: Wire plugin host into bossd cmd/main.go startup/shutdown
- bossanova-zgcy: Verify full build and test suite passes
- bossanova-k3g1: [HANDOFF] Review Flight Leg 4

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-20-1510-plugin-host-infrastructure"` to see available tasks
2. Review files: `services/bossd/internal/plugin/host.go`, `services/bossd/internal/plugin/grpc_plugins.go`, `services/bossd/cmd/main.go`

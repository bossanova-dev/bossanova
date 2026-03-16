## Handoff: Flight Leg 4a — ConnectRPC Server + RPCs

**Date:** 2026-03-16 19:45
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-2eg: Create ConnectRPC server scaffold in services/bossd/internal/server/ with Unix socket listener
- bossanova-kut: Implement repo RPCs (RegisterRepo, ListRepos, RemoveRepo) wired to RepoStore
- bossanova-i8q: Implement session RPCs (Create, Get, List, Stop, Pause, Resume, Retry, Close, Remove, Archive, Resurrect, EmptyTrash) wired to SessionStore
- bossanova-djj: Implement ResolveContext RPC — detect worktree → repo → unregistered git repo → none

### Files Changed

- `services/bossd/internal/server/server.go:1-488` — ConnectRPC server: Unix socket lifecycle (ListenAndServe, Shutdown, stale socket cleanup), all 18 DaemonService RPCs implemented, ResolveContext with directory-based detection, isSubdirOf helper
- `services/bossd/internal/server/convert.go:1-82` — Proto-to-model conversion: repoToProto, sessionToProto, protoToTimestamp
- `services/bossd/go.mod` — Added connectrpc.com/connect v1.19.1, google.golang.org/protobuf v1.36.11
- `services/bossd/go.sum` — Updated with new dependencies

### Learnings & Notes

- **go.work handles cross-module deps**: `go mod tidy` does NOT work outside of workspace context (tries to fetch nonexistent remote). Must use `go build` from workspace root or `go mod tidy` from workspace root only.
- **UnimplementedDaemonServiceHandler embedding**: ConnectRPC's generated code provides an `UnimplementedDaemonServiceHandler` struct. Embedding it in the server struct provides default implementations for all RPCs, allowing incremental development.
- **Unused function lint**: golangci-lint `unused` checker catches helper functions prepared for future legs. Better to add them when needed rather than pre-creating.
- **Double pointer pattern for nullable updates**: `**string` in UpdateSessionParams works well — `nil` = don't update, `*nil` = set to NULL. Used in RetrySession to clear blocked_reason.
- **ListRepoPRs stubbed**: Returns empty response. Real implementation requires VCS provider (Leg 7).
- **State transitions simplified**: Stop/Pause/Resume/Retry directly update DB state rather than firing state machine events. Full state machine integration deferred to Leg 6 session lifecycle manager.

### Issues Encountered

- golangci-lint flagged 3 unused conversion helpers (attemptToProto, stateToProto, checkStateToProto). Removed them; they'll be re-added when needed.
- `go mod tidy -C services/bossd` fails because it doesn't respect go.work — cross-module deps can't be resolved. Workaround: run `go build` from workspace root which uses go.work automatically.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — buf lint + golangci-lint v2
- Tests: PASSED — 18 machine tests + 8 db tests = 26 total
- Vet: PASSED
- Format: PASSED

### Next Flight Leg

Flight Leg 4b: Streaming + Entry Point + Client

Tasks (from bd ready):

- bossanova-9k1: Implement AttachSession server-streaming RPC (stub: tail log file placeholder)
- bossanova-1mr: Implement ListRepoPRs RPC stub (returns empty list, real impl in Leg 7)
- bossanova-blk: Wire daemon entry point: config, DB, migrations, stores, server, signal handling, graceful shutdown
- bossanova-y39: Create ConnectRPC client in services/boss/internal/client/ — BossClient interface + local Unix socket impl
- bossanova-c9x: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/bossd/internal/server/server.go`, `services/bossd/internal/db/store.go`, `proto/bossanova/v1/daemon.proto`

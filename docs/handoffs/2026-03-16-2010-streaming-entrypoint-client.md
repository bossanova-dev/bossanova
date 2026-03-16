## Handoff: Flight Leg 4b — Streaming + Entry Point + Client

**Date:** 2026-03-16 20:10
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-9k1: Implement AttachSession server-streaming RPC stub (validates session, sends initial state, closes)
- bossanova-1mr: Implement ListRepoPRs RPC stub with input validation (returns empty list, real impl in Leg 7)
- bossanova-blk: Wire daemon entry point — SQLite, goose migrations, stores, ConnectRPC server, signal handling, graceful shutdown
- bossanova-y39: Create ConnectRPC client in services/boss/internal/client/ — Unix socket transport, typed methods for all 18 RPCs

### Files Changed

- `services/bossd/internal/server/server.go:238-262` — AttachSession: validates session, sends initial StateChange, closes stream
- `services/bossd/internal/server/server.go:144-156` — ListRepoPRs: added repo_id validation and repo existence check
- `services/bossd/cmd/main.go:1-112` — Full daemon entry point: zerolog, SQLite open, goose migrations, store creation, server start, SIGINT/SIGTERM signal handling, graceful shutdown, socket cleanup
- `services/bossd/migrations/embed.go:1-9` — Embeds `*.sql` migration files via go:embed for goose
- `services/bossd/go.mod` — Added goose v3.27.0, zerolog v1.34.0
- `services/bossd/go.sum` — Updated
- `services/boss/internal/client/client.go:1-208` — ConnectRPC client: Unix socket HTTP transport, typed wrappers for all 18 RPCs (ResolveContext, RegisterRepo, ListRepos, RemoveRepo, ListRepoPRs, CreateSession, GetSession, ListSessions, AttachSession, StopSession, PauseSession, ResumeSession, RetrySession, CloseSession, RemoveSession, ArchiveSession, ResurrectSession, EmptyTrash), Ping health check
- `services/boss/go.mod` — Bumped to go 1.25.0, added connectrpc.com/connect v1.19.1, google.golang.org/protobuf v1.36.11
- `services/boss/go.sum` — New

### Learnings & Notes

- **goose embed pattern**: Migrations live at `services/bossd/migrations/*.sql`. Since `go:embed` is relative to the package dir, created `migrations/embed.go` that exports `FS embed.FS`, imported in `cmd/main.go`. The goose path is `"."` since the embed FS root IS the migrations dir.
- **goose dialect is "sqlite3"**: Even with `modernc.org/sqlite`, goose uses `"sqlite3"` as the dialect name (not `"sqlite"`).
- **Stale WAL files cause errors**: If the DB file is deleted but `.db-shm` and `.db-wal` remain, SQLite will fail with `disk I/O error (522)`. Must clean up all three files.
- **Unix socket client pattern**: `http.Transport.DialContext` overrides the connection target. The base URL host (`http://localhost`) is ignored — all requests go through the Unix socket.
- **goose idempotent re-runs**: On second startup, goose correctly reports "no migrations to run" when DB is already at the latest version.

### Issues Encountered

- Stale DB from earlier test runs (pre-goose) caused migration failures. Fixed by cleaning up all DB artifacts (`.db`, `.db-shm`, `.db-wal`). This won't affect fresh installs.
- `go:embed` can't traverse up directories — had to create a `migrations/embed.go` package instead of embedding from `cmd/main.go`.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — buf lint + golangci-lint v2
- Tests: PASSED — 18 machine tests + 8 db tests = 26 total
- Vet: PASSED
- Format: PASSED
- IPC: VERIFIED — bossd starts, ListRepos returns `{}` via curl over Unix socket, SIGTERM cleans up socket

### Next Flight Leg

Flight Leg 5: CLI — Bubbletea + Local Mode (per plan)

Tasks need to be created via `/pre-flight-checks` for:

- Cobra arg parser: all commands including boss archive/resurrect/trash
- Home screen (Bubbletea): session table, action bar, 2s polling, keyboard nav
- New session wizard: repo select → new/existing PR → plan input → confirm
- Attach view: server-streaming, Claude output, session header, Ctrl+C detach
- Repo management views, boss ls non-interactive mode
- Archive/resurrect/trash commands

### Resume Command

To continue this work:

1. Run `/pre-flight-checks` to create bd tasks for Flight Leg 5
2. Review plan: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 5 section)
3. Review files: `services/boss/internal/client/client.go`, `services/bossd/cmd/main.go`, `services/bossd/internal/server/server.go`

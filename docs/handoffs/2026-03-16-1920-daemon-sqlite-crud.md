## Handoff: Flight Leg 3 — Daemon Core (SQLite + CRUD)

**Date:** 2026-03-16 19:20
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-85h: Create shared migration runner in lib/bossalib/migrate/ using goose + go:embed
- bossanova-daz: Create SQLite module in services/bossd/internal/db/ with WAL mode and FKs
- bossanova-alp: Create initial migration services/bossd/migrations/20260316170000_initial_schema.sql
- bossanova-0jm: Implement Store interfaces + RepoStore, SessionStore, AttemptStore
- bossanova-x7i: Unit tests with in-memory SQLite — CRUD, FK cascades, migration runner
- bossanova-y4a: [HANDOFF]

### Files Changed

- `lib/bossalib/migrate/migrate.go:1-21` — Shared migration runner: accepts `fs.FS` + `*sql.DB`, sets goose dialect to sqlite3, runs `goose.Up`
- `lib/bossalib/go.mod` — Added pressly/goose/v3 v3.27.0, bumped to go 1.25.0
- `services/bossd/internal/db/db.go:1-57` — SQLite init: Open (WAL + FKs + single conn), OpenInMemory, DefaultDBPath
- `services/bossd/internal/db/store.go:1-91` — Store interfaces: RepoStore, SessionStore (with Archive/Resurrect), AttemptStore; param structs
- `services/bossd/internal/db/repo_store.go:1-131` — SQLiteRepoStore: CRUD, GetByPath, Update with partial params
- `services/bossd/internal/db/session_store.go:1-200` — SQLiteSessionStore: CRUD, ListActive/ListArchived, Archive/Resurrect, partial Update
- `services/bossd/internal/db/attempt_store.go:1-119` — SQLiteAttemptStore: CRUD, ListBySession, partial Update
- `services/bossd/internal/db/helpers.go:1-19` — newID (16-char hex), joinStrings helper
- `services/bossd/internal/db/db_test.go:1-413` — 8 tests: migration runner, repo CRUD, unique path, session CRUD, archive/resurrect, attempt CRUD, FK cascade (repo→session→attempt), FK cascade (session→attempt)
- `services/bossd/migrations/20260316170000_initial_schema.sql` — Initial schema: repos, sessions (with archived_at), attempts tables; indexes on repo_id/session_id; FK cascade deletes
- `.golangci.yml` — Updated to v2 format: added `version: "2"`, moved gofmt/goimports to formatters section, removed gosimple (merged into staticcheck)

### Learnings & Notes

- **go.work cross-module resolution**: Do NOT add `require github.com/recurser/bossalib v0.0.0` in service go.mod files. The `use` directives in go.work handle resolution automatically. Adding an explicit require with a fake version causes Go to try to fetch from the (nonexistent) remote, failing the build.
- **goose v3.27.0 requires go >= 1.25.0**: Workspace and all modules bumped from 1.24.4 to 1.25.0.
- **golangci-lint v2 migration**: v2 requires `version: "2"` in config, separates formatters from linters, and merged `gosimple` into `staticcheck`.
- **Migration runner accepts `fs.FS`**: Works with both `embed.FS` (for production binaries) and `os.DirFS` (for tests that reference actual migration files without copying).
- **SQLite timestamps as TEXT**: Stored as ISO 8601 strings (`strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`), parsed back with `time.Parse`.
- **Partial update pattern**: Update params use `*T` for simple fields and `**T` for nullable fields (nil = don't update, `*nil` = set to NULL).

### Issues Encountered

- golangci-lint v1 (built with go1.24) incompatible with go 1.25.0 modules. Resolved by installing golangci-lint v2 via `go install`.
- errcheck lint violations on `rows.Close()` and `db.Close()` in error paths. Resolved with `_ = x.Close()` and `defer func() { _ = rows.Close() }()`.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — buf lint + golangci-lint v2
- Tests: PASSED — 18 machine tests + 8 db tests = 26 total
- Format: PASSED — gofmt clean

### Next Flight Leg

Flight Leg 4: Daemon IPC — ConnectRPC over Unix Socket

Tasks to create (via /pre-flight-checks):
- ConnectRPC server in services/bossd/internal/server/ — Unix socket
- Implement all DaemonService RPCs wired to stores
- Context resolution: detect worktree → repo → unregistered git repo → none
- AttachSession server-streaming RPC (tail log file + fsnotify)
- Daemon entry point: config, DB, migrations, stores, server, graceful shutdown
- ConnectRPC client in services/boss/internal/client/ — BossClient interface, local + remote impls
- [HANDOFF] Review Flight Leg 4

### Resume Command

To continue this work:
1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/bossd/internal/db/store.go`, `lib/bossalib/migrate/migrate.go`, `proto/bossanova/v1/daemon.proto`

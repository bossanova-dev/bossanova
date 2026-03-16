## Handoff: Flight Leg 8a — Orchestrator DB Foundation

**Date:** 2026-03-16 26:30
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-c6o: Create bosso module scaffold (go.mod deps, internal packages, migrations embed)
- bossanova-v7b: Create orchestrator SQLite schema (users, daemons, sessions_registry, audit_log)
- bossanova-vsy: Implement orchestrator DB stores (UserStore, DaemonStore, SessionRegistryStore, AuditStore)
- bossanova-3hk: Add orchestrator DB store tests (in-memory SQLite, CRUD, constraints)

### Files Changed

- `services/bosso/go.mod:1-18` — Updated from stub to real module with modernc.org/sqlite dependency. go.work handles cross-module resolution for bossalib imports.
- `services/bosso/migrations/embed.go:1-9` — go:embed for SQL migration files, same pattern as bossd.
- `services/bosso/migrations/20260316170000_initial_schema.sql:1-55` — 5 tables: users (OIDC sub, unique), daemons (heartbeat, session_token unique, FK to users CASCADE), daemon_repos (composite PK, FK CASCADE), sessions_registry (lightweight routing, FK to daemons CASCADE), audit_log (append-only, FK to users SET NULL). Indexes on FKs and audit timestamps.
- `services/bosso/internal/db/db.go:1-57` — SQLite init (Open, OpenInMemory, DefaultDBPath → bosso.db), WAL mode, FKs, single connection. Same pattern as bossd.
- `services/bosso/internal/db/helpers.go:1-39` — newID (16-char hex), timeNow/parseTime/parseOptionalTime for ISO 8601 timestamps.
- `services/bosso/internal/db/store.go:1-138` — Domain types (User, Daemon, SessionEntry, AuditEntry) and 4 store interfaces (UserStore, DaemonStore, SessionRegistryStore, AuditStore) with param/opts types.
- `services/bosso/internal/db/user_store.go:1-107` — SQLiteUserStore: Create, Get, GetBySub, List, Update, Delete. Scan helpers for Row and Rows.
- `services/bosso/internal/db/daemon_store.go:1-193` — SQLiteDaemonStore: Create (with repo list), Get (with repos), GetByToken, ListByUser (collect-then-fetch to avoid deadlock), Update, UpdateRepos (atomic replace), Delete. Private setRepos/getRepos helpers.
- `services/bosso/internal/db/session_registry_store.go:1-107` — SQLiteSessionRegistryStore: Create, Get, ListByDaemon, Update (supports transfer via DaemonID change), Delete.
- `services/bosso/internal/db/audit_store.go:1-87` — SQLiteAuditStore: Create, List (filters by UserID, Action; default limit 100). Append-only — no update/delete.
- `services/bosso/internal/db/db_test.go:1-352` — 10 tests covering all stores, constraints, and cascades.

### Learnings & Notes

- **SQLite single-connection deadlock**: When iterating `*sql.Rows` and calling another query inside the loop, SQLite with `MaxOpenConns(1)` deadlocks. Fixed by collecting rows first, closing the iterator, then running sub-queries. This affected `DaemonStore.ListByUser` which needs to fetch repos per daemon.
- **go.work cross-module resolution**: bosso's go.mod doesn't need to declare bossalib as a dependency — go.work `use` directives handle it. However, `go mod tidy` fails in isolation because it tries to fetch from remote. Tests and builds work fine via workspace.
- **Domain types in bosso vs bossalib**: Orchestrator-specific types (User, Daemon, SessionEntry, AuditEntry) live in `bosso/internal/db` rather than the shared `bossalib/models`, since they're not needed by the daemon or CLI.
- **Daemon-provided ID**: Unlike other entities that use random hex IDs, the daemon's ID is provided by the daemon itself during registration. This allows the daemon to maintain its identity across reconnections.
- **AuditStore is append-only**: No Update or Delete methods — audit entries are immutable once created. The user_id FK uses SET NULL so entries survive user deletion.

### Issues Encountered

- **go mod tidy failure**: Running `go mod tidy` in the bosso directory fails because it tries to fetch bossalib from the nonexistent remote repo. This is expected behavior — all Go commands should be run from the workspace root. Not a real issue.
- **Stray `cmd` binary**: An untracked compiled binary (`cmd`) exists at the project root. Not related to this leg — should be gitignored or deleted.
- **Leg 8a/8b handoff tasks auto-resolved**: The bd handoff tasks for legs 8a and 8b appear to have been auto-cleaned when dependencies resolved. Only leg 8c handoff remains. This didn't affect execution.

### Current Status

- Build: PASSED — all 4 modules (bosso, bossd, boss, bossalib)
- Vet: PASSED
- Format: PASSED
- Tests: PASSED — bosso: 10 tests, bossd: 66 tests, bossalib: machine tests

### Next Steps (Flight Leg 8b: JWT Auth + Daemon Registry)

- bossanova-nlz: Implement JWT validation middleware for ConnectRPC (connectrpc/authn-go)
- bossanova-e4t: Implement daemon registry (RegisterDaemon, Heartbeat, ListDaemons RPCs)
- bossanova-gi3: Implement orchestrator entry point (ConnectRPC HTTPS server, DB, migrations, graceful shutdown)
- bossanova-vfr: Add auth + registry tests (JWT validation, daemon heartbeat timeout, registration)

### Resume Command

To continue this work:

1. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 8 section)
2. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` — should show bossanova-nlz
3. Key files for context: `services/bosso/internal/db/store.go`, `services/bosso/internal/db/db.go`, `services/bosso/internal/db/daemon_store.go`

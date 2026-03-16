# Bossanova Go Rewrite — Full Implementation Plan

**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite

## Context

Bossanova is a CLI-first orchestrator for managing multiple Claude Code sessions mapped to GitHub PRs. The existing TypeScript implementation (flight legs 1-8) serves as the reference spec. This plan rewrites the entire system in Go for:

- **Stability**: Static binaries, goroutines, no GC pressure for long-running daemon
- **Multi-daemon**: Sessions can transfer between machines (work PC → home PC → phone)
- **Real streaming**: ConnectRPC server-streaming for live Claude output to remote CLIs
- **Distribution**: Single binary per component, no Node.js runtime dependency
- **Open source model**: Daemon + CLI are open source; orchestrator + webhook + web are a paid hosted tier

## Architecture

```
LOCAL (open source)                    CLOUD (paid tier)
                                    ┌──────────────────────┐
┌─────────┐   Unix Socket           │   Orchestrator       │
│ boss    │◄────────────────►bossd  │   (Go on Fly.io)     │
│ (CLI)   │   ConnectRPC    │    │  │   SQLite + Litestream│
└─────────┘                 │    │  │   Auth + Webhook     │
                            │    │  │   Web UI             │
┌─────────┐   ConnectRPC    │    │  └──────────┬───────────┘
│ boss    │◄────────via orchestrator────────────┘
│ (remote)│  (authenticated)│    │
└─────────┘                 └────┘
                             bossd
                           (daemon)
```

**Local mode (free)**: `boss` ↔ `bossd` via Unix socket. No auth, no internet needed. Polling-based CI checks via `gh` CLI. Fully functional single-machine workflow.

**Cloud mode (paid)**: Daemon connects to orchestrator. Auth via OIDC (Auth0/Cognito). Multi-daemon session transfer, real-time webhooks, remote CLI, web UI for mobile.

## Key Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Language | Go everywhere | Single language, static binaries, excellent process mgmt |
| CLI framework | Bubbletea v2 + Lipgloss + Bubbles | Best TUI ecosystem, Elm architecture |
| RPC | ConnectRPC + Protobuf | gRPC-compatible, works in browsers, streaming |
| State machine | qmuntal/stateless | Guards, actions, fluent API, closest to XState |
| Daemon DB | SQLite via modernc.org/sqlite | Pure Go, no CGO, cross-compilation |
| Orchestrator DB | SQLite + Litestream | Same stack as daemon, S3/R2 backup, ~$0.50/mo |
| Migrations | pressly/goose | Works with SQLite everywhere, embeddable via go:embed |
| Auth | OIDC (Auth0 or Cognito) | PKCE flow for CLI, JWT validation server-side |
| DI | Constructor injection (no framework) | Go idiom, explicit, testable |
| Logging | zerolog | Structured JSON, fast, zero-allocation |
| Web UI | templ + htmx | Go-native templates, minimal JS, mobile-friendly |

## Project Structure

```
bossanova/
├── go.mod
├── go.sum
├── Makefile
├── buf.yaml                       # Buf config
├── buf.gen.yaml                   # Protobuf codegen
├── proto/
│   └── bossanova/v1/
│       ├── models.proto           # Shared message types
│       ├── daemon.proto           # Daemon service (local IPC)
│       └── orchestrator.proto     # Orchestrator service (cloud)
├── cmd/
│   ├── boss/main.go               # CLI binary
│   ├── bossd/main.go              # Daemon binary
│   └── bosso/main.go              # Orchestrator binary
├── internal/
│   ├── machine/                   # Session state machine
│   ├── models/                    # Domain types (Go structs)
│   ├── daemon/
│   │   ├── db/                    # SQLite + stores
│   │   ├── git/                   # Worktree, push, utils
│   │   ├── claude/                # Claude subprocess mgmt
│   │   ├── github/                # gh CLI wrapper, polling
│   │   ├── session/               # Lifecycle, PR, completion, fix loop
│   │   ├── server/                # ConnectRPC server (Unix socket)
│   │   └── upstream/              # Optional orchestrator connection
│   ├── cli/
│   │   ├── views/                 # Bubbletea views
│   │   ├── auth/                  # OIDC client (cloud mode only)
│   │   └── client/                # ConnectRPC client (local or remote)
│   ├── orchestrator/
│   │   ├── db/                    # SQLite + stores (server-side registry)
│   │   ├── auth/                  # JWT middleware
│   │   ├── registry/              # Daemon registry + presence
│   │   ├── relay/                 # Stream relay (daemon → CLI)
│   │   ├── webhook/               # GitHub webhook handler
│   │   └── web/                   # Web UI handlers + templ templates
│   └── migrate/                   # Shared migration runner (goose)
├── migrations/
│   ├── daemon/                    # Daemon SQLite migrations (go:embed)
│   │   ├── 001_initial_schema.sql
│   │   ├── 002_add_daemon_id.sql
│   │   └── ...
│   └── orchestrator/              # Orchestrator SQLite migrations (go:embed)
│       ├── 001_initial_schema.sql
│       └── ...
├── web/
│   ├── templates/                 # templ components
│   └── static/                    # CSS, minimal JS
└── docs/
```

## Migration Strategy

### SQLite Migrations (both daemon and orchestrator)

Both `bossd` and `bosso` use SQLite with the same migration tooling:

- Migrations embedded in each binary via `go:embed`
- On startup: goose runs pending migrations automatically
- All migrations are additive (add columns/tables, never drop)
- Rollback: goose supports `down` migrations for development
- Version tracking: `goose_db_version` table
- Shared `internal/migrate/` package provides `RunMigrations(db, embedFS)` used by both

### Daemon (local upgrade)

```
brew upgrade bossd    # or download new binary
bossd                 # auto-migrates SQLite on startup
```

Users upgrade by replacing the binary — migrations run on next start. The DB file lives at `~/Library/Application Support/bossanova/bossd.db`.

### Orchestrator (Fly.io deploy)

```
fly deploy            # release_command: "bosso migrate"
```

The orchestrator SQLite file lives on a Fly.io persistent volume. Litestream continuously replicates to S3/R2 for durability. On deploy, `bosso migrate` runs pending migrations before the server starts.

---

## Flight Legs

### Leg 1: Go Scaffold + Protobuf

#### Tasks

- [ ] Initialize Go module, Makefile, .gitignore
- [ ] Define protobuf messages in `proto/bossanova/v1/models.proto`:
  - Repo, Session, Attempt, SessionState enum, CheckState enum
  - Matches existing TS types exactly
- [ ] Define daemon service in `proto/bossanova/v1/daemon.proto`:
  - All 14 RPC methods from existing `rpc.ts`
  - `AttachSession` as server-streaming RPC (real-time Claude output)
  - `InteractiveSession` as bidirectional streaming (future)
- [ ] Define orchestrator service in `proto/bossanova/v1/orchestrator.proto`:
  - Same methods as daemon (proxied), plus: `RegisterDaemon`, `TransferSession`, `ListDaemons`
  - Auth-aware: requests include JWT
- [ ] Configure buf.yaml + buf.gen.yaml for Go code generation
- [ ] Create `cmd/boss/main.go`, `cmd/bossd/main.go`, `cmd/bosso/main.go` stubs
- [ ] Makefile targets: `make generate` (buf), `make build` (all binaries), `make test`, `make lint` (golangci-lint)

#### Post-Flight Checks

- [ ] `make generate` produces Go code in `internal/gen/`
- [ ] `make build` produces three binaries
- [ ] `make lint` passes

#### [HANDOFF] Review Flight Leg 1

Human reviews: Go module setup, protobuf definitions, Makefile targets, buf configuration

---

### Leg 2: State Machine + Domain Types

#### Tasks

- [ ] Implement session state machine in `internal/machine/`
  - 11 states, 14 event triggers (match existing TS machine exactly)
  - Guards: `hasReachedMaxAttempts`
  - Actions: `incrementAttemptCount`, `setCheckState`, `clearBlockedReason`
  - Use `qmuntal/stateless` with typed context
- [ ] Define domain types in `internal/models/`
  - `Repo`, `Session`, `Attempt` structs with JSON + DB tags
  - Config types: `DaemonConfig`, `OrchestratorConfig`
  - Conversion functions: proto message ↔ domain model
- [ ] Unit tests for state machine
  - Valid transitions succeed
  - Invalid events return error
  - Guard blocks transition after max attempts
  - All 11 states reachable

#### Post-Flight Checks

- [ ] `make test` passes, state machine tests cover full lifecycle
- [ ] State transitions match existing TS implementation

#### [HANDOFF] Review Flight Leg 2

Human reviews: State machine transitions, domain types, proto conversion functions

---

### Leg 3: Daemon Core — SQLite + CRUD

#### Tasks

- [ ] Implement SQLite database module in `internal/daemon/db/`
  - `modernc.org/sqlite` (pure Go, no CGO)
  - WAL mode, foreign keys enabled
  - Connection pool via `database/sql`
- [ ] Create initial daemon migration `migrations/daemon/001_initial_schema.sql`
  - `repos`, `sessions`, `attempts` tables
  - Match existing TS schema from `db-schema.ts`
- [ ] Implement shared migration runner in `internal/migrate/`
  - Uses `pressly/goose` with `go:embed` for migration files
  - `RunMigrations(db *sql.DB, fs embed.FS)` — reusable by both daemon and orchestrator
  - Logs applied migrations
- [ ] Implement stores: `RepoStore`, `SessionStore`, `AttemptStore`
  - Constructor injection: `NewRepoStore(db *sql.DB) *RepoStore`
  - Methods match existing TS stores
  - Prepared statements for hot paths
- [ ] Unit tests with in-memory SQLite
  - CRUD for all stores
  - Migration runner applies and tracks versions
  - FK cascades work (delete repo → deletes sessions)

#### Post-Flight Checks

- [ ] Tests pass with in-memory SQLite
- [ ] Migration 001 creates all tables
- [ ] `bossd` starts, creates `~/Library/Application Support/bossanova/bossd.db`

#### Critical Files

- `internal/daemon/db/db.go` — database initialization
- `internal/migrate/migrate.go` — shared migration runner
- `migrations/daemon/001_initial_schema.sql` — initial schema

#### [HANDOFF] Review Flight Leg 3

Human reviews: SQLite setup, migration runner, store implementations, test coverage

---

### Leg 4: Daemon IPC — ConnectRPC over Unix Socket

#### Tasks

- [ ] Implement ConnectRPC server in `internal/daemon/server/`
  - Listens on Unix socket: `~/Library/Application Support/bossanova/bossd.sock`
  - Implements generated `DaemonServiceHandler` interface
  - All 14 RPC methods wired to stores
- [ ] Implement context resolution handler
  - `ContextResolve(cwd)`: detect worktree → registered repo → unregistered git repo → none
  - Shell out to `git rev-parse` for detection
- [ ] Implement `AttachSession` as server-streaming RPC
  - Reads Claude output log file, streams new lines
  - Uses `fsnotify` or polling for real-time tailing
- [ ] Daemon entry point (`cmd/bossd/main.go`)
  - Parse config (flags + env vars + config file)
  - Initialize DB, run migrations, create stores
  - Start ConnectRPC server on Unix socket
  - Graceful shutdown on SIGTERM/SIGINT (cleanup socket file)
- [ ] Implement ConnectRPC client in `internal/cli/client/`
  - `NewLocalClient(socketPath)` — connects via Unix socket
  - `NewRemoteClient(url, token)` — connects via HTTPS (orchestrator, future)
  - Both implement same `BossClient` interface

#### Post-Flight Checks

- [ ] `bossd` starts, creates socket, logs startup
- [ ] `boss repo ls` returns `[]` (IPC round-trip works)
- [ ] `boss repo add . && boss repo ls` returns the registered repo
- [ ] SIGTERM cleans up socket file

#### [HANDOFF] Review Flight Leg 4

Human reviews: ConnectRPC setup, Unix socket lifecycle, context resolution, client interface

---

### Leg 5: CLI — Bubbletea + Local Mode

#### Tasks

- [ ] Implement argument parser in `cmd/boss/main.go`
  - Commands: `boss` (interactive), `boss new`, `boss ls`, `boss attach <id>`, `boss stop/pause/resume/logs/retry/close/rm <id>`, `boss repo add/ls/remove`, `boss login` (cloud only), `boss daemon install/uninstall/status`
  - Use `spf13/cobra` for arg parsing
- [ ] Implement home screen view (`internal/cli/views/home.go`)
  - Bubbletea model: session table, action bar, polling (every 2s)
  - Arrow keys navigate, Enter attaches, `n` new session, `r` add repo, `q` quit
  - State colors: green/yellow/red/gray/cyan (match existing TS)
  - Context-aware: inside registered repo → filter sessions
- [ ] Implement new session wizard (`internal/cli/views/new_session.go`)
  - Step 1: select repo (auto-detect if inside one)
  - Step 2: new PR or existing PR (list via `gh pr list`)
  - Step 3: enter plan/task (text input)
  - Step 4: confirm and create
- [ ] Implement attach view (`internal/cli/views/attach.go`)
  - Server-streaming RPC: receives `SessionEvent` messages
  - Renders Claude output with formatting
  - Session header: title, state, branch, PR link
  - Ctrl+C/Ctrl+D to detach (does not stop session)
- [ ] Implement repo management views
  - Add repo wizard, repo list table, repo remove confirmation
- [ ] `boss ls` non-interactive mode (print and exit)

#### Post-Flight Checks

- [ ] `boss` shows interactive home screen (daemon must be running)
- [ ] `boss` without daemon shows "bossd is not running — run `bossd` or `boss daemon install`"
- [ ] `boss new "test"` creates session via IPC
- [ ] `boss ls` prints session table and exits
- [ ] `boss repo add .` registers current repo

#### [HANDOFF] Review Flight Leg 5

Human reviews: CLI UX, Bubbletea views, cobra commands, IPC integration

---

### Leg 6: Git Worktree + Claude Process

#### Tasks

- [ ] Implement worktree manager in `internal/daemon/git/`
  - `CreateWorktree(repoPath, session) → worktreePath`
  - Path: `~/Library/Application Support/bossanova/worktrees/<repo-id>/<session-id>/`
  - Branch: `boss/<slug>-<short-id>`
  - Runs repo's setup script in worktree (with 120s timeout)
  - `RemoveWorktree(repoPath, worktreePath)` — `git worktree remove --force`
- [ ] Implement Claude subprocess manager in `internal/daemon/claude/`
  - `StartClaude(worktreePath, plan, sessionId) → *ClaudeProcess`
  - Spawns: `claude --print --output-format stream-json --dangerously-skip-permissions --cwd <path> -p <plan>`
  - Reads stdout via goroutine, writes to log file + in-memory ring buffer
  - `StopClaude(sessionId)` — sends SIGTERM
  - `ResumeClaude(sessionId, prompt)` — spawns with `--resume <id>`
  - Tracks process state: running, paused, completed, errored
- [ ] Implement session output logger in `internal/daemon/claude/`
  - Write Claude JSON events to `~/Library/Application Support/bossanova/logs/<session-id>.jsonl`
  - Ring buffer (last 1000 events) for fast attach streaming
  - `boss logs <session>` reads from file
- [ ] Wire into session lifecycle (`internal/daemon/session/lifecycle.go`)
  - `StartSession`: create DB record → create worktree → start Claude → update state
  - `RemoveSession`: stop Claude → remove worktree → delete DB record
  - State transitions driven by state machine

#### Post-Flight Checks

- [ ] `boss new "create a README"` → worktree created, Claude spawns, output logged
- [ ] `boss attach <id>` → live Claude output streams
- [ ] `boss stop <id>` → Claude terminated
- [ ] `boss rm <id>` → worktree cleaned up

#### [HANDOFF] Review Flight Leg 6

Human reviews: Worktree paths, Claude subprocess management, output logging, lifecycle wiring

---

### Leg 7: GitHub PR + Fix Loop

#### Tasks

- [ ] Implement GitHub client in `internal/daemon/github/`
  - Wraps `gh` CLI: `createDraftPr`, `getPrStatus`, `getPrChecks`, `markReadyForReview`, `getFailedCheckLogs`
  - Parse JSON output from `gh` commands
- [ ] Implement PR lifecycle (`internal/daemon/session/pr.go`)
  - After Claude completes: push branch → create draft PR → `awaiting_checks`
  - Poll every 60s for sessions in `awaiting_checks`/`green_draft`/`ready_for_review`
  - Checks passed + plan complete → `markReadyForReview`
  - PR merged → cleanup worktree, transition to `merged`
- [ ] Implement fix loop (`internal/daemon/session/fixloop.go`)
  - Check failure handler: fetch logs, prompt Claude to fix, push
  - Conflict handler: `git fetch` + `git merge`, prompt Claude if conflicts
  - Review handler: fetch review comments, prompt Claude with feedback
  - Concurrency guard: mutex per session (no concurrent fixes)
  - Max 5 attempts → `blocked`
- [ ] Implement event dispatcher
  - Routes events (from polling or future webhooks) to correct handler
  - Session lock prevents duplicate fix attempts

#### Post-Flight Checks

- [ ] `boss new "add hello world"` → draft PR on GitHub
- [ ] Checks fail → Claude prompted to fix → push → re-check (up to 5x)
- [ ] All checks pass → PR marked ready for review
- [ ] `make test` passes

**At this point the open-source product is complete and fully functional.**

#### [HANDOFF] Review Flight Leg 7

Human reviews: GitHub CLI integration, PR lifecycle, fix loop, polling, concurrency

---

### Leg 8: Auth + Orchestrator Core (Paid Tier)

#### Tasks

- [ ] Implement OIDC auth client in `internal/cli/auth/`
  - `boss login` → opens browser → PKCE flow → JWT stored in OS keychain (`go-keyring`)
  - `boss logout` → removes stored token
  - Token refresh on expiry
  - Auth0 recommended (native PKCE + device flow, free 25K MAU)
- [ ] Implement orchestrator entry point (`cmd/bosso/main.go`)
  - ConnectRPC server on HTTPS (port 8080)
  - SQLite database on persistent Fly.io volume
  - Litestream configured for S3/R2 replication
  - JWT middleware validates tokens on every request
  - Graceful shutdown
- [ ] Create orchestrator schema (`migrations/orchestrator/001_initial_schema.sql`)
  - `users` (id, email, provider_sub, created_at)
  - `daemons` (id, user_id, machine_name, last_seen, repos JSON)
  - `sessions` (global registry: id, user_id, daemon_id, repo, state, pr_url)
  - `audit_log` (user_id, action, details, timestamp)
- [ ] Implement orchestrator migration runner
  - Same shared `internal/migrate/` package as daemon, different embed.FS
  - `bosso migrate` CLI command for manual runs
  - Auto-migrate on startup
- [ ] Implement daemon registry in `internal/orchestrator/registry/`
  - Track online daemons with heartbeat (stale after 90s)
  - `RegisterDaemon`, `ListDaemons`, `GetDaemon` methods
- [ ] Fly.io deployment config
  - `fly.toml` with release_command: `bosso migrate`, persistent volume mount
  - Dockerfile: multi-stage Go build + Litestream sidecar
  - Litestream config: replicate to S3/R2 bucket

#### Post-Flight Checks

- [ ] `boss login` completes OAuth flow, stores JWT
- [ ] `bosso` starts, creates SQLite on volume, runs migrations
- [ ] Litestream replicates DB to S3/R2
- [ ] Daemon registers with orchestrator on startup
- [ ] Authenticated requests succeed, unauthenticated return 401

#### Critical Files

- `migrations/orchestrator/001_initial_schema.sql`
- `internal/orchestrator/auth/middleware.go`
- `internal/orchestrator/registry/registry.go`
- `fly.toml`, `Dockerfile`, `litestream.yml`

#### [HANDOFF] Review Flight Leg 8

Human reviews: Auth flow, orchestrator setup, deployment config, migration runner

---

### Leg 9: Multi-Daemon + Remote CLI + Streaming

#### Tasks

- [ ] Implement daemon → orchestrator connection in `internal/daemon/upstream/`
  - Persistent ConnectRPC connection (reconnect with exponential backoff)
  - Registration: sends daemon ID, machine name, repo list
  - Heartbeat every 30s
  - Receives events (webhooks, session transfers) via server-streaming RPC
  - Optional: daemon works fine without this connection
- [ ] Implement remote CLI mode in `internal/cli/client/`
  - `boss --remote` or auto-detect: if no local daemon, try orchestrator
  - `NewRemoteClient(orchestratorURL, jwt)` — same `BossClient` interface
  - All commands work remotely (list, attach, new, stop, etc.)
  - Orchestrator proxies requests to the target daemon
- [ ] Implement stream relay in `internal/orchestrator/relay/`
  - CLI calls `AttachSession` on orchestrator
  - Orchestrator forwards to daemon's `AttachSession` stream
  - Real-time: Claude output → daemon → orchestrator → CLI (no buffering)
  - Handles disconnect/reconnect gracefully
- [ ] Implement session transfer
  - `boss transfer <session> --to <daemon-id>`
  - Daemon A: commit + push, stop Claude, release session
  - Orchestrator: update session registry, assign to Daemon B
  - Daemon B: fetch branch, create worktree, setup, start Claude with context
  - CLI: auto-reconnects to new daemon's stream

#### Post-Flight Checks

- [ ] Daemon registers with orchestrator, shows in `boss daemons` (remote)
- [ ] `boss ls` from remote machine shows sessions on home daemon
- [ ] `boss attach <session>` from remote streams Claude output in real-time
- [ ] `boss transfer <session> --to <other-daemon>` moves session between machines

#### [HANDOFF] Review Flight Leg 9

Human reviews: Upstream connection, remote CLI, stream relay, session transfer

---

### Leg 10: Webhook Receiver

#### Tasks

- [ ] Implement GitHub webhook handler in `internal/orchestrator/webhook/`
  - `POST /webhook/github` route on orchestrator
  - HMAC-SHA256 signature verification
  - Parse: `pull_request`, `check_run`, `check_suite` events
  - Map to daemon events, route to correct daemon via registry
- [ ] Replace polling with webhook events (when cloud mode active)
  - Daemon receives events via upstream connection
  - Falls back to polling when not connected to orchestrator
- [ ] Webhook setup instructions
  - GitHub App or per-repo webhook configuration
  - Secret management via orchestrator config

#### Post-Flight Checks

- [ ] Mock webhook → event routed to correct daemon
- [ ] Valid signature → 200; invalid → 401
- [ ] Daemon receives event, triggers fix loop

#### [HANDOFF] Review Flight Leg 10

Human reviews: Webhook security, event routing, polling fallback

---

### Leg 11: Web UI

#### Tasks

- [ ] Implement web handlers in `internal/orchestrator/web/`
  - Server-rendered HTML via `templ` + `htmx` for interactivity
  - Auth: redirect to OIDC login if no session cookie
  - Routes: `/ui/sessions`, `/ui/sessions/:id`, `/ui/daemons`
- [ ] Session list page
  - Table: title, state (color-coded), repo, branch, PR link, daemon, last updated
  - Auto-refresh via htmx polling (every 5s)
  - Click to view session detail
- [ ] Session detail page
  - Status header, Claude output stream (SSE via htmx)
  - Actions: stop, pause, resume, transfer
  - PR link, check status, attempt history
- [ ] Daemon list page
  - Online daemons, machine names, repos, session counts
- [ ] Mobile-friendly layout
  - Responsive CSS (Pico CSS or similar minimal framework)
  - Touch-friendly action buttons

#### Post-Flight Checks

- [ ] `/ui/sessions` renders session list in browser
- [ ] Live Claude output streams on session detail page
- [ ] Works on mobile browser (phone on train use case)

#### [HANDOFF] Review Flight Leg 11

Human reviews: Web UI, templ templates, htmx interactivity, mobile layout

---

### Leg 12: Polish + Distribution

#### Tasks

- [ ] macOS LaunchAgent for `bossd`
  - `boss daemon install` → copies plist, `launchctl load`
  - `boss daemon uninstall` → unload + remove plist
  - `boss daemon status` → check if running
- [ ] Cross-platform builds
  - `Makefile` targets for darwin/amd64, darwin/arm64, linux/amd64
  - GitHub Actions CI: build + test + release
  - Homebrew formula for `boss` + `bossd`
- [ ] Error handling polish
  - Friendly messages: daemon not running, auth expired, network errors
  - Structured logging with session IDs throughout
- [ ] E2E integration tests
  - Full lifecycle with mock git repo and mocked Claude
  - Session transfer test with two daemon instances
  - Migration upgrade test (v1 → v2 schema)

#### Post-Flight Checks

- [ ] `boss daemon install` installs plist, `launchctl list | grep bossanova` shows daemon
- [ ] Cross-compiled binaries build for all targets
- [ ] E2E tests pass
- [ ] `make format && make lint && make test` passes

#### [HANDOFF] Final Review

Human reviews: Distribution, LaunchAgent, error handling, E2E tests

---

## Open Source Boundary

| Component | License | Distribution |
|-----------|---------|-------------|
| `boss` (CLI) | MIT | Homebrew, GitHub Releases |
| `bossd` (daemon) | MIT | Homebrew, GitHub Releases |
| `bosso` (orchestrator) | Proprietary | Hosted SaaS on Fly.io |
| `proto/` definitions | MIT | Shared (enables community tooling) |
| `internal/orchestrator/` | Proprietary | Not distributed |

The proto definitions are open so anyone can build alternative orchestrators or integrations.

## Existing TypeScript Code

The TS implementation in flight legs 1-8 serves as the **reference spec** for the Go rewrite:
- State machine states/transitions → `internal/machine/`
- Domain types → `internal/models/` + `proto/bossanova/v1/models.proto`
- RPC methods → `proto/bossanova/v1/daemon.proto`
- SQLite schema → `migrations/daemon/001_initial_schema.sql`
- Session lifecycle → `internal/daemon/session/`
- CLI views → `internal/cli/views/`

The TS code remains in the repo as reference until the Go rewrite reaches parity (end of Leg 7).

## Cost Summary (1,000 users, paid tier)

| Component | Monthly Cost |
|-----------|-------------|
| Fly.io VM (orchestrator, 2x shared-cpu-1x) | ~$4 |
| Fly.io persistent volume (1GB) | ~$0.15 |
| Litestream → S3/R2 backup | ~$0.50 |
| Auth0 (25K MAU free tier) | $0 |
| Domain + DNS | ~$1 |
| **Total** | **~$6/mo** |

## Critical Files

| File | Why It's Critical |
|------|-------------------|
| `proto/bossanova/v1/models.proto` | Shared message types every component depends on |
| `proto/bossanova/v1/daemon.proto` | CLI-daemon contract (all 14 RPC methods) |
| `internal/machine/machine.go` | State machine governing all session behavior |
| `internal/models/models.go` | Go domain types with DB + JSON tags |
| `internal/daemon/db/db.go` | SQLite initialization, WAL mode, connection pool |
| `internal/migrate/migrate.go` | Shared migration runner used by daemon + orchestrator |
| `internal/daemon/session/lifecycle.go` | Central orchestration wiring everything together |
| `internal/daemon/claude/process.go` | Claude subprocess management — process lifecycle |
| `internal/daemon/session/fixloop.go` | Automated repair cycle that makes Bossanova autonomous |
| `internal/daemon/server/server.go` | ConnectRPC server on Unix socket |
| `internal/orchestrator/registry/registry.go` | Daemon registry + presence tracking |

## Rollback Plan

Each flight leg produces independent commits. To roll back:
- `git revert` commits from the specific flight leg
- No external state mutations until Flight Leg 7+ (GitHub PRs)
- Database schema changes are additive only

## Notes

- **Bubbletea v2 for CLI:** Uses the Elm architecture (Model/Update/View). Bubbletea v2 supports concurrent commands via `tea.Cmd`, perfect for async daemon polling.
- **ConnectRPC over Unix socket:** ConnectRPC uses standard HTTP/2 semantics, which work over Unix sockets via Go's `net.Dial` with custom dialer. No TCP port needed for local mode.
- **qmuntal/stateless for state machine:** Provides typed states/triggers, guard clauses, and entry/exit actions. Closest Go equivalent to XState's declarative machine definition.
- **Pure Go SQLite (modernc.org/sqlite):** No CGO dependency means easy cross-compilation for all platforms. Performance is comparable to C SQLite for our workload (< 100 concurrent sessions).
- **Constructor injection:** Go doesn't need DI frameworks. Pass dependencies as constructor arguments: `NewSessionStore(db *sql.DB, logger zerolog.Logger)`. Interfaces for testing.
- **go:embed migrations:** Migration SQL files are embedded in the binary at compile time. No external files needed at runtime. Both daemon and orchestrator use the same `internal/migrate/` package with different embed.FS instances.
- **Litestream for orchestrator:** Continuously replicates SQLite WAL to S3/R2. On Fly.io restart, restore from replica. Cost: ~$0.50/mo for S3 storage. No need for Postgres or managed databases.
- **OIDC auth deferred to Leg 8:** Local mode (Legs 1-7) needs no auth. Cloud mode adds OIDC via Auth0 (free 25K MAU). PKCE flow for CLI, JWT validation server-side.
- **Session transfer:** The key multi-daemon feature. Daemon A pushes all changes, orchestrator updates registry, Daemon B pulls and continues. Context preserved via Claude's `--resume` flag.
- **Webhook fallback:** Local mode polls via `gh` CLI (every 60s). Cloud mode receives GitHub webhooks in real-time. Daemon gracefully falls back to polling when disconnected from orchestrator.

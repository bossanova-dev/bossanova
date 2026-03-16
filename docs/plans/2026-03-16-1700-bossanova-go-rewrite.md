# Bossanova Go Rewrite — Updated Implementation Plan

**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite

## Context

Rewriting Bossanova from TypeScript to Go. This update incorporates feedback on:

- VCS-agnostic abstractions (GitHub now, GitLab later)
- Multi-module monorepo with splitsh/lite for open-source splitting
- Worktree archive/resurrect instead of hard delete
- Timestamp-based migrations (goose default)
- Terraform infrastructure
- Client-side SPA on CF Pages (not templ+htmx)
- Idiomatic Go testing (interface-based DI, table-driven tests)
- 5-minute setup script timeout

## Key Changes from Previous Plan

| Area | Before | After |
| ---- | ------ | ----- |
| Project layout | Single go.mod | Multi-module: go.work + per-service go.mod |
| Open source split | `internal/` boundaries only | splitsh/lite mirrors services/boss, services/bossd, lib/bossalib to separate repos |
| VCS types | GitHub-specific types in daemon | VCS-agnostic interfaces in bossalib/vcs, GitHub impl in bossd |
| Migration naming | `001_initial_schema.sql` | `20260316170000_initial_schema.sql` (goose default timestamp format) |
| Worktree cleanup | `git worktree remove --force` | Archive (remove dir, keep branch) + resurrect + empty trash |
| Web UI | templ + htmx server-rendered | React SPA on CF Pages, ConnectRPC web client, Auth0 PKCE |
| Infrastructure | Manual Fly.io deploy | Terraform modules for Fly.io, Auth0, Cloudflare, R2 |
| Setup timeout | 120s | 5 minutes (300s) |
| DI pattern | Constructor injection | Same — but emphasize interface boundaries at every package edge for testability |

## Architecture

```
LOCAL (open source)                    CLOUD (paid tier)
                                    ┌──────────────────────┐
┌─────────┐   Unix Socket           │   Orchestrator       │
│ boss    │◄────────────────►bossd  │   (Go on Fly.io)     │
│ (CLI)   │   ConnectRPC    │    │  │   SQLite + Litestream│
└─────────┘                 │    │  │   Auth + Webhook     │
                            │    │  │   Web UI (CF Pages)  │
┌─────────┐   ConnectRPC    │    │  └──────────┬───────────┘
│ boss    │◄────────via orchestrator────────────┘
│ (remote)│  (authenticated)│    │
└─────────┘                 └────┘
                             bossd
                           (daemon)
```

**Local mode (free)**: `boss` ↔ `bossd` via Unix socket. No auth, no internet needed. Polling-based CI checks via `gh` CLI. Fully functional single-machine workflow.

**Cloud mode (paid)**: Daemon connects to orchestrator. Auth via OIDC (Auth0). Multi-daemon session transfer, real-time webhooks, remote CLI, web SPA for mobile.

## Updated Key Decisions

| Decision | Choice | Rationale |
| -------- | ------ | --------- |
| Language | Go everywhere (backend), React (web SPA) | Go for services, React for CF Pages SPA |
| CLI framework | Bubbletea v2 + Lipgloss + Bubbles | Best TUI ecosystem |
| RPC | ConnectRPC + Protobuf | JSON transport works in browsers natively, no gRPC-web proxy needed |
| State machine | qmuntal/stateless | Guards, actions, fluent API |
| SQLite | modernc.org/sqlite | Pure Go, no CGO |
| Migrations | pressly/goose (timestamp mode) | Embedded via go:embed, YYYYMMDDHHMMSS format |
| Auth | Auth0 OIDC | PKCE for CLI + SPA, JWT validation server-side, free 25K MAU |
| DI | Constructor injection + interfaces | Every package boundary defined by interface for mock injection |
| Logging | zerolog | Structured JSON, zero-allocation |
| Web UI | React SPA on CF Pages | ConnectRPC web client, server-streaming for live output, Auth0 SPA SDK |
| Infrastructure | Terraform | Modules for Fly.io, Auth0, Cloudflare (Pages + R2), GitHub App |
| Monorepo tooling | go.work + splitsh/lite | Multi-module local dev, automated read-only mirrors for OSS |

## Project Structure

```
bossanova/
├── go.work                        # Local dev workspace (NOT committed to split repos)
├── Makefile                       # Root: generate, build, test, lint, split
├── buf.yaml / buf.gen.yaml        # Protobuf codegen config
│
├── proto/                         # MIT — split to github.com/recurser/bossanova-proto
│   └── bossanova/v1/
│       ├── models.proto           # Domain types + VCS-agnostic event types
│       ├── daemon.proto           # DaemonService RPCs
│       └── orchestrator.proto     # OrchestratorService RPCs
│
├── lib/
│   └── bossalib/                  # MIT — split to github.com/recurser/bossalib
│       ├── go.mod                 # module github.com/recurser/bossalib
│       ├── machine/               # Session state machine
│       │   ├── machine.go
│       │   └── machine_test.go
│       ├── models/                # Domain types (Repo, Session, Attempt)
│       │   └── models.go
│       ├── vcs/                   # VCS-agnostic interfaces
│       │   ├── provider.go        # Provider interface
│       │   ├── types.go           # CheckResult, ReviewComment, PRStatus, etc.
│       │   └── events.go          # Standard VCS events (check_passed, review_submitted, etc.)
│       ├── migrate/               # Shared goose migration runner
│       │   └── migrate.go
│       └── gen/                   # Generated proto Go code
│
├── services/
│   ├── boss/                      # MIT — split to github.com/recurser/boss
│   │   ├── go.mod                 # module github.com/recurser/boss
│   │   ├── cmd/main.go
│   │   └── internal/
│   │       ├── views/             # Bubbletea views (home, new, attach, repo)
│   │       ├── auth/              # OIDC client (cloud mode)
│   │       └── client/            # ConnectRPC client (local + remote)
│   │
│   ├── bossd/                     # MIT — split to github.com/recurser/bossd
│   │   ├── go.mod                 # module github.com/recurser/bossd
│   │   ├── cmd/main.go
│   │   ├── migrations/            # Daemon SQLite migrations (go:embed)
│   │   │   └── 20260316170000_initial_schema.sql
│   │   └── internal/
│   │       ├── db/                # SQLite init + stores (RepoStore, SessionStore, AttemptStore)
│   │       ├── git/               # Worktree manager (create, archive, resurrect, empty trash)
│   │       ├── claude/            # Claude subprocess (start, stop, resume, ring buffer)
│   │       ├── vcs/               # VCS provider implementations
│   │       │   └── github/        # GitHub provider (gh CLI wrapper)
│   │       ├── session/           # Lifecycle, PR automation, fix loop, event dispatcher
│   │       ├── server/            # ConnectRPC server (Unix socket)
│   │       └── upstream/          # Optional orchestrator connection
│   │
│   ├── bosso/                     # Proprietary — NOT split, stays in monorepo only
│   │   ├── go.mod
│   │   ├── cmd/main.go
│   │   ├── migrations/
│   │   │   └── 20260316170000_initial_schema.sql
│   │   └── internal/
│   │       ├── db/                # SQLite + stores
│   │       ├── auth/              # JWT middleware (connectrpc/authn-go)
│   │       ├── registry/          # Daemon registry + heartbeat
│   │       ├── relay/             # Stream relay (daemon → CLI/web)
│   │       └── webhook/           # VCS webhook handlers (GitHub, GitLab)
│   │
│   └── web/                       # Proprietary — deployed to CF Pages
│       ├── package.json           # React + @connectrpc/connect-web + @auth0/auth0-react
│       ├── tsconfig.json
│       ├── vite.config.ts
│       ├── src/
│       │   ├── App.tsx
│       │   ├── auth/              # Auth0 PKCE provider
│       │   ├── api/               # Generated ConnectRPC TypeScript client
│       │   └── views/             # Session list, session detail (streaming), daemon list
│       └── wrangler.toml          # CF Pages deployment config
│
├── infra/                         # Terraform
│   ├── main.tf
│   ├── variables.tf
│   ├── outputs.tf
│   ├── modules/
│   │   ├── fly/                   # Fly.io app, machine, volume, secrets
│   │   ├── auth0/                 # Auth0 tenant, SPA app, API, connections
│   │   ├── cloudflare/            # CF Pages project, DNS records, R2 bucket
│   │   └── github/                # GitHub App (webhook secret, permissions)
│   └── environments/
│       ├── staging/
│       │   ├── main.tf
│       │   └── terraform.tfvars
│       └── production/
│           ├── main.tf
│           └── terraform.tfvars
│
└── docs/
```

## VCS Abstraction Layer

Defined in `lib/bossalib/vcs/` — all VCS interactions go through these interfaces:

```go
// provider.go — interface that GitHub, GitLab, etc. implement
type Provider interface {
    CreateDraftPR(ctx context.Context, opts CreatePROpts) (*PRInfo, error)
    GetPRStatus(ctx context.Context, repoPath string, prID int) (*PRStatus, error)
    GetCheckResults(ctx context.Context, repoPath string, prID int) ([]CheckResult, error)
    GetFailedCheckLogs(ctx context.Context, repoPath string, checkID string) (string, error)
    MarkReadyForReview(ctx context.Context, repoPath string, prID int) error
    GetReviewComments(ctx context.Context, repoPath string, prID int) ([]ReviewComment, error)
    ListOpenPRs(ctx context.Context, repoPath string) ([]PRSummary, error)
}

// types.go — standard types used across all providers
type PRStatus struct {
    State      PRState  // open, closed, merged
    Mergeable  *bool
    Title      string
    HeadBranch string
    BaseBranch string
}

type CheckResult struct {
    ID         string
    Name       string
    Status     CheckStatus     // completed, in_progress, queued
    Conclusion *CheckConclusion // success, failure, neutral, cancelled, skipped, timed_out
}

type ChecksOverall string // pending, passed, failed

type ReviewComment struct {
    Author string
    Body   string
    State  ReviewState // approved, changes_requested, commented, dismissed
    Path   *string     // file path for inline comments
    Line   *int        // line for inline comments
}

// events.go — standard VCS events (from polling or webhooks)
type Event interface{ vcsEvent() }

type ChecksPassed struct { PRID int }
type ChecksFailed  struct { PRID int; FailedChecks []CheckResult }
type ConflictDetected struct { PRID int }
type ReviewSubmitted struct { PRID int; Comments []ReviewComment }
type PRMerged struct { PRID int }
type PRClosed struct { PRID int }
```

Proto `models.proto` includes these as protobuf messages too, so they flow through ConnectRPC.

## Worktree Archive / Resurrect

Instead of hard-deleting worktrees, sessions can be archived:

- **Archive** (`boss archive <id>`): `git worktree remove`, keep branch alive, set `archived_at` timestamp in DB. Session state preserved. `boss ls` hides archived by default.
- **Resurrect** (`boss resurrect <id>`): `git worktree add <path> <branch>`, run setup script (5m timeout), clear `archived_at`. Session resumes from saved state.
- **Empty trash** (`boss trash empty`): For all archived sessions — delete remote branches, purge worktree refs, delete DB records. Optional `--older-than 30d` flag.
- **List archived** (`boss ls --archived`): Shows archived sessions.

DB change: `sessions` table gets `archived_at TEXT` column (nullable ISO 8601).

## Idiomatic Go Testing Pattern

Every package boundary is defined by an interface. Consumers depend on the interface, implementations are injected via constructors:

```go
// Package boundary: define interface
type SessionStore interface {
    Create(ctx context.Context, params CreateSessionParams) (*models.Session, error)
    Get(ctx context.Context, id string) (*models.Session, error)
    List(ctx context.Context, opts ListOpts) ([]*models.Session, error)
    Update(ctx context.Context, id string, fields UpdateFields) error
    Delete(ctx context.Context, id string) error
}

// Constructor injection
func NewSessionLifecycle(
    store SessionStore,        // interface
    worktrees WorktreeManager, // interface
    claude ClaudeRunner,       // interface
    vcs vcs.Provider,          // interface
    machine *machine.Machine,
    logger zerolog.Logger,
) *SessionLifecycle { ... }

// Tests use mock structs or generated mocks
type mockSessionStore struct {
    createFn func(ctx context.Context, params CreateSessionParams) (*models.Session, error)
    // ...
}
```

Table-driven tests everywhere. In-memory SQLite for store tests.

## Web SPA Architecture (CF Pages)

The web UI is a **fully client-side React SPA** deployed to Cloudflare Pages:

1. **Auth**: Auth0 SPA SDK (`@auth0/auth0-react`) handles OIDC PKCE flow in browser
2. **API**: ConnectRPC web client (`@connectrpc/connect-web`) with JSON transport — no gRPC proxy needed
3. **Streaming**: Server-streaming RPC for live Claude output (Connect protocol supports this natively via Fetch API)
4. **CORS**: Orchestrator Go backend uses `rs/cors` middleware to allow CF Pages origin
5. **Deploy**: `wrangler pages deploy` or CF Pages git integration

The SPA talks directly to the orchestrator on Fly.io. No server-rendering needed.

**Limitation**: Client streaming and bidirectional streaming don't work from browsers (Fetch API limitation). We only need server streaming (Claude output → browser), which works fine.

## Migration Strategy

### SQLite Migrations (both daemon and orchestrator)

Both `bossd` and `bosso` use SQLite with the same migration tooling:

- Migrations embedded in each binary via `go:embed`
- On startup: goose runs pending migrations automatically
- All migrations are additive (add columns/tables, never drop)
- Rollback: goose supports `down` migrations for development
- Version tracking: `goose_db_version` table
- Shared `lib/bossalib/migrate/` package provides `RunMigrations(db, embedFS)` used by both
- Timestamp-based naming: `YYYYMMDDHHMMSS_description.sql` (goose default)

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

### Leg 1: Multi-Module Scaffold + Protobuf

#### Tasks

- [ ] Create multi-module structure: go.work, per-service go.mod files for bossalib, boss, bossd, bosso
- [ ] Define protobuf in `proto/bossanova/v1/models.proto`:
  - Domain types: Repo, Session, Attempt, SessionState enum, CheckState enum
  - VCS-agnostic types: PRStatus, CheckResult, ReviewComment, VCSEvent oneof
  - Matches existing TS types
- [ ] Define `daemon.proto`: 17 RPC methods (14 from TS + archive/resurrect/emptyTrash)
  - `AttachSession` as server-streaming RPC
- [ ] Define `orchestrator.proto`: proxied methods + RegisterDaemon, TransferSession, ListDaemons
- [ ] Configure buf.yaml + buf.gen.yaml → generates into `lib/bossalib/gen/`
- [ ] Create cmd stubs: `services/boss/cmd/main.go`, `services/bossd/cmd/main.go`, `services/bosso/cmd/main.go`
- [ ] Root Makefile: `make generate`, `make build`, `make test`, `make lint`, `make split` (splitsh/lite)
- [ ] golangci-lint config (`.golangci.yml`)

#### Post-Flight Checks

- [ ] `make generate` produces Go code in `lib/bossalib/gen/`
- [ ] `make build` produces three binaries
- [ ] `make lint` passes

#### [HANDOFF] Review Flight Leg 1

Human reviews: Go module setup, protobuf definitions, Makefile targets, buf configuration

---

### Leg 2: State Machine + Domain Types + VCS Interfaces

#### Tasks

- [ ] Implement state machine in `lib/bossalib/machine/`
  - 12 states (existing 11 + `closed`), 15 event triggers (match TS exactly)
  - Guards: `hasReachedMaxAttempts`
  - Actions: `incrementAttemptCount`, `setCheckState`, `clearBlockedReason`
  - `qmuntal/stateless` with typed context
- [ ] Define domain types in `lib/bossalib/models/`
  - `Repo`, `Session` (with `ArchivedAt *time.Time`), `Attempt` structs
  - Config types: `DaemonConfig`, `OrchestratorConfig`
  - Proto ↔ model conversion functions
- [ ] Define VCS interfaces in `lib/bossalib/vcs/`
  - `Provider` interface, standard types (PRStatus, CheckResult, ReviewComment, etc.)
  - Standard event types (ChecksPassed, ChecksFailed, ConflictDetected, ReviewSubmitted, PRMerged, PRClosed)
- [ ] Unit tests: state machine full lifecycle, all 12 states reachable, guard behavior

#### Post-Flight Checks

- [ ] `make test` passes. State transitions match TS implementation.

#### [HANDOFF] Review Flight Leg 2

Human reviews: State machine transitions, domain types, VCS interfaces, proto conversion functions

---

### Leg 3: Daemon Core — SQLite + CRUD

#### Tasks

- [ ] SQLite module in `services/bossd/internal/db/` — modernc.org/sqlite, WAL mode, FKs
- [ ] Initial migration `services/bossd/migrations/20260316170000_initial_schema.sql`
  - `repos`, `sessions` (with `archived_at`), `attempts` tables — match TS schema
- [ ] Shared migration runner in `lib/bossalib/migrate/` using goose + go:embed
- [ ] Store interfaces + implementations: `RepoStore`, `SessionStore`, `AttemptStore`
  - All stores accept `*sql.DB` via constructor
  - SessionStore: `ListActive` (excludes archived), `ListArchived`, `Archive`, `Resurrect`
- [ ] Unit tests with in-memory SQLite — CRUD, FK cascades, migration runner

#### Post-Flight Checks

- [ ] Tests pass. `bossd` creates DB at `~/Library/Application Support/bossanova/bossd.db`.

#### Critical Files

- `services/bossd/internal/db/db.go` — database initialization
- `lib/bossalib/migrate/migrate.go` — shared migration runner
- `services/bossd/migrations/20260316170000_initial_schema.sql` — initial schema

#### [HANDOFF] Review Flight Leg 3

Human reviews: SQLite setup, migration runner, store implementations, test coverage

---

### Leg 4: Daemon IPC — ConnectRPC over Unix Socket

#### Tasks

- [ ] ConnectRPC server in `services/bossd/internal/server/` — Unix socket
- [ ] Implement all DaemonService RPCs wired to stores
- [ ] Context resolution: detect worktree → repo → unregistered git repo → none
- [ ] `AttachSession` server-streaming RPC (tail log file + fsnotify)
- [ ] Daemon entry point: config, DB, migrations, stores, server, graceful shutdown
- [ ] ConnectRPC client in `services/boss/internal/client/` — `BossClient` interface, local + remote impls

#### Post-Flight Checks

- [ ] `bossd` starts, `boss repo ls` returns `[]` via IPC, SIGTERM cleans up socket.

#### [HANDOFF] Review Flight Leg 4

Human reviews: ConnectRPC setup, Unix socket lifecycle, context resolution, client interface

---

### Leg 5: CLI — Bubbletea + Local Mode

#### Tasks

- [ ] Cobra arg parser: all commands including `boss archive/resurrect/trash`
- [ ] Home screen (Bubbletea): session table, action bar, 2s polling, keyboard nav
- [ ] New session wizard: repo select → new/existing PR → plan input → confirm
- [ ] Attach view: server-streaming, Claude output, session header, Ctrl+C detach
- [ ] Repo management views, `boss ls` non-interactive mode
- [ ] Archive/resurrect/trash commands

#### Post-Flight Checks

- [ ] Full interactive TUI works. `boss` without daemon shows helpful error.

#### [HANDOFF] Review Flight Leg 5

Human reviews: CLI UX, Bubbletea views, cobra commands, IPC integration

---

### Leg 6: Git Worktree + Claude Process

#### Tasks

- [ ] `WorktreeManager` interface in bossd: Create, Remove, Archive, Resurrect, EmptyTrash
  - Create: `git worktree add`, run setup script (5m timeout)
  - Archive: `git worktree remove`, keep branch, update DB
  - Resurrect: `git worktree add <path> <existing-branch>`, run setup script
  - EmptyTrash: delete remote branches, purge DB records
- [ ] Claude subprocess manager: Start, Stop, Resume via `claude` CLI
  - Stdout → log file + in-memory ring buffer (1000 events)
- [ ] Wire into session lifecycle: StartSession, RemoveSession, ArchiveSession

#### Post-Flight Checks

- [ ] `boss new "create a README"` → worktree + Claude. `boss archive/resurrect` works.

#### [HANDOFF] Review Flight Leg 6

Human reviews: Worktree paths, Claude subprocess management, output logging, lifecycle wiring

---

### Leg 7: VCS Provider (GitHub) + PR + Fix Loop

#### Tasks

- [ ] GitHub provider implementing `vcs.Provider` interface — wraps `gh` CLI
- [ ] PR lifecycle: push → draft PR → awaiting_checks → poll 60s → ready_for_review → merged
- [ ] Fix loop: check failure handler, conflict handler, review handler
  - Mutex per session, max 5 attempts → blocked
- [ ] Event dispatcher: routes VCS events to correct handler

#### Post-Flight Checks

- [ ] Full PR lifecycle works. Fix loop retries up to 5x. `make test` passes.

**Open-source product is complete and fully functional at this point.**

#### [HANDOFF] Review Flight Leg 7

Human reviews: GitHub CLI integration, PR lifecycle, fix loop, polling, concurrency

---

### Leg 8: Auth + Orchestrator Core + Terraform

#### Tasks

- [ ] OIDC auth client (`boss login/logout`) — Auth0 PKCE, JWT in OS keychain
- [ ] Orchestrator entry point: ConnectRPC on HTTPS, SQLite + Litestream, JWT middleware
- [ ] Orchestrator schema: users, daemons, sessions (registry), audit_log
- [ ] Daemon registry: heartbeat tracking, RegisterDaemon, ListDaemons
- [ ] Terraform modules:
  - `infra/modules/fly/` — Fly.io app, machine config, persistent volume, secrets
  - `infra/modules/auth0/` — tenant, SPA application, API audience, connections
  - `infra/modules/cloudflare/` — R2 bucket for Litestream, DNS records
  - `infra/modules/github/` — GitHub App with webhook secret
  - Staging + production environments

#### Post-Flight Checks

- [ ] `boss login` works. `bosso` starts + migrates. Terraform applies cleanly.

#### [HANDOFF] Review Flight Leg 8

Human reviews: Auth flow, orchestrator setup, Terraform modules, deployment config

---

### Leg 9: Multi-Daemon + Remote CLI + Streaming

#### Tasks

- [ ] Daemon → orchestrator upstream connection (reconnect with backoff, heartbeat 30s)
- [ ] Remote CLI: `boss --remote` or auto-detect, same BossClient interface
- [ ] Stream relay: orchestrator proxies AttachSession stream (daemon → CLI)
- [ ] Session transfer: commit+push on A, registry update, fetch+worktree on B

#### Post-Flight Checks

- [ ] Remote `boss ls` and `boss attach` work. Session transfer between daemons.

#### [HANDOFF] Review Flight Leg 9

Human reviews: Upstream connection, remote CLI, stream relay, session transfer

---

### Leg 10: Webhook Receiver (VCS-agnostic)

#### Tasks

- [ ] Webhook handler in orchestrator: HMAC verification, parse GitHub events
- [ ] Map to standard VCS events, route to daemon via registry
- [ ] Extensible: webhook parser interface for future GitLab support
- [ ] Falls back to polling when not connected to orchestrator

#### Post-Flight Checks

- [ ] Mock webhook → event routed to daemon. Invalid signature → 401.

#### [HANDOFF] Review Flight Leg 10

Human reviews: Webhook security, event routing, polling fallback

---

### Leg 11: Web SPA (CF Pages)

#### Tasks

- [ ] React SPA: Vite + React + @connectrpc/connect-web + @auth0/auth0-react
- [ ] Auth0 PKCE flow in browser → JWT → ConnectRPC interceptor
- [ ] Session list page (polling), session detail (server-streaming Claude output)
- [ ] Daemon list page, session actions (stop, pause, resume, transfer)
- [ ] CORS middleware on orchestrator (`rs/cors`)
- [ ] CF Pages deployment via wrangler or git integration
- [ ] Terraform: add `infra/modules/cloudflare/` CF Pages project

#### Post-Flight Checks

- [ ] SPA loads on CF Pages, authenticates, streams Claude output from orchestrator.

#### [HANDOFF] Review Flight Leg 11

Human reviews: React SPA, ConnectRPC web client, Auth0 integration, CF Pages deployment

---

### Leg 12: Polish + Distribution + splitsh/lite

#### Tasks

- [ ] macOS LaunchAgent: `boss daemon install/uninstall/status`
- [ ] Cross-platform builds (darwin/amd64, darwin/arm64, linux/amd64)
- [ ] GitHub Actions CI: build + test + release + splitsh/lite mirrors
- [ ] splitsh/lite config: mirror boss, bossd, bossalib, proto to separate repos
- [ ] Homebrew formula for boss + bossd
- [ ] E2E integration tests (mock git repo + mock Claude)
- [ ] Error handling polish, structured logging

#### Post-Flight Checks

- [ ] `make split` mirrors to public repos. Homebrew install works. E2E passes.

#### [HANDOFF] Final Review

Human reviews: Distribution, splitsh/lite, CI/CD, E2E tests, LaunchAgent

---

## Open Source Boundary (via splitsh/lite)

| Monorepo Path | Split Repo | License |
| ------------- | ---------- | ------- |
| `proto/` | github.com/recurser/bossanova-proto | MIT |
| `lib/bossalib/` | github.com/recurser/bossalib | MIT |
| `services/boss/` | github.com/recurser/boss | MIT |
| `services/bossd/` | github.com/recurser/bossd | MIT |
| `services/bosso/` | _(not split)_ | Proprietary |
| `services/web/` | _(not split)_ | Proprietary |
| `infra/` | _(not split)_ | Proprietary |

## Cost Summary (1,000 users, paid tier)

| Component | Monthly Cost |
| --------- | ------------ |
| Fly.io VM (orchestrator, 2x shared-cpu-1x) | ~$4 |
| Fly.io persistent volume (1GB) | ~$0.15 |
| Litestream → S3/R2 backup | ~$0.50 |
| Auth0 (25K MAU free tier) | $0 |
| Cloudflare Pages (free tier) | $0 |
| Domain + DNS | ~$1 |
| **Total** | **~$6/mo** |

## Critical Files

| File | Why It's Critical |
| ---- | ----------------- |
| `proto/bossanova/v1/models.proto` | Shared message types every component depends on |
| `proto/bossanova/v1/daemon.proto` | CLI-daemon contract (17 RPC methods) |
| `lib/bossalib/machine/machine.go` | State machine governing all session behavior |
| `lib/bossalib/models/models.go` | Go domain types |
| `lib/bossalib/vcs/provider.go` | VCS-agnostic interface for GitHub/GitLab |
| `lib/bossalib/migrate/migrate.go` | Shared migration runner used by daemon + orchestrator |
| `services/bossd/internal/db/db.go` | SQLite initialization, WAL mode, connection pool |
| `services/bossd/internal/session/lifecycle.go` | Central orchestration wiring everything together |
| `services/bossd/internal/claude/process.go` | Claude subprocess management — process lifecycle |
| `services/bossd/internal/session/fixloop.go` | Automated repair cycle that makes Bossanova autonomous |
| `services/bossd/internal/server/server.go` | ConnectRPC server on Unix socket |
| `services/bosso/internal/registry/registry.go` | Daemon registry + presence tracking |

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
- **go:embed migrations:** Migration SQL files are embedded in the binary at compile time. No external files needed at runtime. Both daemon and orchestrator use the same `lib/bossalib/migrate/` package with different embed.FS instances.
- **Litestream for orchestrator:** Continuously replicates SQLite WAL to S3/R2. On Fly.io restart, restore from replica. Cost: ~$0.50/mo for S3 storage. No need for Postgres or managed databases.
- **OIDC auth deferred to Leg 8:** Local mode (Legs 1-7) needs no auth. Cloud mode adds OIDC via Auth0 (free 25K MAU). PKCE flow for CLI, JWT validation server-side.
- **Session transfer:** The key multi-daemon feature. Daemon A pushes all changes, orchestrator updates registry, Daemon B pulls and continues. Context preserved via Claude's `--resume` flag.
- **Webhook fallback:** Local mode polls via `gh` CLI (every 60s). Cloud mode receives GitHub webhooks in real-time. Daemon gracefully falls back to polling when disconnected from orchestrator.
- **splitsh/lite:** Automated read-only mirrors of monorepo subtrees to separate Git repos. Runs in CI via `make split`. Users install from split repos; development happens in monorepo.

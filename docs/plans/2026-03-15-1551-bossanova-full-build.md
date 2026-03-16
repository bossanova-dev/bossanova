# Bossanova Implementation Plan

**Flight ID:** fp-2026-03-15-1551-bossanova-full-build

## Context

Bossanova is a CLI-first orchestrator for managing multiple Claude Code sessions, each mapped to a GitHub PR. The system automatically fixes failing CI, resolves conflicts, and addresses review feedback — keeping PRs green 24/7 without human intervention.

This plan covers the full build from monorepo scaffolding through automated fix loops, split into 12 flight legs.

## Key Architectural Decisions

| Decision              | Choice                         | Rationale                                                                                    |
| --------------------- | ------------------------------ | -------------------------------------------------------------------------------------------- |
| Runtime               | Node.js everywhere             | Ink (CLI framework) has yoga-layout issues with Bun                                          |
| Package manager       | pnpm                           | User preference, workspace support                                                           |
| Linting               | Biome (not ESLint)             | User preference                                                                              |
| Formatting            | Prettier + Biome               | Consistent with madverts/core pattern                                                        |
| Testing               | Vitest                         | Modern, fast, TypeScript-native                                                              |
| SQLite                | better-sqlite3                 | Node.js compatible (no bun:sqlite)                                                           |
| SQLite migrations     | Yes — versioned migrations     | Schema evolves over time; `schema_version` table tracks current version                      |
| State machine         | XState v5                      | Production-grade state machine with `setup().createMachine()` pattern; follows madverts/core |
| Dependency injection  | tsyringe                       | Lightweight DI with decorators; follows madverts/core services/flows pattern                 |
| CLI framework         | Ink v6                         | React-based terminal UI                                                                      |
| Claude integration    | @anthropic-ai/claude-agent-sdk | Official SDK                                                                                 |
| Cloud services        | Cloudflare Workers + Hono      | 2 separate Workers (webhook + orchestrator)                                                  |
| Transport             | WebSocket + TLS (v1)           | Daemon opens persistent WSS to orchestrator                                                  |
| Encryption            | E2E deferred to v2 (iOS)       | v1 uses TLS only; app-layer E2E encryption added when a second peer (iOS app) exists         |
| Multiplexing          | Frame-based over WebSocket     | Channel 0=control, 1=PTY, 2=chat                                                             |
| Scheduled sessions    | Deferred to future phase       | Focus on webhook-triggered automation first                                                  |
| QUIC/WebRTC           | Deferred to future phase       | WebSocket is production-ready; QUIC/WebRTC for iOS P2P later                                 |
| Worktree setup script | Per-repo configurable          | Repos can define a setup script (e.g. `pnpm install`) run after worktree creation            |

## Monorepo Structure

```
bossanova/
├── package.json              # pnpm workspaces
├── pnpm-workspace.yaml
├── tsconfig.json             # Base TypeScript config
├── biome.json                # Linting
├── .prettierrc               # Formatting
├── Makefile                  # Delegates to services
├── .gitignore
├── lib/
│   └── shared/               # @bossanova/shared — types, schemas, IPC client
├── services/
│   ├── cli/                  # @bossanova/cli — `boss` executable (Ink + Node.js)
│   ├── daemon/               # @bossanova/daemon — `bossd` background service
│   ├── webhook/              # @bossanova/webhook — GitHub webhook receiver (CF Worker)
│   └── orchestrator/         # @bossanova/orchestrator — event routing (CF Worker)
└── docs/
    └── plans/
```

Follows the madverts/core pattern: root workspace delegates `make format/lint/test/dev/build` to each service's Makefile. Each service has its own `package.json`, `tsconfig.json`, `Makefile`, and `src/` directory.

## Affected Areas

- `lib/shared/` — Shared types, state machine, RPC schema, IPC client, WebSocket protocol
- `services/cli/` — Ink TUI client, argument parsing, session views, attach mode
- `services/daemon/` — SQLite database, IPC server, Git worktree management, Claude supervisor, fix loop, WebSocket client
- `services/webhook/` — GitHub webhook verification, event parsing (Hono + CF Worker)
- `services/orchestrator/` — Daemon registry, event routing, Durable Objects for WebSocket (Hono + CF Worker)

---

## Flight Leg 1: Monorepo Scaffold

### Tasks

- [ ] Create root `package.json` with pnpm workspace declarations for all 5 packages
  - Files: `package.json`, `pnpm-workspace.yaml`
  - Pattern: Follow madverts/core — `"private": true`, `"type": "module"`, devDependencies: `typescript`, `vitest`, `@biomejs/biome`, `prettier`
  - Key shared deps: `xstate@^5`, `tsyringe@^4`, `reflect-metadata`
- [ ] Create root `tsconfig.json` with base compiler options
  - Files: `tsconfig.json`
  - ES2022 target, ESNext module, bundler moduleResolution, strict, noEmit, `jsx: "react-jsx"`
- [ ] Create `biome.json` with linting and formatting rules
  - Files: `biome.json`
  - organizeImports enabled, recommended linter rules, consistent formatting
- [ ] Create root `Makefile` delegating to each service, plus `.gitignore` and `.prettierrc`
  - Files: `Makefile`, `.gitignore`, `.prettierrc`
  - Targets: `format`, `lint`, `test`, `build` — each delegates via `make -C services/cli/ format` etc.
- [ ] Create per-service scaffolds: `package.json`, `tsconfig.json`, `Makefile`, `src/index.ts`
  - `services/cli/`: deps `ink@^6`, `react@^19`; bin entry `boss`; Makefile uses `npx tsx` for dev
  - `services/daemon/`: deps `better-sqlite3`; Makefile uses `npx tsx` for dev
  - `services/webhook/`: deps `hono`, `wrangler`, `@cloudflare/workers-types`; `wrangler.toml`; Makefile uses `npx wrangler dev`
  - `services/orchestrator/`: same pattern as webhook, separate `wrangler.toml`
  - `lib/shared/`: no runtime deps initially; barrel export from `src/index.ts`

### Post-Flight Checks

- [ ] `pnpm install` completes without errors, all workspace packages linked
- [ ] `make build` compiles all services without TypeScript errors
- [ ] `npx tsx services/cli/src/index.tsx` renders "boss" text in terminal via Ink
- [ ] `npx tsx services/daemon/src/index.ts` outputs startup message
- [ ] `cd services/webhook && npx wrangler dev` starts, `curl http://localhost:8787/` returns "ok"
- [ ] `make format && make lint` passes from root

### [HANDOFF] Review Flight Leg 1

Human reviews: Monorepo structure, naming conventions, Makefile targets, biome/prettier config

---

## Flight Leg 2: Shared Types and Schemas

### Tasks

- [ ] Define session state machine using XState v5
  - Files: `lib/shared/src/session-machine.ts`
  - Use `setup().createMachine()` pattern (same as madverts/core welcome-web)
  - States enum: `creating_worktree`, `starting_claude`, `pushing_branch`, `opening_draft_pr`, `implementing_plan`, `awaiting_checks`, `fixing_checks`, `green_draft`, `ready_for_review`, `blocked`, `merged`, `closed`
  - Events enum: `WORKTREE_CREATED`, `CLAUDE_STARTED`, `BRANCH_PUSHED`, `PR_OPENED`, `PLAN_COMPLETE`, `CHECKS_PASSED`, `CHECKS_FAILED`, `CONFLICT_DETECTED`, `REVIEW_SUBMITTED`, `FIX_COMPLETE`, `FIX_FAILED`, `BLOCK`, `UNBLOCK`, `PR_MERGED`, `PR_CLOSED`
  - Context type: session metadata (repoId, title, plan, worktreePath, branchName, prNumber, attemptCount, blockedReason, etc.)
  - Guards: `hasReachedMaxAttempts`, `isPlanComplete`, `areChecksGreen`
  - Actions: `incrementAttemptCount`, `setBlockedReason`, `clearBlockedReason`, `updatePrInfo`
  - Actors (via `fromCallback`): `createWorktree`, `startClaude`, `pushBranch`, `openDraftPr`, `pollChecks`, `fixChecks`, `resolveConflict`, `handleReview` — these are defined as string references in `setup()` and provided at runtime by the daemon
  - Export the machine definition, state enum, event types, and context type
- [ ] Define core domain types (Repo, Session) and database row types
  - Files: `lib/shared/src/types.ts`, `lib/shared/src/db-schema.ts`
  - `Repo` interface: id, displayName, localPath, originUrl, defaultBaseBranch, worktreeBaseDir, setupScript (optional — run after worktree creation), timestamps
  - `Session` interface: id, repoId, title, plan, worktreePath, branchName, baseBranch, state, claudeSessionId, prNumber, prUrl, lastCheckState, automationEnabled, attemptCount, blockedReason, timestamps
  - SQL `CREATE TABLE` statements as string constants for `repos`, `sessions`, `attempts`, `schema_version`
  - `RepoRow`, `SessionRow`, `AttemptRow` TypeScript interfaces matching SQL columns
  - Include migration SQL arrays: `MIGRATIONS: string[]` — each entry is a SQL migration, indexed by version number
- [ ] Define JSON-RPC schema for CLI-daemon IPC
  - Files: `lib/shared/src/rpc.ts`
  - JSON-RPC 2.0 envelope: `{ jsonrpc: "2.0", method, params, id }`
  - Methods: `context.resolve`, `repo.register`, `repo.list`, `repo.remove`, `repo.listPrs` (fetch open PRs from GitHub for a repo), `session.list`, `session.create` (accepts optional `prNumber` for existing PR), `session.get`, `session.attach`, `session.stop`, `session.pause`, `session.resume`, `session.retry`, `session.close`, `session.remove`
  - Typed request params and response types for each method
- [ ] Define webhook event types and daemon event types
  - Files: `lib/shared/src/webhook-events.ts`
  - `WebhookEvent` union: `pull_request`, `check_run`, `check_suite` actions
  - `DaemonEvent`: simplified event type sent to daemon: `{ type: 'check_failed' | 'pr_updated' | 'conflict_detected' | 'review_submitted', sessionId, payload }`
- [ ] Define WebSocket protocol frame types
  - Files: `lib/shared/src/ws-protocol.ts`
  - Frame format: `[1-byte channel][4-byte length][payload]`
  - Channels: 0=control, 1=PTY, 2=chat
  - Control message types: `register`, `heartbeat`, `event`, `ack`
  - Encode/decode functions for frames

### Post-Flight Checks

- [ ] `make build` in `lib/shared/` — TypeScript compiles without errors
- [ ] XState machine has all 12 states; transitions cover the full lifecycle including fix loop (`awaiting_checks` <-> `fixing_checks`)
- [ ] Machine can be instantiated with `createActor(sessionMachine)` and stepped through valid transitions
- [ ] Invalid events in wrong states are ignored (XState default behavior)
- [ ] Session interface fields align with SessionRow columns
- [ ] Every CLI command from the spec has a corresponding RPC method
- [ ] Migration array includes initial schema as version 1
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 2

Human reviews: Type definitions, state machine transitions, RPC method signatures, SQLite schema, WebSocket protocol

---

## Flight Leg 3: Daemon Core — DI Container, SQLite, and Session CRUD

### Tasks

- [ ] Set up tsyringe DI container for daemon
  - Files: `services/daemon/src/di/tokens.ts`, `services/daemon/src/di/container.ts`
  - Pattern: Follow madverts/core services/flows — Symbol-based tokens, `setupContainer()` function
  - Tokens: `Service.Database`, `Service.RepoStore`, `Service.SessionStore`, `Service.AttemptStore`, `Service.Config`, `Service.Logger`
  - Singletons: Database, Config, Logger
  - Transients: RepoStore, SessionStore, AttemptStore
  - All services use `@injectable()` and `@inject(Service.X)` decorators
- [ ] Implement SQLite database module with versioned migrations
  - Files: `services/daemon/src/db/database.ts`
  - Use `better-sqlite3` (synchronous API), registered as `@injectable()` singleton
  - `DatabaseService.initialize(dbPath)` — creates DB, runs migrations from `MIGRATIONS` array in `@bossanova/shared`, enables WAL mode, foreign keys
  - Migration runner: reads `schema_version` table, applies any migrations with version > current, updates version
  - Default DB path: `~/Library/Application Support/bossanova/bossd.db`
- [ ] Implement repository CRUD as injectable service
  - Files: `services/daemon/src/db/repos.ts`
  - `@injectable() class RepoStore` with `@inject(Service.Database)` in constructor
  - Methods: `register(params): Repo`, `list(): Repo[]`, `get(id): Repo | null`, `remove(id): void`, `findByPath(path): Repo | null`
  - Use prepared statements for performance
- [ ] Implement session CRUD as injectable service (state changes driven by XState)
  - Files: `services/daemon/src/db/sessions.ts`
  - `@injectable() class SessionStore`
  - Methods: `create(params): Session`, `list(repoId?): Session[]`, `get(id): Session | null`, `update(id, fields): void`, `delete(id): void`
  - State changes are NOT validated here — XState machine is the authority on valid transitions. The store persists whatever state the machine resolves to.
  - Attempt tracking: `recordAttempt(sessionId, trigger): Attempt`, `completeAttempt(attemptId, result, error?): void`, `getAttempts(sessionId): Attempt[]`
- [ ] Write unit tests for all database operations
  - Files: `services/daemon/src/db/__tests__/database.test.ts`, `repos.test.ts`, `sessions.test.ts`
  - Use in-memory SQLite (`:memory:`) for tests
  - Test CRUD for repos and sessions, migration runner, DI container resolution
  - Test XState machine separately: valid transitions succeed, invalid events are ignored

### Post-Flight Checks

- [ ] `make test` in `services/daemon/` — all tests pass
- [ ] DI container resolves all registered services without errors
- [ ] In-memory test creates all tables, schema_version = 1
- [ ] Create repo → create session → list by repo → update → delete — all work
- [ ] Migration runner applies new migrations and skips already-applied ones
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 3

Human reviews: Database implementation, prepared statements, state machine enforcement, test coverage

---

## Flight Leg 4: Daemon IPC — Unix Socket Server

### Tasks

- [ ] Implement Unix socket JSON-RPC server as injectable service
  - Files: `services/daemon/src/ipc/server.ts`
  - `@injectable() class IpcServer` with `@inject(Service.Dispatcher)` in constructor
  - Use Node.js `net.createServer` with Unix domain socket
  - Socket path: `~/Library/Application Support/bossanova/bossd.sock`
  - Newline-delimited JSON-RPC 2.0 messages
- [ ] Implement RPC method dispatcher as injectable service
  - Files: `services/daemon/src/ipc/dispatcher.ts`
  - `@injectable() class Dispatcher` with injected stores (`@inject(Service.RepoStore)`, `@inject(Service.SessionStore)`, etc.)
  - Map method names to handler methods
  - JSON-RPC error responses for invalid method, invalid params, internal errors
- [ ] Implement context resolution logic
  - Files: `services/daemon/src/ipc/handlers/context.ts`
  - `handleContextResolve(db, { cwd })` — detection priority: (1) inside boss worktree? (2) inside registered repo? (3) inside unregistered Git repo? (4) none
  - Uses `git rev-parse --show-toplevel` and `git rev-parse --git-common-dir`
- [ ] Implement daemon main entry point with graceful shutdown
  - Files: `services/daemon/src/index.ts`
  - Initialize database, start IPC server, handle SIGTERM/SIGINT
  - Create `~/Library/Application Support/bossanova/` directory if needed
  - Clean up socket file on shutdown
- [ ] Implement IPC client utility for CLI
  - Files: `lib/shared/src/ipc-client.ts`
  - `createIpcClient(socketPath?): IpcClient` — typed methods for all RPC calls
  - Sends JSON-RPC over Unix socket, awaits response
  - Handles "daemon not running" error gracefully

### Post-Flight Checks

- [ ] Daemon starts: `npx tsx services/daemon/src/index.ts` creates socket file, logs startup
- [ ] IPC round-trip: start daemon → `repo.list` returns `[]` → `repo.register` returns repo → `repo.list` returns `[repo]`
- [ ] Context resolution: from inside a Git repo returns repo path; from outside returns `none`
- [ ] SIGTERM → socket file cleaned up
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 4

Human reviews: IPC protocol, error handling, context resolution, socket lifecycle

---

## Flight Leg 5: CLI Basics — Ink Rendering, DI, and Daemon Connection

### Tasks

- [ ] Set up tsyringe DI container for CLI
  - Files: `services/cli/src/di/tokens.ts`, `services/cli/src/di/container.ts`
  - Tokens: `Service.IpcClient`, `Service.Config`, `Service.Logger`
  - Follow same pattern as daemon DI — `setupContainer()` function
  - IpcClient registered as singleton
- [ ] Set up CLI entry point with argument parsing
  - Files: `services/cli/src/cli.tsx`
  - Hashbang `#!/usr/bin/env node`, parse `process.argv`
  - Commands: `boss` (default — interactive), `boss new [plan]`, `boss ls`, `boss attach <id>`, `boss stop/pause/resume/logs/retry/close/rm <id>`, `boss repo add/ls/remove`
  - Route to appropriate Ink component
  - Initialize DI container before rendering
- [ ] Implement interactive home screen (the `boss` default command)
  - Files: `services/cli/src/views/HomeScreen.tsx`
  - The main TUI hub. Shows:
    1. **Session list** — live-updating table (polls daemon every 2s), arrow keys to navigate, Enter to attach
    2. **Action bar** at bottom with keyboard shortcuts: `n` = New Session, `r` = Add Repository, `q` = Quit
  - Table columns: ID (short), Title, State (color-coded), Branch, PR#, Last Updated
  - Colors: green=`green_draft`/`ready_for_review`/`merged`, yellow=`implementing_plan`/`awaiting_checks`, red=`blocked`/`fixing_checks`, gray=`closed`
  - Context-aware: inside a repo → show that repo's sessions; otherwise → show all across repos
  - `boss ls` is the non-interactive (one-shot print) variant
- [ ] Implement guided "New Session" flow
  - Files: `services/cli/src/views/NewSession.tsx`
  - Triggered by `n` from home screen or `boss new` command
  - Step-by-step wizard:
    1. **Select repo** — if inside a registered repo, auto-select; if inside unregistered repo, offer to register; otherwise show repo picker
    2. **Choose mode** — "New PR" (create fresh branch) or "Existing PR" (pick from open PRs via `gh pr list`)
    3. If existing PR: show selectable list of open PRs fetched from GitHub
    4. **Enter plan/task** — free-text input describing what Claude should do
    5. **Confirm** — summary of repo, branch, plan; Enter to start
  - Calls `client.sessionCreate(repoId, { plan, prNumber? })`
- [ ] Implement guided "Add Repository" flow
  - Files: `services/cli/src/views/AddRepo.tsx`
  - Triggered by `r` from home screen or `boss repo add` command
  - Step-by-step wizard:
    1. **Enter path** — defaults to cwd if inside a Git repo, otherwise prompt for path
    2. **Confirm repo** — show detected repo name, origin URL, default branch
    3. **Setup script** — prompt: "Enter a setup script to run in new worktrees (e.g. `pnpm install`), or leave blank to skip"
    4. **Confirm** — summary; Enter to register
  - Calls `client.repoRegister(path, { setupScript? })`
- [ ] Implement `boss repo ls` and `boss repo remove`
  - Files: `services/cli/src/views/RepoList.tsx`
  - `boss repo ls` — table of repos (ID, Name, Path, Default Branch, Setup Script)
  - `boss repo remove <id>` — confirm and remove
- [ ] Connect CLI to daemon via IPC client
  - Files: `services/cli/src/client.ts`
  - Import `createIpcClient` from `@bossanova/shared`
  - Handle "daemon not running" with helpful message
  - Registered as singleton in DI container

### Post-Flight Checks

- [ ] `boss` with no daemon shows "bossd is not running" message
- [ ] `boss` with daemon running shows interactive home screen (session list + action bar)
- [ ] Arrow keys navigate sessions, `q` quits
- [ ] `n` opens new session wizard: repo selection → mode (new/existing PR) → plan input → confirm
- [ ] `r` opens add repository wizard: path → confirm details → setup script prompt → registered
- [ ] `boss ls` prints session list non-interactively and exits
- [ ] `boss repo ls` shows registered repos with setup script column
- [ ] `boss new "test plan"` creates session record (stub — no Git/Claude work yet)
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 5

Human reviews: CLI UX, Ink components, argument parsing, IPC integration

---

## Flight Leg 6: Git Worktree Management

### Tasks

- [ ] Implement worktree creation with setup script support
  - Files: `services/daemon/src/git/worktree.ts`
  - `createWorktree(repoPath, session): Promise<string>` — returns worktree path
  - Path: `~/Library/Application Support/bossanova/worktrees/<repo-id>/<session-id>/`
  - Branch: `boss/<slug>-<short-id>` (slug from title, kebab-case, max 30 chars)
  - Runs: `git worktree add <path> -b <branch>` from repo root
  - After creation: if repo has `setupScript` configured, execute it in the new worktree (e.g. `pnpm install`, `make setup`)
  - Setup script is configured per-repo via `boss repo add --setup "pnpm install"` or `boss repo setup <repo> "pnpm install"`
- [ ] Implement worktree cleanup
  - Files: `services/daemon/src/git/worktree.ts` (same file)
  - `removeWorktree(repoPath, worktreePath): Promise<void>`
  - Runs `git worktree remove <path> --force`, cleans up directory
- [ ] Implement Git utilities
  - Files: `services/daemon/src/git/utils.ts`
  - `getCurrentSha`, `getOriginUrl`, `getDefaultBranch`, `isInsideWorktree`, `getGitCommonDir`, `fetchLatest`, `hasConflictsWithBase`
- [ ] Implement branch push module
  - Files: `services/daemon/src/git/push.ts`
  - `pushBranch(worktreePath, branchName): Promise<string>` — runs `git push -u origin <branch>`, returns HEAD SHA
- [ ] Wire worktree into session creation lifecycle
  - Files: `services/daemon/src/session/lifecycle.ts`
  - `startSession(db, repoId, plan)`: create session record → create worktree → update state to `starting_claude` (Claude step comes next leg)
  - Wire `handleSessionCreate` to call `startSession`
  - Wire `handleSessionRemove` to call `removeWorktree`

### Post-Flight Checks

- [ ] `boss new "test plan"` creates a git worktree at expected path, creates `boss/*` branch
- [ ] `git worktree list` from main repo shows the new worktree
- [ ] `git branch` shows the `boss/<slug>-<id>` branch
- [ ] `boss rm <session>` removes worktree and cleans up
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 6

Human reviews: Worktree paths, branch naming, error handling for edge cases

---

## Flight Leg 7: Claude Session Management

### Tasks

- [ ] Implement Claude session launcher using Agent SDK
  - Files: `services/daemon/src/claude/session.ts`
  - `startClaudeSession(worktreePath, plan, sessionId): Promise<ClaudeHandle>`
  - Use `query()` from `@anthropic-ai/claude-agent-sdk` with:
    - `prompt`: user's plan
    - `options.cwd`: worktree path
    - `options.permissionMode`: `'bypassPermissions'`
    - `options.systemPrompt`: custom prompt explaining the Bossanova context
  - Return handle wrapping the async generator + AbortController
- [ ] Implement Claude session supervisor
  - Files: `services/daemon/src/claude/supervisor.ts`
  - `ClaudeSupervisor` class: `start(sessionId)`, `stop(sessionId)`, `pause(sessionId)`, `resume(sessionId)`, `getStatus(sessionId)`
  - Consumes `query()` async generator, processes `SDKMessage` events
  - On success result: transition session to next state
  - On error: retry logic, transition to `blocked` after max retries
  - Active sessions tracked in `Map<string, ClaudeHandle>`
- [ ] Implement session output log capture
  - Files: `services/daemon/src/claude/logger.ts`
  - Capture Claude output to `~/Library/Application Support/bossanova/logs/<session-id>.log`
  - Support `boss logs <session>` via file read
- [ ] Wire Claude session into session lifecycle
  - Files: `services/daemon/src/session/lifecycle.ts` (extend)
  - After worktree creation: start Claude, transition `starting_claude` → `implementing_plan`
  - On Claude completion: transition to `pushing_branch`, push, then `opening_draft_pr`
  - Connect supervisor to IPC handlers for stop/pause/resume
- [ ] Implement `boss attach` (basic output streaming)
  - Files: `services/daemon/src/ipc/handlers/attach.ts`, `services/cli/src/views/AttachView.tsx`
  - Stream Claude's output messages over IPC
  - CLI renders streaming output in Ink

### Post-Flight Checks

- [ ] `boss new "create a README.md"` starts Claude in the worktree, Claude creates files
- [ ] Session state transitions: `creating_worktree` → `starting_claude` → `implementing_plan`
- [ ] `boss logs <session>` shows Claude's output
- [ ] `boss stop <session>` terminates Claude
- [ ] `boss attach <session>` shows live output (basic)
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 7

Human reviews: Agent SDK integration, supervisor design, system prompt, streaming architecture

---

## Flight Leg 8: GitHub PR Automation

### Tasks

- [ ] Implement GitHub API client (via `gh` CLI)
  - Files: `services/daemon/src/github/client.ts`
  - `createDraftPr(worktreePath, title, body, baseBranch): Promise<{ number, url }>`
  - `getPrStatus(worktreePath, prNumber): Promise<PrStatus>` — state, mergeable, checks
  - `markReadyForReview(worktreePath, prNumber): Promise<void>`
  - `closePr(worktreePath, prNumber): Promise<void>`
  - `getPrChecks(worktreePath, prNumber): Promise<Check[]>`
- [ ] Implement PR lifecycle management
  - Files: `services/daemon/src/session/pr-lifecycle.ts`
  - After Claude finishes + pushes: create draft PR
  - `pushing_branch` → `opening_draft_pr` → `awaiting_checks`
  - Store PR number/URL in session record
- [ ] Implement PR state polling (fallback when no webhooks)
  - Files: `services/daemon/src/github/poll.ts`
  - Poll every 60s for sessions in `awaiting_checks`
  - Check: checks status, mergeable state, conflict detection
- [ ] Implement ready-for-review transition
  - Files: `services/daemon/src/session/completion.ts`
  - Two conditions: `planComplete === true` AND `checksGreen === true`
  - When both met: `markReadyForReview()`, transition to `ready_for_review`
  - On PR merged (detected by poll): transition to `merged`, cleanup worktree
- [ ] Wire PR automation into session lifecycle
  - Files: `services/daemon/src/session/lifecycle.ts` (extend)

### Post-Flight Checks

- [ ] `boss new "add a hello world script"` → draft PR appears on GitHub
- [ ] `boss ls` shows PR number and state
- [ ] Session transitions to `awaiting_checks` and polls for check status
- [ ] If checks pass on simple PR: session transitions to `ready_for_review`, PR marked ready on GitHub
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 8

Human reviews: GitHub CLI integration, PR flow, polling logic, ready-for-review conditions

---

## Flight Leg 9: Webhook Receiver (Cloudflare Worker)

### Tasks

- [ ] Implement GitHub webhook signature verification
  - Files: `services/webhook/src/verify.ts`
  - HMAC-SHA256 via Web Crypto API (Workers-compatible)
  - Verify `X-Hub-Signature-256` header
- [ ] Implement webhook event handler
  - Files: `services/webhook/src/handler.ts`
  - Parse payloads for: `pull_request`, `check_run`, `check_suite`
  - Map GitHub events to `DaemonEvent` types from `@bossanova/shared`
- [ ] Implement Hono webhook app
  - Files: `services/webhook/src/index.ts`
  - `POST /webhook/github` — verify signature, parse event, forward to orchestrator
  - `GET /health` — health check
  - Return 200 immediately after forwarding
- [ ] Configure wrangler and environment
  - Files: `services/webhook/wrangler.toml`
  - Worker name, compatibility date, environment variables: `GITHUB_WEBHOOK_SECRET`, `ORCHESTRATOR_URL`
- [ ] Write tests for signature verification and event parsing
  - Files: `services/webhook/src/__tests__/verify.test.ts`, `handler.test.ts`

### Post-Flight Checks

- [ ] `cd services/webhook && npx wrangler dev` starts
- [ ] Request with valid signature → 200; invalid signature → 401
- [ ] Mock `check_run` completed event → correctly parsed into `DaemonEvent`
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 9

Human reviews: Webhook security, event parsing, Workers config

---

## Flight Leg 10: Orchestrator (Event Routing + Durable Objects)

### Tasks

- [ ] Implement daemon registry using Cloudflare Durable Objects
  - Files: `services/orchestrator/src/daemon-registry.ts`
  - Durable Object class `DaemonSession` — holds persistent WebSocket connection per daemon
  - `registerDaemon(repoFullName, daemonId)`: creates/updates mapping in DO storage
  - `lookupDaemon(repoFullName)`: returns DaemonSession reference
  - Heartbeat tracking, stale daemon cleanup
- [ ] Implement WebSocket connection handler in Durable Object
  - Files: `services/orchestrator/src/daemon-session.ts`
  - Accept WebSocket upgrade from daemon
  - Handle frame-based protocol from `@bossanova/shared/ws-protocol`
  - Forward `DaemonEvent`s to the daemon over the open WebSocket
  - Channel 0 (control): registration, heartbeats, event delivery
- [ ] Implement orchestrator Hono app
  - Files: `services/orchestrator/src/index.ts`
  - `POST /events` — receive events from webhook worker, route to correct DaemonSession DO
  - `GET /ws/daemon` — WebSocket upgrade endpoint for daemons
  - `GET /health` — health check
  - Authentication: shared secret between webhook worker and orchestrator
- [ ] Implement daemon-side WebSocket client
  - Files: `services/daemon/src/transport/ws-client.ts`
  - On startup: connect to orchestrator via WSS
  - Send registration message with repo list
  - Receive events, dispatch to fix loop
  - Reconnect on disconnect with exponential backoff
  - Heartbeat every 30s
- [ ] Design crypto abstraction layer for future E2E encryption
  - Files: `lib/shared/src/crypto.ts`
  - Define `TransportEncryption` interface: `encrypt(plaintext): Buffer`, `decrypt(ciphertext): Buffer`
  - v1 implementation: `PlaintextTransport` (no-op passthrough) — TLS provides transport security
  - Future v2: `E2ETransport` using tweetnacl with device keypairs, activated when iOS peer connects
  - The frame protocol supports encrypted payloads on channels 1+2 (PTY, chat) while channel 0 (control) stays plaintext for routing

### Post-Flight Checks

- [ ] Both workers start locally (`npx wrangler dev` in each)
- [ ] Daemon connects to orchestrator via WebSocket, registration succeeds
- [ ] Mock event sent to orchestrator → forwarded to daemon over WebSocket
- [ ] Daemon reconnects after orchestrator restart
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 10

Human reviews: Durable Objects design, WebSocket lifecycle, E2E encryption, reconnect logic

---

## Flight Leg 11: Fix Loop (Automated Repair Cycle)

### Tasks

- [ ] Implement check failure handler
  - Files: `services/daemon/src/fix-loop/check-handler.ts`
  - On `check_failed` event: fetch failing check logs (`gh run view --log-failed`), prepare fix prompt for Claude, resume/start Claude query in worktree
  - Transition: `awaiting_checks` → `fixing_checks`
  - After Claude fixes: commit, push, transition back to `awaiting_checks`
  - Max 5 attempts → transition to `blocked`
- [ ] Implement conflict resolver
  - Files: `services/daemon/src/fix-loop/conflict-handler.ts`
  - Detect via PR API `mergeable` field or `git merge-base`
  - `git fetch origin` → `git merge origin/<base>` → if conflicts, prompt Claude to resolve → commit merge, push
- [ ] Implement review feedback handler
  - Files: `services/daemon/src/fix-loop/review-handler.ts`
  - On review event: fetch review comments (`gh pr view --json reviews,comments`)
  - Extract actionable feedback (changes requested, not approvals)
  - Prompt Claude with feedback → commit, push
- [ ] Implement event dispatcher with concurrency guard
  - Files: `services/daemon/src/fix-loop/dispatcher.ts`
  - `handleDaemonEvent(db, event)` — route to correct handler
  - Lock per session (prevent concurrent fix attempts on same session)
  - Update session state throughout
- [ ] Implement daemon HTTP endpoint for local event receipt (fallback)
  - Files: `services/daemon/src/http/server.ts`
  - Simple HTTP server for receiving events when WebSocket is unavailable
  - `POST /events` — verify shared secret, dispatch to fix loop

### Post-Flight Checks

- [ ] Create session with intentionally failing CI → send `check_failed` event → Claude receives fix prompt
- [ ] After 5 failed fixes → session transitions to `blocked`
- [ ] Conflict scenario: handler detects conflict, prompts Claude to resolve
- [ ] State transitions: `awaiting_checks` → `fixing_checks` → `awaiting_checks` works correctly
- [ ] Concurrent events for same session are serialized (not duplicated)
- [ ] `make format && make lint && make test` passes

### [HANDOFF] Review Flight Leg 11

Human reviews: Fix loop logic, retry limits, conflict detection, review parsing, concurrency

---

## Flight Leg 12: LaunchAgent, Polish, and End-to-End Testing

### Tasks

- [ ] Create macOS LaunchAgent plist for `bossd`
  - Files: `services/daemon/com.bossanova.bossd.plist`
  - LaunchAgent: `Label`, `ProgramArguments`, `KeepAlive`, `RunAtLoad`, `StandardOutPath`, `StandardErrorPath`
  - `boss daemon install/uninstall/status` CLI commands
  - Install: copy plist to `~/Library/LaunchAgents/`, `launchctl load`
- [ ] Implement `boss attach` with full bidirectional terminal
  - Files: `services/cli/src/views/AttachView.tsx` (extend)
  - Display Claude output, accept user input via Ink `<TextInput>`
  - Session metadata header: title, state, branch, PR link
  - Detach via Ctrl-C/Ctrl-D
- [ ] Add comprehensive error handling across CLI and daemon
  - Friendly error messages: daemon not running, session not found, repo not registered, Git auth failures
  - Structured logging with timestamps and session IDs
  - Edge cases: daemon crash mid-session, dirty worktree state
- [ ] Write end-to-end integration tests
  - Files: `services/daemon/src/__tests__/e2e/full-lifecycle.test.ts`
  - Full lifecycle with mock Git repo: register repo → create session → worktree created → Claude starts (mocked) → state transitions → PR creation (mocked `gh`) → fix loop triggers → cleanup
  - Error scenarios: invalid repo, concurrent creation, daemon restart recovery
- [ ] Add `boss` bin entry to package.json and ensure `npx boss` works
  - Files: `services/cli/package.json`
  - Verify: `pnpm --filter @bossanova/cli build && npx boss ls`

### Post-Flight Checks

- [ ] `boss daemon install` installs plist, `launchctl list | grep bossanova` shows daemon
- [ ] End-to-end demo:
  1. `boss daemon install` — daemon starts
  2. `cd <any-git-repo> && boss repo add .` — registers repo
  3. `boss new "add CONTRIBUTING.md"` — creates session, worktree, starts Claude
  4. `boss ls` — shows session in progress
  5. `boss attach <session>` — shows live Claude output
  6. Wait for completion — draft PR on GitHub
  7. `boss ls` — shows `awaiting_checks` or `green_draft`
- [ ] `make format && make lint && make test` from root passes across all services
- [ ] Integration tests pass

### [HANDOFF] Final Review

Human reviews: Complete feature set, end-to-end flow, LaunchAgent, error handling

---

## Rollback Plan

Each flight leg produces independent commits. To roll back:

- `git revert` commits from the specific flight leg
- No external state mutations until Flight Leg 8+ (GitHub PRs)
- Database schema changes are additive only

## Notes

- **Ink + Node.js only:** CLI must use Node.js (Ink has yoga-layout issues with Bun). All services use Node.js with pnpm for consistency.
- **XState v5 for state machine:** The session lifecycle is modeled as an XState v5 machine using the `setup().createMachine()` pattern. The daemon creates an actor per session and persists the state to SQLite on each transition. This replaces the manual `VALID_TRANSITIONS` map — XState handles transition validation natively.
- **tsyringe for DI:** Both daemon and CLI use tsyringe with Symbol-based tokens, following the madverts/core services/flows pattern. This enables clean testability (mock injected services in tests) and separation of concerns.
- **SQLite migrations:** The `schema_version` table tracks the current schema version. The `MIGRATIONS` array in `@bossanova/shared` contains versioned SQL strings. On startup, the daemon applies any migrations with version > current. This supports schema evolution without data loss.
- **Worktree setup scripts:** Repos can configure a setup script (e.g. `pnpm install`, `make setup`) that runs automatically after worktree creation. This ensures new worktrees have dependencies installed before Claude starts working.
- **Interactive `boss` command:** Running `boss` with no arguments launches an interactive Ink TUI with keyboard navigation (arrow keys + Enter). `boss ls` is the non-interactive variant for scripting.
- **Attempt tracking purpose:** Each fix cycle (check failure, conflict, review feedback) is recorded as an "attempt" with trigger, timestamp, and result. This enforces the max retry limit (5 attempts → blocked), powers the `boss logs` command, and provides debugging history.
- **E2E encryption deferred to v2:** v1 uses TLS (WSS) for transport security. App-layer E2E encryption will be added when a second peer (iOS app) is introduced — the orchestrator needs to route but not read PTY/chat traffic. The `TransportEncryption` interface is designed now so the encryption layer can be swapped in without changing the frame protocol.
- **QUIC deferred:** The spec's QUIC vision (multiplexed streams) is deferred. v1 uses WebSocket with frame-based multiplexing. The abstraction layer in `lib/shared/src/ws-protocol.ts` is designed so QUIC/WebTransport can be swapped in later without changing consumer code.
- **Scheduled sessions deferred:** Cron-based session creation (tech debt cleanup) is a future flight leg.
- **iOS app deferred:** Future phase. The WebSocket + E2E encryption architecture supports it. WebRTC for direct P2P can be added later.
- **Claude Agent SDK `query()` vs V2:** Plan uses `query()` which is stable. V2 `unstable_v2_createSession()` may be preferable for multi-turn fix loops — evaluate at implementation time.
- **Cloudflare Durable Objects:** Required for holding persistent WebSocket connections to daemons. The orchestrator Worker needs a paid Workers plan for Durable Objects.

## Critical Files

| File                                          | Why It's Critical                                              |
| --------------------------------------------- | -------------------------------------------------------------- |
| `lib/shared/src/types.ts`                     | Core domain types every service depends on                     |
| `lib/shared/src/session-machine.ts`           | XState v5 state machine governing all session behavior         |
| `lib/shared/src/db-schema.ts`                 | SQLite schema + versioned migrations array                     |
| `lib/shared/src/rpc.ts`                       | CLI-daemon contract                                            |
| `services/daemon/src/di/container.ts`         | DI container wiring all daemon services together               |
| `lib/shared/src/ws-protocol.ts`               | Transport abstraction (WebSocket now, QUIC later)              |
| `services/daemon/src/session/lifecycle.ts`    | Central orchestration wiring everything together               |
| `services/daemon/src/claude/session.ts`       | Agent SDK integration — prompt design determines effectiveness |
| `services/daemon/src/fix-loop/dispatcher.ts`  | The automated repair cycle that makes Bossanova autonomous     |
| `services/daemon/src/ipc/server.ts`           | CLI-daemon communication backbone                              |
| `services/orchestrator/src/daemon-session.ts` | Durable Object holding daemon WebSocket connections            |

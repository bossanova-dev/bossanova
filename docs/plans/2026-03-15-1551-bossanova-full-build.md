# Bossanova Implementation Plan

**Flight ID:** fp-2026-03-15-1551-bossanova-full-build

## Context

Bossanova is a CLI-first orchestrator for managing multiple Claude Code sessions, each mapped to a GitHub PR. The system automatically fixes failing CI, resolves conflicts, and addresses review feedback — keeping PRs green 24/7 without human intervention.

This plan covers the full build from monorepo scaffolding through automated fix loops, split into 12 flight legs.

## Key Architectural Decisions

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Runtime | Node.js everywhere | Ink (CLI framework) has yoga-layout issues with Bun |
| Package manager | pnpm | User preference, workspace support |
| Linting | Biome (not ESLint) | User preference |
| Formatting | Prettier + Biome | Consistent with madverts/core pattern |
| Testing | Vitest | Modern, fast, TypeScript-native |
| SQLite | better-sqlite3 | Node.js compatible (no bun:sqlite) |
| CLI framework | Ink v6 | React-based terminal UI |
| Claude integration | @anthropic-ai/claude-agent-sdk | Official SDK |
| Cloud services | Cloudflare Workers + Hono | 2 separate Workers (webhook + orchestrator) |
| Transport | WebSocket + app-layer E2E encryption | Daemon opens persistent WSS to orchestrator |
| Encryption | libsodium (tweetnacl-js) | App-layer E2E so bossanova.dev can't read traffic |
| Multiplexing | Frame-based over WebSocket | Channel 0=control, 1=PTY, 2=chat |
| Scheduled sessions | Deferred to future phase | Focus on webhook-triggered automation first |
| QUIC/WebRTC | Deferred to future phase | WebSocket is production-ready; QUIC/WebRTC for iOS P2P later |

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

- [ ] Define session state enum and state machine transitions
  - Files: `lib/shared/src/session-states.ts`
  - `SessionState` enum: `creating_worktree`, `starting_claude`, `pushing_branch`, `opening_draft_pr`, `implementing_plan`, `awaiting_checks`, `fixing_checks`, `green_draft`, `ready_for_review`, `blocked`, `merged`, `closed`
  - `VALID_TRANSITIONS: Record<SessionState, SessionState[]>` — allowed state transitions
  - `isTerminalState(state): boolean`
- [ ] Define core domain types (Repo, Session) and database row types
  - Files: `lib/shared/src/types.ts`, `lib/shared/src/db-schema.ts`
  - `Repo` interface: id, displayName, localPath, originUrl, defaultBaseBranch, worktreeBaseDir, timestamps
  - `Session` interface: id, repoId, title, plan, worktreePath, branchName, baseBranch, state, claudeSessionId, prNumber, prUrl, lastCheckState, automationEnabled, attemptCount, blockedReason, timestamps
  - SQL `CREATE TABLE` statements as string constants for `repos`, `sessions`, `attempts`, `schema_version`
  - `RepoRow`, `SessionRow`, `AttemptRow` TypeScript interfaces matching SQL columns
- [ ] Define JSON-RPC schema for CLI-daemon IPC
  - Files: `lib/shared/src/rpc.ts`
  - JSON-RPC 2.0 envelope: `{ jsonrpc: "2.0", method, params, id }`
  - Methods: `context.resolve`, `repo.register`, `repo.list`, `repo.remove`, `session.list`, `session.create`, `session.get`, `session.attach`, `session.stop`, `session.pause`, `session.resume`, `session.retry`, `session.close`, `session.remove`
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
- [ ] All 12 session states present in enum; transition map covers the full lifecycle including fix loop (`awaiting_checks` <-> `fixing_checks`)
- [ ] Session interface fields align with SessionRow columns
- [ ] Every CLI command from the spec has a corresponding RPC method
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 2

Human reviews: Type definitions, state machine transitions, RPC method signatures, SQLite schema, WebSocket protocol

---

## Flight Leg 3: Daemon Core — SQLite and Session CRUD

### Tasks

- [ ] Implement SQLite database module with schema initialization
  - Files: `services/daemon/src/db/database.ts`
  - Use `better-sqlite3` (synchronous API)
  - `initDatabase(dbPath: string): Database` — creates DB, runs schema SQL from `@bossanova/shared`, enables WAL mode, foreign keys
  - Default DB path: `~/Library/Application Support/bossanova/bossd.db`
- [ ] Implement repository CRUD operations
  - Files: `services/daemon/src/db/repos.ts`
  - `registerRepo(db, params): Repo`, `listRepos(db): Repo[]`, `getRepo(db, id): Repo | null`, `removeRepo(db, id): void`, `findRepoByPath(db, path): Repo | null`
  - Use prepared statements for performance
- [ ] Implement session CRUD operations with state machine enforcement
  - Files: `services/daemon/src/db/sessions.ts`
  - `createSession(db, params): Session`, `listSessions(db, repoId?): Session[]`, `getSession(db, id): Session | null`, `updateSessionState(db, id, newState, extra?): void`, `deleteSession(db, id): void`
  - `updateSessionState` validates transitions against `VALID_TRANSITIONS` — throws on invalid transition
- [ ] Implement attempt tracking
  - Files: `services/daemon/src/db/attempts.ts`
  - `recordAttempt(db, sessionId, trigger): Attempt`, `completeAttempt(db, attemptId, result, error?): void`, `getAttempts(db, sessionId): Attempt[]`
- [ ] Write unit tests for all database operations
  - Files: `services/daemon/src/db/__tests__/database.test.ts`, `repos.test.ts`, `sessions.test.ts`
  - Use in-memory SQLite (`:memory:`) for tests
  - Test CRUD for repos and sessions, state transition validation (valid passes, invalid throws), schema initialization

### Post-Flight Checks

- [ ] `make test` in `services/daemon/` — all tests pass
- [ ] In-memory test creates all tables, schema_version = 1
- [ ] Create repo → create session → list by repo → update state → delete — all work
- [ ] Attempting `merged -> creating_worktree` throws error
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 3

Human reviews: Database implementation, prepared statements, state machine enforcement, test coverage

---

## Flight Leg 4: Daemon IPC — Unix Socket Server

### Tasks

- [ ] Implement Unix socket JSON-RPC server
  - Files: `services/daemon/src/ipc/server.ts`
  - Use Node.js `net.createServer` with Unix domain socket
  - Socket path: `~/Library/Application Support/bossanova/bossd.sock`
  - Newline-delimited JSON-RPC 2.0 messages
  - `startIpcServer(db: Database): { close: () => void }`
- [ ] Implement RPC method dispatcher
  - Files: `services/daemon/src/ipc/dispatcher.ts`
  - Map method names to handler functions
  - Handlers receive `(db, params)`, return result
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

## Flight Leg 5: CLI Basics — Ink Rendering and Daemon Connection

### Tasks

- [ ] Set up CLI entry point with argument parsing
  - Files: `services/cli/src/cli.tsx`
  - Hashbang `#!/usr/bin/env node`, parse `process.argv`
  - Commands: `boss` (default), `boss new [plan]`, `boss ls`, `boss attach <id>`, `boss stop/pause/resume/logs/retry/close/rm <id>`, `boss repo add/ls/remove`
  - Route to appropriate Ink component
- [ ] Implement session list view
  - Files: `services/cli/src/views/SessionList.tsx`
  - Table: ID (short), Title, State (color-coded), Branch, PR#, Last Updated
  - Colors: green=`green_draft`/`ready_for_review`/`merged`, yellow=`implementing_plan`/`awaiting_checks`, red=`blocked`/`fixing_checks`, gray=`closed`
  - Context-aware: inside a repo → show that repo's sessions; otherwise → all
- [ ] Implement `boss new` flow
  - Files: `services/cli/src/views/NewSession.tsx`
  - Accept plan as argument or prompt via Ink `<TextInput>`
  - Resolve context, select repo, call `client.sessionCreate(repoId, plan)`
- [ ] Implement `boss repo` subcommands
  - Files: `services/cli/src/views/RepoList.tsx`, `services/cli/src/views/RepoAdd.tsx`
  - `boss repo ls` — table of repos (ID, Name, Path, Branch)
  - `boss repo add <path>` — register, display result
  - `boss repo remove <id>` — remove, confirm
- [ ] Connect CLI to daemon via IPC client
  - Files: `services/cli/src/client.ts`
  - Import `createIpcClient` from `@bossanova/shared`
  - Handle "daemon not running" with helpful message
  - Singleton `client` instance

### Post-Flight Checks

- [ ] `boss` with no daemon shows "bossd is not running" message
- [ ] With daemon running: `boss repo add .` registers repo, `boss repo ls` shows it
- [ ] `boss ls` shows empty session list (formatted table)
- [ ] `boss new "test plan"` creates session record (stub — no Git/Claude work yet)
- [ ] `make format && make lint` passes

### [HANDOFF] Review Flight Leg 5

Human reviews: CLI UX, Ink components, argument parsing, IPC integration

---

## Flight Leg 6: Git Worktree Management

### Tasks

- [ ] Implement worktree creation
  - Files: `services/daemon/src/git/worktree.ts`
  - `createWorktree(repoPath, session): Promise<string>` — returns worktree path
  - Path: `~/Library/Application Support/bossanova/worktrees/<repo-id>/<session-id>/`
  - Branch: `boss/<slug>-<short-id>` (slug from title, kebab-case, max 30 chars)
  - Runs: `git worktree add <path> -b <branch>` from repo root
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
- [ ] Implement app-layer E2E encryption
  - Files: `lib/shared/src/crypto.ts`
  - Use `tweetnacl` (libsodium-compatible) for encryption
  - `generateKeyPair()`, `encrypt(plaintext, recipientPublicKey, senderSecretKey)`, `decrypt(ciphertext, senderPublicKey, recipientSecretKey)`
  - Wrap in frame encode/decode so orchestrator sees only ciphertext
  - Key exchange during daemon registration

### Post-Flight Checks

- [ ] Both workers start locally (`npx wrangler dev` in each)
- [ ] Daemon connects to orchestrator via WebSocket, registration succeeds
- [ ] Mock event sent to orchestrator → forwarded to daemon over WebSocket
- [ ] Daemon reconnects after orchestrator restart
- [ ] Encrypted payload: orchestrator logs show ciphertext, daemon decrypts correctly
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
- **QUIC deferred:** The spec's QUIC vision (multiplexed streams) is deferred. v1 uses WebSocket with frame-based multiplexing. The abstraction layer in `lib/shared/src/ws-protocol.ts` is designed so QUIC/WebTransport can be swapped in later without changing consumer code.
- **Scheduled sessions deferred:** Cron-based session creation (tech debt cleanup) is a future flight leg.
- **iOS app deferred:** Future phase. The WebSocket + E2E encryption architecture supports it. WebRTC for direct P2P can be added later.
- **Claude Agent SDK `query()` vs V2:** Plan uses `query()` which is stable. V2 `unstable_v2_createSession()` may be preferable for multi-turn fix loops — evaluate at implementation time.
- **Cloudflare Durable Objects:** Required for holding persistent WebSocket connections to daemons. The orchestrator Worker needs a paid Workers plan for Durable Objects.

## Critical Files

| File | Why It's Critical |
|------|-------------------|
| `lib/shared/src/types.ts` | Core domain types every service depends on |
| `lib/shared/src/session-states.ts` | State machine that governs all session behavior |
| `lib/shared/src/rpc.ts` | CLI-daemon contract |
| `lib/shared/src/ws-protocol.ts` | Transport abstraction (WebSocket now, QUIC later) |
| `services/daemon/src/session/lifecycle.ts` | Central orchestration wiring everything together |
| `services/daemon/src/claude/session.ts` | Agent SDK integration — prompt design determines effectiveness |
| `services/daemon/src/fix-loop/dispatcher.ts` | The automated repair cycle that makes Bossanova autonomous |
| `services/daemon/src/ipc/server.ts` | CLI-daemon communication backbone |
| `services/orchestrator/src/daemon-session.ts` | Durable Object holding daemon WebSocket connections |

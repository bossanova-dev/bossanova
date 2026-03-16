## Handoff: Flight Leg 2 — Shared Types and Schemas

**Date:** 2026-03-16 08:30
**Branch:** main
**Flight ID:** fp-2026-03-15-1551-bossanova-full-build
**Planning Doc:** docs/plans/2026-03-15-1551-bossanova-full-build.md

### Tasks Completed This Flight Leg

- bossanova-478: Define session state machine using XState v5 (setup/createMachine)
- bossanova-qgs: Define core domain types (Repo, Session) and database row types
- bossanova-mm3: Define JSON-RPC schema for CLI-daemon IPC
- bossanova-9a6: Define webhook event types and daemon event types
- bossanova-ctc: Define WebSocket protocol frame types and encode/decode

### Files Changed

- `lib/shared/src/session-machine.ts` — XState v5 state machine with 12 states, 15 event types, typed context/input, guards (hasReachedMaxAttempts), and actions (incrementAttemptCount, setChecksPassed/Failed, clearBlockedReason)
- `lib/shared/src/types.ts` — Repo, Session, Attempt domain interfaces with typed fields
- `lib/shared/src/db-schema.ts` — RepoRow, SessionRow, AttemptRow, SchemaVersionRow SQL row types; MIGRATIONS array with initial schema (version 1) including repos, sessions, attempts tables with indexes
- `lib/shared/src/rpc.ts` — JSON-RPC 2.0 envelope types, RpcErrorCode constants, typed request/response pairs for 17 RPC methods (context.resolve, repo._, session._), RpcMethods map type
- `lib/shared/src/webhook-events.ts` — GitHub webhook event types (pull_request, check_run, check_suite, pull_request_review) and DaemonEvent union (check_failed, pr_updated, conflict_detected, review_submitted)
- `lib/shared/src/ws-protocol.ts` — Frame-based WebSocket protocol (channel/length/payload), Channel enum (0=control, 1=PTY, 2=chat), ControlMessage types (register, heartbeat, event, ack), encodeFrame/decodeFrame/encodeControlMessage/decodeControlMessage functions
- `lib/shared/src/index.ts` — Barrel exports for all shared modules
- `lib/shared/src/__tests__/session-machine.test.ts` — 11 tests covering all state transitions, fix loop, max attempts blocking, invalid events, block/unblock

### Implementation Notes

- XState v5 `setup().createMachine()` pattern: parameterized actions don't work well with inline `params` callbacks in v5.28 — use inline `assign()` in transitions instead of referencing parameterized actions by string name
- Guard `hasReachedMaxAttempts` checks `attemptCount + 1 >= maxAttempts` because guards evaluate before actions in XState v5
- `SessionMachineInput` type provides typed input for `createActor(sessionMachine, { input: ... })`
- Biome enforces `export type { ... }` when all exports are types (not `export { type X, type Y }`)
- Biome bans `{}` as a type — use `Record<string, never>` for empty object types
- SQLite uses `INTEGER NOT NULL DEFAULT 1` for boolean `automation_enabled` field

### Current Status

- Tests: 11/11 PASSED
- Build: PASSED (all 5 packages)
- Lint: PASSED
- Format: PASSED

### Next Flight Leg

Flight Leg 3: Daemon Core — DI Container, SQLite, and Session CRUD

- bossanova-oft: Set up tsyringe DI container for daemon (tokens, container, setupContainer)
- bossanova-cxf: Implement SQLite database service with versioned migration runner
- bossanova-996: Implement RepoStore and SessionStore as injectable services
- bossanova-ieh: Implement AttemptStore for fix-cycle tracking
- Plus unit tests and handoff

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-15-1551-bossanova-full-build"` — should show bossanova-oft
2. Review files: `lib/shared/src/types.ts`, `lib/shared/src/db-schema.ts`, `lib/shared/src/session-machine.ts`

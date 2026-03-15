## Handoff: Flight Leg 4 — Daemon IPC (Unix Socket Server)

**Date:** 2026-03-16 11:00
**Branch:** main
**Flight ID:** fp-2026-03-15-1551-bossanova-full-build
**Planning Doc:** docs/plans/2026-03-15-1551-bossanova-full-build.md

### Tasks Completed

- bossanova-poo: Implement Unix socket JSON-RPC server as injectable service
- bossanova-692: Implement RPC method dispatcher with injected stores
- bossanova-0vo: Implement context resolution logic (cwd → session/repo detection)
- bossanova-d03: Implement daemon main entry point with DI bootstrap and graceful shutdown
- bossanova-72t: Implement IPC client utility in lib/shared for CLI usage

### Files Changed

- `services/daemon/src/ipc/server.ts` — `@injectable() class IpcServer` using `net.createServer` with newline-delimited JSON-RPC 2.0 over Unix socket. Validates jsonrpc/method/id, delegates to Dispatcher
- `services/daemon/src/ipc/dispatcher.ts` — `@injectable() class Dispatcher` with handler map routing to repo/session/attempt stores. Handles method-not-found, internal errors. Uses `execSync` for git operations in repo.register
- `services/daemon/src/ipc/handlers/context.ts` — `resolveContext(cwd, repos, sessions)` with priority: session worktree → registered repo → unregistered git repo → none. Uses `git rev-parse --show-toplevel` and `--git-common-dir` for worktree detection
- `services/daemon/src/index.ts` — Daemon entry point: setupContainer → mkdir data dir → initialize DB → remove stale socket → start IPC server → SIGTERM/SIGINT handlers for graceful shutdown with socket cleanup
- `services/daemon/src/di/tokens.ts` — Added `Dispatcher` and `IpcServer` DI tokens
- `services/daemon/src/di/container.ts` — Switched from `useClass` to `registerSingleton` for all services. Added explicit registration for Dispatcher and IpcServer
- `lib/shared/src/ipc-client.ts` — `createIpcClient(socketPath?)` with typed `call<M>()` method. Per-call connection, DaemonNotRunningError for ECONNREFUSED/ENOENT
- `lib/shared/src/index.ts` — Exported createIpcClient, DaemonNotRunningError, IpcClient
- `services/daemon/src/ipc/__tests__/server.test.ts` — 6 tests: connection, parse error, invalid request, method not found, multiple messages, clean stop
- `services/daemon/src/ipc/__tests__/dispatcher.test.ts` — 12 tests: method not found, method listing, repo CRUD, session CRUD, attempts
- `services/daemon/src/ipc/__tests__/context.test.ts` — 5 tests: non-git dir → none, unregistered repo, registered repo, subdirectory, session worktree
- `lib/shared/src/__tests__/ipc-client.test.ts` — 5 tests: round-trip, RPC error, daemon not running, param passing, ID incrementing

### Learnings & Notes

- **Singleton DI is essential**: `container.register(token, { useClass: X })` creates a NEW instance per `resolve()`. For shared state (DatabaseService), must use `container.registerSingleton(token, X)` — otherwise each store gets its own uninitialized DB
- **ESM + require() don't mix**: Dynamic `require('node:child_process')` fails at runtime in ESM context ("require is not defined"). Use top-level `import { execSync } from 'node:child_process'` instead
- **macOS /var symlink**: `fs.mkdtempSync(os.tmpdir())` returns `/var/folders/...` but `git rev-parse --show-toplevel` resolves through the symlink to `/private/var/...`. Tests must use `fs.realpathSync()` on temp dirs
- **tsx path resolution**: `npx tsx services/daemon/src/index.ts` from root fails because `~/` path aliases need tsconfig.json in cwd. Must run from `services/daemon/` directory: `cd services/daemon && npx tsx src/index.ts`
- **Newline-delimited JSON-RPC**: Each message is `JSON\n`. Server buffers incoming data and splits on `\n`, allowing multiple messages per TCP segment and partial message handling
- **IPC client per-call connections**: Each `call()` creates a new Unix socket connection. Simple and avoids connection management, acceptable for CLI's request/response pattern

### Issues Encountered

- Singleton vs transient DI — discovered during live integration test, resolved by switching to `registerSingleton`
- ESM require — discovered during live integration test, resolved by top-level imports
- Both issues caught by post-flight integration tests, not unit tests (unit tests construct services manually, bypassing DI)

### Current Status

- Tests: 76/76 PASSED (16 shared + 60 daemon)
- Build: PASSED (all 5 packages)
- Lint: PASSED
- Format: PASSED
- Integration: Daemon starts, IPC round-trip works, context resolution works, SIGTERM cleanup works

### Next Flight Leg

Flight Leg 5: CLI Basics — Ink Rendering, DI, and Daemon Connection
- bossanova-lya: Set up tsyringe DI container for CLI
- bossanova-0n5: Set up CLI entry point with argument parsing and DI bootstrap
- bossanova-a3v: Implement interactive home screen with session list and action bar
- bossanova-ece: Implement guided New Session wizard
- Plus repo management commands, CLI-to-daemon connection, and handoff

### Resume Command

To continue this work:
1. Run `bd ready --label "flight:fp-2026-03-15-1551-bossanova-full-build"` — should show bossanova-lya
2. Review files: `services/daemon/src/ipc/server.ts`, `services/daemon/src/ipc/dispatcher.ts`, `services/daemon/src/di/container.ts`, `lib/shared/src/ipc-client.ts`
3. Read the plan for Leg 5: `docs/plans/2026-03-15-1551-bossanova-full-build.md` lines 254-322

## Handoff: Flight Leg 5 — CLI Basics (Ink Rendering, DI, and Daemon Connection)

**Date:** 2026-03-16 12:30
**Branch:** main
**Flight ID:** fp-2026-03-15-1551-bossanova-full-build
**Planning Doc:** docs/plans/2026-03-15-1551-bossanova-full-build.md

### Tasks Completed

- bossanova-lya: Set up tsyringe DI container for CLI
- bossanova-0n5: Set up CLI entry point with argument parsing and DI bootstrap
- bossanova-a3v: Implement interactive home screen with session list and action bar
- bossanova-ece: Implement guided New Session wizard (repo select, new/existing PR, plan input)
- bossanova-lck: Implement guided Add Repository wizard and boss repo ls/remove

### Files Changed

- `services/cli/src/di/tokens.ts` — DI tokens: `Service.IpcClient`, `Service.Config`, `Service.Logger`
- `services/cli/src/di/container.ts` — `setupContainer()` with IpcClient as singleton value, CliConfig with defaults, console Logger
- `services/cli/src/di/__tests__/container.test.ts` — 5 tests for DI container setup and resolution
- `services/cli/src/router.ts` — `parseArgs()` and `resolveRoute()` — command/subcommand/positional/flag parsing, maps to typed Route union
- `services/cli/src/__tests__/router.test.ts` — 24 tests for argument parsing and route resolution
- `services/cli/src/cli.tsx` — Entry point: reflect-metadata import, DI bootstrap, route resolution, App component dispatching to views
- `services/cli/src/views/HomeScreen.tsx` — `HomeScreen` (interactive, polling 2s, arrow nav, action bar n/r/q) + `SessionList` (non-interactive, auto-exit)
- `services/cli/src/views/__tests__/HomeScreen.test.tsx` — 7 tests: loading, empty, sessions, action bar, daemon-not-running, session list
- `services/cli/src/views/NewSession.tsx` — Multi-step wizard: repo picker (auto-select from context), New/Existing PR mode, PR list, plan input, confirm, create
- `services/cli/src/views/__tests__/NewSession.test.tsx` — 4 tests: repo picker, auto-select, no-repos error, mode options
- `services/cli/src/views/AddRepo.tsx` — Wizard: path input (auto-detect from context), confirm repo info, setup script prompt, register
- `services/cli/src/views/RepoList.tsx` — `RepoList` (table with ID/Name/Path/Branch/Setup columns) + `RepoRemove` (y/n confirmation)
- `services/cli/src/views/__tests__/RepoViews.test.tsx` — 5 tests: add-repo start, repo table, empty repos, remove prompt, remove confirmation
- `services/cli/vitest.config.ts` — Vitest setup with reflect-metadata for tsyringe
- `services/cli/package.json` — Added ink-text-input, ink-testing-library deps

### Learnings & Notes

- **Ink 6 + React 19**: Works well. `useInput` for keyboard, `useApp().exit()` for clean exit. `ink-testing-library` provides `render()` with `lastFrame()` + `stdin.write()` for testing
- **ink-text-input**: Separate package for text input fields in Ink. Required for wizard forms
- **Biome import sorting**: Biome enforces alphabetical import sorting — run `biome check --write` to auto-fix
- **Biome hook deps**: Biome flags unnecessary hook dependencies (like `exit` from `useApp` which is stable). Remove them
- **Biome non-null assertions**: Biome forbids `!` — use optional chaining with fallback instead
- **Per-call IPC connections**: The IPC client creates a new connection per `call()`, so registering it as a singleton value (not class) in DI is correct
- **Route architecture**: Extracted `parseArgs`/`resolveRoute` into `router.ts` separate from the Ink rendering in `cli.tsx` for testability without React dependencies
- **State color mapping**: Session states map to 4 colors: green (green_draft, ready_for_review, merged), yellow (implementing_plan, awaiting_checks), red (fixing_checks, blocked), cyan (setup states), gray (closed)

### Issues Encountered

- Prettier reformatted some JSX on first `make format` — cosmetic only, no logic changes
- 12 Biome lint errors on first pass (import sorting, non-null assertion, hook dep) — all auto-fixed or trivially fixed

### Current Status

- Tests: 121/121 PASSED (16 shared + 60 daemon + 45 CLI)
- Build: PASSED (all 5 packages)
- Lint: PASSED
- Format: PASSED

### Next Flight Leg

Flight Leg 6: Git Worktree Management

- bossanova-vgn: Implement Claude session launcher using Agent SDK query()
- Plus worktree creation/cleanup, git utilities, branch push, session lifecycle wiring

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-15-1551-bossanova-full-build"` — should show worktree/git tasks
2. Review files: `services/cli/src/cli.tsx`, `services/cli/src/views/HomeScreen.tsx`, `services/cli/src/router.ts`, `services/cli/src/di/container.ts`
3. Read the plan for Leg 6: `docs/plans/2026-03-15-1551-bossanova-full-build.md` lines 325-362

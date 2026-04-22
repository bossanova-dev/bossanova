# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->


## Build & Test

```bash
make deps           # Install Go, pnpm, and tooling
make generate       # Regenerate protobuf stubs (required after .proto changes)
make build          # Build boss, bossd, bosso binaries into ./bin
make plugins        # Build bossd-plugin-* binaries
make test           # Run unit tests for all Go modules with -race and coverage
make lint           # Run golangci-lint across all Go modules (pinned version)
make format         # Run gofmt + goimports + prettier
```

Per-module targets are also available (e.g. `make test-boss`, `make lint-bossd`, `make build-bosso`).

## Architecture Overview

Bossanova manages multiple Claude Code sessions across git worktrees. The repository is a Go workspace plus a pnpm workspace:

- **boss** (`services/boss`) — Bubble Tea TUI for managing sessions across repositories.
- **bossd** (`services/bossd`) — Background daemon handling session lifecycle, git ops, and plugin dispatch over gRPC.
- **bosso** (`services/bosso`) — Web UI / HTTP server (Go + Vite/React under `services/bosso/web`).
- **bossalib** (`lib/bossalib`) — Shared Go library (safego, sqlutil, keyringutil, tuidriver, etc.).
- **plugins** (`plugins/bossd-plugin-*`) — Out-of-process plugins (autopilot, dependabot, linear, repair) that subscribe to bossd events via gRPC.
- **proto** — Protobuf definitions compiled to Go via `buf`.

Sessions are isolated in git worktrees; plugins react to events (PR creation, CI failures, merge conflicts) and take autonomous actions.

## Conventions & Patterns

- **Module boundaries**: plugin binaries must not import host config/internal packages — duplicate small types rather than create a dependency.
- **Concurrency**: use `safego.Go` for goroutines that need panic recovery; it returns a `done` channel — do not fire-and-forget.
- **IDs**: use `sqlutil.NewID()` and handle the returned error (it no longer panics).
- **Secrets**: use `keyringutil` for credentials; file-backend fallback requires the explicit `--allow-insecure-keyring` flag.
- **CI**: `-race` and coverage are required for all Go tests; `golangci-lint` is pinned — update via `make lint-check-version`.

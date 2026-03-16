## Handoff: Flight Leg 12d — E2E Tests + Panic Recovery

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-636q, bossanova-aben, bossanova-loey, bossanova-7vc6

### Tasks Completed

- bossanova-636q: Add panic recovery to goroutines in bossd and bosso
- bossanova-aben: Create E2E test harness with mock git repo and mock Claude process
- bossanova-loey: Write E2E test: full session lifecycle (create → run → PR → fix loop)
- bossanova-7vc6: [HANDOFF]

### Files Changed

- `lib/bossalib/safego/safego.go:1-26` — NEW: `safego.Go()` utility wrapping goroutine launches with defer/recover + zerolog logging with stack trace
- `lib/bossalib/safego/safego_test.go:1-57` — NEW: Tests for normal execution and panic recovery
- `lib/bossalib/go.mod:9` — Added `github.com/rs/zerolog v1.34.0` dependency
- `services/bossd/cmd/main.go:126-133` — Replaced bare `go` with `safego.Go` for dispatcher and server goroutines
- `services/bossd/internal/session/poller.go:56` — Replaced bare `go func` with `safego.Go` for polling loop
- `services/bossd/internal/session/dispatcher.go:183-266` — Replaced 3 fix loop `go func` launches with `safego.Go`
- `services/bossd/internal/claude/runner.go:188-207` — Replaced 4 goroutines (stdin writer, stdout/stderr capture, process waiter) with `safego.Go`
- `services/bossd/internal/upstream/upstream.go:105` — Replaced `go m.heartbeatLoop()` with `safego.Go`
- `services/bosso/cmd/main.go:130-133` — Replaced bare `go func` server goroutine with `safego.Go`
- `services/bossd/internal/testharness/harness.go:1-120` — NEW: E2E test harness wiring in-memory DB, mock deps, and real ConnectRPC server on Unix socket
- `services/bossd/internal/testharness/harness_test.go:1-80` — NEW: Basic harness tests (RegisterRepo, CreateSession via RPC)
- `services/bossd/internal/testharness/mock_git.go:1-100` — NEW: Mock WorktreeManager recording calls, returning configurable results
- `services/bossd/internal/testharness/mock_claude.go:1-135` — NEW: Mock ClaudeRunner with EmitOutput for simulating Claude output in tests
- `services/bossd/internal/testharness/mock_vcs.go:1-90` — NEW: Mock VCS provider with configurable PR status, check results
- `services/bossd/internal/testharness/e2e_lifecycle_test.go:1-443` — NEW: 5 E2E tests: full lifecycle, checks-failed fix loop, archive/resurrect, state-filtered listing, PR merged transition

### Learnings & Notes

- Unix socket paths have a 104-char limit on macOS — `t.TempDir()` paths are too long; use `/tmp` with an atomic counter for test sockets
- `safego.Go` wraps the entire goroutine body with recover, so the user's deferred functions inside `fn` run before the recovery handler catches — this creates a race if you try to signal completion from inside the panicking function and then check logs outside
- The `make test` Makefile target uses `cd $mod && go test` which fails for cross-module dependencies; workspace-level `go test ./...` works correctly
- `go mod tidy` cannot be run from inside individual modules that depend on unpublished local modules — build/test from workspace root instead
- One trivial goroutine in `claude/runner.go` (context cleanup: `<-ctx.Done(); s.remove(ch)`) was left without safego since it cannot panic and has no logger in scope — annotated with a comment

### Issues Encountered

- Pre-existing lint errors (14 issues across boss, bossd, bosso) — not introduced by this leg, still present. Should be addressed in a cleanup task.

### Next Steps

This completes the Go rewrite flight legs as defined in the plan. The remaining work would be new feature legs (web SPA, Terraform infra, etc.) which are not yet planned as bd tasks.

Potential next work:
- Fix pre-existing lint errors across boss/bossd/bosso modules
- Add more E2E test coverage (conflict detection, review feedback paths)
- Web SPA implementation (React + ConnectRPC web client)
- Terraform infrastructure modules

### Resume Command

To continue this work:
1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/bossd/internal/testharness/harness.go`, `lib/bossalib/safego/safego.go`

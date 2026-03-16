## Handoff: Flight Leg 12b — macOS LaunchAgent + Daemon Commands

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-s0fv, bossanova-inl2, bossanova-0fii, bossanova-wia6

### Tasks Completed

- bossanova-s0fv: Create macOS LaunchAgent plist template for bossd
- bossanova-inl2: Implement boss daemon install/uninstall/status subcommands
- bossanova-0fii: Add auto-start daemon on first boss CLI invocation
- bossanova-wia6: [HANDOFF]

### Files Changed

- `services/boss/internal/daemon/launchd.go:1-300` — NEW: Package managing bossd lifecycle via macOS LaunchAgent — plist generation, install/uninstall via launchctl, status checking, auto-start with socket polling
- `services/boss/internal/daemon/launchd_test.go:1-40` — NEW: Tests for plist generation and path resolution
- `services/boss/cmd/main.go:34,47` — Added `daemonCmd()` to root command's `AddCommand`
- `services/boss/cmd/main.go:154-188` — NEW: `daemonCmd()` with `install`, `uninstall`, `status` subcommands
- `services/boss/cmd/handlers.go:17` — Added `daemon` package import
- `services/boss/cmd/handlers.go:20-31` — Updated `newClient` to call `daemon.EnsureRunning` for auto-start
- `services/boss/cmd/handlers.go:271-313` — NEW: `runDaemonInstall`, `runDaemonUninstall`, `runDaemonStatus` handlers

### Learnings & Notes

- LaunchAgent plist uses `RunAtLoad` + `KeepAlive` to ensure bossd runs continuously and restarts on crash
- `ResolveBossdPath` checks next to the boss executable first (co-located install), then falls back to $PATH
- `EnsureRunning` tries LaunchAgent (if installed) first, then falls back to starting bossd directly as a background process — this means `boss` works even without explicit `daemon install`
- CGO warnings during build are benign clang deployment version warnings (macOS 26 vs 16 target)
- The `go vet` output only shows these clang warnings, no actual issues
- PATH in the plist includes `/opt/homebrew/bin` for Apple Silicon Homebrew installs

### Issues Encountered

- None — implementation was straightforward following existing Cobra command patterns

### Next Steps (Flight Leg 12c: GitHub Actions CI + Release + Homebrew)

- bossanova-h3mk: Create GitHub Actions CI workflow (lint + test on PR/push)
- bossanova-trad: Create GitHub Actions release workflow (cross-platform build on tag)
- bossanova-va5c: Create GitHub Actions splitsh-lite mirror workflow (post-merge to main)
- bossanova-idlp: Create Homebrew formula for boss + bossd tap
- bossanova-4l31: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `services/boss/internal/daemon/launchd.go`, `services/boss/cmd/main.go`, `services/boss/cmd/handlers.go`

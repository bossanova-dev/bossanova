## Handoff: Flight Leg 12a — Cross-Platform Build + Version Injection

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-tpqq, bossanova-2vuv, bossanova-s2ti, bossanova-ru6x

### Tasks Completed

- bossanova-tpqq: Add version/commit/date ldflags to Makefile build targets
- bossanova-2vuv: Add cross-platform build targets to Makefile (darwin/amd64, darwin/arm64, linux/amd64)
- bossanova-s2ti: Add version command to boss and bossd CLIs
- bossanova-ru6x: [HANDOFF]

### Files Changed

- `Makefile:9-16` — Added VERSION/COMMIT/DATE variables and LDFLAGS with `-s -w` stripping, injecting `bossalib/buildinfo` package vars
- `Makefile:26-33` — Updated build targets to pass `$(LDFLAGS)` to `go build`
- `Makefile:58-70` — NEW: `build-all` target for cross-platform builds (darwin/amd64, darwin/arm64, linux/amd64) with CGO_ENABLED=0 for static linking; bosso only built for linux/amd64
- `lib/bossalib/buildinfo/buildinfo.go` — NEW: Package with `Version`, `Commit`, `Date` vars (set via ldflags) and `String()` helper
- `services/boss/cmd/main.go:6,34,51-60` — Added `buildinfo` import, `versionCmd()` cobra subcommand printing `boss <version>`
- `services/bossd/cmd/main.go:5,17,28-36` — Added `flag` and `buildinfo` imports, `--version` flag printing `bossd <version>`

### Learnings & Notes

- CGO_ENABLED=0 is required for true cross-compilation since boss uses `go-keychain` (macOS CGO dep) and bossd uses `modernc.org/sqlite` — both cross-compile fine with CGO disabled
- `-s -w` ldflags strip debug info and DWARF symbols, reducing binary size significantly
- `git describe --tags --always --dirty` produces clean version strings: tag-based when tags exist, commit hash otherwise, `-dirty` suffix for uncommitted changes
- bosso only needs linux/amd64 since it deploys to Fly.io — no need to cross-compile for darwin

### Issues Encountered

- `make clean && make build` failed because `clean` removes `lib/bossalib/gen/` (protobuf generated code) — must run `make generate` first. This is expected behavior, not a bug.
- The original 12a handoff task and 12b handoff task IDs were lost during creation (subagent ID resolution issue). Created replacement tasks `bossanova-ru6x` (12a) and `bossanova-wia6` (12b) with proper dependency wiring.

### Next Steps (Flight Leg 12b: macOS LaunchAgent + Daemon Commands)

- bossanova-s0fv: Create macOS LaunchAgent plist template for bossd
- bossanova-inl2: Implement boss daemon install/uninstall/status subcommands
- bossanova-0fii: Add auto-start daemon on first boss CLI invocation
- bossanova-wia6: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `Makefile`, `lib/bossalib/buildinfo/buildinfo.go`, `services/boss/cmd/main.go`, `services/bossd/cmd/main.go`

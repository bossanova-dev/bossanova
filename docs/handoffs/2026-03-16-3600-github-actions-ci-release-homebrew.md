## Handoff: Flight Leg 12c — GitHub Actions CI + Release + Homebrew

**Date:** 2026-03-16
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md
**bd Issues Completed:** bossanova-h3mk, bossanova-trad, bossanova-va5c, bossanova-idlp, bossanova-4l31

### Tasks Completed

- bossanova-h3mk: Create GitHub Actions CI workflow (lint + test on PR/push)
- bossanova-trad: Create GitHub Actions release workflow (cross-platform build on tag)
- bossanova-va5c: Create GitHub Actions splitsh-lite mirror workflow (post-merge to main)
- bossanova-idlp: Create Homebrew formula for boss + bossd tap
- bossanova-4l31: [HANDOFF]

### Files Changed

- `.github/workflows/ci.yml:1-73` — NEW: CI workflow with lint (buf + golangci-lint per module), test (all modules), and build jobs. Triggered on push to main and PRs
- `.github/workflows/release.yml:1-74` — NEW: Release workflow triggered by v* tags. Matrix build for darwin/amd64, darwin/arm64, linux/amd64 with CGO_ENABLED=0. Uses softprops/action-gh-release for GitHub Release creation with auto-generated notes
- `.github/workflows/split.yml:1-44` — NEW: splitsh-lite mirror workflow. Matrix strategy for proto→bossanova-proto, bossalib→bossalib, boss→boss, bossd→bossd. Requires SPLIT_PUSH_TOKEN secret
- `infra/homebrew/bossanova.rb:1-51` — NEW: Homebrew formula template with platform-specific binary downloads and bossd as a resource. Uses ${VERSION} and ${SHA256_*} placeholders
- `infra/homebrew/generate-formula.sh:1-28` — NEW: Script to generate formula from template by filling in version and SHA256 checksums from release artifacts

### Learnings & Notes

- golangci-lint-action@v8 supports `working-directory` for per-module linting in a multi-module workspace
- The release workflow uses matrix builds + upload-artifact/download-artifact to parallelize cross-platform builds, then a single release job collects all artifacts
- splitsh-lite needs `fetch-depth: 0` for full history; requires a PAT (SPLIT_PUSH_TOKEN) with push access to mirror repos (not yet created)
- Homebrew formula uses `resource` blocks for bossd since it's a companion binary — both boss and bossd install to bin/
- Pre-existing lint issues exist in boss/auth, boss/daemon, bossd/upstream, and bosso/cmd — these are not from this leg but will surface in CI
- Mirror repos (recurser/bossanova-proto, recurser/bossalib, recurser/boss, recurser/bossd) need to be created on GitHub before the split workflow can push

### Issues Encountered

- Pre-existing lint errors (14 issues across boss, bossd, bosso) — not introduced by this leg, will fail CI lint job. Should be addressed in a future cleanup task.

### Next Steps (Flight Leg 12d: E2E Tests + Panic Recovery)

- bossanova-636q: Add panic recovery to goroutines in bossd and bosso
- bossanova-aben: Create E2E test harness with mock git repo and mock Claude process
- bossanova-loey: Write E2E test: full session lifecycle (create → run → PR → fix loop)
- bossanova-7vc6: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review files: `.github/workflows/ci.yml`, `.github/workflows/release.yml`, `.github/workflows/split.yml`, `infra/homebrew/bossanova.rb`

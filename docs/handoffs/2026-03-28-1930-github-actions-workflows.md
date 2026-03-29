## Handoff: Flight Leg 5 - GitHub Actions Release Pipeline + Public Mirror

**Date:** 2026-03-28 19:30 UTC
**Branch:** office-hours
**Flight ID:** fp-2026-03-28-1513-distribution-first-external-user
**Planning Doc:** docs/plans/2026-03-28-1513-distribution-first-external-user.md
**bd Issues Completed:** bossanova-9q6q, bossanova-a9ve, bossanova-i5by, bossanova-uqfb, bossanova-omzd

### Tasks Completed

- bossanova-9q6q: Create perform-production-release.yml workflow
- bossanova-a9ve: Create semantic-release config
- bossanova-i5by: Create mirror-public.yml workflow
- bossanova-uqfb: Deprecate old deploy and split workflows
- bossanova-omzd: [HANDOFF] Run /boss-verify and /boss-handoff

### Files Changed

- `.github/workflows/perform-production-release.yml:1-293` - New GitOps release pipeline with 6 jobs: version (semantic-release), build (3 platforms + plugins), notarize (macOS), release (public repo), homebrew (tap update), bump-versions
- `.releaserc.yml:1-54` - Semantic-release configuration for Go monorepo with conventional commits, commit-analyzer, release-notes-generator, changelog, and git plugins
- `.github/workflows/mirror-public.yml:1-50` - Copy-and-strip workflow that removes private directories and force-pushes to public repo (bossanova-dev/bossanova)
- `.github/workflows/deploy.yml:1` - Added deprecation comment (replaced by perform-production-release.yml)
- `.github/workflows/split.yml:1` - Added deprecation comment (replaced by mirror-public.yml)

### Learnings & Notes

- perform-production-release.yml uses semantic-release to determine version dynamically, then builds/signs/releases all binaries
- Build matrix covers 3 platforms (darwin/amd64, darwin/arm64, linux/amd64) and builds 5 binaries per platform (boss, bossd, 3 plugins)
- Notarization job runs on macos-latest for darwin binaries only, uses Apple Developer certificates
- Release creates on public repo (bossanova-dev/bossanova) using gh CLI
- Homebrew job generates formula using infra/homebrew/generate-formula.sh and pushes to bossanova-dev/homebrew-tap
- mirror-public.yml strips: plugins/, services/bosso/, web/, infra/, docs/, TODOS.md, .github/workflows/
- Both workflows use BOSSANOVA_PUBLIC_DEPLOY_KEY secret for public repo access
- Semantic-release config writes version to .VERSION file and updates CHANGELOG.md
- Old workflows marked deprecated but not deleted for transition period

### Issues Encountered

- None - implementation straightforward, all post-flight checks passed

### Next Steps (Flight Leg 6: README + First-Run Empty State)

According to the plan (lines 370-455), the next flight leg includes:

- Create public-facing README.md
- Implement first-run empty state in TUI home view
- Test first-run empty state
- [HANDOFF] Review Flight Leg 6

The next handoff task is: bossanova-nmbl

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-28-1513-distribution-first-external-user"` - should show bossanova-nmbl or tasks for Flight Leg 6
2. Review files: .github/workflows/perform-production-release.yml, .releaserc.yml, .github/workflows/mirror-public.yml
3. Review plan: docs/plans/2026-03-28-1513-distribution-first-external-user.md (Flight Leg 6 starts at line 370)

### Post-Flight Verification Summary

All verification tests passed:

- ✓ YAML validation (both workflows parse correctly)
- ✓ perform-production-release.yml triggers on production branch
- ✓ Build matrix covers 3 platforms
- ✓ Plugins built (autopilot, dependabot, repair)
- ✓ Notarize job configured for darwin only
- ✓ Release targets bossanova-dev/bossanova public repo
- ✓ mirror-public.yml triggers on main and production branches
- ✓ Strip list matches spec exactly
- ✓ Both workflows use BOSSANOVA_PUBLIC_DEPLOY_KEY secret
- ✓ Semantic-release config properly structured
- ✓ Old workflows marked deprecated
- ✓ make format: no changes
- ✓ make test: all tests pass

Known limitations:

- Workflows cannot be fully tested until production branch exists and secrets are configured
- Requires GitHub Actions secrets: BOSSANOVA_PUBLIC_DEPLOY_KEY, Apple Developer certificates
- Public repos must exist: bossanova-dev/bossanova, bossanova-dev/homebrew-tap

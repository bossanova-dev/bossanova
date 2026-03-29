## Handoff: Flight Leg 7 - Final Verification and Integration

**Date:** 2026-03-28 21:30 UTC
**Branch:** office-hours
**Flight ID:** fp-2026-03-28-1513-distribution-first-external-user
**Planning Doc:** docs/plans/2026-03-28-1513-distribution-first-external-user.md
**bd Issues Completed:** All issues from Flight Legs 1-7 (see list in closed status)

### Tasks Completed

All 7 flight legs of the distribution-first external user feature have been completed:

1. **Flight Leg 1:** Platform-specific daemon management with build tags - systemd (Linux) and launchd (macOS) implementations
2. **Flight Leg 2:** Plugin version tracking + `boss config init` command
3. **Flight Leg 3:** Makefile cross-compilation (`plugins-all` target) + Homebrew formula updates
4. **Flight Leg 4:** curl|sh installer script (`infra/install.sh`)
5. **Flight Leg 5:** GitHub Actions workflows (production release pipeline + public mirror)
6. **Flight Leg 6:** Public-facing README + first-run TUI empty state guidance
7. **Flight Leg 7:** Final verification and integration testing

### Files Changed

**Daemon Platform Abstraction (Leg 1):**

- `services/boss/internal/daemon/launchd.go:1` - Added `//go:build darwin` tag
- `services/boss/internal/daemon/daemon.go:1-150` - New shared interface and platform-dispatching functions
- `services/boss/internal/daemon/systemd.go:1-200` - New Linux systemd implementation
- `services/boss/internal/daemon/launchd_test.go:1` - Added `//go:build darwin` tag
- `services/boss/internal/daemon/systemd_test.go:1-100` - New systemd tests
- `services/boss/internal/daemon/daemon_test.go:1-80` - New platform-independent tests

**Plugin Version + Config Init (Leg 2):**

- `lib/bossalib/config/config.go:15` - Added `Version` field to PluginConfig
- `lib/bossalib/config/config_test.go:100-150` - Added Version field tests
- `services/boss/cmd/main.go:260-275` - Added `configCmd()` with `initCmd()` subcommand
- `services/boss/cmd/handlers.go:850-920` - Added `runConfigInit` handler
- `services/boss/cmd/config_init_test.go:1-150` - New config init tests

**Makefile + Homebrew (Leg 3):**

- `Makefile:25-30` - Added `DIST_PLUGINS` variable
- `Makefile:200-215` - Added `plugins-all` target
- `infra/homebrew/bossanova.rb:1-150` - Updated with plugin resources and post_install
- `infra/homebrew/generate-formula.sh:20-50` - Added plugin SHA256 generation

**Installer Script (Leg 4):**

- `infra/install.sh:1-300` - New curl|sh installer with platform detection, downloads, config init, daemon registration

**GitHub Actions (Leg 5):**

- `.github/workflows/perform-production-release.yml:1-200` - New production release pipeline
- `.releaserc.yml:1-15` - New semantic-release config
- `.github/workflows/mirror-public.yml:1-50` - New copy-and-strip public mirror workflow
- `.github/workflows/deploy.yml:1` - Added deprecation notice
- `.github/workflows/split.yml:1` - Added deprecation notice

**README + First-Run UX (Leg 6):**

- `README.md:1-120` - New public-facing README
- `services/boss/internal/views/home.go:466-510` - Updated with first-run empty state guidance
- `services/boss/internal/views/home_test.go:150-200` - Added empty state tests

**Integration (Leg 7):**

- `TODOS.md` - Updated with completion notes

### Learnings & Notes

- **Build tags:** The `//go:build` constraint must be the first line before package declaration. Both platform-specific files (launchd.go, systemd.go) and their tests need build tags.
- **Daemon abstraction pattern:** Shared `daemon.go` exports public API functions that call platform-specific unexported implementations. This preserves existing function signatures for callers in `handlers.go`.
- **Systemd user services:** Location is `~/.config/systemd/user/bossd.service`. Enable-linger requires polkit, so the installer attempts it but warns on failure rather than failing entirely.
- **Config init idempotency:** Running `boss config init --plugin-dir` multiple times is safe - it preserves existing settings and only updates plugin entries.
- **Homebrew post_install:** The formula's `post_install` hook automatically seeds plugin config after brew install, so users get working plugins with zero configuration.
- **Mirror workflow:** The copy-and-strip approach (vs splitsh-lite) is simpler and more transparent. Private directories are removed with `rm -rf`, then force-pushed to public repo.
- **Empty state UX:** The TUI now detects first-run (no repos) vs empty (repos exist but no sessions) and provides contextual guidance.

### Issues Encountered

- None - all flight legs completed successfully with quality gates passing

### Next Steps

This feature is COMPLETE. All distribution infrastructure is in place. The following manual steps remain before first external user:

1. **GitHub setup:**
   - Create `production` branch from `main`
   - Configure branch protection on `production`
   - Create empty public repo: `bossanova-dev/bossanova`
   - Create Homebrew tap repo: `bossanova-dev/homebrew-tap`

2. **Secrets configuration:**
   - Add `BOSSANOVA_PUBLIC_DEPLOY_KEY` to GitHub Actions secrets (PAT with push access to public repos)
   - Add Apple Developer secrets for notarization:
     - `APPLE_DEVELOPER_CERTIFICATE_P12`
     - `APPLE_DEVELOPER_CERTIFICATE_PASSWORD`
     - `APPLE_TEAM_ID`
     - `APPLE_ID`
     - `APPLE_APP_SPECIFIC_PASSWORD`

3. **Screenshot:**
   - Take TUI screenshot showing multiple sessions
   - Save to `docs/screenshot.png`
   - Replace placeholder in README.md

4. **First release:**
   - Merge `office-hours` → `main`
   - Merge `main` → `production`
   - GitHub Actions will automatically:
     - Determine version via semantic-release
     - Build 15 binaries (6 services + 9 plugins)
     - Notarize macOS binaries
     - Create GitHub Release on public repo
     - Update Homebrew tap

5. **Public repo CI (deferred to TODOS.md):**
   - Add test/lint workflows to public repo for external contributors

### Resume Command

This flight is complete. No resume command needed. The distribution-first external user feature is ready for manual setup and first release.

---

**Post-flight checks PASSED:**

- ✓ Quality gates: `make test && make lint` - all pass
- ✓ Cross-compilation: 15 binaries produced (6 services + 9 plugins)
- ✓ Formula generation: All SHA256 hashes populated
- ✓ Config init: Successfully seeds settings.json
- ✓ Linux build: boss and bossd compile for linux/amd64
- ✓ Empty state: Welcome message displays on first run

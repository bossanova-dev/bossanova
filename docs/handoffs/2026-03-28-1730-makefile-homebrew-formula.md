## Handoff: Flight Leg 3 - Makefile Cross-Compilation + Homebrew Formula Update

**Date:** 2026-03-28 17:30 UTC
**Branch:** office-hours
**Flight ID:** fp-2026-03-28-1513-distribution-first-external-user
**Planning Doc:** docs/plans/2026-03-28-1513-distribution-first-external-user.md
**bd Issues Completed:** bossanova-gsy1, bossanova-e3cx, bossanova-ow8a

### Tasks Completed

- bossanova-gsy1: Add plugins-all target to root Makefile
- bossanova-e3cx: Update Homebrew formula template with plugin resources
- bossanova-ow8a: Update formula generator script for plugins

### Files Changed

- `Makefile:7-11` - Added DIST_PLUGINS variable for 3 plugin binaries
- `Makefile:199-211` - Added plugins-all target for cross-platform plugin compilation
- `Makefile:213-214` - Added plugins-all to .PHONY and all target
- `infra/homebrew/bossanova.rb:1-183` - Updated formula with plugin resource blocks, updated install method to stage plugins to libexec/plugins/, added post_install hook to run boss config init
- `infra/homebrew/generate-formula.sh:6-97` - Added 9 new plugin SHA256 substitutions and updated usage documentation

### Learnings & Notes

- Plugins-all target mirrors build-all pattern with platform loop and plugin loop
- Each plugin has 3 platform variants (darwin/arm64, darwin/amd64, linux/amd64) = 9 binaries total
- Homebrew formula resource blocks are platform-specific - each platform (arm64, amd64, linux) has separate resource entries for all 3 plugins
- Plugin binaries staged to libexec/plugins/ to keep them separate from main bin/ directory
- post_install hook runs `boss config init --plugin-dir` to seed settings.json automatically on Homebrew install
- Formula generator expects 15 binaries: 6 existing (boss + bossd × 3 platforms) + 9 new (3 plugins × 3 platforms)

### Issues Encountered

- None - implementation straightforward, followed existing patterns

### Next Steps (Flight Leg 4: curl|sh Installer Script)

Check `bd ready --label "flight:fp-2026-03-28-1513-distribution-first-external-user"` for next tasks. Remaining work includes:

- Create infra/install.sh installer script
- Test installer locally (manual verification)

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-28-1513-distribution-first-external-user"` to see available tasks for this flight
2. Review files: Makefile, infra/homebrew/bossanova.rb, infra/homebrew/generate-formula.sh

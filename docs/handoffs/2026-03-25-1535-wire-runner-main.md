## Handoff: Flight Leg 3 — Wire Into Runner + Main

**Date:** 2026-03-25 15:35
**Branch:** inject-our-boss-claude-skills-into-claude-sessions
**Flight ID:** fp-2026-03-25-1319-inject-boss-skills-into-claude-sessions
**Planning Doc:** docs/plans/2026-03-25-1319-inject-boss-skills-into-claude-sessions.md

### Tasks Completed This Flight Leg

- bossanova-0x32: Add --add-dir flag to runner.go Start()
- bossanova-5ks2: Add --add-dir flag tests to runner_test.go
- bossanova-awel: Add ExtractSkills call to main.go startup

### Files Changed

- `services/bossd/internal/claude/runner.go` — Added `--add-dir cfg.SkillsDir` after the DangerouslySkipPermissions block (3 lines)
- `services/bossd/internal/claude/runner_test.go` — Added TestStartWithAddDir and TestStartWithoutAddDir (mirroring DangerouslySkipPermissions test pattern)
- `services/bossd/cmd/main.go` — Added `skilldata` import and `claude.ExtractSkills()` call after `config.Load()`, with warn-only error handling

### Implementation Notes

- `--add-dir` is appended to args only when `cfg.SkillsDir != ""` (empty string disables)
- TestStartWithoutAddDir uses `{"skills_dir": ""}` to explicitly override the default — empty JSON `{}` inherits `~/.boss` from DefaultSettings
- ExtractSkills failure is a warning, not fatal — daemon continues even if skill extraction fails
- All 11 test packages pass

### Current Status

- Runner tests: PASS (including 2 new --add-dir tests)
- Full test suite: PASS (all 11 packages green)
- Build: PASS (go build ./cmd/main.go)
- Git: clean (committed 15d193a)

### Next Flight Leg

- bossanova-i0tf: Run full test suites across bossalib and bossd
- bossanova-rb5x: Verify end-to-end Makefile flow and skill extraction
- bossanova-wfjy: [HANDOFF] Final Review

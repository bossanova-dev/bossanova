## Handoff: Flight Leg 2 — Extraction Logic + Config

**Date:** 2026-03-25 15:20
**Branch:** inject-our-boss-claude-skills-into-claude-sessions
**Flight ID:** fp-2026-03-25-1319-inject-boss-skills-into-claude-sessions
**Planning Doc:** docs/plans/2026-03-25-1319-inject-boss-skills-into-claude-sessions.md

### Tasks Completed This Flight Leg

- bossanova-pm6p: Add SkillsDir field to config.Settings with default
- bossanova-jpsp: Add SkillsDir tests to config_test.go
- bossanova-r5s6: Create ExtractSkills function in skills.go
- bossanova-gmay: Create skills_test.go with extraction tests

### Files Changed

- `lib/bossalib/config/config.go` — Added `SkillsDir string` field to Settings struct; DefaultSettings() returns `~/.boss`
- `lib/bossalib/config/config_test.go` — Added TestDefaultSettings assertions for SkillsDir, TestSkillsDirRoundTrip, TestSkillsDirOmittedWhenEmpty
- `services/bossd/internal/claude/skills.go` — New ExtractSkills(destDir, fsys) function that walks embed.FS and writes to `<destDir>/.claude/skills/`
- `services/bossd/internal/claude/skills_test.go` — TestExtractSkills, TestExtractSkillsIdempotent, TestExtractSkillsCreatesDirectories using fstest.MapFS

### Implementation Notes

- `SkillsDir` uses `json:"skills_dir,omitempty"` — omitted from JSON when empty string, present when set by DefaultSettings
- ExtractSkills accepts `fs.FS` (not `embed.FS`) for testability — tests use `fstest.MapFS`
- ExtractSkills overwrites existing files on every call (handles binary upgrades)
- Test FS includes both SKILL.md files and a shell script to verify non-md files are also extracted

### Current Status

- Config tests: PASS (all config tests green)
- Skills tests: PASS (3 extraction tests green)
- Git: clean (committed 6089af1)

### Next Flight Leg

- bossanova-0x32: Add --add-dir flag to runner.go Start()
- bossanova-5ks2: Add --add-dir flag tests to runner_test.go
- bossanova-awel: Add ExtractSkills call to main.go startup
- bossanova-zvkc: [HANDOFF] Review Flight Leg 3

# Inject Boss Skills into Claude Sessions

**Flight ID:** fp-2026-03-25-1319-inject-boss-skills-into-claude-sessions

## Overview

Make boss-\*.md skills available as real `/slash-commands` in every Claude session the daemon spawns — including sessions running in user repo worktrees that don't have `.claude/skills/`. Skills are embedded in the bossd binary at build time, extracted to `~/.boss/.claude/skills/` at daemon startup, and injected via `claude --add-dir ~/.boss/`.

## Architecture

```
BUILD TIME                              RUNTIME (daemon startup)
────────────────────────────            ────────────────────────────
.claude/skills/boss-*/SKILL.md          bossd binary (embedded skills)
         │                                       │
    Makefile copy                          ExtractSkills()
         │                                       │
         v                                       v
services/bossd/internal/claude/         ~/.boss/.claude/skills/
  skilldata/skills/boss-*/SKILL.md        boss-create-tasks/SKILL.md
         │                                boss-implement/SKILL.md
    //go:embed skills/*                   boss-finalize/SKILL.md
         │                                ...
         v                                       │
    embed.FS in binary               runner.go adds --add-dir
                                             │
                                             v
                                      Claude discovers skills as
                                      real /boss-* slash commands
```

## Affected Areas

- [ ] `services/bossd/internal/claude/` — New skilldata embed package + extraction logic
- [ ] `services/bossd/internal/claude/runner.go` — Add `--add-dir` flag to CLI args
- [ ] `services/bossd/cmd/main.go` — Extract skills at startup
- [ ] `services/bossd/Makefile` — Copy skills before build
- [ ] `lib/bossalib/config/config.go` — Add `SkillsDir` to Settings

## Design References

- Existing embed pattern: `services/bossd/migrations/embed.go` (line 8)
- Existing config pattern: `lib/bossalib/config/config.go:105` (Settings struct)
- Existing flag test pattern: `services/bossd/internal/claude/runner_test.go:413` (TestStartWithDangerouslySkipPermissions)
- Claude CLI `--add-dir` flag: loads `.claude/skills/` from additional directories

## Key Decisions (from eng review)

1. **`--add-dir` over prompt inlining** — Skills appear as real `/slash-commands` usable by both autopilot and interactive users
2. **Embed + extract over runtime file read** — Works when installed via Homebrew (no source repo on disk)
3. **`--add-dir` in `runner.go:Start()`** — Single injection point covers all call sites (lifecycle, autopilot, fixloop, resurrect)
4. **`SkillsDir` in config with `~/.boss/` default** — Testable, overridable, follows existing patterns
5. **Project skills win on collision** — Claude's precedence: project > add-dir. Users can customize.

---

## Flight Leg 1: Build Infrastructure (Makefile + Embed)

### Tasks

- [ ] Create `services/bossd/internal/claude/skilldata/` directory
- [ ] Create `services/bossd/internal/claude/skilldata/embed.go` — embed.FS for skill files
  - Pattern: Follow `services/bossd/migrations/embed.go` (same `//go:embed` pattern)
  - Content:

    ```go
    package skilldata

    import "embed"

    // SkillsFS contains the embedded boss skill files.
    // The skills/ directory is populated by `make copy-skills` before build.
    //
    //go:embed skills
    var SkillsFS embed.FS
    ```

- [ ] Create `services/bossd/internal/claude/skilldata/.gitignore` — ignore copied skill files
  - Content: `skills/`
- [ ] Add `copy-skills` and `clean-skills` targets to `services/bossd/Makefile`
  - `copy-skills` copies `../../.claude/skills/boss-*` into `internal/claude/skilldata/skills/`
  - `build` depends on `copy-skills`
  - `dev` depends on `copy-skills`
  - `clean` runs `clean-skills`
  - Implementation:

    ```makefile
    SKILLS_SRC := ../../.claude/skills
    SKILLS_DST := internal/claude/skilldata/skills

    ## copy-skills: Copy boss skill files for embedding
    copy-skills:
    	rm -rf $(SKILLS_DST)
    	mkdir -p $(SKILLS_DST)
    	for dir in $(SKILLS_SRC)/boss-*; do \
    		name=$$(basename $$dir); \
    		mkdir -p $(SKILLS_DST)/$$name; \
    		cp $$dir/SKILL.md $(SKILLS_DST)/$$name/SKILL.md; \
    	done

    ## clean-skills: Remove copied skill files
    clean-skills:
    	rm -rf $(SKILLS_DST)
    ```

  - Update `build:` to depend on `copy-skills`
  - Update `dev:` to depend on `copy-skills`
  - Update `clean:` to include `clean-skills`

- [ ] Create placeholder file `services/bossd/internal/claude/skilldata/skills/.gitkeep` so go embed works in CI before copy-skills runs
  - Actually: the `.gitignore` ignores `skills/`, and `//go:embed skills` requires the directory to exist. The Makefile creates it. For `go test ./...` without make, we need a build tag or the directory pre-created. Simplest: the Makefile `test` target also depends on `copy-skills`.

### Post-Flight Checks for Flight Leg 1

- [ ] **Build works:** `cd services/bossd && make clean && make build` succeeds
- [ ] **Embedded files present:** `go run ./cmd -version` doesn't crash (binary starts, has embedded FS)
- [ ] **Skills copied:** `ls services/bossd/internal/claude/skilldata/skills/boss-implement/SKILL.md` exists after `make copy-skills`
- [ ] **Gitignore works:** `git status services/bossd/internal/claude/skilldata/skills/` shows no untracked files after `make copy-skills`
- [ ] **Tests pass:** `cd services/bossd && make test` passes

### [HANDOFF] Review Flight Leg 1

Human reviews: Makefile targets, embed.go structure, .gitignore correctness.

---

## Flight Leg 2: Extraction Logic + Config

### Tasks

- [ ] Add `SkillsDir` field to `lib/bossalib/config/config.go` Settings struct
  - Add field: `SkillsDir string \`json:"skills_dir,omitempty"\``
  - Update `DefaultSettings()` to set default: `SkillsDir: filepath.Join(home, ".boss")`
  - Files: `lib/bossalib/config/config.go:105-129`
- [ ] Add `SkillsDir` test to `lib/bossalib/config/config_test.go`
  - Add assertion in `TestDefaultSettings`: `SkillsDir` is non-empty and ends with `.boss`
  - Add `TestSkillsDirRoundTrip`: save/load with custom `skills_dir`, verify it persists
  - Add `TestSkillsDirOmittedWhenEmpty`: when `SkillsDir` matches default, verify `skills_dir` appears in JSON (it's set by DefaultSettings so it won't be empty)
  - Pattern: Follow existing `TestSaveAndLoad` and `TestPluginsRoundTrip`
- [ ] Create `services/bossd/internal/claude/skills.go` — extraction logic
  - Function: `ExtractSkills(destDir string, fsys fs.FS) error`
  - Walks the embedded FS, creates `<destDir>/.claude/skills/<name>/SKILL.md` for each skill
  - Overwrites existing files (handles upgrades — binary may have newer skills)
  - Creates directories as needed (`os.MkdirAll`)
  - Returns error only on write failures (not if destDir exists already)
  - Implementation sketch:
    ```go
    func ExtractSkills(destDir string, fsys fs.FS) error {
        return fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
            if err != nil { return err }
            if d.IsDir() { return nil }
            // path is "skills/boss-implement/SKILL.md"
            // destPath is "<destDir>/.claude/skills/boss-implement/SKILL.md"
            // Strip leading "skills/" and prepend destDir/.claude/skills/
            rel := strings.TrimPrefix(path, "skills/")
            destPath := filepath.Join(destDir, ".claude", "skills", rel)
            if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
                return fmt.Errorf("create skill dir: %w", err)
            }
            data, err := fs.ReadFile(fsys, path)
            if err != nil { return fmt.Errorf("read embedded skill: %w", err) }
            return os.WriteFile(destPath, data, 0o644)
        })
    }
    ```
- [ ] Create `services/bossd/internal/claude/skills_test.go` — test extraction
  - `TestExtractSkills`: create a test embed.FS with a fake skill, extract to temp dir, verify files exist with correct content
  - `TestExtractSkillsIdempotent`: extract twice, verify no error and content matches
  - `TestExtractSkillsCreatesDirectories`: extract to a non-existent nested path, verify it works
  - Pattern: Use `testing/fstest.MapFS` to create an in-memory FS for testing

### Post-Flight Checks for Flight Leg 2

- [ ] **Config tests pass:** `cd lib/bossalib && go test ./config/...` passes
- [ ] **Skills tests pass:** `cd services/bossd && make copy-skills && go test ./internal/claude/...` passes
- [ ] **DefaultSettings includes SkillsDir:** Verify via test output that default is `~/.boss`
- [ ] **Extraction works:** Test output confirms files are written to correct paths

### [HANDOFF] Review Flight Leg 2

Human reviews: Config field naming, extraction path structure (`~/.boss/.claude/skills/`), test coverage.

---

## Flight Leg 3: Wire Into Runner + Main

### Tasks

- [ ] Add `--add-dir` flag to `services/bossd/internal/claude/runner.go:Start()`
  - After the `DangerouslySkipPermissions` block (line 157), add:
    ```go
    if cfg.SkillsDir != "" {
        args = append(args, "--add-dir", cfg.SkillsDir)
    }
    ```
  - This adds `--add-dir ~/.boss/` (or custom path) to every Claude invocation
- [ ] Add `TestStartWithAddDir` to `services/bossd/internal/claude/runner_test.go`
  - Write a config file with `{"skills_dir": "/tmp/test-skills"}`
  - Capture args via `WithCommandFactory`
  - Assert `--add-dir` and `/tmp/test-skills` appear in captured args
  - Pattern: Mirror `TestStartWithDangerouslySkipPermissions` (line 413)
- [ ] Add `TestStartWithoutAddDir` to `services/bossd/internal/claude/runner_test.go`
  - Config file with no `skills_dir` (empty string)
  - Assert `--add-dir` does NOT appear in captured args
  - Pattern: Mirror `TestStartWithoutDangerouslySkipPermissions` (line 455)
- [ ] Add skill extraction call to `services/bossd/cmd/main.go`
  - After `settings, _ := config.Load()` (line 104), add:
    ```go
    // Extract embedded skills to disk for Claude --add-dir.
    if err := claude.ExtractSkills(settings.SkillsDir, skilldata.SkillsFS); err != nil {
        log.Warn().Err(err).Msg("failed to extract skills (Claude sessions will lack boss skills)")
    }
    ```
  - Import `skilldata` package
  - Note: this is a warning, not a fatal error — the daemon should still start even if extraction fails

### Post-Flight Checks for Flight Leg 3

- [ ] **Runner tests pass:** `cd services/bossd && make copy-skills && go test ./internal/claude/...` passes
- [ ] **Full test suite passes:** `cd services/bossd && make copy-skills && go test ./...` passes
- [ ] **--add-dir appears in args:** The new test verifies `--add-dir` and the path are in captured args
- [ ] **Daemon starts cleanly:** `cd services/bossd && make dev` starts without errors and logs extraction

### [HANDOFF] Review Flight Leg 3

Human reviews: Flag placement in args, extraction call location in main, error handling approach.

---

## Flight Leg 4: Final Verification

### Tasks

- [ ] Run full test suite across all affected modules:
  ```bash
  cd lib/bossalib && go test ./...
  cd services/bossd && make copy-skills && go test ./...
  ```
- [ ] Verify Makefile flow end-to-end:
  ```bash
  cd services/bossd && make clean && make all
  ```
- [ ] Verify skills are extracted at daemon startup:
  ```bash
  rm -rf ~/.boss/.claude/skills/
  cd services/bossd && make dev
  # Check: ls ~/.boss/.claude/skills/boss-implement/SKILL.md
  ```
- [ ] Verify no untracked files in git:
  ```bash
  git status services/bossd/internal/claude/skilldata/
  ```

### Post-Flight Checks for Final Verification

- [ ] **All tests pass:** Both `bossalib` and `bossd` test suites green
- [ ] **Clean build:** `make clean && make all` succeeds from scratch
- [ ] **Skills extracted:** `~/.boss/.claude/skills/boss-implement/SKILL.md` exists after daemon start
- [ ] **Git clean:** No untracked `skills/` files in the skilldata directory
- [ ] **Upgrade scenario:** Delete `~/.boss/.claude/skills/`, restart daemon, files reappear

### [HANDOFF] Final Review

Human reviews: Complete feature before merge. Test with a real autopilot run if possible.

---

## File Change Summary

| File                                                  | Change                                             | Est. Lines |
| ----------------------------------------------------- | -------------------------------------------------- | ---------- |
| `services/bossd/internal/claude/skilldata/embed.go`   | **New**: embed.FS for skill files                  | ~10        |
| `services/bossd/internal/claude/skilldata/.gitignore` | **New**: ignore copied skills                      | ~1         |
| `services/bossd/internal/claude/skills.go`            | **New**: ExtractSkills function                    | ~40        |
| `services/bossd/internal/claude/skills_test.go`       | **New**: extraction tests                          | ~60        |
| `services/bossd/Makefile`                             | **Modified**: add copy-skills/clean-skills targets | ~15        |
| `services/bossd/internal/claude/runner.go`            | **Modified**: add `--add-dir` to args              | ~3         |
| `services/bossd/internal/claude/runner_test.go`       | **Modified**: add --add-dir flag tests             | ~50        |
| `services/bossd/cmd/main.go`                          | **Modified**: call ExtractSkills at startup        | ~5         |
| `lib/bossalib/config/config.go`                       | **Modified**: add SkillsDir field + default        | ~5         |
| `lib/bossalib/config/config_test.go`                  | **Modified**: add SkillsDir tests                  | ~20        |
| **Total**                                             | 4 new, 6 modified                                  | ~210       |

## Rollback Plan

1. Revert the Makefile changes (removes copy step)
2. Delete `services/bossd/internal/claude/skilldata/` directory
3. Delete `services/bossd/internal/claude/skills.go` and `skills_test.go`
4. Revert `runner.go`, `main.go`, `config.go`, `config_test.go`, `runner_test.go`
5. `~/.boss/.claude/skills/` can be left in place harmlessly (Claude ignores unused --add-dir if flag is removed)

## NOT in Scope

- **Hot-reload of skills during session** — Claude's `--add-dir` already handles live reload from disk
- **User-facing config UI for skills_dir** — Config file is sufficient for now
- **Non-boss skills (golang-pro, tui-design, git-committing)** — Dev-time only, not needed for autopilot
- **Skill versioning/compatibility checks** — Skills ship with binary, always in sync
- **Homebrew formula changes** — Separate PR once the binary packaging is set up

## Notes

- Skill files total ~84KB (7 files, 8-16KB each). Well within embed limits.
- `--add-dir` is a Claude Code CLI flag that makes `.claude/skills/` in the added directory discoverable as real slash commands.
- Precedence: project `.claude/skills/` > `--add-dir` skills. Users can override boss skills.
- The `boss-finalize/add-pr-numbers.sh` script is also in `.claude/skills/boss-finalize/` — the Makefile should copy the entire `boss-*` directory contents (not just SKILL.md).

## GSTACK REVIEW REPORT

| Review        | Trigger               | Why                             | Runs | Status | Findings                  |
| ------------- | --------------------- | ------------------------------- | ---- | ------ | ------------------------- |
| CEO Review    | `/plan-ceo-review`    | Scope & strategy                | 0    | —      | —                         |
| Codex Review  | `/codex review`       | Independent 2nd opinion         | 0    | —      | —                         |
| Eng Review    | `/plan-eng-review`    | Architecture & tests (required) | 1    | CLEAR  | 5 issues, 0 critical gaps |
| Design Review | `/plan-design-review` | UI/UX gaps                      | 0    | —      | —                         |

**VERDICT:** ENG CLEARED — ready to implement.

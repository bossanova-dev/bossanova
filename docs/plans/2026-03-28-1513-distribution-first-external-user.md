# Distribution: First External User — Implementation Plan

**Flight ID:** fp-2026-03-28-1513-distribution-first-external-user
**Design Doc:** `~/.gstack/projects/recurser-bossanova/dave-office-hours-design-20260328-133549.md`
**Branch:** `office-hours`
**Status:** APPROVED (Eng Review CLEAR, Design Review CLEAR)

## Overview

Ship Bossanova to its first external user. Build the distribution infrastructure: platform-specific daemon management (build tags), `boss config init` command, `plugins-all` cross-compilation, plugin version tracking, curl|sh installer, Homebrew formula with plugins, GitOps release pipeline (semantic-release), copy-and-strip public repo mirror, README, macOS notarization, and first-run TUI empty state.

## Affected Areas

- [ ] `services/boss/internal/daemon/` — Build tags, shared interface, Linux systemd support
- [ ] `services/boss/cmd/` — New `config init` command
- [ ] `services/boss/internal/views/home.go` — First-run empty state
- [ ] `lib/bossalib/config/config.go` — `Version` field on PluginConfig
- [ ] `Makefile` — `plugins-all` cross-compilation target
- [ ] `infra/install.sh` — New curl|sh installer script
- [ ] `infra/homebrew/bossanova.rb` — Plugin resources in formula template
- [ ] `infra/homebrew/generate-formula.sh` — Plugin SHA256 generation
- [ ] `.github/workflows/perform-production-release.yml` — New GitOps release pipeline
- [ ] `.github/workflows/mirror-public.yml` — New copy-and-strip public repo sync
- [ ] `README.md` — New public-facing README

## Design References

- Design doc: `~/.gstack/projects/recurser-bossanova/dave-office-hours-design-20260328-133549.md`
- Existing daemon: `services/boss/internal/daemon/launchd.go` (310 lines, no build tags)
- Existing config: `lib/bossalib/config/config.go` (PluginConfig struct, Load/Save pattern)
- Existing CLI: `services/boss/cmd/main.go` (cobra commands, `settingsCmd()` pattern)
- Existing formula: `infra/homebrew/bossanova.rb` (template with `${SHA256_*}` placeholders)
- Existing deploy: `.github/workflows/deploy.yml` (tag-triggered, matrix builds, ldflags)
- Existing split: `.github/workflows/split.yml` (splitsh-lite, force-push to mirrors)
- Test patterns: `services/boss/internal/daemon/launchd_test.go`, `lib/bossalib/config/config_test.go`

---

> **IMPORTANT — Machine-Parsed Headers:** The `## Flight Leg N:` headings and
> `### [HANDOFF]` markers are parsed by the autopilot to count flight legs.
> Plans MUST use exactly this heading format. Without them, the autopilot cannot
> determine how many legs the plan has.

## Flight Leg 1: Platform-Specific Daemon Management (Build Tags)

### Context

`launchd.go` currently has no build tags — it compiles on all platforms, which will fail on Linux (uses macOS-specific paths like `~/Library/LaunchAgents`). Need to: add `//go:build darwin` to `launchd.go`, create a shared `daemon.go` interface, and implement `systemd.go` with `//go:build linux`.

### Tasks

- [ ] Add `//go:build darwin` build tag to `services/boss/internal/daemon/launchd.go`
  - Files: `services/boss/internal/daemon/launchd.go`
  - Details: Add `//go:build darwin` as the first line (before package declaration). No other changes to launchd.go.

- [ ] Create shared daemon interface in `services/boss/internal/daemon/daemon.go`
  - Files: `services/boss/internal/daemon/daemon.go` (NEW)
  - Details: No build tags on this file — it compiles on all platforms. Define:
    ```go
    type Status struct {
        Installed bool
        Running   bool
        PID       int
        ServicePath string // plist path on macOS, unit path on Linux
    }
    ```
    Move the `Status` struct here from `launchd.go`. Export platform-dispatching functions: `Install(bossdPath string) error`, `Uninstall() error`, `GetStatus() (*Status, error)`, `EnsureRunning(socketPath string) error`, `ResolveBossdPath() (string, error)`.
  - Pattern: The struct and helper functions (`isSocketReachable`, `waitForSocket`, `ResolveBossdPath`) should live in this file since they're platform-independent. Platform-specific files implement `install()`, `uninstall()`, `getStatus()`, `servicePath()`, `ensureRunningPlatform()` (unexported).
  - **Important:** `launchd.go` already exports `Install`, `Uninstall`, `GetStatus`, `EnsureRunning`, `ResolveBossdPath` as package-level functions. These are called from `services/boss/cmd/handlers.go`. The refactor must preserve these exact function signatures. Approach: keep them as package-level functions in `daemon.go` that call platform-specific unexported implementations. Move `ResolveBossdPath`, `isSocketReachable`, `waitForSocket` to `daemon.go` (they have no platform-specific code). Rename `launchd.go`'s exported functions to unexported `platformInstall`, `platformUninstall`, etc.

- [ ] Create Linux systemd user service in `services/boss/internal/daemon/systemd.go`
  - Files: `services/boss/internal/daemon/systemd.go` (NEW)
  - Details: `//go:build linux`. Implements unexported `platformInstall`, `platformUninstall`, `platformGetStatus`, `platformEnsureRunning`, `platformServicePath`. Unit template from design doc:

    ```ini
    [Unit]
    Description=Bossanova Daemon
    After=network.target

    [Service]
    ExecStart={bossd_path}
    Restart=always
    RestartSec=5

    [Install]
    WantedBy=default.target
    ```

  - Service file location: `~/.config/systemd/user/bossd.service`
  - Install steps: write unit file, `systemctl --user daemon-reload`, `systemctl --user enable --now bossd.service`
  - `loginctl enable-linger $(whoami)` — attempt it, warn if it fails (polkit may not be available)
  - Log location: `journalctl --user -u bossd.service` (no custom log files)

- [ ] Update `services/boss/internal/daemon/launchd.go` to use unexported platform functions
  - Files: `services/boss/internal/daemon/launchd.go`
  - Details: Rename exported functions to unexported `platformInstall`, `platformUninstall`, `platformGetStatus`, `platformEnsureRunning`, `platformServicePath`. Remove `Status` struct (moved to `daemon.go`). Remove `isSocketReachable`, `waitForSocket`, `ResolveBossdPath` (moved to `daemon.go`). Keep `plistData`, `PlistPath` (renamed to `platformServicePath`), `logDir`, `GeneratePlist` as platform-specific.

- [ ] Add `//go:build darwin` to test file and write systemd tests
  - Files: `services/boss/internal/daemon/launchd_test.go` (add build tag), `services/boss/internal/daemon/systemd_test.go` (NEW), `services/boss/internal/daemon/daemon_test.go` (NEW)
  - Details:
    - `launchd_test.go`: Add `//go:build darwin`. Tests remain as-is.
    - `systemd_test.go`: `//go:build linux`. Test unit file generation (template renders correct ExecStart path), test service path returns `~/.config/systemd/user/bossd.service`.
    - `daemon_test.go`: No build tags. Test `ResolveBossdPath` (creates temp binary, finds it). Test `isSocketReachable` (mock socket). Test `waitForSocket` timeout.

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make lint-boss && make test-boss` — all pass on macOS
- [ ] **Build tag verification:** `GOOS=linux go build ./services/boss/cmd` — compiles without errors (verifies systemd.go compiles on Linux target, launchd.go excluded)
- [ ] **Build tag verification:** `GOOS=darwin go build ./services/boss/cmd` — compiles without errors (verifies launchd.go compiles on macOS target, systemd.go excluded)
- [ ] **Function signatures preserved:** `daemon.Install`, `daemon.Uninstall`, `daemon.GetStatus`, `daemon.EnsureRunning` still callable from `handlers.go` — no compile errors

### [HANDOFF] Review Flight Leg 1

Human reviews: Build tag structure, shared interface design, systemd unit template, that existing macOS behavior is preserved.

---

## Flight Leg 2: Plugin Version Tracking + `boss config init` Command

### Context

Need a `Version` field on `PluginConfig` for version mismatch warnings, and a `boss config init --plugin-dir <path>` command that seeds `settings.json` with plugin entries. This command is called by both the curl installer and Homebrew post_install (DRY).

### Tasks

- [ ] Add `Version` field to `PluginConfig` in `lib/bossalib/config/config.go`
  - Files: `lib/bossalib/config/config.go`
  - Details: Add `Version string \`json:"version,omitempty"\``to`PluginConfig`struct after`Enabled`. This is the installed plugin version (e.g., "1.0.0").

- [ ] Add config tests for Version field
  - Files: `lib/bossalib/config/config_test.go`
  - Details: Add test case to existing roundtrip tests that verifies `Version` field serializes/deserializes correctly. Test that existing settings.json without `Version` field still loads (backwards compatibility). Follow existing `t.Run()` subtest pattern.

- [ ] Implement `boss config init` command
  - Files: `services/boss/cmd/main.go` (add `configCmd()` with `initCmd()` subcommand), `services/boss/cmd/handlers.go` (add `runConfigInit` handler)
  - Pattern: Follow existing `settingsCmd()` pattern in main.go (lines 243-256) and handler in handlers.go (lines 793-844).
  - Details:
    - Command: `boss config init --plugin-dir <path>`
    - `--plugin-dir` flag: required string, path to directory containing plugin binaries
    - Behavior:
      1. Validate `--plugin-dir` exists and is a directory
      2. Scan for plugin binaries: `bossd-plugin-autopilot`, `bossd-plugin-dependabot`, `bossd-plugin-repair`
      3. Load existing settings (or defaults if none)
      4. For each found plugin binary: add/update `PluginConfig` entry with `Name`, `Path` (absolute), `Enabled: true`, `Version` (from `buildinfo.Version`)
      5. Preserve all other existing settings (don't overwrite user config)
      6. Save settings
      7. Print summary: "Configured 3 plugins in <settings-path>"
    - Error cases:
      - `--plugin-dir` missing: "Error: --plugin-dir is required"
      - Directory doesn't exist: "Error: plugin directory not found: <path>"
      - No plugin binaries found: "Warning: no plugin binaries found in <path>"

- [ ] Add `config init` tests
  - Files: `services/boss/cmd/config_init_test.go` (NEW)
  - Details: Test cases:
    - `--plugin-dir` with 3 valid plugin binaries → settings.json has 3 entries
    - `--plugin-dir` on existing settings.json → preserves non-plugin settings, updates plugin entries
    - `--plugin-dir` with missing directory → error
    - `--plugin-dir` with empty directory → warning
  - Use `t.TempDir()` for isolated test directories. Create dummy plugin binaries (empty files named correctly).

### Post-Flight Checks for Flight Leg 2

- [ ] **Quality gates:** `make lint-boss && make test-boss && make lint-bossalib && make test-bossalib` — all pass
- [ ] **Config init smoke test:** Build boss (`make build-boss`), then run:
  ```bash
  mkdir -p /tmp/test-plugins
  touch /tmp/test-plugins/bossd-plugin-autopilot
  touch /tmp/test-plugins/bossd-plugin-dependabot
  touch /tmp/test-plugins/bossd-plugin-repair
  bin/boss config init --plugin-dir /tmp/test-plugins
  ```
  Expected: prints "Configured 3 plugins in <path>/settings.json"
- [ ] **Idempotency test:** Run `boss config init --plugin-dir /tmp/test-plugins` twice — second run should succeed without error, settings unchanged

### [HANDOFF] Review Flight Leg 2

Human reviews: PluginConfig Version field, config init command UX, settings.json preservation logic.

---

## Flight Leg 3: Makefile Cross-Compilation + Homebrew Formula Update

### Context

Need `plugins-all` Makefile target for cross-compiling all 3 plugins across 3 platforms (9 binaries). Homebrew formula needs plugin resource blocks. Generator script needs plugin SHA256s.

### Tasks

- [ ] Add `plugins-all` target to root Makefile
  - Files: `Makefile`
  - Details: Add after the `build-all` target (line 197). Pattern mirrors `build-all` loop but for plugins:

    ```makefile
    DIST_PLUGINS := bossd-plugin-autopilot bossd-plugin-dependabot bossd-plugin-repair

    plugins-all: $(GEN_STAMP)
    	@for platform in $(PLATFORMS); do \
    		os=$${platform%%/*}; \
    		arch=$${platform##*/}; \
    		for plugin in $(DIST_PLUGINS); do \
    			echo "==> Building $$plugin ($$os/$$arch)"; \
    			GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' \
    				-o $(BIN_DIR)/$$plugin-$$os-$$arch ./plugins/$$plugin; \
    		done; \
    	done
    ```

  - Also add `plugins-all` to the `.PHONY` list and update `all` target to include `plugins-all`.

- [ ] Update Homebrew formula template with plugin resources
  - Files: `infra/homebrew/bossanova.rb`
  - Details: Update homepage to `https://github.com/bossanova-dev/bossanova`. Add 3 plugin `resource` blocks inside each platform section (arm64, amd64, linux). Each plugin is a separate resource: `bossd-plugin-autopilot`, `bossd-plugin-dependabot`, `bossd-plugin-repair`. Update `install` method to stage plugins into `libexec/plugins/`:

    ```ruby
    def install
      bin.install buildpath/File.basename(stable.url) => "boss"
      resource("bossd").stage { bin.install Dir["bossd*"].first => "bossd" }
      (libexec/"plugins").mkpath
      %w[bossd-plugin-autopilot bossd-plugin-dependabot bossd-plugin-repair].each do |p|
        resource(p).stage { (libexec/"plugins").install Dir["#{p}*"].first => p }
      end
    end

    def post_install
      system bin/"boss", "config", "init", "--plugin-dir", libexec/"plugins"
    end
    ```

  - Template variables to add (9 new): `${SHA256_DARWIN_ARM64_AUTOPILOT}`, `${SHA256_DARWIN_ARM64_DEPENDABOT}`, `${SHA256_DARWIN_ARM64_REPAIR}`, same for AMD64 and Linux.

- [ ] Update formula generator script for plugins
  - Files: `infra/homebrew/generate-formula.sh`
  - Details: Add 9 new `sed` substitutions for plugin SHA256 hashes. Update usage comment to document expected plugin binaries. Script now expects 15 binaries in artifacts dir (6 existing + 9 plugins).

### Post-Flight Checks for Flight Leg 3

- [ ] **Quality gates:** `make lint` — all pass (Makefile changes don't need Go tests)
- [ ] **Cross-compile test:** `make plugins-all` — produces 9 plugin binaries in `bin/`:
  ```
  bossd-plugin-autopilot-darwin-arm64
  bossd-plugin-autopilot-darwin-amd64
  bossd-plugin-autopilot-linux-amd64
  bossd-plugin-dependabot-darwin-arm64
  bossd-plugin-dependabot-darwin-amd64
  bossd-plugin-dependabot-linux-amd64
  bossd-plugin-repair-darwin-arm64
  bossd-plugin-repair-darwin-amd64
  bossd-plugin-repair-linux-amd64
  ```
- [ ] **Formula generation test:** `./infra/homebrew/generate-formula.sh v1.0.0 bin/` — produces valid Ruby with all SHA256 hashes filled in (no `${...}` placeholders remaining)

### [HANDOFF] Review Flight Leg 3

Human reviews: Makefile structure, formula template correctness, generator output.

---

## Flight Leg 4: curl|sh Installer Script

### Context

New `infra/install.sh` — the primary distribution mechanism for non-Homebrew users. Downloads binaries from GitHub Releases, installs to `~/.local/bin` or `/usr/local/bin`, seeds config via `boss config init`, registers daemon.

### Tasks

- [ ] Create `infra/install.sh` installer script
  - Files: `infra/install.sh` (NEW)
  - Details: Full installer per design doc specification. Key sections:
    1. **Header:** `#!/bin/sh`, `set -eu`, banner with version
    2. **Prerequisites:** Check for `claude` and `gh` CLIs. Print exact error messages from design doc if missing (with install instructions and links).
    3. **Platform detection:** `uname -s` (Darwin/Linux), `uname -m` (x86_64→amd64, arm64/aarch64→arm64). Error on unsupported platform.
    4. **Existing install detection:** Check if `boss` is already installed. Set UPGRADE=true if found.
    5. **Download:** Create temp dir. Download 5 binaries (boss, bossd, 3 plugins) from `https://github.com/bossanova-dev/bossanova/releases/latest/download/`. Use `curl -fsSL`. Verify all downloads succeeded before proceeding.
    6. **Install location:** Use `~/.local/bin` if it exists and is on PATH; otherwise `/usr/local/bin` (may need sudo).
    7. **Install binaries:** `chmod +x`, move boss+bossd to install dir. Plugin dir: `~/Library/Application Support/bossanova/plugins/` (macOS) or `~/.config/bossanova/plugins/` (Linux). Create plugin dir, move plugin binaries.
    8. **Seed config:** Run `boss config init --plugin-dir <plugin-dir>`
    9. **Register daemon:** Run `boss daemon install`
    10. **Progress output:** Live checkmarks per design doc:
        ```
          Downloading boss (darwin/arm64)...          done
          Downloading bossd (darwin/arm64)...         done
          Downloading plugins (3)...                  done
          Installing to ~/.local/bin...               done
          Configuring plugins...                      done
          Registering daemon (launchd)...             done
        ```
    11. **Success message:** Fresh install vs upgrade variant per design doc.
    12. **Failure handling:** If download fails mid-way, clean up temp dir, print error, exit 1. No partial installs.
  - Script must be POSIX sh compatible (not bash) for maximum portability.

- [ ] Test installer locally (manual)
  - Details: No automated test for the installer script itself — it's a shell script that downloads from GitHub. Manual verification during release. The post-flight check below validates syntax and structure.

### Post-Flight Checks for Flight Leg 4

- [ ] **Shell lint:** `shellcheck infra/install.sh` — no errors (install shellcheck if not present: `brew install shellcheck`)
- [ ] **Dry-run structure check:** Read the script and verify:
  - Prerequisite checks for `claude` and `gh` with correct error messages
  - Platform detection handles darwin/arm64, darwin/amd64, linux/amd64
  - Downloads go to temp dir first
  - `boss config init --plugin-dir` is called
  - `boss daemon install` is called
  - Error messages match design doc templates exactly
  - Progress output uses checkmark style from design doc

### [HANDOFF] Review Flight Leg 4

Human reviews: Installer script completeness, error messages match design doc, POSIX compatibility, idempotent upgrade behavior.

---

## Flight Leg 5: GitHub Actions — Release Pipeline + Public Mirror

### Context

Replace tag-triggered `deploy.yml` with branch-triggered `perform-production-release.yml`. Replace splitsh-lite `split.yml` with copy-and-strip `mirror-public.yml`. These are the CI/CD backbone.

### Tasks

- [ ] Create `perform-production-release.yml` workflow
  - Files: `.github/workflows/perform-production-release.yml` (NEW)
  - Details: Triggered on push to `production` branch. Jobs:
    1. **version:** Run semantic-release to determine version. Write to `$GITHUB_OUTPUT`. Needs `.releaserc.yml` or `package.json` with semantic-release config.
    2. **build:** Matrix build (darwin/amd64, darwin/arm64, linux/amd64). Build boss, bossd, 3 plugins per platform. Use ldflags for version injection. Upload artifacts. Pattern: follow existing `deploy.yml` build job but add plugins.
    3. **notarize** (macOS only): Download darwin artifacts. `codesign --sign` with Developer ID cert. `xcrun notarytool submit`. `xcrun stapler staple`. Re-upload signed artifacts. Needs secrets: `APPLE_DEVELOPER_CERTIFICATE_P12`, `APPLE_DEVELOPER_CERTIFICATE_PASSWORD`, `APPLE_TEAM_ID`, `APPLE_ID`, `APPLE_APP_SPECIFIC_PASSWORD`.
    4. **release:** Download all artifacts. Generate SHA256 checksums. Push version tag to public repo (`bossanova-dev/bossanova`). Create GitHub Release on public repo with `gh release create`. Upload all binaries as release assets.
    5. **homebrew:** Generate updated formula using `generate-formula.sh`. Push to `bossanova-dev/homebrew-tap`.
    6. **bump-versions:** Commit version bumps back to `production` with `[skip ci]`.
  - Needs secret: `BOSSANOVA_PUBLIC_DEPLOY_KEY` (PAT with push access to public repos).

- [ ] Create semantic-release config
  - Files: `.releaserc.yml` (NEW)
  - Details: Minimal config for Go monorepo. Use `@semantic-release/commit-analyzer`, `@semantic-release/release-notes-generator`. Write version to `.VERSION` file. Branch: `production`. No npm publish.

- [ ] Create `mirror-public.yml` workflow (copy-and-strip)
  - Files: `.github/workflows/mirror-public.yml` (NEW)
  - Details: Triggered on push to `main` and `production` branches. Job:
    1. Checkout private repo with full history
    2. Remove private directories: `rm -rf plugins/ services/bosso/ web/ infra/ docs/ TODOS.md .github/workflows/`
    3. Force-push to `bossanova-dev/bossanova` (same branch)
  - Uses `BOSSANOVA_PUBLIC_DEPLOY_KEY` secret.

- [ ] Deprecate old workflows (mark for removal, don't delete yet)
  - Files: `.github/workflows/deploy.yml`, `.github/workflows/split.yml`
  - Details: Add comment at top: `# DEPRECATED: Replaced by perform-production-release.yml and mirror-public.yml. Remove after v1.0.0 ships.` Don't delete — the old tag-trigger may still be needed during transition.

### Post-Flight Checks for Flight Leg 5

- [ ] **YAML lint:** Validate all new workflow YAML files parse correctly:
  ```bash
  python3 -c "import yaml; yaml.safe_load(open('.github/workflows/perform-production-release.yml'))"
  python3 -c "import yaml; yaml.safe_load(open('.github/workflows/mirror-public.yml'))"
  ```
- [ ] **Workflow structure check:** Read workflows and verify:
  - `perform-production-release.yml` triggers on push to `production`
  - Build matrix covers 3 platforms
  - Plugins are built alongside boss/bossd
  - Notarize job only runs for darwin artifacts
  - Release creates on `bossanova-dev/bossanova` (public repo)
  - `mirror-public.yml` strips exactly: `plugins/`, `services/bosso/`, `web/`, `infra/`, `docs/`, `TODOS.md`, `.github/workflows/`
  - Both use `BOSSANOVA_PUBLIC_DEPLOY_KEY` secret

### [HANDOFF] Review Flight Leg 5

Human reviews: Workflow correctness, secret references, release sync flow, strip list completeness. Note: workflows can't be fully tested until `production` branch exists and secrets are configured.

---

## Flight Leg 6: README + First-Run Empty State

### Context

Create the public-facing README for `bossanova-dev/bossanova`. Update TUI home view with guided first-run empty state.

### Tasks

- [ ] Create `README.md`
  - Files: `README.md` (NEW at repo root)
  - Details: Follow design doc information hierarchy exactly:
    1. `# bossanova` + tagline: "Manage multiple Claude Code sessions from one terminal."
    2. Screenshot placeholder: `![Bossanova TUI showing 6 Claude Code sessions with status indicators across multiple repos](docs/screenshot.png)` — actual screenshot to be added manually later
    3. Install: `brew install bossanova-dev/tap/boss`
    4. Quick Start: 3 steps (install, `boss repo add <path>`, `boss`)
    5. What You Get: boss, bossd, 3 plugins with one-line descriptions
    6. Prerequisites: Claude Code CLI, GitHub CLI
    7. How It Works: brief architecture
    8. Alternative Install: curl|sh command
    9. Uninstall instructions
  - Content rules from design review: No emojis. No marketing fluff. Direct, technical, zero filler. Like ripgrep's or lazygit's README.

- [ ] Implement first-run empty state in TUI home view
  - Files: `services/boss/internal/views/home.go`
  - Details: Replace the current empty-state block (lines 466-471):

    ```go
    if len(h.sessions) == 0 {
        return tea.NewView(
            lipgloss.NewStyle().Padding(0, 2).Render("No active sessions.") + "\n" +
                styleActionBar.Render("[n]ew session  [p]ilot  [r]epos  [s]ettings  [t]rash  [q]uit"),
        )
    }
    ```

    With a richer guided empty state that checks if repos exist. If no repos:

    ```
      Welcome to Bossanova!

      Add your first repo to get started:

        boss repo add /path/to/your/repo

      Then create a session:

        Press 'n' to create a new session

      Docs: https://github.com/bossanova-dev/bossanova
    ```

    Plus the action bar: `[n]ew session  [r]epos  [s]ettings  [q]uit`

    If repos exist but no sessions, show:

    ```
      No active sessions.

      Press 'n' to create a new session, or 'p' for autopilot.
    ```

    Plus the full action bar.

  - Implementation: The `HomeModel` needs to know if repos exist. Add a `repoCount int` field. Fetch repo count alongside sessions in the poll. Or simpler: add a `ListRepos` call in `Init()` once, store the result. The empty state only matters on first load.

- [ ] Test first-run empty state
  - Files: `services/boss/internal/views/home_test.go` (add test or create if it doesn't exist)
  - Details: Test that `View()` output contains "Welcome to Bossanova" when sessions is empty and repoCount is 0. Test that it shows "No active sessions" with repo guidance when sessions is empty but repos exist. Use the Bubble Tea testing pattern from the codebase.

### Post-Flight Checks for Flight Leg 6

- [ ] **Quality gates:** `make lint-boss && make test-boss` — all pass
- [ ] **README content check:** Read `README.md` and verify:
  - Tagline matches: "Manage multiple Claude Code sessions from one terminal."
  - Information hierarchy matches design doc order
  - No emojis, no marketing fluff
  - Screenshot placeholder has correct alt text
  - Install command: `brew install bossanova-dev/tap/boss`
  - curl installer command present
  - Uninstall instructions present
- [ ] **Empty state check:** Build and run `bin/boss` with no repos configured — verify welcome message appears

### [HANDOFF] Review Flight Leg 6

Human reviews: README tone and content, empty state UX, screenshot placeholder.

---

## Flight Leg 7: Final Verification + Integration

### Tasks

- [ ] Run full test suite across all modules

  ```bash
  make test
  ```

  Expected: All tests pass.

- [ ] Run full lint across all modules

  ```bash
  make lint
  ```

  Expected: No errors. Pre-existing warnings acceptable.

- [ ] Verify cross-compilation produces all expected binaries

  ```bash
  make build-all && make plugins-all
  ```

  Expected: 6 service binaries + 9 plugin binaries = 15 binaries in `bin/`.

- [ ] Verify formula generation with all binaries

  ```bash
  ./infra/homebrew/generate-formula.sh v1.0.0 bin/
  ```

  Expected: Valid Ruby formula with all 15 SHA256 hashes.

- [ ] Verify `boss config init` end-to-end

  ```bash
  bin/boss config init --plugin-dir bin/
  ```

  Expected: Settings.json created/updated with 3 plugin entries.

- [ ] Verify no compilation errors on Linux target
  ```bash
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./services/boss/cmd
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./services/bossd/cmd
  ```
  Expected: Both compile without errors.

### Post-Flight Checks for Final Verification

- [ ] **Full test suite:** `make test` — all pass
- [ ] **Full lint:** `make lint` — no errors
- [ ] **Cross-compilation:** `make build-all && make plugins-all` — 15 binaries produced
- [ ] **Formula generation:** `./infra/homebrew/generate-formula.sh v1.0.0 bin/` — no template placeholders remain
- [ ] **Config init:** `bin/boss config init --plugin-dir bin/` — prints success message
- [ ] **Linux build:** Both boss and bossd compile for linux/amd64

### [HANDOFF] Final Review

Human reviews: Complete feature set before merge. Verify all design doc requirements are met. Items NOT in scope for this plan (require manual setup): creating `production` branch, configuring GitHub Actions secrets (Apple Developer certs, public repo deploy key), taking TUI screenshot for README, first production release.

---

## Rollback Plan

All changes are additive:

- New files can be deleted (systemd.go, daemon.go, install.sh, workflows, README)
- Build tag on launchd.go can be removed
- PluginConfig.Version field is backwards-compatible (omitempty)
- `config init` command can be removed from cobra tree
- Makefile targets are additive
- Old workflows (`deploy.yml`, `split.yml`) are preserved (deprecated, not deleted)

No database migrations. No proto changes. No breaking API changes.

## Notes

- **Secrets needed before first release:** `BOSSANOVA_PUBLIC_DEPLOY_KEY` (PAT for public repo push), `APPLE_DEVELOPER_CERTIFICATE_P12`, `APPLE_DEVELOPER_CERTIFICATE_PASSWORD`, `APPLE_TEAM_ID`, `APPLE_ID`, `APPLE_APP_SPECIFIC_PASSWORD` (for notarization)
- **Branch setup needed:** Create `production` branch from `main`, add branch protection
- **Screenshot:** README has a placeholder — take actual TUI screenshot manually and add to `docs/screenshot.png`
- **semantic-release:** Needs Node.js in the CI environment. The existing CI uses Go + Node.js (for web builds), so this is already available.
- **Public repo:** `bossanova-dev/bossanova` must exist before `mirror-public.yml` can run. Create it empty on GitHub.
- **Deferred (in TODOS.md):** Public repo CI workflows (test + lint for external contributors)

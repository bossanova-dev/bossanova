# TODOS

## Phase 2: go-plugin Host Design Specification

**What:** Write a detailed technical spec for the plugin host infrastructure before building it.

**Why:** The design doc (docs/plans/open-core-plugin-architecture.md) describes Phase 2 at the product level. Several engineering questions need answers before implementation:
1. **Host services interface:** How do plugins call back to bossd to create sessions? go-plugin supports bidirectional gRPC — need to define a `HostService` proto that exposes `CreateSession`, `ListRepos`, etc. to plugins.
2. **Plugin state storage:** Where do plugins persist their state (cron jobs, sync cursors)? Options: plugin's own SQLite, bossd's SQLite via host services, plugin config file.
3. **Event bus implementation:** Is it a merge channel into the existing Poller→Dispatcher pipeline, or a new pub/sub system? The current architecture uses a simple `chan SessionEvent` — plugins would need to feed into this.
4. **Dynamic vs. static loading:** Start with static (plugins loaded at daemon startup) or support hot-reload from day one?

**Depends on:** Phase 1 completion. Living with end-to-end automation working will inform what the plugin interfaces need to expose.

**Added:** 2026-03-20 (eng review of open-core-plugin-architecture design doc)

**Status:** Partially addressed by Phase 3. Questions 1 (HostService) and 3 (event bus) are partially resolved. See Phase 3 TODOs below.

---

## Phase 3 Deferred: TUI Notification/Toast System

**What:** A daemon → TUI notification channel for surfacing alerts about automated actions (failed merges, previously-rejected libraries, blocked sessions).

**Why:** The Phase 3 orchestrator generates events that users need to see (e.g. "Dependabot PR for library X was previously closed without merging"). Without a notification system, these events only appear in daemon logs.

**Approach:** (1) Add a `Notifications` RPC to the daemon service that returns unread alerts. (2) TUI polls for notifications on each render tick. (3) Bubble Tea overlay renders toast-style notifications that auto-dismiss.

**Depends on:** Phase 3 core (orchestrator + task mappings must exist to generate notifications).

**Added:** 2026-03-21 (eng review of Phase 3 design doc)

---

## Phase 3 Deferred: License Gating for Paid Plugins

**What:** Implement license checking so paid plugin binaries (like the dependabot plugin) require a valid license key to run.

**Why:** The open-core business model requires a gate between free core and paid plugins. Without this, all features are free.

**Approach:** Start with a local license file at `~/.config/bossanova/license.key`. Plugin reads it at startup via HostService. Offline-friendly, no server dependency. Online validation with grace period can come later. The plugin binary ships with boss — a license key unlocks it.

**Depends on:** At least one working paid plugin (dependabot plugin from Phase 3).

**Added:** 2026-03-21 (eng review of Phase 3 design doc)

---

## Phase 3 Deferred: Dependabot Blacklist UX

**What:** When the history intelligence feature detects a previously-rejected library update, let the user auto-edit `.github/dependabot.yml` to add an ignore rule so dependabot stops proposing updates for that library.

**Why:** Detection + skip + notify is Phase 3 scope. Blacklisting closes the loop permanently — the user doesn't see the same rejected library again. Cool idea: the blacklist action could itself be a Claude session that edits `dependabot.yml` and opens a PR.

**Depends on:** Phase 3 history intelligence feature, TUI notification system (TODO above).

**Added:** 2026-03-21 (eng review of Phase 3 design doc)

---

## Phase 3 Deferred: Expand HostService Beyond VCS Reads

**What:** Phase 3 HostService has 3 read-only RPCs (ListOpenPRs, GetCheckResults, GetPRStatus). Expand to include: CreateSession, ListRepos, GetSessionStatus, MergePR. This lets plugins orchestrate daemon actions directly.

**Why:** Richer plugins (Linear, Jira, GitHub Issues) need to create sessions, query repo state, and trigger merges without going through the TaskSource → Orchestrator round-trip. Also resolves TODOS Phase 2 question #1 fully.

**Approach:** Incrementally add RPCs to the HostService proto. Each RPC wraps an existing daemon capability (session store, repo store, VCS provider). Server-side implementation is thin — just proxying to existing stores/interfaces.

**Depends on:** Phase 3 HostService foundation (bidirectional gRPC via go-plugin broker).

**Added:** 2026-03-21 (eng review of Phase 3 design doc)

---

## Autopilot Deferred: Generic Plugin Preference Store

**What:** Build a generic preferences table (plugin_name, key, value, type) in SQLite with HostService RPCs for typed plugin configuration.

**Why:** Currently plugin config lives in `settings.json` as `map[string]string` (unstructured) or typed structs (autopilot). When a 3rd plugin needs typed config, this unstructured approach won't scale. A DB-backed preference store would support per-repo overrides, history, and concurrent access.

**Approach:** New `plugin_preferences` table: `(plugin_name TEXT, scope TEXT, key TEXT, value TEXT, type TEXT)`. HostService RPCs: `GetPreference`, `SetPreference`, `ListPreferences`. Scope supports global, per-repo, and per-session overrides.

**Depends on:** 3rd plugin requiring typed configuration. Currently only dependabot (unstructured map) and autopilot (typed struct) exist.

**Added:** 2026-03-23 (eng review of autopilot plugin concept)

---

## Autopilot Deferred: Cost Budgets / Spend Limits

**What:** A configurable `max_cost_usd` setting for autopilot workflows that pauses the workflow when the cumulative API spend exceeds the limit.

**Why:** An autopilot run can burn significant tokens (5-20 flight legs at $0.50-$5 each = $2.50-$100). A paid feature should have cost guardrails to prevent runaway spending. Builds user trust.

**Approach:** Add `max_cost_usd` to `AutopilotConfig`. AttemptRunner returns cost per attempt. Plugin tracks cumulative cost in workflow state. When exceeded, pause workflow and notify user: "Autopilot paused: spent $X of $Y budget."

**Depends on:** Autopilot plugin MVP (workflows table, attempt runner must track costs).

**Added:** 2026-03-23 (eng review of autopilot plugin concept)

---

## Distribution Deferred: Public Repo CI Workflows

**What:** Create minimal CI workflows for the public repo (`bossanova-dev/bossanova`) that run `go test ./...` and `golangci-lint` on PRs from external contributors.

**Why:** The copy-and-strip mirror approach strips `.github/workflows/` from the public repo (private CI references secrets and private repos). Without public CI, there's no automated quality gate for external contributions.

**Approach:** Create a simple `ci.yml` in the public repo: trigger on PRs, run `go test ./...` for boss, bossd, bossalib modules, run `golangci-lint`. No deploy, no plugin builds. This is maintained separately from the private repo's CI.

**Depends on:** Public repo existing with copy-and-strip mirror workflow.

**Added:** 2026-03-28 (eng review of distribution design doc)

---

## Mutation Testing CI Workflow

**What:** Add `.github/workflows/mutate.yml` that runs `make mutate-diff` on PR branches, enforcing minimum mutation score thresholds.

**Why:** Mutation testing locally establishes a baseline. CI enforcement catches test gaps in changed code before merge. The `--diff main` flag keeps CI runs fast (seconds to minutes, not hours).

**Approach:** GitHub Actions workflow triggered on push to non-main branches. Uses `gremlins unleash --diff main --threshold-efficacy 70 --threshold-mcover 60`. Upload `.mutate/*.json` as artifacts. Optional: post summary as PR comment via `gh pr comment`.

**Depends on:** Establishing baseline mutation scores across all modules by running `make mutate` locally first.

**Added:** 2026-03-29 (eng review of mutation testing plan)

---

## TUI Deferred: Narrow Terminal Action Bar Handling

**What:** Detect narrow terminals (< ~100 columns) and truncate, abbreviate, or reflow action bars so they don't wrap awkwardly.

**Why:** The grouped action bars with `·` separators assume ~100+ column terminals. On narrower terminals, the bar wraps mid-group, breaking the visual grouping. This is a developer CLI tool so most users have wide terminals, but it's worth handling gracefully.

**Approach:** Options: (1) Truncate to show only the most-used actions with a `...` indicator. (2) Abbreviate labels (e.g., `[n]` instead of `[n]ew`). (3) Stack groups vertically. The `actionBar()` helper introduced in the TUI navigation cleanup could accept a width parameter and adapt.

**Depends on:** TUI navigation cleanup (improve-the-tui-navigation branch) completing first.

**Added:** 2026-04-05 (eng review of TUI navigation plan)

---

## Linear Plugin Deferred: Encrypt API Keys at Rest

**What:** Encrypt Linear API keys stored in the SQLite `repos` table, rather than storing them as plaintext.

**Why:** V1 stores API keys as plaintext in SQLite with TUI masking (last 4 chars visible). This is acceptable for a single-user local daemon, but doesn't protect against filesystem access to the SQLite DB.

**Approach:** Use OS keychain (macOS Keychain via `keychain` package, Linux `secret-service` via D-Bus) or an AES-encrypted field with a machine-local key derived from the OS. Fallback to plaintext on unsupported systems with a warning.

**Depends on:** Linear plugin V1 (per-repo `linear_api_key` column in repos table).

**Added:** 2026-04-07 (eng review of Linear integration plugin plan)

---

## TUI Deferred: Keyboard Shortcut Help Overlay

**What:** Add a `?` key binding that opens a full-screen overlay showing all available keyboard shortcuts for the current view. Similar to lazygit and k9s help panels.

**Why:** Action bars serve as inline help but can only show so many shortcuts. A help overlay provides full discoverability without cluttering the main view. Especially useful for new users learning the TUI.

**Approach:** `?` key triggers a modal overlay in the App root model. Each view implements a `Shortcuts() []Shortcut` method. The overlay renders a formatted table of key/description pairs grouped by category (item actions, navigation, global). `esc` or `?` dismisses.

**Depends on:** TUI navigation cleanup (improve-the-tui-navigation branch) completing first, so the shortcut definitions are consistent.

**Added:** 2026-04-05 (eng review of TUI navigation plan)

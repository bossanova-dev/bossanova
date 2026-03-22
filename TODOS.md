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

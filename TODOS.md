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

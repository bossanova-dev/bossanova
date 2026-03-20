## Handoff: Flight Leg 3 — Dependabot Detection & Auto-Merge

**Date:** 2026-03-20 15:30
**Branch:** implement-a-gstack-plan
**Flight ID:** fp-open-core-plugin-architecture
**Planning Doc:** docs/plans/open-core-plugin-architecture.md

### Tasks Completed

- bossanova-t4p1: Implement dependabot PR detection in Poller
- bossanova-wk1t: Add DependabotReady handler to Dispatcher with auto-merge
- bossanova-p7bn: Add dependabot and auto-merge integration tests
- bossanova-yn42: [HANDOFF] Review Flight Leg 3

### Files Changed

- `lib/bossalib/vcs/events.go:38-44` - Added `DependabotReady` event type with PRID, RepoID, RepoPath fields and `vcsEvent()` marker
- `lib/bossalib/vcs/types.go:75` - Added `Author` field to `PRSummary` struct
- `lib/bossalib/vcs/provider.go:32-33` - Added `MergePR(ctx, repoPath, prID) error` to `Provider` interface
- `services/bossd/internal/vcs/github/provider.go:219` - Updated `ListOpenPRs` to include `author` in gh JSON fields and populate `Author` from `author.login`
- `services/bossd/internal/vcs/github/provider.go:252-268` - Implemented `MergePR` using `gh pr merge --rebase --delete-branch`
- `services/bossd/internal/session/poller.go:82` - Added `DependabotAuthor` constant (`dependabot[bot]`)
- `services/bossd/internal/session/poller.go:110-111` - Poller `poll()` now calls `checkDependabotPRs()` when `repo.CanAutoMergeDependabot` is true
- `services/bossd/internal/session/poller.go:118-175` - New `checkDependabotPRs()` method: lists open PRs, filters by dependabot author, checks passing CI and mergeable status, emits `DependabotReady` events
- `services/bossd/internal/session/dispatcher.go:79-82` - Dispatch now handles `DependabotReady` as a repo-level event (no session lookup)
- `services/bossd/internal/session/dispatcher.go:337-367` - New `handleDependabotReady()`: checks `CanAutoMergeDependabot` flag, calls `provider.MergePR()`
- `services/bossd/internal/session/dispatcher_test.go:531-583` - 2 new tests: `DependabotReadyAutoMergeEnabled` and `DependabotReadyAutoMergeDisabled`
- `services/bossd/internal/session/poller_test.go:262-410` - 4 new tests: `PollerEmitsDependabotReady`, `SkipsDependabotWhenFlagDisabled`, `SkipsDependabotWithFailingChecks`, `SkipsNonDependabotPRs`
- `services/bossd/internal/session/lifecycle_test.go:300-353` - Updated mock VCS provider with `MergePR`, `nextOpenPRs`, `mergePRCalls` tracking
- `services/bossd/internal/testharness/mock_vcs.go:20,45-49,102-107` - Added `MergePRCalls`, `mergePRCall` struct, `MergePRErr`, and `MergePR()` to E2E mock

### Learnings & Notes

- `DependabotReady` is a repo-level event, not session-level. The dispatcher handles it before the session lookup by checking the event type first and short-circuiting. `SessionID` is empty string for these events.
- The Poller already iterates repos, so adding dependabot detection was a natural extension of the existing loop — just an extra call per repo after checking sessions.
- `PRSummary.Author` was added to the VCS abstraction layer rather than creating a separate `ListDependabotPRs` method. This keeps the interface minimal and lets any consumer filter by author.
- The GitHub provider's `ListOpenPRs` now requests `author` in the JSON fields, which returns `{"login": "dependabot[bot]"}`.
- `MergePR` uses `--rebase --delete-branch` per the plan spec. The strategy is hardcoded — if configurable merge strategies are needed later, add a parameter.

### Issues Encountered

- None. Implementation was straightforward. All existing tests continue to pass.

### Current Status

- Build: PASS (bossd, boss, bossalib)
- Tests: PASS (all packages, 6 new tests added this leg, 52 total session tests)
- Format: PASS (gofmt alignment fixes committed)
- Branch is 4 commits ahead of remote (this leg)

### Phase 1 Status

Phase 1 of the open-core plugin architecture plan is now **complete**:

- **Flight Leg 1:** Automation flags wired up, AttentionStatus proto and computation added
- **Flight Leg 2:** AttentionStatus hydrated in ListSessions/GetSession, TUI attention indicator with sort
- **Flight Leg 3:** Dependabot PR detection, DependabotReady event, auto-merge handler

All three automation flags (`can_auto_merge_dependabot`, `can_auto_address_reviews`, `can_auto_resolve_conflicts`) now drive behavior in the Dispatcher and FixLoop. The attention routing system surfaces which sessions need human intervention. Phase 2 (plugin host infrastructure) is the next major milestone.

### Resume Command

To continue this work:

1. Phase 1 is complete. Next steps would be Phase 2 from the plan: plugin host infrastructure with hashicorp/go-plugin.
2. Review the plan: `docs/plans/open-core-plugin-architecture.md` (Phase 2 section)

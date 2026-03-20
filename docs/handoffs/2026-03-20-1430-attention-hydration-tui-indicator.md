## Handoff: Flight Leg 2 — AttentionStatus Hydration + TUI Indicator

**Date:** 2026-03-20 14:30
**Branch:** implement-a-gstack-plan
**Flight ID:** fp-open-core-plugin-architecture
**Planning Doc:** docs/plans/open-core-plugin-architecture.md

### Tasks Completed

- bossanova-cnhf: Hydrate attention_status field in ListSessions and GetSession server responses
- bossanova-ws60: Add attention indicator to TUI home screen
- bossanova-2zd3: Add attention hydration and TUI attention tests

### Files Changed

- `services/bossd/internal/server/convert.go:112-125` - Added `attentionStatusToProto` helper converting `vcs.AttentionStatus` to proto `AttentionStatus`, returns nil when no attention needed
- `services/bossd/internal/server/server.go:465-473` - GetSession: hydrate `AttentionStatus` and `RepoDisplayName` from repo fetch
- `services/bossd/internal/server/server.go:509-527` - ListSessions: refactored repo lookup from `map[string]string` (names only) to `map[string]*models.Repo` (full objects) for both display name and attention hydration
- `services/bossd/internal/server/convert_test.go:244-292` - 3 new tests for `attentionStatusToProto`: nil return, blocked mapping, review mapping
- `services/boss/internal/views/home.go:82-118` - Added `renderAttentionIndicator` (colored `!` by reason), `sessionNeedsAttention` predicate, `sortSessionsByAttention` stable sort
- `services/boss/internal/views/home.go:130-133` - Added narrow attention column (width 1) between cursor and REPO columns
- `services/boss/internal/views/home.go:155` - Each row now includes attention indicator in column position 1
- `services/boss/internal/views/home.go:490-496` - Attention summary shown in action bar area when selected session needs attention
- `services/boss/internal/views/home_test.go:1-120` - 11 new tests: renderAttentionIndicator (5 cases), sortSessionsByAttention (1 case), sessionNeedsAttention (3 cases)

### Learnings & Notes

- ListSessions already had a repo lookup loop for display names. Refactoring from `map[string]string` to `map[string]*models.Repo` was a clean way to add attention hydration without a second repo fetch pass.
- Attention indicator color mapping: red (`colorDanger`) for blocked, orange (`#FF8C00` inline) for conflict, yellow (`colorWarning`) for review. Orange is inline because there's no `colorOrange` in theme.go — consider adding one if more uses emerge.
- `sort.SliceStable` preserves relative order within groups, which is what we want (attention sessions sorted to top, order within each group unchanged).
- Table rows gained a column (6 total: cursor, attention, repo, name, pr, status). The `updateCursorColumn` function only touches index 0 (cursor), so no changes needed there.

### Issues Encountered

- None. Implementation was straightforward.

### Current Status

- Build: PASS (bossd, boss, bossalib)
- Tests: PASS (all packages, 14 new tests added this leg)
- Branch is 6 commits ahead of remote

### Next Steps (Flight Leg 3: Dependabot Auto-Merge)

- bossanova-t4p1: Implement dependabot PR detection in Poller
- bossanova-wk1t: Add DependabotReady handler to Dispatcher with auto-merge
- bossanova-p7bn: Add dependabot and auto-merge integration tests
- bossanova-yn42: [HANDOFF] Review Flight Leg 3

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-open-core-plugin-architecture"` — should show bossanova-t4p1
2. Review files: `services/bossd/internal/session/dispatcher.go`, `services/bossd/internal/upstream/poller.go`

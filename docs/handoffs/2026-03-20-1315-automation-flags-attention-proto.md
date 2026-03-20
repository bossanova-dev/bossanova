## Handoff: Flight Leg 1 — Automation Flags + AttentionStatus Proto

**Date:** 2026-03-20 13:15
**Branch:** implement-a-gstack-plan
**Flight ID:** fp-open-core-plugin-architecture
**Planning Doc:** docs/plans/open-core-plugin-architecture.md

### Tasks Completed This Flight Leg

- bossanova-8dsb: Add AttentionReason enum and AttentionStatus message to models.proto
- bossanova-j2ds: Add automation flag checks to Dispatcher before invoking FixLoop
- bossanova-q3bf: Add Dispatcher automation flag tests
- bossanova-0l22: Implement ComputeAttentionStatus function in lib/bossalib/vcs/attention.go

### Files Changed

- `proto/bossanova/v1/models.proto` - Added AttentionReason enum (5 values), AttentionStatus message, and optional attention_status field (=22) on Session message
- `lib/bossalib/gen/bossanova/v1/models.pb.go` - Regenerated Go code from proto
- `services/bossd/internal/session/dispatcher.go` - Added automation flag checks: can_auto_merge gates mark-ready-for-review, automation_enabled gates check failure fix loop, can_auto_resolve_conflicts gates conflict fix loop, can_auto_address_reviews gates review fix loop
- `services/bossd/internal/session/dispatcher_test.go` - Added mockFixHandler and 8 new tests covering enabled/disabled paths for all automation flags
- `services/bossd/internal/testharness/e2e_lifecycle_test.go` - Updated TestE2E_FullSessionLifecycle and TestE2E_PRMergedTransition to enable can_auto_merge via UpdateRepo API before dispatching ChecksPassed
- `lib/bossalib/vcs/attention.go` - New file: ComputeAttentionStatus(session, repo) returning AttentionStatus based on session state and repo automation flags
- `lib/bossalib/vcs/attention_test.go` - New file: 11 table-driven test cases covering all attention states

### Implementation Notes

- Automation flag check pattern: repo-level flags (can_auto_merge, can_auto_resolve_conflicts, can_auto_address_reviews) are fetched via d.repos.Get() before fix loop invocation. Session-level flag (automation_enabled) used for check failures since repo fetch is already done in the handler.
- ReviewSubmitted event only fires from GreenDraft or ReadyForReview states (not AwaitingChecks). Tests reflect this.
- can_auto_merge defaults to false in the DB (opt-in), while can_auto_address_reviews, can_auto_resolve_conflicts, can_auto_merge_dependabot default to true (opt-out).
- AttentionReason enum placed after VCS event messages in proto to avoid renumbering existing field IDs.

### Current Status

- Tests: PASS (all 50+ tests across bossd and bossalib)
- Build: PASS (bossd, bossalib, boss all compile)
- Lint: Not run separately

### Next Flight Leg

- bossanova-cnhf: Hydrate attention_status field in ListSessions server response
- bossanova-ws60: Add attention indicator to TUI home screen
- bossanova-2zd3: Add attention hydration and TUI attention tests
- bossanova-udjg: [HANDOFF] Review Flight Leg 2

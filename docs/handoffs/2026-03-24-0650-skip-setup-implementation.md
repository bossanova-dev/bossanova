## Handoff: Skip Setup Script Implementation + Tests

**Date:** 2026-03-24 06:50
**Branch:** don-t-run-the-setup-script-for-dependabot-prs
**Flight ID:** fp-2026-03-24-1422-skip-setup-for-dependabot
**Planning Doc:** docs/plans/2026-03-24-1422-skip-setup-for-dependabot.md

### Tasks Completed This Flight Leg

- bossanova-prh3/bossanova-29lp/bossanova-6nwc/bossanova-h8hu/bossanova-p9zh/bossanova-sx99: Implementation tasks (closed in prior session, code committed this session)
- bossanova-cdij: Add test: orchestrator sets SkipSetupScript for dependabot tasks
- bossanova-kx4a: Add test: lifecycle nils SetupScript when skipSetupScript is true
- bossanova-x5zl: Add test: lifecycle passes SetupScript when skipSetupScript is false

### Files Changed

- `services/bossd/internal/taskorchestrator/session_creator.go` — Added `SkipSetupScript bool` to `CreateSessionOpts`, added `skipSetupScript bool` param to `SessionStarter` interface, pass flag to `StartSession`
- `services/bossd/internal/session/lifecycle.go:59` — Added `skipSetupScript bool` param to `StartSession()`, nil out `SetupScript` when flag is set
- `services/bossd/internal/taskorchestrator/orchestrator.go:452` — Detect "dependabot" label via `slices.Contains(task.GetLabels(), "dependabot")` and set `SkipSetupScript: true`
- `services/bossd/internal/server/server.go:446` — Pass `false` for `skipSetupScript` in direct server calls
- `services/bossd/internal/session/lifecycle_test.go` — Added `createdFromExisting` capture to mock, added 3 new tests for skip/no-skip behavior
- `services/bossd/internal/taskorchestrator/orchestrator_test.go` — Added 2 tests for dependabot label detection
- `services/bossd/internal/taskorchestrator/session_creator_test.go` — Updated mock signatures for new `skipSetupScript` param

### Implementation Notes

- Setup script nil-out happens in the lifecycle layer (not git layer), keeping changes contained
- Detection uses task labels (not branch names) — the dependabot plugin already labels tasks with "dependabot"
- All existing tests updated for new `StartSession` 5-param signature
- Duplicate task chain (bossanova-31xk through bossanova-pab8) was deleted — it was a stale re-creation

### Current Status

- Tests: PASS (all bossd tests)
- Build: PASS (`make build-bossd`)
- Lint: not yet run

### Next Flight Leg

- bossanova-5dw7: Run full test suite: `make test`
- bossanova-xqre: Run linter: `make lint-bossd`
- bossanova-lexo: Verify no unused exports or dead code
- bossanova-p61l: [HANDOFF] Final Review

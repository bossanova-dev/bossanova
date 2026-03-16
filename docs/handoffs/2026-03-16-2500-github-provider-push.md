## Handoff: Flight Leg 7a — GitHub Provider + Push

**Date:** 2026-03-16 25:00
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-sbw: Implement GitHub provider wrapping gh CLI (CreateDraftPR, GetPRStatus, GetCheckResults)
- bossanova-vfg: Implement GitHub provider remaining methods (GetFailedCheckLogs, MarkReadyForReview, GetReviewComments, ListOpenPRs)
- bossanova-p09: Add git push helper to WorktreeManager interface and implementation
- bossanova-o8k: Add GitHub provider tests with mock gh CLI output

### Files Changed

- `services/bossd/internal/vcs/github/provider.go:1-325` — Full `vcs.Provider` implementation wrapping `gh` CLI. 7 methods: CreateDraftPR (gh pr create), GetPRStatus (gh pr view --json), GetCheckResults (gh pr checks --json), GetFailedCheckLogs (gh api repos/.../actions/jobs/.../logs), MarkReadyForReview (gh pr ready), GetReviewComments (gh api repos/.../pulls/.../reviews), ListOpenPRs (gh pr list --json). Includes compile-time interface check, JSON parsing helpers, and string→enum conversion functions.
- `services/bossd/internal/vcs/github/provider_test.go:1-154` — Table-driven tests for all parsing functions: parsePRNumberFromURL (4 cases), parsePRState (7 cases), parseCheckStatus (7 cases), parseCheckConclusion (8 cases), parseReviewState (5 cases). Total 31 test cases.
- `services/bossd/internal/git/worktree.go:37-40,227-237` — Added `Push(ctx, worktreePath, branch) error` to WorktreeManager interface. Implementation runs `git push -u origin <branch>` from the worktree directory.
- `services/bossd/internal/session/lifecycle_test.go:163-168` — Updated mockWorktreeManager with Push method to satisfy new interface.

### Learnings & Notes

- **gh CLI JSON output**: `gh pr view --json` returns camelCase field names (headRefName, baseRefName). `gh pr checks --json` returns state/conclusion as UPPER_CASE strings matching GitHub API.
- **Mergeable field**: GitHub API returns "MERGEABLE", "CONFLICTING", or "UNKNOWN" — mapped to `*bool` (nil for UNKNOWN).
- **Check ID composition**: GitHub doesn't provide a single check ID in `gh pr checks` output. We compose it as `workflowName/name` for identification.
- **GetFailedCheckLogs**: Uses `gh api` directly since there's no dedicated `gh` subcommand for job logs. Requires the job ID (not the workflow/name composite).
- **Push uses -u flag**: Sets upstream tracking for future pushes. Runs from the worktree directory so the correct branch context is used.

### Issues Encountered

- None — implementation was straightforward. All tests pass, lint clean.

### Current Status

- Build: PASSED — all 3 binaries (bossd, boss, bosso)
- Lint: PASSED — golangci-lint 0 issues
- Tests: PASSED — all packages (github: 5 tests/31 cases, session: 6 tests, claude: 15 tests, db: 8 tests, git: 6 tests)
- Vet: PASSED
- Format: PASSED

### Next Steps (Flight Leg 7b: PR Lifecycle + Poller + Dispatcher)

- bossanova-d0n: Implement PR lifecycle in SessionLifecycle (push, create draft PR, update session with PR number/URL)
- bossanova-93o: Implement check poller (60s polling loop for sessions in AwaitingChecks state)
- bossanova-jk4: Implement event dispatcher routing VCS events to state machine transitions
- bossanova-92s: Add PR lifecycle and poller tests
- bossanova-2fp: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` — should show bossanova-d0n
2. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 7 section)
3. Key files: `services/bossd/internal/vcs/github/provider.go`, `services/bossd/internal/session/lifecycle.go`, `services/bossd/internal/server/server.go`, `services/bossd/internal/git/worktree.go`

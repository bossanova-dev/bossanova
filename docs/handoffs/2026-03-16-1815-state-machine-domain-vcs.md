## Handoff: Flight Leg 2 — State Machine + Domain Types + VCS Interfaces

**Date:** 2026-03-16 18:15
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-4ri: Implement state machine in lib/bossalib/machine/ using qmuntal/stateless (12 states, 15 events, guards, actions)
- bossanova-sat: Define domain types in lib/bossalib/models/ (Repo, Session, Attempt structs + proto conversion)
- bossanova-r4s: Define VCS interfaces in lib/bossalib/vcs/ (Provider interface, types, events)
- bossanova-uj2: Unit tests for state machine lifecycle (all 12 states reachable, guard behavior, full happy path + fix loop)
- bossanova-12e: [HANDOFF]

### Files Changed

- `lib/bossalib/go.mod` — Added qmuntal/stateless v1.8.0 dependency
- `lib/bossalib/go.sum` — Updated checksums
- `lib/bossalib/machine/machine.go` — Session state machine: 12 states, 15 events, PermitDynamic for guarded transitions, OnEntry actions for check state and blocked reason
- `lib/bossalib/machine/machine_test.go` — 18 tests: happy path, fix loop, max attempts, unblock reset, conflict, review feedback, PRClosed from all states, all 12 states reachable, invalid transitions, context restore
- `lib/bossalib/models/models.go` — Domain types: Repo, Session (with ArchivedAt), Attempt structs; AttemptTrigger and AttemptResult enums
- `lib/bossalib/models/convert.go` — Bidirectional proto conversion: RepoToProto/FromProto, SessionToProto/FromProto, AttemptToProto/FromProto; state/enum mapping tables
- `lib/bossalib/vcs/provider.go` — Provider interface with 7 methods (CreateDraftPR, GetPRStatus, GetCheckResults, GetFailedCheckLogs, MarkReadyForReview, GetReviewComments, ListOpenPRs)
- `lib/bossalib/vcs/types.go` — VCS types: PRStatus, CheckResult, ReviewComment, PRSummary, CreatePROpts, PRInfo; enums: PRState, CheckStatus, CheckConclusion, ReviewState, ChecksOverall
- `lib/bossalib/vcs/events.go` — 6 event types implementing sealed Event interface: ChecksPassed, ChecksFailed, ConflictDetected, ReviewSubmitted, PRMerged, PRClosed

### Learnings & Notes

- **qmuntal/stateless API**: No `PermitIf` method — use `Permit(trigger, dest, guards...)` for single-dest guarded transitions, `PermitDynamic(trigger, selector)` for multi-dest. `CanFire` returns `(bool, error)`. `OnExitWith` not `OnExitFrom`.
- **Guard timing**: Guards evaluate BEFORE entry actions. The `fixOrBlock` guard checks `attemptCount + 1 >= maxAttempts` (old value + 1), matching TS behavior where guards run before `assign()`.
- **PermitDynamic for guarded branching**: When the same trigger can go to two different states (e.g., ChecksFailed → FixingChecks or Blocked), use `PermitDynamic` with a selector function. Cannot use two `Permit` calls with different guards for the same trigger.
- **OnEntry for actions**: Used `OnEntry` on target states for actions (e.g., FixingChecks.OnEntry increments attempt count), rather than per-trigger entry actions, since PermitDynamic determines the destination.
- **Unblock resets attempts**: OnExit from Blocked clears BlockedReason and resets AttemptCount to 0, giving the session fresh retry capacity.

### Issues Encountered

- None — implementation matched the TS reference spec cleanly.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — buf lint + golangci-lint
- Tests: PASSED — 18 tests in machine package
- Format: PASSED — gofmt clean

### Next Flight Leg

Flight Leg 3: Daemon Core — SQLite + CRUD

Tasks to create (via /pre-flight-checks):
- SQLite module in services/bossd/internal/db/ (modernc.org/sqlite, WAL mode, FKs)
- Initial migration services/bossd/migrations/20260316170000_initial_schema.sql
- Shared migration runner in lib/bossalib/migrate/ using goose + go:embed
- Store interfaces + implementations: RepoStore, SessionStore, AttemptStore
- Unit tests with in-memory SQLite
- [HANDOFF] Review Flight Leg 3

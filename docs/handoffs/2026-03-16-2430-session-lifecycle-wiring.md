## Handoff: Flight Leg 6c — Session Lifecycle Wiring

**Date:** 2026-03-16 24:30
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-iwc: Create SessionLifecycle orchestrator wiring worktree + claude + state machine
- bossanova-m9x: Wire SessionLifecycle into Server and daemon entry point
- bossanova-ph1: Update AttachSession RPC to stream from Claude ring buffer
- bossanova-wbq: Update RegisterRepo to detect origin URL from git config

### Files Changed

- `services/bossd/internal/session/lifecycle.go:1-222` — SessionLifecycle orchestrator. Constructor takes SessionStore, RepoStore, WorktreeManager, ClaudeRunner, zerolog.Logger. Methods: StartSession (creates worktree, starts claude, fires state machine events CreatingWorktree→StartingClaude→ImplementingPlan, updates session with worktree path/branch/claude session ID), StopSession (stops claude, sets Closed state), ArchiveSession (stops claude, archives worktree, marks DB archived), ResurrectSession (checks archived, resurrects worktree from existing branch, starts claude with --resume, updates state to ImplementingPlan).
- `services/bossd/internal/session/lifecycle_test.go:1-358` — 6 tests with full mock implementations of SessionStore, RepoStore, WorktreeManager, ClaudeRunner. Tests: StartSession (verifies worktree create + claude start + state transitions + session field updates), StopSession (verifies claude stop + Closed state), ArchiveSession (verifies claude stop + worktree archive), ResurrectSession (verifies worktree resurrect + claude resume with old session ID), ResurrectSessionNotArchived (error for non-archived), StopSessionNoClaudeProcess (graceful when no claude running).
- `services/bossd/internal/db/store.go:49-60` — Added WorktreePath and BranchName fields to UpdateSessionParams.
- `services/bossd/internal/db/session_store.go:67-74` — Added SQL handling for WorktreePath and BranchName in Update method.
- `services/bossd/internal/server/server.go:34-55` — Server struct now holds lifecycle, claude, and worktrees fields. New() accepts these via constructor. CreateSession calls lifecycle.StartSession after DB create. StopSession/ArchiveSession/ResurrectSession delegate to lifecycle. AttachSession replaced: sends initial state, ring buffer history burst, subscribes to live output, sends SessionEnded on process exit. RegisterRepo auto-detects origin URL via worktrees.DetectOriginURL.
- `services/bossd/cmd/main.go:57-68` — Creates WorktreeManager, ClaudeRunner, and Lifecycle instances; injects all into Server.

### Learnings & Notes

- **Lifecycle pattern**: The SessionLifecycle acts as a facade over WorktreeManager, ClaudeRunner, and the state machine. Each method (Start/Stop/Archive/Resurrect) handles the full coordination sequence, updating the DB at each step.
- **State machine is local to StartSession**: A new `machine.New(CreatingWorktree)` is created per StartSession call. The state machine is used to validate the transition sequence but isn't persisted — the session's state is stored in the DB via UpdateSessionParams.State.
- **strPtr helper**: Double-pointer pattern `**string` in UpdateSessionParams requires a `strPtr(s string) **string` helper to create the double pointer from a plain string value.
- **AttachSession streaming flow**: Initial state → ring buffer history (burst) → subscribe to live output → for/range on channel (blocks until process exits and channel closes) → re-fetch session → send SessionEnded with final state.
- **Server dependency growth**: Server.New now takes 6 params (repos, sessions, attempts, lifecycle, claude, worktrees). Consider an Options struct if this grows further in Leg 7.

### Issues Encountered

- None — implementation was straightforward. All tests pass, lint clean.

### Current Status

- Build: PASSED — 3 binaries (bossd, boss, bosso)
- Lint: PASSED — golangci-lint 0 issues
- Tests: PASSED — all packages (session: 6 tests, claude: 15 tests, db: 8 tests, git: 6 tests)
- Vet: PASSED
- Format: PASSED

### Leg 6 Summary (Complete)

Leg 6 is now fully complete across 3 sub-legs:

- **6a** (handoff: 2026-03-16-2330): WorktreeManager — create, archive, resurrect, empty trash, detect origin URL
- **6b** (handoff: 2026-03-16-2400): ClaudeRunner — process manager, ring buffer, subscriber broadcast, log files
- **6c** (this handoff): SessionLifecycle — orchestrator wiring, server integration, attach streaming, origin URL detection

### Next Steps (Flight Leg 7: VCS Provider + PR + Fix Loop)

Per the planning doc, Leg 7 implements:

- GitHub provider implementing `vcs.Provider` interface — wraps `gh` CLI
- PR lifecycle: push → draft PR → awaiting_checks → poll 60s → ready_for_review → merged
- Fix loop: check failure handler, conflict handler, review handler (mutex per session, max 5 attempts → blocked)
- Event dispatcher: routes VCS events to correct handler

Note: "Open-source product is complete and fully functional at this point" after Leg 7.

### Resume Command

To continue this work:

1. Run `/pre-flight-checks` or `/file-a-flight-plan` for Leg 7 to create bd tasks
2. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 7 section)
3. Key files: `lib/bossalib/vcs/provider.go` (interface), `services/bossd/internal/session/lifecycle.go`, `services/bossd/internal/server/server.go`

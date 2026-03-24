# Skip Setup Script for Dependabot PRs

**Flight ID:** fp-2026-03-24-1422-skip-setup-for-dependabot

## Overview

Skip the setup script (e.g. `npm install`, `make setup`) when creating sessions for dependabot PRs. Dependabot PRs are lightweight dependency bumps — the setup script is unnecessary overhead. Instead, just create the worktree and let Claude work directly.

## Affected Areas

- [ ] `services/bossd/internal/taskorchestrator/` — Thread `SkipSetupScript` flag from orchestrator through session creation
- [ ] `services/bossd/internal/session/` — Nil out `SetupScript` before passing to worktree creation when flag is set

## Design References

- Existing task label "dependabot" set by plugin: `plugins/bossd-plugin-dependabot/server.go:83,136`
- Setup script execution: `services/bossd/internal/git/worktree.go:226,410` (both `Create` and `CreateFromExistingBranch` check `opts.SetupScript != nil`)
- Orchestrator session creation: `services/bossd/internal/taskorchestrator/orchestrator.go:438-490`
- Session lifecycle worktree creation: `services/bossd/internal/session/lifecycle.go:86-106`
- Existing test patterns: argument-capturing mocks in `lifecycle_test.go:188-247`, `session_creator_test.go`, `orchestrator_test.go`

## Approach

Nil out `SetupScript` in the lifecycle layer rather than modifying git layer structs. This keeps changes contained:

1. Add `SkipSetupScript bool` to `CreateSessionOpts` (orchestrator layer)
2. Add `skipSetupScript bool` parameter to `SessionStarter.StartSession()` interface
3. In `Lifecycle.StartSession()`, when `skipSetupScript` is true, pass `nil` for `SetupScript` in worktree opts
4. In `handleCreateSession()`, detect "dependabot" label and set the flag

**No changes to `git/worktree.go`** — the lifecycle just nils out the setup script before passing it down.

---

## Flight Leg 1: Thread SkipSetupScript Through the Stack

### Tasks

- [ ] Add `SkipSetupScript bool` to `CreateSessionOpts`
  - File: `services/bossd/internal/taskorchestrator/session_creator.go:18-26`
  - Add alongside existing `HeadBranch` field
- [ ] Add `skipSetupScript bool` parameter to `SessionStarter` interface
  - File: `services/bossd/internal/taskorchestrator/session_creator.go:29-31`
  - Change `StartSession(ctx, sessionID, existingBranch, forceBranch)` → `StartSession(ctx, sessionID, existingBranch, forceBranch, skipSetupScript)`
- [ ] Update `Lifecycle.StartSession()` to accept and use `skipSetupScript`
  - File: `services/bossd/internal/session/lifecycle.go:59`
  - When `skipSetupScript` is true, pass `nil` for `SetupScript` in both `CreateFromExistingBranchOpts` (line 89-95) and `CreateOpts` (line 97-105)
  - Implementation: `setupScript := repo.SetupScript; if skipSetupScript { setupScript = nil }`
- [ ] Update `lifecycleSessionCreator.CreateSession()` to pass `SkipSetupScript` to `StartSession()`
  - File: `services/bossd/internal/taskorchestrator/session_creator.go:82`
  - Pass `opts.SkipSetupScript` as the new 5th argument
- [ ] In `handleCreateSession()`, detect dependabot tasks and set `SkipSetupScript: true`
  - File: `services/bossd/internal/taskorchestrator/orchestrator.go:445-451`
  - Check `task.GetLabels()` for "dependabot" label using `slices.Contains`
  - Set `opts.SkipSetupScript = true` when detected

### Post-Flight Checks for Flight Leg 1

- [ ] **Quality gates:** `make build-bossd && make test-bossd` — compiles and all existing tests pass
- [ ] **New test: orchestrator sets SkipSetupScript for dependabot tasks**
  - In `orchestrator_test.go`, add test verifying `handleCreateSession()` captures `SkipSetupScript: true` when task has "dependabot" label
  - Verify non-dependabot tasks have `SkipSetupScript: false`
  - Pattern: use existing `mockSessionCreatorOrch` argument-capture pattern (see `orchestrator_test.go:403-440`)
- [ ] **New test: lifecycle nils SetupScript when skipSetupScript is true**
  - In `lifecycle_test.go`, add test calling `StartSession(ctx, "sess-1", "dependabot/npm/lodash", false, true)`
  - Verify `wt.createdFromExisting[0].SetupScript` is nil even though repo has a setup script
  - Requires: add `createdFromExisting []gitpkg.CreateFromExistingBranchOpts` capture to `mockWorktreeManager`
- [ ] **New test: lifecycle passes SetupScript when skipSetupScript is false**
  - Verify existing behavior: when `skipSetupScript` is false, `SetupScript` is passed through
- [ ] **Update existing test mocks** for new `StartSession` signature
  - `mockSessionStarter` in `session_creator_test.go` — add `skipSetupScript bool` param
  - All `StartSession` callers in `lifecycle_test.go` — add `false` as 5th arg to existing calls

### [HANDOFF] Review Flight Leg 1

Human reviews: Correct threading of SkipSetupScript through all layers, test coverage, no regressions

---

## Flight Leg 2: Final Verification

### Tasks

- [ ] Run full test suite: `make test`
- [ ] Run linter: `make lint-bossd`
- [ ] Verify no unused exports or dead code

### Post-Flight Checks for Final Verification

- [ ] **Full test suite passes:** `make test`
- [ ] **Linter passes:** `make lint-bossd`

### [HANDOFF] Final Review

Human reviews: Complete feature before merge

---

## Implementation Notes

### Why nil out SetupScript in the lifecycle rather than the git layer?

The git layer already handles `nil` SetupScript gracefully (both `Create` and `CreateFromExistingBranch` check `opts.SetupScript != nil`). By nilling it out in the lifecycle, we avoid adding a new flag to the git layer's option structs. The lifecycle is the right place because it's where session-level policy decisions belong.

### Detection via labels (not branch names)

The dependabot plugin already labels every task with `"dependabot"` (see `server.go:83,136`). Using labels is explicit and doesn't couple to branch naming conventions. The plugin already classifies tasks — we use that classification.

### Mock updates needed

The `mockWorktreeManager` in `lifecycle_test.go` doesn't capture `CreateFromExistingBranch` opts in a slice. A `createdFromExisting []gitpkg.CreateFromExistingBranchOpts` field needs to be added to verify that `SetupScript` is nil when `skipSetupScript` is true. This follows the existing `created []gitpkg.CreateOpts` pattern.

## Rollback Plan

Revert the commit. No database migrations or persistent state changes.

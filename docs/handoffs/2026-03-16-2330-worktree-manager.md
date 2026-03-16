## Handoff: Flight Leg 6a — WorktreeManager

**Date:** 2026-03-16 23:30
**Branch:** main
**Flight ID:** fp-2026-03-16-1700-bossanova-go-rewrite
**Planning Doc:** docs/plans/2026-03-16-1700-bossanova-go-rewrite.md

### Tasks Completed

- bossanova-8ju: Define WorktreeManager interface and Git helper utilities
- bossanova-7ok: Implement WorktreeManager.Create with git worktree add + setup script (5m timeout)
- bossanova-gwg: Implement Archive, Resurrect, EmptyTrash, and DetectOriginURL
- bossanova-g1h: Write unit tests for WorktreeManager (git operations with temp repos)

### Files Changed

- `services/bossd/internal/git/worktree.go:1-167` — WorktreeManager interface (Create, Archive, Resurrect, EmptyTrash, DetectOriginURL). Manager implementation using exec.Command git wrappers. sanitizeBranchName converts title to `boss/<kebab>` (max 60 chars). runGit helper for stdout/stderr capture. Setup script execution with 5-minute context timeout. Archive uses `git rev-parse --git-common-dir` to find repo root from worktree. EmptyTrash deletes remote branches (git push origin --delete), local branches (git branch -D), and prunes worktree refs.
- `services/bossd/internal/git/worktree_test.go:1-210` — 7 test functions using real git repos in t.TempDir(). initTestRepo helper creates bare-minimum repo with initial commit. Tests: sanitizeBranchName (6 table-driven cases), Create, CreateWithSetupScript, Archive (dir removed + branch kept), Resurrect (dir re-created), EmptyTrash (branch deleted), DetectOriginURL (no remote + with remote).

### Learnings & Notes

- **Archive needs repo path discovery**: The worktree path alone isn't enough for `git worktree remove`. Used `git rev-parse --git-common-dir` run from the worktree dir, then `filepath.Dir()` to get the repo root.
- **sanitizeBranchName pattern**: Lowercase, replace non-alphanumeric with hyphens, trim, truncate to 60 chars, prefix with `boss/`.
- **Setup script timeout**: Uses `context.WithTimeout` wrapping the parent context, so both the caller's cancel and the 5-minute deadline work.
- **EmptyTrash is best-effort**: Remote branch deletion and local branch deletion log warnings on failure rather than returning errors, since branches may already be cleaned up.

### Issues Encountered

- None — implementation was straightforward. All tests pass on first run.

### Current Status

- Build: PASSED — 3 binaries
- Lint: PASSED — 0 issues
- Tests: PASSED — all packages (machine, db, git)
- Vet: PASSED
- Format: PASSED

### Next Steps (Flight Leg 6b: Claude Process Manager)

- bossanova-r16: Define ClaudeRunner interface and implement process manager
- bossanova-6kb: Implement Claude process stdout/stderr capture with log file + ring buffer
- bossanova-97r: Write unit tests for ClaudeRunner (mock process, ring buffer, subscriber)
- bossanova-w4g: [HANDOFF]

### Resume Command

To continue this work:

1. Run `bd ready --label "flight:fp-2026-03-16-1700-bossanova-go-rewrite"` to see available tasks
2. Review planning doc: `docs/plans/2026-03-16-1700-bossanova-go-rewrite.md` (Leg 6 section)
3. Key files: `services/bossd/internal/git/worktree.go`, `services/bossd/internal/server/server.go`

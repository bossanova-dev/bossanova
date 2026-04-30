# Agent Instructions

## Task Tracking

Use the harness's built-in todo tooling (e.g. `TodoWrite` / `TaskCreate`) for in-session task tracking. There is no external issue tracker for this project; persistent follow-ups belong in commit messages, PR descriptions, or GitHub issues.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **Run quality gates** (if code changed) — `make`, `make lint`, `make test` must pass
2. **Commit** — group changes into logical commits with conventional-commit messages
3. **PUSH TO REMOTE** — this is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
4. **Clean up** — clear stashes, prune remote branches
5. **Verify** — all changes committed AND pushed
6. **Hand off** — provide context for next session

**CRITICAL RULES:**

- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing — that leaves work stranded locally
- NEVER say "ready to push when you are" — YOU must push
- If push fails, resolve and retry until it succeeds

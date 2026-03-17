---
name: git-committing
description: Conventional commit format and git workflow. Use when committing changes or creating PRs.
---

# Git Commit Conventions

This project uses conventional commits.

## Commit Format

```
type(scope): [#PR-NUM] subject
```

### Examples

```bash
feat(bossd): [#57] implement claude chat tracking RPCs
fix(boss): [#123] resolve chat picker crash on empty worktree
docs(global): [#456] update API documentation
chore(global): [#789] upgrade go dependencies
```

## Types

| Type       | Purpose                         |
| ---------- | ------------------------------- |
| `feat`     | New feature                     |
| `fix`      | Bug fix                         |
| `docs`     | Documentation only              |
| `style`    | Formatting, whitespace          |
| `refactor` | Code change without feature/fix |
| `perf`     | Performance improvement         |
| `test`     | Adding/updating tests           |
| `build`    | Build system, dependencies      |
| `ci`       | CI/CD configuration             |
| `chore`    | Maintenance tasks               |

## Scopes

Match the scope to the module being modified:

| Scope    | Module                        |
| -------- | ----------------------------- |
| `boss`   | services/boss (CLI TUI)       |
| `bossd`  | services/bossd (daemon)       |
| `bosso`  | services/bosso (orchestrator) |
| `lib`    | lib/bossalib (shared library) |
| `proto`  | proto/ (Protobuf definitions) |
| `global` | Root-level or cross-module    |

## PR Reference

**Always include the PR reference in square brackets:**

```bash
# With PR number
feat(boss): [#123] add dark mode toggle

# Without PR (during development - update when PR created)
feat(boss): add dark mode toggle
```

## Breaking Changes

For breaking changes, add `!` after the type:

```bash
feat(bossd)!: [#500] change session state machine transitions
```

Or include `BREAKING CHANGE:` in the commit body:

```bash
feat(bossd): [#500] change session state machine

BREAKING CHANGE: Session states renamed from snake_case to PascalCase.
```

## Subject Guidelines

- Use imperative mood ("add" not "added" or "adds")
- Don't capitalize first letter
- No period at end
- Keep under 72 characters
- Focus on WHAT changed, not HOW

### Good Subjects

```bash
add claude chat tracking RPCs
fix worktree creation for existing branches
update TUI layout with dynamic column widths
remove deprecated chat discovery code
```

### Bad Subjects

```bash
Added new feature           # Past tense
Adds user auth.             # Third person, period
Fixed the thing             # Vague
Update                      # Too vague
```

## Commit Workflow

### Run Format Before Committing

**This step is NON-NEGOTIABLE. You MUST run format before every commit.**

```bash
make format
```

This runs `gofmt` across all Go modules, web linting, and prettier for docs.

If any files were reformatted, stage the formatting changes along with your other changes.

### Stage Specific Files

```bash
# CORRECT - stage specific files
git add services/bossd/internal/server/server.go services/bossd/internal/db/store.go

# AVOID - can include unintended files
git add -A
git add .
```

### Use HEREDOC for Multi-line Messages

```bash
git commit -m "$(cat <<'EOF'
feat(bossd): [#123] implement claude chat tracking

- Add claude_chats migration and store
- Implement RecordChat, ListChats, UpdateChatTitle RPCs
- Wire chat store into daemon server

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>
EOF
)"
```

### Single-line Commit

```bash
git commit -m "fix(boss): [#456] resolve chat picker crash on empty session"
```

## Files to Avoid Committing

- `.env` files (credentials)
- `*.local` files
- Build outputs (`bin/`, `dist/`)
- IDE settings (`.idea/`, `.vscode/`)
- OS files (`.DS_Store`)

## Beads Integration

When committing with beads issues:

```bash
# Sync beads before commit
bd sync

# Stage code changes
git add <files>

# Commit with conventional format
git commit -m "feat(bossd): [#123] implement feature X"

# Sync beads again (captures any updates)
bd sync

# Push everything
git push
```

## Pre-commit Hooks

This repo may have pre-commit hooks. **If hooks fail:**

1. Fix the issue
2. Re-stage files (`git add`)
3. Create a NEW commit (don't use `--amend` after hook failure)

## Checklist

- [ ] Ran `make format`
- [ ] Correct type for the change
- [ ] Scope matches module modified
- [ ] PR reference included (if PR exists)
- [ ] Subject is imperative mood
- [ ] Subject under 72 characters
- [ ] No credentials or sensitive files staged
- [ ] Staged specific files (not `git add -A`)
- [ ] `bd sync` run before and after commit

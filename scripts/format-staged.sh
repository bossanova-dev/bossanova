#!/bin/sh
# format-staged.sh — format only staged files, re-stage them, exit 0 always.
#
# Invoked by .husky/pre-commit from the repo root.  Never blocks a commit:
# missing tools or formatter failures are swallowed silently.
#
# Pipeline:
#   pre-commit
#      │
#      ├─ no node_modules AND no gofmt ─► exit 0   (dep-free cron worktree — do nothing)
#      │
#      ├─ staged *.go ───► gofmt/goimports -w  ─► git add (re-stage)   [skip if tools absent]
#      ├─ staged web/json/md/css ─► prettier --write ─► git add        [skip if prettier absent]
#      │
#      └─ exit 0 always   (never blocks the commit; CI make lint is the hard gate)
#
# Unstaged-edits safety:
#   A file that has BOTH staged changes (in the index) AND unstaged changes (in
#   the working tree) is skipped entirely.  Running a formatter on the working-
#   tree copy and then re-staging it with `git add` would silently promote the
#   unstaged edits into the commit — exactly the opposite of what the user
#   intended.  Skipping such files is the safest behaviour; the user can re-run
#   the formatter manually and stage deliberately.

# Disable pathname expansion: staged paths are word-split below, and a literal
# glob char in a filename must never expand against the working tree.  (case
# patterns still match — set -f only affects expansion, not `case` globbing.)
set -f

# ── dep-free short-circuit ────────────────────────────────────────────────────
# Cron worktrees have no node_modules and no Go toolchain.  Both absent → exit
# immediately so no git commands or tool lookups are even attempted.
if [ ! -d "node_modules" ] && ! command -v gofmt >/dev/null 2>&1; then
  exit 0
fi

# ── collect staged and unstaged paths ─────────────────────────────────────────
# core.quotePath=false keeps non-ASCII paths verbatim (not C-quoted) so the
# `[ -f "$f" ]` guard and re-staging below still match them.
STAGED=$(git -c core.quotePath=false diff --cached --name-only --diff-filter=ACMR 2>/dev/null) || STAGED=""
if [ -z "$STAGED" ]; then
  exit 0
fi

# Formatting the canonical embedded payload writes one file at a time. Re-enter
# under the shared rewrite lock so startup snapshots never accept that interim
# state. The Node wrapper keeps the lock descriptor alive across this shell.
if [ -z "${BOSS_SKILL_SOURCE_REWRITE_LOCK_HELD:-}" ]; then
  NEEDS_SKILL_SOURCE_LOCK=""
  IFS='
'
  for f in $STAGED; do
    case "$f" in
      services/boss/internal/skillinstall/skills/*)
        NEEDS_SKILL_SOURCE_LOCK=1
        break
        ;;
    esac
  done
  unset IFS
  if [ -n "$NEEDS_SKILL_SOURCE_LOCK" ]; then
    command -v node >/dev/null 2>&1 || exit 0
    REPO_ROOT=$(git rev-parse --show-toplevel 2>/dev/null) || exit 0
    SCRIPT_DIR=$(CDPATH= cd "$(dirname "$0")" && pwd) || exit 0
    node "$SCRIPT_DIR/skill-source-rewrite-lock.mjs" --repo-root "$REPO_ROOT" -- sh "$SCRIPT_DIR/format-staged.sh" || true
    exit 0
  fi
fi

# Files that have working-tree modifications on top of what is staged.
UNSTAGED=$(git -c core.quotePath=false diff --name-only 2>/dev/null) || UNSTAGED=""

# Returns 0 if the given relative path also has unstaged edits.  `-e` guards a
# filename that begins with `-` from being read as a grep option.
has_unstaged() {
  printf '%s\n' "$UNSTAGED" | grep -qxF -e "$1"
}

# ── resolve formatters ────────────────────────────────────────────────────────
PRETTIER_CMD=""
if [ -x "node_modules/.bin/prettier" ]; then
  PRETTIER_CMD="node_modules/.bin/prettier"
elif command -v prettier >/dev/null 2>&1; then
  PRETTIER_CMD="prettier"
fi
HAVE_GOFMT=""
command -v gofmt >/dev/null 2>&1 && HAVE_GOFMT=1
HAVE_GOIMPORTS=""
command -v goimports >/dev/null 2>&1 && HAVE_GOIMPORTS=1

# ── format each staged file individually ──────────────────────────────────────
# Split the staged list on NEWLINES ONLY (not spaces/tabs), so paths containing
# whitespace stay intact, and pass each path to a formatter as a single quoted
# argument — a stray token can never word-split into, or (with set -f) glob onto,
# a path outside the commit set.
IFS='
'
for f in $STAGED; do
  # Skip files that also have unstaged edits — re-staging them would promote the
  # unstaged work into the commit (see header comment).
  has_unstaged "$f" && continue
  # Skip deletions / non-regular files, and symlinks: a formatter's `-w` would
  # write THROUGH a symlink to its target, a file outside the staged set.
  [ -f "$f" ] && [ ! -L "$f" ] || continue

  case "$f" in
    *.go)
      [ -n "$HAVE_GOFMT" ] || continue
      gofmt -w "$f" 2>/dev/null || true
      [ -n "$HAVE_GOIMPORTS" ] && { goimports -w "$f" 2>/dev/null || true; }
      git add "$f" 2>/dev/null || true
      ;;
    *.ts | *.tsx | *.js | *.jsx | *.json | *.md | *.css)
      [ -n "$PRETTIER_CMD" ] || continue
      $PRETTIER_CMD --write "$f" 2>/dev/null || true
      git add "$f" 2>/dev/null || true
      ;;
  esac
done

exit 0

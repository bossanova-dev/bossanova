#!/usr/bin/env bash
#
# Leak guard for the public mirror.
#
# Scans every commit in BASE..HEAD and fails if any commit's tree carries a
# known-private path or a .env* file with a private env-var assignment. This is
# the last line of defence before pushing to the public repo: it catches gaps
# in the mirror's PRIVATE_PATHS denylist (e.g. a new private root file, or a new
# BOSSO_*/TF_VAR_* secret) that would otherwise leak silently.
#
# It scans the whole commit RANGE, not just the final tree, because an
# intermediate replayed commit can leak while the final tree looks clean.
#
# Usage: check-mirror-leaks.sh <base-ref> <head-ref>
#   e.g. check-mirror-leaks.sh public/main public-mirror
# When <base-ref> does not resolve (first-run orphan mirror), every commit
# reachable from <head-ref> is scanned.
#
# Single source of truth for the patterns; exercised by
# scripts/check-mirror-leaks.test.mjs and invoked from
# .github/workflows/mirror-public.yml.

set -euo pipefail

# Name the guard when a producer failure aborts the script. Without this the run
# dies with only git's own `fatal: …` and a bare non-zero status, so a CI log
# reader sees a failure whose label names git rather than this gate. The trap is
# not reached by the deliberate `exit 1` leak path below, nor by the `git
# rev-parse` probe (bash suppresses ERR inside an `if` condition), so it fires
# only for a genuinely unchecked producer failure.
trap 'echo "Leak guard aborted: a git producer failed — refusing to certify this range." >&2' ERR

BASE="${1:?usage: check-mirror-leaks.sh <base-ref> <head-ref>}"
HEAD="${2:?usage: check-mirror-leaks.sh <base-ref> <head-ref>}"

# Paths that must never appear in the public tree. Anchored at repo root, so
# bossd-plugin-* matches only a stray root binary, never plugins/bossd-plugin-*.
FORBIDDEN_PATH_RE='^(AGENTS\.md|CLAUDE\.md|bossd-plugin-[^/]+|\.env\.example\.public|plans/|docs/|services/mcp-gateway/)'

# Private env-var assignment prefixes. Only meaningful inside .env* files, so we
# never false-match these tokens where they legitimately appear in source code.
FORBIDDEN_ENV_RE='^(TF_VAR_|CLOUDFLARE_|BOSSO_|VITE_)'

if git rev-parse --verify --quiet "$BASE" >/dev/null; then
  RANGE="$BASE..$HEAD"
else
  RANGE="$HEAD"
fi

leak=0
# Assignment on its OWN line so a failing `git rev-list` aborts the script: under
# `set -e` a failing command substitution in a simple assignment DOES abort, while
# the `for sha in $(...)` word list it replaces discarded the status entirely — a
# broken range scanned zero commits and printed the pass line.
shas="$(git rev-list "$RANGE")"
for sha in $shas; do
  # Assignment on its OWN line so a failing `git ls-tree` aborts the script: the
  # `done < <(git ls-tree …)` process substitution it replaces never has its exit
  # status checked, even under `pipefail` — a broken tree scanned zero paths and
  # printed the pass line.
  #
  # core.quotePath=false is load-bearing, not cosmetic: by default git C-quotes
  # any path with non-ASCII bytes, so `docs/wéird.md` arrives as the literal
  # "docs/w\303\251ird.md" — which does not match FORBIDDEN_PATH_RE's `^docs/`
  # and sails past the guard onto the public mirror. It also breaks the
  # `git show "$sha:$envf"` lookup below, which needs the real pathname.
  #
  # It closes the non-ASCII half only: core.quotePath governs bytes >= 0x80, and
  # git still C-quotes `"`, `\` and control characters unconditionally. The
  # quote-detection below covers that remainder. `-z` would be the general fix,
  # but NUL-delimited output cannot survive the own-line command substitution
  # this gate depends on for its exit-status check — bash silently strips NUL
  # bytes from `$(...)`.
  paths="$(git -c core.quotePath=false ls-tree -r --name-only "$sha")"

  # Path check: any tree entry matching a forbidden path.
  #
  # Here-string, never `printf … | grep -q`: `grep -q` exits the moment it
  # matches, so on input larger than the pipe buffer the writer is killed by
  # SIGPIPE and `pipefail` makes the whole pipeline exit 141 — i.e. a MATCH
  # reads as "no match" and the leak goes unrecorded. See the .env content
  # check below, where that bug was live and demonstrable.
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    # A path git still C-quoted (a `"`, `\` or control byte in the name, which
    # core.quotePath cannot unquote) reaches us as a literal `"…"` that no
    # longer matches FORBIDDEN_PATH_RE and cannot be handed back to `git show`.
    # The gate cannot evaluate it, so it must not certify it: treat it as a
    # leak rather than silently passing a path it never actually checked.
    case "$path" in
      '"'*)
        echo "LEAK ${sha:0:12}: unparseable (C-quoted) path, refusing to certify: $path" >&2
        leak=1
        continue
        ;;
    esac
    if grep -qE "$FORBIDDEN_PATH_RE" <<<"$path"; then
      echo "LEAK ${sha:0:12}: forbidden path: $path" >&2
      leak=1
    fi
  done <<<"$paths"

  # Reuse the already status-checked $paths rather than re-running `git ls-tree`:
  # grepping the variable keeps the `|| true` scoped to the grep alone (it must
  # legitimately swallow "no .env* file in this commit", never a ls-tree failure).
  env_files="$(printf '%s\n' "$paths" | grep -E '(^|/)\.env(\.|$)' || true)"

  # Content check: private env assignments inside any .env* file.
  while IFS= read -r envf; do
    [ -n "$envf" ] || continue
    # Assignment on its OWN line so a failing `git show` aborts the script: bash
    # suppresses `errexit` inside an `if` condition, and under `pipefail` a failing
    # `git show` piped straight into `grep -q` produced the same false result as
    # "grep found no private assignment" — a missing blob read as clean.
    blob="$(git show "$sha:$envf")"
    # Here-string, NOT `printf "$blob" | grep -q`: `grep -q` exits on its first
    # match, so for a blob larger than the pipe buffer printf is killed by
    # SIGPIPE and `pipefail` turns the pipeline's status into 141. The `if`
    # then takes the false branch and the guard prints "Leak guard passed" for
    # a commit that really does carry a private assignment — demonstrated with
    # a 200 KB .env.example whose first line was BOSSO_WORKOS_CLIENT_SECRET.
    if grep -qE "$FORBIDDEN_ENV_RE" <<<"$blob"; then
      echo "LEAK ${sha:0:12}: private env assignment in $envf" >&2
      leak=1
    fi
  done <<<"$env_files"
done

if [ "$leak" -ne 0 ]; then
  echo "Leak guard failed — refusing to push to the public mirror." >&2
  exit 1
fi

echo "Leak guard passed: ${RANGE} clean."

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

BASE="${1:?usage: check-mirror-leaks.sh <base-ref> <head-ref>}"
HEAD="${2:?usage: check-mirror-leaks.sh <base-ref> <head-ref>}"

# Paths that must never appear in the public tree. Anchored at repo root, so
# bossd-plugin-* matches only a stray root binary, never plugins/bossd-plugin-*.
FORBIDDEN_PATH_RE='^(AGENTS\.md|CLAUDE\.md|bossd-plugin-[^/]+|\.env\.example\.public|plans/|docs/)'

# Private env-var assignment prefixes. Only meaningful inside .env* files, so we
# never false-match these tokens where they legitimately appear in source code.
FORBIDDEN_ENV_RE='^(TF_VAR_|FLY_API_TOKEN|CLOUDFLARE_|LITESTREAM_|BOSSO_|VITE_)'

if git rev-parse --verify --quiet "$BASE" >/dev/null; then
  RANGE="$BASE..$HEAD"
else
  RANGE="$HEAD"
fi

leak=0
for sha in $(git rev-list "$RANGE"); do
  # Path check: any tree entry matching a forbidden path.
  while IFS= read -r path; do
    if printf '%s\n' "$path" | grep -qE "$FORBIDDEN_PATH_RE"; then
      echo "LEAK ${sha:0:12}: forbidden path: $path" >&2
      leak=1
    fi
  done < <(git ls-tree -r --name-only "$sha")

  # Content check: private env assignments inside any .env* file.
  while IFS= read -r envf; do
    [ -n "$envf" ] || continue
    if git show "$sha:$envf" | grep -qE "$FORBIDDEN_ENV_RE"; then
      echo "LEAK ${sha:0:12}: private env assignment in $envf" >&2
      leak=1
    fi
  done < <(git ls-tree -r --name-only "$sha" | grep -E '(^|/)\.env(\.|$)' || true)
done

if [ "$leak" -ne 0 ]; then
  echo "Leak guard failed — refusing to push to the public mirror." >&2
  exit 1
fi

echo "Leak guard passed: ${RANGE} clean."

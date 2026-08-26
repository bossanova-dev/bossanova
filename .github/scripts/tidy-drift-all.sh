#!/usr/bin/env bash
# Consolidated `go mod tidy` drift guard for every Go module in the workspace.
#
# BOS-343: replaces the per-workflow `go mod tidy drift` step that used to live in
# each test-<module>.yml. Enumerates the module set the same way the Makefile does
# (lib/*/go.mod services/*/go.mod plugins/*/go.mod) and, for each module, runs the
# proxy-flake retry loop extracted verbatim from test-bossd.yml.
#
# proxy.golang.org occasionally resets HTTP/2 streams mid-download (INTERNAL_ERROR),
# which causes `go mod tidy -e` to silently produce an incomplete go.mod/go.sum and
# trip the drift check on what is really a transient network flake. Retry up to 3
# times per module, but fail immediately when consecutive attempts produce identical
# bytes: that is deterministic drift, not a proxy flake.
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: tidy-drift-all.sh [--fix]

Check every workspace Go module for go.mod/go.sum drift. With --fix, leave tidy
changes in the working tree and exit 0.
EOF
}

MODE=check
case "${1:-}" in
  "")
    ;;
  --fix)
    MODE=fix
    ;;
  -h|--help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Module dirs, discovered exactly like the Makefile's MODULES var
# ($(patsubst %/go.mod,%,$(wildcard lib/*/go.mod services/*/go.mod plugins/*/go.mod))).
modules=()
for gomod in lib/*/go.mod services/*/go.mod plugins/*/go.mod; do
  [ -e "$gomod" ] || continue
  modules+=("$(dirname "$gomod")")
done

if [ "${#modules[@]}" -eq 0 ]; then
  echo "::error::no Go modules discovered under lib/ services/ plugins/"
  exit 1
fi

drifted=()
fixed=()
TMP_ROOT="$(mktemp -d)"
trap 'rm -rf "$TMP_ROOT"' EXIT

snapshot_module() {
  local module="$1"
  local snapshot="$2"
  mkdir -p "$snapshot"
  for file in go.mod go.sum; do
    if [ -e "$module/$file" ]; then
      cp "$module/$file" "$snapshot/$file"
    else
      : >"$snapshot/$file.absent"
    fi
  done
}

restore_module() {
  local module="$1"
  local snapshot="$2"
  for file in go.mod go.sum; do
    if [ -e "$snapshot/$file.absent" ]; then
      rm -f "$module/$file"
    else
      cp "$snapshot/$file" "$module/$file"
    fi
  done
}

module_changed_from_snapshot() {
  local module="$1"
  local snapshot="$2"
  for file in go.mod go.sum; do
    if [ -e "$snapshot/$file.absent" ]; then
      [ ! -e "$module/$file" ] || return 0
    elif ! cmp -s "$snapshot/$file" "$module/$file"; then
      return 0
    fi
  done
  return 1
}

copy_result() {
  local module="$1"
  local result="$2"
  mkdir -p "$result"
  for file in go.mod go.sum; do
    if [ -e "$module/$file" ]; then
      cp "$module/$file" "$result/$file"
    else
      : >"$result/$file.absent"
    fi
  done
}

same_result() {
  local left="$1"
  local right="$2"
  for file in go.mod go.sum; do
    if [ -e "$left/$file.absent" ] || [ -e "$right/$file.absent" ]; then
      [ -e "$left/$file.absent" ] && [ -e "$right/$file.absent" ] || return 1
    elif ! cmp -s "$left/$file" "$right/$file"; then
      return 1
    fi
  done
  return 0
}

for module in "${modules[@]}"; do
  echo "==> go mod tidy drift: $module"
  snapshot="$TMP_ROOT/${module//\//__}.before"
  snapshot_module "$module" "$snapshot"

  if [ "$MODE" = "fix" ]; then
    (cd "$module" && go mod tidy -e)
    if module_changed_from_snapshot "$module" "$snapshot"; then
      fixed+=("$module")
    fi
    continue
  fi

  attempt=1
  previous_result=""
  while :; do
    if ! (
      cd "$module"
      go mod tidy -e
    ); then
      echo "::error::go mod tidy failed in $module"
      restore_module "$module" "$snapshot"
      drifted+=("$module")
      break
    fi
    if module_changed_from_snapshot "$module" "$snapshot"; then
      result="$TMP_ROOT/${module//\//__}.attempt-$attempt"
      copy_result "$module" "$result"

      if [ -n "$previous_result" ] && same_result "$previous_result" "$result"; then
        echo "::error::deterministic go.mod/go.sum drift in $module after $attempt attempts"
        git -C "$module" diff -- go.mod go.sum
        echo "::error::run make tidy to update workspace module go.mod/go.sum files"
        restore_module "$module" "$snapshot"
        drifted+=("$module")
        break
      fi

      if [ "$attempt" -ge 3 ]; then
        echo "::error::go.mod/go.sum drift in $module after $attempt attempts"
        git -C "$module" diff -- go.mod go.sum
        echo "::error::run make tidy to update workspace module go.mod/go.sum files"
        restore_module "$module" "$snapshot"
        drifted+=("$module")
        break
      fi

      if [ -n "$previous_result" ]; then
        echo "::warning::go.mod/go.sum drift in $module changed between attempts — likely transient proxy.golang.org flake, retrying"
      fi
      previous_result="$result"
      restore_module "$module" "$snapshot"
      attempt=$((attempt + 1))
      sleep "${TIDY_DRIFT_RETRY_SLEEP_SECONDS:-$((attempt * 5))}"
    else
      break
    fi
  done
done

if [ "$MODE" = "fix" ]; then
  if [ "${#fixed[@]}" -gt 0 ]; then
    echo "Tidied ${#fixed[@]} module(s): ${fixed[*]}"
  else
    echo "All ${#modules[@]} modules already tidy."
  fi
  exit 0
fi

if [ "${#drifted[@]}" -gt 0 ]; then
  echo "::error::go.mod/go.sum drift in: ${drifted[*]}"
  echo "::error::run make tidy to update workspace module go.mod/go.sum files"
  exit 1
fi

echo "All ${#modules[@]} modules tidy — no go.mod/go.sum drift."

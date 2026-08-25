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
# times per module — real drift will reproduce; a flake will not.
set -euo pipefail

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

for module in "${modules[@]}"; do
  echo "==> go mod tidy drift: $module"
  (
    cd "$module"
    attempt=1
    while :; do
      go mod tidy -e
      if git diff --quiet go.mod go.sum; then
        break
      fi
      if [ "$attempt" -ge 3 ]; then
        echo "::error::go.mod/go.sum drift in $module after $attempt attempts"
        git diff go.mod go.sum
        exit 1
      fi
      echo "::warning::go.mod/go.sum drift in $module on attempt $attempt — likely transient proxy.golang.org flake, retrying"
      git checkout -- go.mod go.sum
      attempt=$((attempt + 1))
      sleep $((attempt * 5))
    done
  ) || drifted+=("$module")
done

if [ "${#drifted[@]}" -gt 0 ]; then
  echo "::error::go.mod/go.sum drift in: ${drifted[*]}"
  exit 1
fi

echo "All ${#modules[@]} modules tidy — no go.mod/go.sum drift."

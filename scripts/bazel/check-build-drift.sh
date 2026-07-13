#!/usr/bin/env bash
# Fails when committed BUILD.bazel files drift from what gazelle would generate,
# or when the binary-target inventory no longer matches the live Bazel graph.
#
# After BOS-344 the canonical pattern is repo-wide (`//...`): gazelle runs over
# the whole workspace, and every service/plugin/auxiliary `go_binary` is pinned
# in scripts/bazel/binary-inventory.json (consumed by BOS-339's make facade).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# 1) Gazelle drift over ALL modules (no directory args = whole repo). Hand-added
#    attrs marked `# keep` (data, rundir, tags, visibility) must survive regen.
bazel run //:gazelle -- -mode=diff
echo "BUILD files clean (repo-wide gazelle diff)"

# 2) binary-inventory.json must match `bazel query 'kind(go_binary, //...)'`
#    EXACTLY — no missing labels, no stale labels.
query_bins="$(bazel query 'kind(go_binary, //...)' 2>/dev/null | sort -u)"
inv_bins="$(jq -r '.[] | .[]' scripts/bazel/binary-inventory.json | sort -u)"
if ! diff <(printf '%s\n' "$query_bins") <(printf '%s\n' "$inv_bins") >/tmp/inventory-drift.diff 2>&1; then
  echo "ERROR: scripts/bazel/binary-inventory.json is out of sync with 'bazel query kind(go_binary, //...)':" >&2
  echo "  (< = in query but missing from inventory; > = in inventory but not in the graph)" >&2
  cat /tmp/inventory-drift.diff >&2
  exit 1
fi
echo "binary-inventory.json matches the live go_binary graph"

# 3) scripts/bazel/ledger.json must match `bazel query 'attr(tags,"manual",tests(//...))'`
#    EXACTLY. The BOS-339 make facade runs `bazel test //...` (which excludes every
#    `manual`-tagged target) plus the ledger's native step, so their union == today's
#    `go test ./...`. A new `manual` tag that is not reconciled into the ledger would be
#    dropped by `//...` AND absent from the ledger — silently shrinking `make test`
#    coverage with no failing test. Fail loudly on that drift here.
query_manual="$(bazel query 'attr(tags, "manual", tests(//...))' 2>/dev/null | sort -u)"
ledger_manual="$(jq -r '.[].label' scripts/bazel/ledger.json | sort -u)"
if ! diff <(printf '%s\n' "$query_manual") <(printf '%s\n' "$ledger_manual") >/tmp/ledger-drift.diff 2>&1; then
  echo "ERROR: scripts/bazel/ledger.json is out of sync with 'bazel query attr(tags,\"manual\",tests(//...))':" >&2
  echo "  (< = manual target missing from the ledger; > = ledger label no longer a manual target)" >&2
  cat /tmp/ledger-drift.diff >&2
  exit 1
fi
echo "ledger.json matches the live manual-tagged test set"

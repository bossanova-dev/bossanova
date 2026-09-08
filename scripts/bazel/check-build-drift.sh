#!/usr/bin/env bash
# Fails when committed BUILD.bazel files drift from what gazelle would generate,
# or when the binary-target inventory no longer matches the live Bazel graph.
#
# After BOS-344 the canonical pattern is repo-wide (`//...`): gazelle runs over
# the whole workspace, and every service/plugin/auxiliary `go_binary` is pinned
# in scripts/bazel/binary-inventory.json (consumed by BOS-339's make facade).
#
# BOS-1205: every drift verdict below is gated on the comparison having actually
# been computed. A bazel invocation that never ran (disk exhaustion, OOM, a
# toolchain fetch, a stalled network) yields the same non-zero status as a real
# finding, so a status alone cannot tell "the committed artifact drifted" from
# "the tool that measures drift never produced a measurement". Each check now
# establishes that it observed a measurement first, and otherwise reports a
# host/toolchain failure that names no committed artifact as the suspect.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# 1) Gazelle drift over ALL modules (no directory args = whole repo). Hand-added
#    attrs marked `# keep` (data, rundir, tags, visibility) must survive regen.
#    Build the target before running it: `bazel run` returns 1 for a build failure
#    AND propagates the built binary's own exit code, so no exit-code partition can
#    separate a diff finding from a build that never happened. A separate build
#    turns "did the tool exist to measure drift?" into an observable fact.
gazelle_build_log="$(mktemp)"
if ! bazel build //:gazelle >"$gazelle_build_log" 2>&1; then
  echo "ERROR: could not build //:gazelle - toolchain/host failure, NOT BUILD drift:" >&2
  echo "  No gazelle diff was computed, so no BUILD.bazel file is implicated." >&2
  cat "$gazelle_build_log" >&2 || true
  rm -f "$gazelle_build_log"
  exit 1
fi
rm -f "$gazelle_build_log"
if ! gazelle_diff="$(bazel run //:gazelle -- -mode=diff 2>&1)"; then
  echo "ERROR: BUILD files are out of sync with 'bazel run //:gazelle':" >&2
  echo "Run: bazel run //:gazelle" >&2
  printf '%s\n' "$gazelle_diff" >&2
  exit 1
fi
echo "BUILD files clean (repo-wide gazelle diff)"

# 2) binary-inventory.json must match `bazel query 'kind(go_binary, //...)'`
#    EXACTLY — no missing labels, no stale labels.
#    The query's stderr is captured to a mktemp file rather than discarded: with
#    `set -euo pipefail` a discarded-stderr failure aborted the script at the
#    assignment, exiting non-zero with no output at all. Its stdout stays
#    label-only — bazel writes INFO: progress lines to stderr, and folding those
#    in would manufacture labels out of progress noise.
query_bins_log="$(mktemp)"
query_bins_status=0
query_bins_raw="$(bazel query 'kind(go_binary, //...)' 2>"$query_bins_log")" || query_bins_status=$?
if [ "$query_bins_status" -ne 0 ]; then
  echo "ERROR: 'bazel query kind(go_binary, //...)' failed (exit $query_bins_status) - toolchain/host failure, NOT inventory drift:" >&2
  echo "  The live graph was never read, so scripts/bazel/binary-inventory.json is not implicated." >&2
  cat "$query_bins_log" >&2 || true
  rm -f "$query_bins_log"
  exit 1
fi
rm -f "$query_bins_log"
query_bins="$(printf '%s\n' "$query_bins_raw" | sort -u)"
inv_bins="$(jq -r '.[] | .[]' scripts/bazel/binary-inventory.json | sort -u)"
if [ -z "$query_bins_raw" ] && [ -n "$inv_bins" ]; then
  echo "ERROR: 'bazel query kind(go_binary, //...)' succeeded but returned zero labels while scripts/bazel/binary-inventory.json holds targets - host/workspace configuration failure, NOT inventory drift:" >&2
  echo "  A query that saw no graph is not a graph that holds nothing; check .bazelignore and the workspace layout." >&2
  echo "  If every go_binary target really was deleted, reconcile scripts/bazel/binary-inventory.json instead." >&2
  exit 1
fi
if ! diff <(printf '%s\n' "$query_bins") <(printf '%s\n' "$inv_bins") >/dev/null; then
  echo "ERROR: scripts/bazel/binary-inventory.json is out of sync with 'bazel query kind(go_binary, //...)':" >&2
  echo "  (< = in query but missing from inventory; > = in inventory but not in the graph)" >&2
  diff <(printf '%s\n' "$query_bins") <(printf '%s\n' "$inv_bins") >&2 || true
  exit 1
fi
echo "binary-inventory.json matches the live go_binary graph"

# 3) scripts/bazel/ledger.json must match `bazel query 'attr(tags,"manual",tests(//...))'`
#    EXACTLY. The BOS-339 make facade runs `bazel test //...` (which excludes every
#    `manual`-tagged target) plus the ledger's native step, so their union == today's
#    `go test ./...`. A new `manual` tag that is not reconciled into the ledger would be
#    dropped by `//...` AND absent from the ledger — silently shrinking `make test`
#    coverage with no failing test. Fail loudly on that drift here.
#    Same two-question discriminator as check 2: did the query succeed, and did it
#    return anything against a committed file that is not empty?
query_manual_log="$(mktemp)"
query_manual_status=0
query_manual_raw="$(bazel query 'attr(tags, "manual", tests(//...))' 2>"$query_manual_log")" || query_manual_status=$?
if [ "$query_manual_status" -ne 0 ]; then
  echo "ERROR: 'bazel query attr(tags,\"manual\",tests(//...))' failed (exit $query_manual_status) - toolchain/host failure, NOT ledger drift:" >&2
  echo "  The live graph was never read, so scripts/bazel/ledger.json is not implicated." >&2
  cat "$query_manual_log" >&2 || true
  rm -f "$query_manual_log"
  exit 1
fi
rm -f "$query_manual_log"
query_manual="$(printf '%s\n' "$query_manual_raw" | sort -u)"
ledger_manual="$(jq -r '.[].label' scripts/bazel/ledger.json | sort -u)"
if [ -z "$query_manual_raw" ] && [ -n "$ledger_manual" ]; then
  echo "ERROR: 'bazel query attr(tags,\"manual\",tests(//...))' succeeded but returned zero labels while scripts/bazel/ledger.json holds targets - host/workspace configuration failure, NOT ledger drift:" >&2
  echo "  A query that saw no graph is not a graph that holds nothing; check .bazelignore and the workspace layout." >&2
  echo "  If every manual-tagged test really was removed, reconcile scripts/bazel/ledger.json instead." >&2
  exit 1
fi
if ! diff <(printf '%s\n' "$query_manual") <(printf '%s\n' "$ledger_manual") >/dev/null; then
  echo "ERROR: scripts/bazel/ledger.json is out of sync with 'bazel query attr(tags,\"manual\",tests(//...))':" >&2
  echo "  (< = manual target missing from the ledger; > = ledger label no longer a manual target)" >&2
  diff <(printf '%s\n' "$query_manual") <(printf '%s\n' "$ledger_manual") >&2 || true
  exit 1
fi
echo "ledger.json matches the live manual-tagged test set"

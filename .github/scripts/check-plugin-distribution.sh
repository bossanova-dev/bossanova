#!/usr/bin/env bash
# Asserts every built bossd-plugin-* binary appears in the three
# distribution lists (release workflow, Homebrew, install.sh).
# Catches plugin additions that forget to update distribution.
#
# Plugins listed in NO_DISTRIBUTE are explicitly internal/dev-only and
# are skipped — add a name here only when you've decided not to ship
# the plugin to end users.

set -euo pipefail

NO_DISTRIBUTE=(
    bossd-plugin-linear   # internal task source; not shipped to end users
)

REPO_ROOT="$(git rev-parse --show-toplevel)"
RELEASE_YML="$REPO_ROOT/.github/workflows/perform-production-release.yml"
HOMEBREW_RB="$REPO_ROOT/infra/homebrew/bossanova.rb"
INSTALL_SH="$REPO_ROOT/infra/install.sh"

is_excluded() {
    local name=$1
    for excl in "${NO_DISTRIBUTE[@]}"; do
        [ "$name" = "$excl" ] && return 0
    done
    return 1
}

missing=0
for plugin_dir in "$REPO_ROOT"/plugins/bossd-plugin-*/; do
    name=$(basename "$plugin_dir")
    if is_excluded "$name"; then
        echo "skip: $name (in NO_DISTRIBUTE)"
        continue
    fi
    for f in "$RELEASE_YML" "$HOMEBREW_RB" "$INSTALL_SH"; do
        rel=${f#"$REPO_ROOT/"}
        if ! grep -q "$name" "$f"; then
            echo "ERROR: $name not found in $rel"
            missing=1
        fi
    done
done

if [ "$missing" -eq 1 ]; then
    echo
    echo "Add the missing plugin name(s) to the file(s) above."
    echo "See docs/plans/2026-05-02-extract-claude-into-plugin.md Task 23 for details."
    exit 1
fi
echo "All plugins present in all distribution lists."

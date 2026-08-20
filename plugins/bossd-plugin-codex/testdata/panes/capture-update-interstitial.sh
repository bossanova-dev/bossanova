#!/usr/bin/env bash
# capture-update-interstitial.sh — refresh testdata/panes/update_interstitial.txt
# from a live codex CLI.
#
# This is the BOS-600 incident pane: a codex process that OPENS on the
# "Update available!" boot interstitial rather than on a composer. Row 1 of its
# menu is drawn with codex's own composer glyph ("› 1. Update now"), so the
# session-start readiness gate's row anchoring accepts it and the Enter that
# follows selects "Update now" — which runs `npm install -g @openai/codex`,
# replaces the running binary, kills the pane and destroys the chat. The
# committed fixture is what teaches hasCodexModalPrompt to refuse it instead.
#
# Unlike its siblings capture.sh and capture-submit.sh, this capture needs NO
# account, NO network and NO real conversation: the interstitial is forced
# mechanically from a throwaway CODEX_HOME whose version.json advertises a
# version newer than the installed one. Nothing about the real install is
# mutated — the sandbox is created under $TMPDIR and removed on exit.
#
# Requirements:
#   - codex on PATH (a nodenv/asdf/volta shim is fine; see CODEX_BIN below)
#   - tmux on PATH
#   - node on PATH (the codex entrypoint is a `#!/usr/bin/env node` script)
#   - NO credentials, and none are used — see "Credential hygiene" below.
#
# Output: overwrites
#   plugins/bossd-plugin-codex/testdata/panes/update_interstitial.txt
#   plugins/bossd-plugin-codex/testdata/panes/update_interstitial.provenance.txt
#
# ---------------------------------------------------------------------------
# PROVENANCE (the environment the COMMITTED fixture was captured on)
#
#   codex-cli   0.147.0
#   tmux        3.7b
#   pane        200x50 (tmux new -x 200 -y 50)
#   OS          darwin-arm64 (macOS, Darwin 25.4.0)
#
# The same four facts are re-recorded mechanically into
# update_interstitial.provenance.txt on every run, so a re-capture cannot leave
# this block silently describing an environment nobody used any more. The block
# above is the human-readable copy; the sidecar is the machine-written one.
#
# Fixture provenance is NOT byte-identity. A rerun on another machine, or after
# a codex bump, legitimately produces different bytes — the capture embeds the
# installed version and a sleep-timed render. The contract is the same one
# capture.sh carries: the committed fixture contains the interstitial text, and
# a rerun exits zero on its sanity grep.
#
# ---------------------------------------------------------------------------
# Credential hygiene
#
# The child is launched through `env -i`, so it inherits NOTHING from this
# shell: no OPENAI_API_KEY, no ANTHROPIC_*, no bearer token, no ambient HOME
# holding ~/.codex/auth.json. Only HOME (a throwaway dir), PATH, TERM and
# CODEX_HOME are passed, and none of them is a secret. Nothing is passed via
# `tmux new -e` either, because that would place the value in the tmux server's
# environment where a later pane could read it back.
#
# Belt and braces, the capture is written to a temp file and asserted against
# API-key / bearer-token / email-address patterns BEFORE it is moved into
# testdata. A match aborts the run with the fixture left unwritten, so a pane
# that somehow rendered a secret can never be committed by accident.
set -euo pipefail

FIXTURE_DIR="$(cd "$(dirname "$0")" && pwd)"
FIXTURE="$FIXTURE_DIR/update_interstitial.txt"
PROVENANCE="$FIXTURE_DIR/update_interstitial.provenance.txt"

PANE_WIDTH=200
PANE_HEIGHT=50

# The version the sandbox advertises as "latest". Deliberately absurd so it is
# newer than any real codex release and the interstitial always fires.
SANDBOX_LATEST_VERSION="9.99.0"

# A nodenv/asdf shim resolves to "command not found" inside tmux: the pane does
# not inherit the login shell's version-manager wiring, and `env -i` strips what
# little it might have. Resolve the REAL binary here, in the login shell, and
# hand tmux an absolute path. Override with CODEX_BIN=/path/to/codex if your
# version manager is not one of the two probed below.
resolve_codex_bin() {
    if [[ -n "${CODEX_BIN:-}" ]]; then
        printf '%s' "$CODEX_BIN"
        return
    fi
    local resolved
    if command -v nodenv >/dev/null 2>&1 && resolved="$(nodenv which codex 2>/dev/null)"; then
        printf '%s' "$resolved"
        return
    fi
    if command -v asdf >/dev/null 2>&1 && resolved="$(asdf which codex 2>/dev/null)"; then
        printf '%s' "$resolved"
        return
    fi
    command -v codex
}

CODEX_BIN="$(resolve_codex_bin)"
if [[ ! -x "$CODEX_BIN" ]]; then
    echo "FAIL: could not resolve an executable codex binary (got: ${CODEX_BIN:-<empty>})." >&2
    echo "      Set CODEX_BIN=/absolute/path/to/codex and re-run." >&2
    exit 1
fi

# node must be on the child's PATH for the same reason: the codex entrypoint is
# a `#!/usr/bin/env node` script, and `env -i` gives the child no PATH of its own.
NODE_BIN="$(command -v node)"
NODE_BIN_DIR="$(cd "$(dirname "$NODE_BIN")" && pwd)"

SANDBOX="$(mktemp -d "${TMPDIR:-/tmp}/codex-update-interstitial.XXXXXX")"
SESSION="codex-update-capture-$$"
CAPTURE="$SANDBOX/capture.txt"

cleanup() {
    tmux kill-session -t "$SESSION" 2>/dev/null || true
    rm -rf "$SANDBOX"
}
trap cleanup EXIT

mkdir -p "$SANDBOX/home" "$SANDBOX/codex"

# The whole trick: codex reads version.json to decide whether a newer release
# exists. A sandbox copy claiming one does forces the interstitial on the next
# launch, with no network call and no mutation of the real ~/.codex.
cat > "$SANDBOX/codex/version.json" <<EOF
{"latest_version":"$SANDBOX_LATEST_VERSION","last_checked_at":"2099-01-01T00:00:00.000000Z","dismissed_version":null}
EOF

# Build the child command as ONE properly quoted string: tmux joins its argv
# into a command line and runs it through sh, so an unquoted path containing a
# space would silently split.
child_cmd="$(printf '%q ' \
    /usr/bin/env -i \
    "HOME=$SANDBOX/home" \
    "PATH=$NODE_BIN_DIR:/usr/bin:/bin:/usr/sbin:/sbin" \
    TERM=xterm-256color \
    "CODEX_HOME=$SANDBOX/codex" \
    "$CODEX_BIN")"

tmux new -d -s "$SESSION" -x "$PANE_WIDTH" -y "$PANE_HEIGHT" "$child_cmd"

# Codex needs a few seconds to draw the interstitial. It is a static screen with
# no network dependency, so this is a render wait, not a round trip.
sleep 12

# Production captures with "-p -S -1000" (tmux.Client.CapturePane); this omits
# the scrollback flag because the pane was created seconds ago by this script
# and has none, so the two produce identical bytes here. What must NOT diverge
# is the absence of -e: an escape-preserving capture puts "\x1b[0m" in front of
# the header row, and codexBootDecoration excludes digits, so the fixture would
# stop matching the grammar it exists to exercise.
tmux capture-pane -t "$SESSION" -p > "$CAPTURE"

# --- negative assertion: never write a fixture carrying a secret -------------
#
# Ordered BEFORE the sanity grep and before the move into testdata, so the run
# aborts with nothing written rather than leaving a secret on disk under a
# tracked path. Each pattern is checked separately so the failure names which
# one tripped.
secret_scan_failed=0
scan_for() {
    local label="$1" pattern="$2"
    if grep -nEi "$pattern" "$CAPTURE" >/dev/null; then
        echo "FAIL: captured pane matches a $label pattern; refusing to write $FIXTURE." >&2
        secret_scan_failed=1
    fi
}
scan_for "API-key" 'sk-[A-Za-z0-9_-]{16,}'
scan_for "bearer-token" 'bearer[[:space:]]+[A-Za-z0-9._~+/=-]{16,}'
scan_for "email-address" '[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}'
if [[ "$secret_scan_failed" -ne 0 ]]; then
    echo "      The capture is at $CAPTURE and is deleted when this script exits." >&2
    echo "      Re-run only after establishing why a credential-free sandbox rendered one." >&2
    exit 1
fi

# --- sanity: the pane must actually BE the interstitial ----------------------
#
# Both halves of the grammar the detector matches on, checked independently, so
# a partial render (header drawn, menu not yet) fails loudly here instead of
# committing a fixture that no longer exercises the clause it exists for.
# Mirrors codexBootInterstitialHeader, ARROW INCLUDED. The words alone are a
# weaker test than the Go anchor, so a codex that stopped rendering the version
# arrow would pass here, write a green fixture, and silently drop the header
# alternation — leaving the structural pair as the only coverage.
if ! grep -qE 'Update available![[:space:]]+v?[0-9]+(\.[0-9]+)+[[:space:]]*(->|=>|→)[[:space:]]*v?[0-9]+(\.[0-9]+)+' "$CAPTURE"; then
    echo "FAIL: captured pane has no 'Update available! <old> -> <new>' header;" >&2
    echo "      codex may have started straight into a composer, the sandbox" >&2
    echo "      version.json may not have been read, or the header was restyled" >&2
    echo "      and codexBootInterstitialHeader no longer matches it. Review the" >&2
    echo "      live pane by re-running without the trap." >&2
    exit 1
fi
if ! grep -qE 'Press[[:space:]]+enter[[:space:]]+to[[:space:]]+continue' "$CAPTURE"; then
    echo "FAIL: captured pane has no 'Press enter to continue' footer; the render" >&2
    echo "      may have been caught mid-frame. Increase the sleep and re-run." >&2
    exit 1
fi
# A deliberately STRICTER mirror of codexBootInterstitialMenu: a RUN of at
# least codexBootInterstitialMinOptions (2) CONSECUTIVE numbered option rows,
# where a leading "›" is the menu's own selection cursor and so is ALLOWED
# here even though codexBootDecoration excludes it elsewhere.
#
# The cursor is REQUIRED on at least one row of the run, matching
# codexBootInterstitialSelectedRow: a run of numbered rows on its own is what
# an agent's ordered list looks like, and the grammar stopped accepting that.
#
# Stricter because the Go prefix class admits any non-letter/non-digit
# decoration, box borders included, while this admits only spaces and that one
# cursor. So a boxed menu ("| 1. Update now") satisfies the grammar and trips
# this check. That divergence is the safe direction for a drift detector: it
# can red-alarm on a fixture the grammar would have accepted, but it can never
# green-light one the grammar rejects. Widen it here if codex ever ships the
# boxed form -- do not widen the grammar to match this.
#
# Adjacency is the load-bearing part and the reason this is awk rather than
# grep -c: any numbered row the Go grammar does not accept resets its run, so
# two numbered lines separated by prose are an ordered list it REJECTS. A count
# of matches anywhere in the file would green-light exactly that shape, which
# would make this check agree with the grammar only by accident.
if ! awk '
    /^[[:space:]]*(›)?[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]/ {
        run++
        if ($0 ~ /^[[:space:]]*›[[:space:]]*[0-9]+\.[[:space:]]+[^[:space:]]/) { cursor = 1 }
        if (run >= 2 && cursor) { found = 1; exit }
        next
    }
    { run = 0; cursor = 0 }
    END { exit found ? 0 : 1 }
' "$CAPTURE"; then
    echo "FAIL: captured pane has no run of 2+ consecutive numbered option rows" >&2
    echo "      with the '›' selection cursor drawn on one of them; the menu" >&2
    echo "      shape the structural alternation anchors on is gone." >&2
    exit 1
fi

mv "$CAPTURE" "$FIXTURE"

cat > "$PROVENANCE" <<EOF
# Written by capture-update-interstitial.sh. Do not hand-edit — re-run the
# script instead. Describes the environment update_interstitial.txt was
# captured on; see the script header for why the fixture is not byte-pinned to
# this environment.
codex-cli: $("$CODEX_BIN" --version 2>/dev/null | tr -d '\r')
tmux: $(tmux -V | tr -d '\r')
pane: ${PANE_WIDTH}x${PANE_HEIGHT}
os: $(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m) ($(uname -r))
advertised-latest-version: $SANDBOX_LATEST_VERSION
EOF

echo "OK: refreshed $FIXTURE ($(wc -l < "$FIXTURE" | tr -d ' ') lines)"
echo "OK: refreshed $PROVENANCE"

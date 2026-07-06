#!/usr/bin/env bash
# worktree-lock.sh — atomic, re-entrant per-worktree mutex for
# boss-implement. One deterministic artifact:
#   - acquired with an ATOMIC `mkdir` (exactly one winner under a double-dispatch race),
#   - RE-ENTRANT on the run-id (a run reading its own lock can never collide with itself),
#   - stored OUTSIDE the worktree (~/.local/state/bossanova) so it never shows in `git status`,
#   - heartbeat-stale so a CRASHED run's lock is taken over (resume), not wedged.
#
# Exit codes: 0 acquired/re-entrant/took-over/released/status, 2 usage,
#             3 held-by-peer / not-owner.
set -euo pipefail

TOP="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "ERR: not in a git worktree" >&2; exit 2; }
# Canonicalize (resolve symlinks + normalize) so the same worktree always keys identically.
CANON_TOP="$(cd "$TOP" && pwd -P)"
HOME_DIR="${BLI_LOCK_HOME:-$HOME/.local/state/bossanova}"
SLUG="${BLI_SLUG:-bossanova}"
STALE_SECS="${BLI_LOCK_STALE_SECS:-5400}"   # 90 min: dominates the worst silent stretch of a healthy run

# Collision-safe per-worktree key: <basename>-<short sha256 of canonical path>.
# NOT slashes->underscores (that collides /a/b_c with /a_b/c and skips canonicalization).
hash_path() { printf '%s' "$1" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -d' ' -f1 | cut -c1-16; }
LOCK="$HOME_DIR/linear-implement/locks/$SLUG/$(basename "$CANON_TOP")-$(hash_path "$CANON_TOP")"
META="$LOCK/owner"   # 4 lines: runid, pid (non-load-bearing), heartbeat-epoch, ticket

now() { date +%s; }
# Atomic meta write: temp file + rename, so a racing reader never sees half-written meta.
write_meta() {
  mkdir -p "$LOCK"
  local tmp="$LOCK/.owner.$$"
  # Field 2 (PID) is non-load-bearing: liveness reads only the runid + heartbeat.
  # Kept so the 4-line meta format and `status` output stay stable.
  printf '%s\n%s\n%s\n%s\n' "$1" "$PPID" "$(now)" "$2" > "$tmp" && mv -f "$tmp" "$META"
}
owner_field() { [ -f "$META" ] && sed -n "${1}p" "$META" 2>/dev/null || true; }
# Portable dir mtime as a bare epoch int (GNU `stat -c %Y` then BSD/macOS `stat -f %m`);
# numeric guard so this can only ever emit an integer (a text mtime would poison `age`).
dir_mtime() {
  local m
  m="$(stat -c %Y "$LOCK" 2>/dev/null)" || m="$(stat -f %m "$LOCK" 2>/dev/null)" || m="$(now)"
  case "$m" in ''|*[!0-9]*) m="$(now)" ;; esac
  printf '%s' "$m"
}
# Effective heartbeat: META heartbeat when written, else dir mtime. The fallback closes
# the TOCTOU window between `mkdir` and the META write — a mid-acquire peer has no
# heartbeat yet and must read LIVE (mtime≈now), never stale. If the mtime cannot
# be read because the lock dir is moving under a concurrent acquire/revival,
# treat it as fresh; epoch-zero would misclassify the live peer as stale.
eff_heartbeat() { local hb; hb="$(owner_field 3)"; if [ -n "$hb" ]; then printf '%s' "$hb"; else dir_mtime; fi; }

cmd="${1:-}"; runid="${2:-}"; ticket="${3:-}"

case "$cmd" in
  acquire)
    [ -n "$runid" ] || { echo "ERR: acquire needs <runid>" >&2; exit 2; }
    mkdir -p "$(dirname "$LOCK")"
    if mkdir "$LOCK" 2>/dev/null; then
      write_meta "$runid" "$ticket"; echo "ACQUIRED runid=$runid"; exit 0
    fi
    o_runid="$(owner_field 1)"
    if [ "$o_runid" = "$runid" ]; then
      write_meta "$runid" "$ticket"; echo "ACQUIRED runid=$runid (re-entrant)"; exit 0
    fi
    o_hb="$(eff_heartbeat)"
    age=$(( $(now) - ${o_hb:-0} ))
    if [ "$age" -lt "$STALE_SECS" ]; then
      echo "HELD_BY_PEER runid=${o_runid:-unknown} age=${age}s"; exit 3
    fi
    # Genuinely stale -> steal ATOMICALLY: rename(2) the stale lock aside. Exactly one
    # reviver can move the canonical name; others' `mv` fails (source gone) and they re-read.
    stamp="${LOCK}.stale.$$"
    if mv "$LOCK" "$stamp" 2>/dev/null; then
      # Another reviver may have replaced the stale lock after this process read
      # o_runid/o_hb but before this mv. Only steal the exact lock observed.
      s_runid="$(sed -n '1p' "$stamp/owner" 2>/dev/null || true)"
      s_hb="$(sed -n '3p' "$stamp/owner" 2>/dev/null || true)"
      if { [ -n "$o_runid" ] && [ "$s_runid" != "$o_runid" ]; } || { [ -n "$o_hb" ] && [ -n "$s_hb" ] && [ "$s_hb" != "$o_hb" ]; }; then
        mv "$stamp" "$LOCK" 2>/dev/null || rm -rf "$stamp"
      else
        rm -rf "$stamp" 2>/dev/null || true
        if mkdir "$LOCK" 2>/dev/null; then
          write_meta "$runid" "$ticket"; echo "TOOK_OVER_STALE runid=$runid prev=${o_runid:-none} age=${age}s"; exit 0
        fi
      fi
    fi
    if [ "$(owner_field 1)" = "$runid" ]; then echo "ACQUIRED runid=$runid (re-entrant)"; exit 0; fi
    echo "HELD_BY_PEER runid=$(owner_field 1) age=$(( $(now) - $(eff_heartbeat) ))s"; exit 3 ;;

  heartbeat)
    [ -n "$runid" ] || { echo "ERR: heartbeat needs <runid>" >&2; exit 2; }
    [ "$(owner_field 1)" = "$runid" ] || { echo "NOT_OWNER"; exit 3; }
    write_meta "$runid" "$(owner_field 4)"; echo "OK"; exit 0 ;;

  release)
    [ -n "$runid" ] || { echo "ERR: release needs <runid>" >&2; exit 2; }
    # Fast path: not ours -> leave a live peer's lock completely untouched.
    [ "$(owner_field 1)" = "$runid" ] || { echo "NOT_OWNER owner=$(owner_field 1)"; exit 3; }
    # Ours as of the read above. Now ATOMICALLY claim the canonical dir by renaming it
    # aside BEFORE deleting, so a stale-takeover racing in the read->rm window cannot make
    # us delete a live successor's lock: rename(2) on the canonical name has exactly one
    # winner. If our mv loses (a takeover renamed first), the lock is gone -> NOT_OWNER.
    # If our mv wins but a new owner slipped in just before it, restore it untouched
    # (stale timeout reclaims the orphan) rather than delete a live successor.
    stamp="${LOCK}.releasing.$$"
    mv "$LOCK" "$stamp" 2>/dev/null || { echo "NOT_OWNER owner=$(owner_field 1)"; exit 3; }
    if [ "$(sed -n '1p' "$stamp/owner" 2>/dev/null || true)" = "$runid" ]; then
      rm -rf "$stamp"; echo "RELEASED"; exit 0
    fi
    mv "$stamp" "$LOCK" 2>/dev/null || rm -rf "$stamp"
    echo "NOT_OWNER owner=$(owner_field 1)"; exit 3 ;;

  status)
    if [ -f "$META" ]; then
      hb="$(eff_heartbeat)"
      printf 'LOCKED runid=%s pid=%s heartbeat=%s ticket=%s age=%ss\n' \
        "$(owner_field 1)" "$(owner_field 2)" "$(owner_field 3)" "$(owner_field 4)" \
        "$(( $(now) - ${hb:-0} ))"
    else
      echo "UNLOCKED"
    fi
    exit 0 ;;

  *) echo "usage: worktree-lock.sh {acquire|heartbeat|release|status} <runid> [ticket]" >&2; exit 2 ;;
esac

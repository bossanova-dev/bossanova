#!/usr/bin/env bash
# worktree-lock.sh — atomic, re-entrant per-worktree mutex for
# boss-build. One deterministic artifact:
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
# Single emitter for the line-ordered owner meta so the 4-line format (read back by
# owner_field / eff_heartbeat by line index) lives in exactly one place and cannot drift.
# Field 2 (PID) is non-load-bearing: liveness reads only the runid + heartbeat; it is
# kept so the 4-line meta format and `status` output stay stable.
emit_owner() { printf '%s\n%s\n%s\n%s\n' "$1" "$PPID" "$(now)" "$2"; }
# CLAIM-path meta write: publish the owner meta into a lock this process just won with its
# own exclusive `mkdir "$LOCK"` (fresh acquire, or stale steal). Atomic temp-file + rename so
# a racing reader never sees half-written meta, and robust against a concurrent reviver
# transiently renaming $LOCK aside then restoring it: the whole `ensure $LOCK -> write temp ->
# rename onto owner` retries a bounded number of times instead of aborting mid-claim under
# `set -e` when $LOCK is momentarily gone (the lock-vanish race). Recreate-on-vanish is safe HERE
# because a stale-observation peer that grabs our freshly-mkdir'd lock never matches what it
# observed, so its guard RESTORES our lock rather than taking ownership — it can never become a
# rival owner for us to clobber. This must NOT be used to refresh a lock we did not just create
# (see refresh_meta). Temp stays INSIDE $LOCK so it is reclaimed when $LOCK is removed/renamed.
write_meta() {
  local tmp n=0
  while [ "$n" -lt 100 ]; do
    mkdir -p "$LOCK" 2>/dev/null || true
    tmp="$LOCK/.owner.$$"
    if emit_owner "$1" "$2" > "$tmp" 2>/dev/null && mv -f "$tmp" "$META" 2>/dev/null; then
      return 0
    fi
    n=$(( n + 1 ))
  done
  return 1
}
# REFRESH-path meta write: update our OWN heartbeat in place (heartbeat + re-entrant acquire),
# on a lock we did NOT just create and that may already be stale. Unlike write_meta it NEVER
# recreates a vanished $LOCK and NEVER overwrites a different owner: if a genuine stale-steal
# took our lock over between the caller's ownership check and here, `mv -f` lands on the moved
# temp / foreign dir and fails -> return non-zero, so we can never resurrect-and-clobber a
# successor into a double-takeover (the orphan-resume race). Ownership is re-checked first.
refresh_meta() {
  [ "$(owner_field 1)" = "$1" ] || return 1
  local tmp="$LOCK/.owner.$$"
  emit_owner "$1" "$2" > "$tmp" 2>/dev/null && mv -f "$tmp" "$META" 2>/dev/null
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
      # Re-entrant refresh of our own lock (never recreates/clobbers — see refresh_meta).
      if refresh_meta "$runid" "$ticket"; then echo "ACQUIRED runid=$runid (re-entrant)"; exit 0; fi
      # Refresh failed: our lock may have been stale and just genuinely stolen. Re-read; if we
      # still own it, it is ours (a benign transient); otherwise fall through to peer/steal.
      o_runid="$(owner_field 1)"
      if [ "$o_runid" = "$runid" ]; then echo "ACQUIRED runid=$runid (re-entrant)"; exit 0; fi
    fi
    o_hb="$(eff_heartbeat)"
    age=$(( $(now) - ${o_hb:-0} ))
    # Decisively-stale boundary: live is strictly age < STALE_SECS, so an age exactly
    # equal to the threshold STEALS. `date +%s` truncates to whole seconds, so with a
    # `<=` gate two racers computing age == STALE_SECS both read live and both back off,
    # yielding zero winners (the stale-boundary tie). `-lt` makes the boundary decisively stealable.
    if [ "$age" -lt "$STALE_SECS" ]; then
      echo "HELD_BY_PEER runid=${o_runid:-unknown} age=${age}s"; exit 3
    fi
    # Genuinely stale -> steal ATOMICALLY: rename(2) the stale lock aside. Exactly one
    # reviver can move the canonical name; others' `mv` fails (source gone) and they re-read.
    stamp="${LOCK}.stale.$$"
    if mv "$LOCK" "$stamp" 2>/dev/null; then
      # Another reviver may have replaced the stale lock after this process read
      # o_runid/o_hb but before this mv. Only steal the exact lock observed; if a fresh
      # successor slipped in, restore it untouched (its own stale timeout reclaims it).
      s_runid="$(sed -n '1p' "$stamp/owner" 2>/dev/null || true)"
      s_hb="$(sed -n '3p' "$stamp/owner" 2>/dev/null || true)"
      if { [ -n "$o_runid" ] && [ "$s_runid" != "$o_runid" ]; } || { [ -n "$o_hb" ] && [ -n "$s_hb" ] && [ "$s_hb" != "$o_hb" ]; }; then
        mv "$stamp" "$LOCK" 2>/dev/null || rm -rf "$stamp"
      else
        # Genuine steal. Discard the stale dir, then claim the canonical name with an
        # EXCLUSIVE `mkdir` — the same unambiguous single-winner primitive as a fresh acquire
        # (above): among all concurrent revivers AND fresh acquirers, exactly one `mkdir
        # "$LOCK"` succeeds, so there is never a double-takeover. (Publishing with `mv "$stamp"
        # "$LOCK"` cannot select a winner safely: portable `mv` silently NESTS into an existing
        # dir instead of failing, so two processes can both "succeed".) On a win, write_meta
        # publishes robustly (it now retries a transiently-renamed-aside $LOCK, so the
        # lock-vanish race can no longer abort the winner's meta write); on a loss (another reviver or
        # fresh acquirer already recreated $LOCK) fall through to the re-read tail below.
        rm -rf "$stamp" 2>/dev/null || true
        if mkdir "$LOCK" 2>/dev/null; then
          write_meta "$runid" "$ticket"
          echo "TOOK_OVER_STALE runid=$runid prev=${o_runid:-none} age=${age}s"; exit 0
        fi
      fi
    fi
    if [ "$(owner_field 1)" = "$runid" ]; then echo "ACQUIRED runid=$runid (re-entrant)"; exit 0; fi
    echo "HELD_BY_PEER runid=$(owner_field 1) age=$(( $(now) - $(eff_heartbeat) ))s"; exit 3 ;;

  heartbeat)
    [ -n "$runid" ] || { echo "ERR: heartbeat needs <runid>" >&2; exit 2; }
    [ "$(owner_field 1)" = "$runid" ] || { echo "NOT_OWNER"; exit 3; }
    # Refresh in place — never recreate/clobber a lock a stale-steal may have taken over.
    if refresh_meta "$runid" "$(owner_field 4)"; then echo "OK"; exit 0; fi
    echo "NOT_OWNER"; exit 3 ;;

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

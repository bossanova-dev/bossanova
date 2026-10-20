# boss-plan-env.sh — resolve the installed boss-plan toolbox for one Bash block.
#
# SOURCED, never executed. Every boss-plan command block that dereferences
# $BOSS_PLAN_TOOLBOX begins with this single line:
#
#   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
#
# Each Bash tool call is a fresh shell, so an exported value never survives to the
# next block — the source line is per-block, not once per run.
#
# That line only LOCATES this file, because a helper cannot resolve its own path
# before it is read. It must therefore probe every install tree itself, in the same
# order this file does: a pre-set BOSS_SKILLS_HOME, then ~/.claude/skills, then
# ~/.codex/skills. It is a LOOP over those three candidates rather than a hand-unrolled
# `||` chain, which is both ~100 bytes shorter per block — and it appears in eleven — and
# structurally unable to disagree with the scan below about the candidate order.
#
# BOSS_SKILLS_HOME is a candidate, never the default. `${BOSS_SKILLS_HOME:-…}` substitutes
# its default only when the variable is UNSET, so spelling it as a default would drop
# ~/.claude out of the search entirely whenever a value is pre-set, and a machine with a
# healthy Claude install would BLOCK with a remedy that cannot fix it. As a candidate it
# is tried first and simply falls through when it carries nothing. ~/.codex is not
# redundant either: a Codex-only install has no ~/.claude/skills at all.
#
# The locate tests `[ -f ]` and never relies on `.` to fail: `.` is a POSIX special
# built-in, so in `sh`/`dash` a missing file EXITS the shell outright rather than
# returning non-zero — a `. a || . b || { echo …; exit 1; }` chain would silently skip
# every remaining candidate along with its own error message. The loop clears
# BOSS_PLAN_ENV on a miss for the same reason the chain reassigned it: the emptiness
# check after the loop, not `.`, is what decides whether anything was found.
#
# The resolution below is the AUTHORITATIVE one: take the first install tree that
# carries this very file, honouring a pre-set BOSS_SKILLS_HOME only when it does, and
# fail loud and fatal otherwise — message on stdout, then exit 1.
#
# Test for THIS FILE, not merely for a boss-plan/toolbox DIRECTORY. A stale tree keeps
# its directory long after it stops carrying the helper, so a directory test lets the
# locate line fall through to ~/.codex for the helper while this scan sends
# BOSS_PLAN_TOOLBOX straight back to the stale ~/.claude tree — helper from one install,
# guards from another, silently, with exit 0. The locate line has already proved which
# trees carry the helper; agreeing with it is what keeps a stale install loud, which is
# the entire point of the BLOCKED path below.

if [ -z "${BOSS_SKILLS_HOME:-}" ] || [ ! -f "$BOSS_SKILLS_HOME/boss-plan/toolbox/boss-plan-env.sh" ]; then
  BOSS_SKILLS_HOME=""
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -f "$candidate/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_SKILLS_HOME="$candidate"; break; fi
  done
fi
test -n "${BOSS_SKILLS_HOME:-}" || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }
BOSS_PLAN_TOOLBOX="$BOSS_SKILLS_HOME/boss-plan/toolbox"
export BOSS_SKILLS_HOME BOSS_PLAN_TOOLBOX

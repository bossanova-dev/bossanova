# boss-plan-env.sh — resolve the installed boss-plan toolbox for one Bash block.
#
# SOURCED, never executed. Every boss-plan command block that dereferences
# $BOSS_PLAN_TOOLBOX begins with this single line:
#
#   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
#
# Each Bash tool call is a fresh shell, so an exported value never survives to the
# next block — the source line is per-block, not once per run.
#
# That line only LOCATES this file, because a helper cannot resolve its own path
# before it is read. It must therefore probe every install tree itself, in the same
# order this file does: a pre-set BOSS_SKILLS_HOME, then ~/.claude/skills, then
# ~/.codex/skills. ~/.claude appears twice on purpose. `${BOSS_SKILLS_HOME:-…}`
# supplies its default only when the variable is UNSET, so without the explicit second
# candidate any pre-set value would drop ~/.claude out of the search entirely and a
# machine with a healthy Claude install would BLOCK with a remedy that cannot fix it.
# The third candidate is not redundant either: a Codex-only install has no
# ~/.claude/skills, so a locate that stopped there could not find the helper at all.
# The locate tests `[ -f ]` and never relies on `.` to fail: `.` is a POSIX special
# built-in, so in `sh`/`dash` a missing file EXITS the shell outright rather than
# returning non-zero — a `. a || . b || { echo …; exit 1; }` chain would silently skip
# every remaining candidate along with its own error message.
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

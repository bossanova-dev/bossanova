# Cron gate — full detail

Read this when scheduling `boss-build` as an unattended implementation cron (a setup-time concern,
not part of a run). Register this **gate command** on the job (scheduler UI, `GateCommand` — see
PR #870) so the run only fires when there is a candidate, spending **zero** agent tokens otherwise:

```
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
node "$BOSS_BUILD_TOOLBOX/cron-gates/boss-build.mjs"
```

Append any shared selection flags (`--label`, `--exclude-label`, `--assignee`, `--creator`,
`--project` and their `--exclude-` forms) to narrow the scan, and pass the **same** flags in the
job's `/boss-build` prompt so the worker's `list-planned` scans identically; any other argument
makes the gate exit non-zero.

Opt-in open-PR limit: `--max-open-prs N` (also `--max-open-prs=N`, a positive integer) exits `1`
while the repository already has `N` or more open PRs, drafts included, naming the count, the
limit and the PR numbers. It is checked through `gh` before the tracker scan; an unreadable count
or an invalid `N` exits `69`. Without the flag `gh` is never called for it. Pass the same flag in
the job's prompt (`/boss-build headless --max-open-prs N`) so the worker stops identically.

It exits `0` (run) iff at least one tracker issue is in the configured team
(`trackerConfig.<tracker>.team`, when set), is in the **planned** state, carries the
**`agent-build`** label but not `needs-human`, matches the effective selection (config
`trackerConfig.<tracker>.selection`, then the flags), **and is not blocked by an uncleared
blocker** (neither completed/canceled, linked to a merged GitHub PR, nor possibly review-clearable), and `1` (`gated`) when there is no work. This
keeps the cron from waking to a fully-blocked backlog and burning a run that only exits
`NO_CHANGE`. It is still a deliberately **loose superset** of Step 2's exact selection: the
gate does not read descriptions, so a ticket with nothing to build from (no readable plan
attachment and no title or description text) can occasionally let a run fire that then exits `NO_CHANGE` — a rare
false-positive in exchange for one cheap query, never a false-negative. Step 2's filter remains the source of truth. The gate is **fail-closed**: Linear uses `LINEAR_API_KEY`, injected by bossd; GitHub uses the host's own `gh` login, with `gh` on the daemon's `PATH`. Missing credentials, an unresolvable selection value, network failure or API error exits `69` (`gate_failed`) with a one-line reason on stderr, captured in the scheduler's `gate_output` log. The adapter's `hasUnblockedWork` answers whether a candidate's blockers are cleared; this entry is a thin I/O wrapper.

The Linear gate first checks all candidates from state alone; an already-unblocked candidate costs
no PR reads. Otherwise it memoizes linked PR lookups, stops at the first eligible candidate and
caps lookups at 25 per run. PR failures or the cap leave blockers uncleared. The gate also admits a blocker in the configured in-review state when it has at least one open
linked PR and every open PR is non-draft and green. It deliberately omits the file-disjointness
test: only Step 2 reads dependent Key changes paths. This loose superset avoids gating a candidate
selection can clear. Unknown PR/check facts fail closed. Epic scheduling and merge ordering retain
strict clearance and never use the selection-only class.

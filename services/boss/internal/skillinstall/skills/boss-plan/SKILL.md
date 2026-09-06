---
name: boss-plan
description: Plan a tracker backlog ticket. Grabs the next unplanned issue by priority (or a ticket ID you provide), resolves drafting through boss-plan draft extensions or portable fallbacks, attaches the plan natively to the tracker issue, then writes a summary, labels, Fibonacci estimate, and priority before moving it from the unplanned to the planned state. Interactive by default; runs fully headless when BOSS_CRON=true.
---

# boss-plan

Turn a vague, one-line tracker ticket into a fully-planned ticket (in the **planned**
state) with an implementation-ready plan attached. Use when asked to "plan a tracker ticket",
"plan the next ticket", "boss-plan", or given a ticket ID.

This skill is **interactive by default** — it may drive `AskUserQuestion` through a discovered
draft extension. Under `BOSS_CRON=true` it runs **fully headless**, dispatching a single awaited
subagent for recon + drafting (Phase 2), so it is safe to schedule unattended.

- **Leave no local artifacts.** At every terminal state, discard the scratch you created (gitignored dirs, seeded design docs, `mktemp` files) so the worktree is clean — in all modes, headless (`BOSS_CRON=true`) especially.
- **Dispatch zero-change work as such.** Planning runs commit nothing, so a plain session finalizes `blocked` behind an empty draft PR. See _Sessions that change nothing_ in the `boss` skill: `create_session` takes `quick_chat` (no worktree or PR) or `defer_pr` (worktree, no up-front PR).

**Headless mode.** If `BOSS_CRON=true`, no human can answer `AskUserQuestion`, so **never call it** —
in orchestrator/subagent, any phase. Path: preflight → select the
queue head → **dispatch ONE `general-purpose` drafting subagent** (`toolbox/bs-dispatch-await.mjs`;
`Task`/`spawn_agent`+`wait_agent`) → classify its sentinel → upload + write back to Linear. Use reasonable defaults,
discard local artifacts, and never block waiting for input.

## On-demand references (read only when the mode calls for it)

Mode-exclusive prose lives in `references/*.md`, loaded **only** on the path that needs it. The
**default headless orchestrator path reads neither** — the resident body carries the whole skeleton.

| Reference                               | Read it when…                                                                             |
| --------------------------------------- | ----------------------------------------------------------------------------------------- |
| `references/interactive-mode.md`        | Interactive `/boss-plan` only — Phase 1 confirm loop, design-doc seed, draft resolution   |
| `references/headless-drafting-brief.md` | Passed (by **path**) to the Phase 2 drafting subagent — never read by the orchestrator    |
| `references/extension-reviewers.md`     | Phase 3.5 — repo-local `boss-plan-*` extension plan-reviewers (additive; no-op when none) |

Workspace facts (do not re-discover). Load the config once in Phase 0 —
`loadSkillConfig({cwd})` → `config`; `tc = trackerConfigFor(config)` — and reference these role
names generically everywhere else:

- Reach the tracker only through the resolved tracker adapter; its server, team, team-key and
  workspace come from `trackerConfigFor(config)` (never inline them, and never pass a `project` filter).
- Statuses by role: the **unplanned** state (start) and the **planned** state (end), resolved from
  `trackerConfigFor(config).states.{unplanned,planned}` (with `inProgress`/`inReview` for the
  active-backlog reads).
- **Pipeline label roles** resolve through `labelName(config, '<role>')`, whose keys are **camelCase**: `agentFriendly`, `needsHuman`, `agentPlan`, `agentQuestion`, `epic` — the display
  names they resolve to are `agent-friendly`, `needs-human`, and so on. `labelName` fails closed —
  an unconfigured or misspelled role throws — so never hand it a display name. Never create labels.
  `agentFriendly` and `needsHuman` are mutually exclusive (every plan gets exactly one).
- **Content-taxonomy labels** (`bug`, `feature`, `improvement`, `docs`) are the tracker's own
  display names, not fixed pipeline roles. Read the issue's existing set with the `readLabels` op
  and merge — preserve what it returned, and add one only when it genuinely applies. Resolve each
  taxonomy name with `optionalLabelName(config, '<role>')`, whose keys are `bug`, `feature`,
  `improvement`, `docs`; if it returns `null`, apply the literal display name. Never create labels.
- Tracker priority numeric: `1=Urgent, 2=High, 3=Medium, 4=Low, 0=None`.
- Dependency links use the tracker's `blocks`/`blocked by` relations. A blocker is "cleared" only
  when its state type is completed or canceled (PR merged / work dropped) — the
  `DEFAULT_CLEARED_STATE_TYPES` / `DEFAULT_CANCELED_STATE_TYPES` rule in
  `toolbox/plan-deps-lib.mjs`. boss-build will not start a ticket blocked by an uncleared blocker.
- Proof publishing remains independent of implementation-plan storage. Its configured publish
  adapter and `publishConfig` continue to govern proof artifacts only.

## Phase 0 — Preflight

1. **Self-disable when this repo has no configured tracker.** This runs in **both** interactive and
   headless modes and **precedes every tracker read/write**. Probe the config seam and, when the repo
   has no `.boss-skills.json` / no configured tracker, print exactly one line and exit **0** — a clean
   no-op, not an error (a `/boss-plan` in an unrelated repo is a no-op; a non-zero exit would surface
   as a cron/agent error):
   ```bash
   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   CONFIGURED=$(node -e 'import(require("node:url").pathToFileURL(process.env.BOSS_PLAN_TOOLBOX+"/skill-config.mjs").href).then(m=>{const c=m.loadSkillConfig({cwd:process.cwd()});process.stdout.write(m.isConfiguredForPlanning(c)?"yes":"no")}).catch(e=>{process.stderr.write("boss-plan preflight: "+(e&&e.message||e)+"\n");process.stdout.write("error")})')
   # `isConfiguredForPlanning` requires the tracker identity AND the full state role map
   # (`states.{unplanned,planned,inProgress,inReview}`), so a repo configured only for a stateless
   # core self-disables cleanly ('no') instead of running with undefined state names.
   # Distinguish a loader failure (malformed/invalid .boss-skills.json → 'error' or empty) from a
   # valid "not planning-ready" ('no'): loadSkillConfig throws a `skill-config:` error on a present
   # but broken config, so a broken config must abort loudly, never skip silently as a clean no-op.
   if [ "$CONFIGURED" != "yes" ] && [ "$CONFIGURED" != "no" ]; then
     echo "boss-plan: .boss-skills.json is present but could not be loaded (see error above) — aborting instead of skipping." >&2
     exit 1
   fi
   if [ "$CONFIGURED" != "yes" ]; then
     echo "boss-plan: no configured tracker in .boss-skills.json for this repo — nothing to plan here; skipping."
     exit 0
   fi
   # Amortized self-heal for regular plan-scratch files orphaned by runs that abort before cleanup.
   node "$BOSS_PLAN_TOOLBOX/plan-scratch-reap.mjs" .linear-plans ||
     echo "warning: stale plan-scratch reap failed (non-fatal)" >&2
   ```
   That first `.` line is the **toolbox preamble**. Each Bash tool call is a fresh shell, so every
   command block that dereferences `$BOSS_PLAN_TOOLBOX` must begin with it; an exported value never
   survives to the next block. It sources `toolbox/boss-plan-env.sh`, which is what actually resolves
   `BOSS_SKILLS_HOME` (a pre-set value first, else the first of `~/.claude/skills` and
   `~/.codex/skills` that carries `boss-plan/toolbox/boss-plan-env.sh` — the helper _file_, not
   merely the directory, which a stale tree keeps long after it stops carrying the helper), sets and
   exports `BOSS_PLAN_TOOLBOX`, and fails loudly when no tree carries it. The line probes those same
   candidates itself, with `[ -f ]` tests and `||` reassignments, because the helper cannot locate
   itself before it is read. `~/.claude/skills` is named a second time on purpose:
   `${BOSS_SKILLS_HOME:-…}` supplies its default only when the variable is _unset_, so without the
   explicit candidate a pre-set value drops that tree out of the search entirely.
   `loadSkillConfig` is synchronous and takes an options object
   (`loadSkillConfig({ cwd })`); positional or awaited calls read as broken config.
2. **Report installed skill drift before planning.** If `boss` resolves, run the read-only
   `boss skills check --gate` before any tracker write. It fails only on installed-vs-checkout drift
   that is not `self-edited` by this branch; non-zero is **advisory** — print a `warning:` line with
   the reported reinstall remedy and continue, because drift is bookkeeping and a stale tree still
   runs. The hard stop is the line before it: a missing `boss-plan-env.sh` means the skills are not
   installed at all, and nothing downstream can execute. Without `boss`, keep the older warning probe
   so drift is visible rather than called clean. Re-derive the path first, since an unset guard is
   silent like a clean tree:
   ```bash
   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
     if O="$("$BOSS_BIN" skills check --gate 2>&1)"; then
       if [ -n "$O" ]; then printf '%s\n' "$O" >&2; fi
     else
       case "$O" in
         *--gate*) node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true ;;
         *)
           printf '%s\n' "$O" >&2
           R="$(printf '%s\n' "$O" | sed -n 's/^  run `\(.*\)`$/\1/p' | head -n 1)"
           if [ -n "$R" ]; then
             echo "warning: installed boss skills drift from checkout source; run: $R — bookkeeping only, work state unaffected" >&2
           else
             echo "warning: installed boss skills drift from checkout source; see gate output above — bookkeeping only, work state unaffected" >&2
           fi
           ;;
       esac
     fi
   elif [ -f "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" ]; then
     node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true
   else
     echo "boss-toolbox-drift: (drift helper not installed) — this install predates the check; drift is UNKNOWN, not clean." >&2
   fi
   ```
   A `boss-toolbox-drift:` line is the no-CLI fallback signal: warning-only, because that helper may
   itself be stale. Re-vendor and reinstall the skills to clear it. Either way drift never decides a
   terminal state.
3. Require the configured tracker's optional `preparePlanAttachment`, `finalizePlanAttachment`,
   `readPlanAttachment`, and `deletePlanAttachment` operations now. If any is absent, stop before
   drafting or tracker writes. These names are conventional tracker-adapter operations declared in
   the adapter `operationMap` (`OPTIONAL_TRACKER_OPERATIONS` in `tracker/adapter-core.mjs`), not
   toolbox exports or greppable helper symbols; for a tool-backed adapter, each op's `tool` field is
   the concrete capability to probe. `deletePlanAttachment` is required here, not at its first use:
   every upload site reads its artifact back and deletes a confirmed-unreadable orphan
   (`references/plan-storage.md` step 5), so a missing op must fail with nothing written.
   Native tracker attachments are the only implementation-plan store and never change proof storage.
4. Confirm the tracker adapter is reachable with a cheap read (its status-list capability scoped to
   `trackerConfigFor(config).team`).

## Phase 1 — Select the issue

- **If the user gave a ticket ID**: call `get_issue` with it. Respect that choice
  regardless of status.
  - **Interactive:** if it is already in the planned/in-progress/`Done`/`Canceled` state, warn and
    confirm before re-planning (see `references/interactive-mode.md`).
  - **Headless (`BOSS_CRON=true`):** do not ask. A cron job that names a ticket means to consider that
    ticket, but the idempotence precheck below still wins: an already-planned ticket with a valid
    description and canonical plan attachment exits cleanly without re-drafting. If the ticket is
    `Done`/`Canceled`, log a warning and **stop** (re-planning finished work unattended is almost
    never intended) rather than blocking.
- **Otherwise**: list the team's unplanned issues via the tracker adapter's list/select capability —
  scoped to `trackerConfigFor(config).team` and the `unplanned` state, `limit=250`. **Rank the whole
  queue** by **priority**, reading the tracker's numbers correctly: Urgent(1) > High(2) > Medium(3)
  > Low(4) > None(0). Tie-break by **oldest `createdAt` first**. Keep this ranked list.
  - **Interactive:** show the head of the ranked queue and run the confirm loop (**plan this one /
    skip this one / pick a different one / cancel**) — see `references/interactive-mode.md`. `skip`
    walks down the ranked list.
  - **Headless (`BOSS_CRON=true`):** do not ask. Select the **head of the ranked queue** (highest
    priority, oldest tie-break) and proceed straight to Phase 2. If the unplanned queue is empty,
    report that and stop.

Before Phase 2 in both modes, run the idempotence precheck. Write the selected issue payload from
the Phase 1 read to `.linear-plans/<ISSUE-ID>.precheck.json` and invoke the deterministic guard
(`planIdempotencePrecheck(...)` in `$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs`):

```bash
BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
PRECHECK=".linear-plans/<ISSUE-ID>.precheck.json"
node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" idempotence "$PRECHECK"
```

If it prints `action: "noop"`, delete the scratch file, print one line naming the ticket and the
satisfied conjuncts (planned state, valid description, canonical plan attachment), then exit **0**
with **zero tracker writes**. If it prints `action: "plan"`, log every `reasons[]` token and
continue. This precheck applies to explicitly-named tickets as well as queue-selected tickets; a
named ticket is not permission to destructively re-draft an already valid plan.

## Phase 2 — Draft the plan

The plan itself — codebase recon, the review dimensions, and the polished write-up — is produced
per the **Phase 3 plan requirements** (the shared contract for what a plan must contain). The two
modes differ only in **who drafts**:

## Draft-resolution (shared Fallback contract)

Resolve drafting by the Fallback contract: discovered `boss-plan-*` `role: draft`
extension → host built-in → inline prompt; tiers 2/3 suppressed only when a Tier-1 dispatch
**succeeded**, never merely because an extension exists. A dispatch **succeeded** only when its
result is valid **AND** the requested non-empty plan exists at the per-dispatch plan path that
dispatch alone was given, written by **that** dispatch — never at a path a peer could have written;
promote the first success to the real plan path. Record
`extension <name>: skipped (<reason>)` for every failed dispatch, including when a sibling
succeeded; when none succeeded, fall through to tier 2, then tier 3.

### Interactive (default `/boss-plan`)

Resolve the draft/review step via the Fallback contract; the interactive
resolution and tier-3 inline drafting prompt live in `references/interactive-mode.md`. Then
continue to Phase 3.5 → Phase 4.

### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent

Do **not** draft inline. Recon, drafting, and the self-review dimensions are bulk
context; keeping them on the main thread is exactly the cost this mode avoids. Instead:

**Bulk-output discipline (no raw bulk in the orchestrator).** The drafting dispatch keeps its bulk
material — the codebase recon and the drafted plan body — in the **subagent's own context** and
returns **only the plan-file path plus a bounded metadata object**; the orchestrator **never** pastes
the plan body or a subagent transcript back into its own context. It classifies the outcome from the
**run-file sentinel only** (never from returned prose) and reads the finished plan file exactly once,
for the Phase 4 secret gate.

1. Create the per-run sentinel context (the subagent writes its terminal decision here; the
   orchestrator classifies **from the file only**). `DISPATCH_FAILURE` must stay byte-identical to
   the module constant in `bs-run-sentinel.mjs`:

   ```bash
   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   RUN_SENTINEL="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
   test -f "$RUN_SENTINEL" || { echo "BLOCKED: bs-run-sentinel.mjs missing" >&2; exit 1; }
   DISPATCH_FAILURE="dispatch-failure"
   PLAN_PATH=".linear-plans/<ISSUE-ID>-<slug>.md"   # compute the slug with plan-slug.mjs issueSlug
   RUN="$(node "$RUN_SENTINEL" make-ctx boss-plan)"
   RUN_ID="${RUN%%$'\t'*}"; RUN_DIR="${RUN#*$'\t'}"
   export RUN_SENTINEL DISPATCH_FAILURE PLAN_PATH RUN_ID RUN_DIR
   ```

2. Before dispatch, write the byte copy of the Phase 1 `get_issue` description to
   `.linear-plans/<ISSUE-ID>.image-guard-orig.md`. This is the single raw-description snapshot for
   the whole run: Phase 4 reuses it, and the worker receives this path as its **only** description
   source. Do not let the worker re-read the tracker description; signed upload URLs can rotate and
   fail the parity gate.

3. **Dispatch ONE awaited `general-purpose` subagent** (`subagent_type: general-purpose`,
   <!-- tier: opus --> plan drafting is judgment, so **tier: opus**; **await** the dispatch —

   **never** `run_in_background`). Pass it the **path** `references/headless-drafting-brief.md` (not
   its text), the ticket `id`/`title`, the description snapshot path, the target `PLAN_PATH`, and the sentinel context
   `RUN_SENTINEL`/`RUN_DIR`/`RUN_ID`. The brief tells it to recon, work the review dimensions, write
   the plan to `PLAN_PATH`, write the terminal sentinel with a `planPath` payload, and **return only**
   the bounded metadata object
   (`planPath`, `labels`, `agentFriendly`, `estimate`, `priority`, `openQuestions`,
   `descriptionSummary`) — **never the plan file's content** (returning content re-inflates the
   caller: codex fold).

   If the dispatch tool itself errors before the subagent starts, treat that as a dispatch failure:
   print one clear stderr line, clean up the sentinel context if it exists, make **no Linear write**,
   and exit non-zero. Do **not** draft inline in headless mode.

4. **Classify from the run-file sentinel only**, then re-verify (never trust the sentinel alone —
   epic D11):
   **Measurement is orchestrator-owned.** The orchestrator measures on-disk artifacts with `stat` or
   `wc -c`; reported size is never the input. After `ok`, re-verify every orchestrator-consumed
   artifact: `PLAN_PATH`, guard, child-plan and epic-spec scratch. Epics
   require artifact manifests (`guardScratchPaths`,`epicSpecPaths`).
   Zero-byte original guard sources
   (`.image-guard-orig.md` / `.attachment-guard-orig.md`) are ok; others non-empty.
   Missing, empty, directory or wrong-path ⇒
   `echo "$DISPATCH_FAILURE: sentinel ok but artifact missing/empty or wrong path (<path>) — no Linear write, aborting" >&2`.

   ```bash
   READ="$(node "$RUN_SENTINEL" read "$RUN_DIR" "$RUN_ID" draft)"
   RC_STATUS="$(printf '%s' "$READ" | jq -r '.status')"
   if [ "$RC_STATUS" != "ok" ]; then
     # missing/stale sentinel: SAFE branch — NO Linear write, non-zero exit.
     echo "$DISPATCH_FAILURE: drafting subagent left no valid sentinel (status=$RC_STATUS) — no Linear write, aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
     # Abort skips Phase 5; delete the epic/guard/run-boundary scratch families now.
     CLEANUP_RC=0
     rm -f .linear-plans/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json || CLEANUP_RC=1
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md.rejected' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.image-guard-*.md' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-guard-orig.md' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-headers-*.json' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.epic-spec.json' -delete || CLEANUP_RC=1; fi
     if [ -d .linear-plans ] && [ -n "$(find .linear-plans -maxdepth 1 -type f \( -name '<ISSUE-ID>-child-*.md' -o -name '<ISSUE-ID>-child-*.md.rejected' -o -name '<ISSUE-ID>*.image-guard-*.md' -o -name '<ISSUE-ID>*.attachment-guard-orig.md' -o -name '<ISSUE-ID>*.attachment-headers-*.json' -o -name '<ISSUE-ID>*.epic-spec.json' \) -print)" ]; then CLEANUP_RC=1; fi
     if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans may still hold plan text, tracker state or signed upload headers" >&2; fi
     exit 1
   fi
   node -e 'const f=require("fs"),p=require("path"),[r,L,F]=process.argv.slice(1),x=JSON.parse(r).payload||{},T=c=>c?.trim?.(),B="epicSpecPaths",H="guardScratchPaths",P="childPlanPaths",K=["planPath",H,P,B,"attachmentHeaderPaths"],v=k=>{const q=x[k]||[];return k===P&&q&&!Array.isArray(q)&&typeof q=="object"?Object.values(q):[].concat(q)},g=s=>s.toLowerCase().replace(/[^a-z\d]+/g,"-").replace(/^-+|-+$/g,""),n=(id,t)=>id.toUpperCase()+"-"+g(t);let b=0,E=c=>{console.error(`${F}: sentinel ok but artifact missing/empty or wrong path (${c}) — no Linear write, aborting`);b=1},S=v(B).filter(T),G=v(H).filter(T),D=p.resolve(".linear-plans");if(x.epic){const I=v("childIds").filter(T),M=typeof x[P]=="object"&&!Array.isArray(x[P])?x[P]:{},C=I.map(id=>M[id]).filter(T),R=T(x.epicParentId),A=[],U=new Set,O=p.resolve(D,`${R}.epic-spec.json`);for(const k of[H,B])if(!Array.isArray(x[k]))E(k);if(!S.length)E(B);for(const s of S){if(p.resolve(s)!==O){E(B);continue}try{const q=JSON.parse(f.readFileSync(s));if(T(q.parentId)!==R)E(B);for(const c of q.children||[])if(T(c.key)&&T(c.title))A.push([c.key,c.title])}catch{E(s)}}if(!R||I.some(id=>"image-guard-orig attachment-guard-orig image-guard-new".split` `.some(w=>!G.some(c=>p.basename(c)===`${R}.child-${id}.${w}.md`))))E(H);if(!R||!I.length||A.length!==I.length||C.length!==I.length||new Set(C.map(c=>p.resolve(c))).size!==I.length)E(P);for(const id of I){const c=T(M[id]);if(!c){E(`${P}.${id}`);continue}const j=A.findIndex(y=>p.basename(c)===`${R}-child-${y[0]}-${n(id,y[1])}.md`);if(j<0||U.has(j))E(c);else U.add(j)}if(U.size!==A.length)E(P)}else if(!v("planPath").some(T))E("planPath");const P0=p.resolve(L);for(const k of K)for(const c of v(k))if(T(c)){const z=p.resolve(c),a=z===P0||p.dirname(z)===D,m=a&&f.existsSync(z)&&f.statSync(z),s=m&&m.isFile()&&(m.size||k===H&&/-guard-orig[.]md$/.test(z));if(!s)E(c)}process.exit(b)' "$READ" "$PLAN_PATH" "$DISPATCH_FAILURE" ||
     {
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       # Artifact verification failure also skips Phase 5; remove the same scratch families.
       CLEANUP_RC=0
       rm -f .linear-plans/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json || CLEANUP_RC=1
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md.rejected' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.image-guard-*.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-guard-orig.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-headers-*.json' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.epic-spec.json' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ] && [ -n "$(find .linear-plans -maxdepth 1 -type f \( -name '<ISSUE-ID>-child-*.md' -o -name '<ISSUE-ID>-child-*.md.rejected' -o -name '<ISSUE-ID>*.image-guard-*.md' -o -name '<ISSUE-ID>*.attachment-guard-orig.md' -o -name '<ISSUE-ID>*.attachment-headers-*.json' -o -name '<ISSUE-ID>*.epic-spec.json' \) -print)" ]; then CLEANUP_RC=1; fi
       if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans may still hold plan text, tracker state or signed upload headers" >&2; fi
       exit 1
     }
   EPIC="$(printf '%s' "$READ" | jq -r '.payload.epic // empty')"
   PREMISES="$(printf '%s' "$READ" | jq -c '.payload.premises // []')"
   if [ "$EPIC" = "true" ]; then
     # EPIC outcome: the subagent claims it performed ALL tracker writes itself (children
     # created + wired, parent repurposed with the parent-label exception, moved
     # unplanned → planned). BEFORE accepting, RE-VERIFY the epic against Linear (never trust
     # the sentinel alone — the subagent may have written `ok` too early / with partial
     # tracker writes, mirroring the single-ticket plan-file re-verify below):
     EPIC_PARENT="$(printf '%s' "$READ" | jq -r '.payload.epicParentId // empty')"
     # Run BOTH Linear MCP reads NOW and promote only from their actual results.
     EPIC_REVERIFIED=false
     # (a) get_issue "$EPIC_PARENT": parent planned, epic-labeled, not unplanned.
     # (b) list_issues parentId="$EPIC_PARENT" limit=250; hydrate each child with get_issue; require
     # payload `childIds` match, every child planned + canonical-plan attached, and
     # `reconcileEpicChildren(spec, hydratedLiveChildren)` passes. missing/empty childIds is a sentinel-shape failure, not a silent fallback.
     # Both true ⇒ EPIC_REVERIFIED=true; otherwise SAFE branch — NO success report.
     if [ "$EPIC_REVERIFIED" != "true" ]; then
       echo "$DISPATCH_FAILURE: epic sentinel ok but reverify failed (parent still unplanned, or children missing/short) — no success report, aborting" >&2
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       # Reverify-fail also skips Phase 5; remove the same scratch families.
       CLEANUP_RC=0
       rm -f .linear-plans/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json || CLEANUP_RC=1
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md.rejected' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.image-guard-*.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-guard-orig.md' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-headers-*.json' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.epic-spec.json' -delete || CLEANUP_RC=1; fi
       if [ -d .linear-plans ] && [ -n "$(find .linear-plans -maxdepth 1 -type f \( -name '<ISSUE-ID>-child-*.md' -o -name '<ISSUE-ID>-child-*.md.rejected' -o -name '<ISSUE-ID>*.image-guard-*.md' -o -name '<ISSUE-ID>*.attachment-guard-orig.md' -o -name '<ISSUE-ID>*.attachment-headers-*.json' -o -name '<ISSUE-ID>*.epic-spec.json' \) -print)" ]; then CLEANUP_RC=1; fi
     if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans may still hold plan text, tracker state or signed upload headers" >&2; fi
       exit 1
     fi
     # reverify PASSED: there is NO single-ticket plan file, and the single-ticket
     # metadata (labels/agentFriendly/estimate/…) does NOT apply. SKIP Phase 3.5 and
     # Phase 4 entirely and go straight to Phase 5 (cleanup) + Phase 6 (report), using
     # the bounded epic metadata (epicParentId, childIds) for the report.
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
   else
     PLAN_FILE_RAW="$(printf '%s' "$READ" | jq -r '.payload.planPath // empty')"
     # Normalize an equivalent absolute path; reject every path resolving elsewhere.
     PLAN_FILE="$(node -e 'const {resolve}=require("node:path");const [reportedPath,expectedPath]=process.argv.slice(1);if(!reportedPath||resolve(reportedPath)!==resolve(expectedPath))process.exit(1);process.stdout.write(expectedPath)' "$PLAN_FILE_RAW" "$PLAN_PATH")"
     # single-ticket `ok` sentinel → re-verify the expected plan file is non-empty.
     if [ "$PLAN_FILE" != "$PLAN_PATH" ] || [ ! -s "$PLAN_FILE" ]; then
       echo "$DISPATCH_FAILURE: sentinel ok but plan file missing/empty or wrong path ($PLAN_FILE_RAW) — no Linear write, aborting" >&2
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       exit 1
     fi
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
   fi
   ```

   **Branch on the `ok` payload.** An **epic** outcome (`payload.epic == true`, no `planPath`) means
   the subagent already did every Phase 2.5 tracker write. Re-read Linear before accepting: its parent
   must be planned and its children must match required `childIds` / `parseEpicSpec`; a sentinel that
   omits `childIds` is rejected separately from a child-reconciliation miss. Recovery is to decode the
   spec attachment body with `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" decode <in-file> <out-file>`
   and re-run reconciliation, never to accept the sentinel alone. Otherwise safe-abort so the next
   sweep resumes it. On success skip Phase 3.5–4; re-running them would turn the parent into a
   `boss-build` target. A single-ticket `ok` sentinel proceeds only when its metadata `planPath`
   resolves to `PLAN_PATH` and names a non-empty plan file. Its `descriptionSummary` becomes the
   Linear description; read the plan file only for the secret gate.

   After an `ok` sentinel and the plan-file reverify pass, validate the returned bounded metadata
   before Phase 3.5. Write exactly the returned metadata object to
   `.linear-plans/<ISSUE-ID>.draft-metadata.json` and run:

   ```bash
   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   METADATA=".linear-plans/<ISSUE-ID>.draft-metadata.json"
   if ! node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" metadata "$METADATA"; then
     echo "$DISPATCH_FAILURE: draft metadata failed plan-run-guards.mjs metadata — no Linear write, aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
    rm -f "$METADATA" .linear-plans/<ISSUE-ID>.{precheck,premises,premise-states}.json
     exit 1
   fi
   ```

   Unknown top-level keys, a missing `descriptionSummary`, non-boolean `agentFriendly`, a
   non-single-ticket estimate, or an off-contract `descriptionSummary` are all the same SAFE branch:
   `DISPATCH_FAILURE`, no Phase 3.5, no tracker write. Preserve `PREMISES` from the sentinel payload
   before cleanup; Phase 4 re-verifies those tracker premises immediately before writeback.

## Phase 2.5 — Epic decomposition (triage = EPIC only)

When triage classifies the ticket **EPIC** — the honest estimate is **≥ 5**, or the work spans
**multiple independently-shippable
PRs** with **≥ 2** genuinely separable PR-sized pieces (an honest `≤ 3` single-PR ticket is
`SUBSTANTIAL`, plan as one) — decompose it into a Linear **parent + N fully-planned
children** wired by an intra-epic `blockedBy` DAG, the exact shape `boss-epic` consumes.
**Estimate is the forcing function:** a single ticket may be estimated only `0/1/2/3`; an honest `5`
triages EPIC (unless genuinely atomic & un-splittable — then it survives as one ticket with a
recorded `- Atomic-5:` justification under `## Planning`); an `8` is never a single-ticket estimate. The
interactive propose → confirm → create flow lives in `references/interactive-mode.md`; the headless
decompose-and-auto-create flow in `references/headless-drafting-brief.md`. The deterministic core —
validation, cycle safety, stable creation order, and the tracker-write plan — is the unit-tested
`$BOSS_PLAN_TOOLBOX/plan-epic-lib.mjs` (`validateDecomposition`, `validateLayering`, `assertAcyclic`,
`topoOrderChildren`, `epicWiringPlan`, `epicParentEstimate`, `stableChildKey`, `serializeEpicSpec`,
`parseEpicSpec`, `validateSpecIdentity`, `specAttachmentFilename`, `specAttachmentTitle`,
`reconcileEpicChildren`, `EPIC_LABEL`, `EPIC_MIN_CHILDREN`, `EPIC_MAX_CHILDREN`,
`CHILD_MAX_ESTIMATE`, `SPEC_ATTACHMENT_MIME`) plus this phase's own
`$BOSS_PLAN_TOOLBOX/plan-epic-phase25.mjs` (`detectEpicParent`, `epicSpecRecoveryGate`,
`stalePlanAttachmentSweep`, `epicPhase25WritePlan`);
never re-derive either inline.

**Precondition — the source ticket MUST be unplanned and not already an epic child.** The whole epic model depends on it:
parent-repurpose-last keeps the original in unplanned until the epic is fully built, and idempotent
resume re-picks a stranded partial epic via the **headless unplanned sweep** (`list_issues
state=unplanned`). If the source carries `parentId`, record a single-ticket `SUBSTANTIAL` fallback:
decomposing a child mints grandchildren `boss-epic` never schedules. Phase 1 admits an
explicitly-named planned/in-progress source; if such a non-unplanned source triages **EPIC**, **check
BOTH spec stores before falling back** — one
`get_issue(parent)` already returns `attachments[]` **and** `description`, so checking both costs
**zero** extra calls. `detectEpicParent(issue)` is the whole classification, over that one payload:
it returns `{isEpicParent, source, specAttachmentId, ambiguous, reasons}` and owns the
store-specific presence rule, the attachment-wins-over-legacy ordering, and the two-or-more
`Epic spec (…)` attachments case (`ambiguous: true` ⇒ **abort loudly** per the contract's duplicate
policy, never guess which is current). An `isEpicParent` verdict means an **existing epic parent** (a
planned fully-built epic keeps its spec), so route to the **idempotent resume/no-op path** — read
attachment specs from the **attachment body**, never a description quote, and **never** fall back to
a buildable single-ticket plan. Attachment specs must pass `validateSpecIdentity(spec, <ISSUE-ID>)`;
legacy inline specs are accepted by provenance because that read-only store predates
`schemaVersion`/`parentId`. Only a legacy parse _failure_ reaches the gate below. **Unreadable-spec recovery gate** — when the spec cannot be read, or an
attachment-sourced spec fails `validateSpecIdentity`, the decision is
`epicSpecRecoveryGate({parent, children, plannedState, epicLabel})`, which owns the ALL-of conjunct
set — parent planned + epic-labelled; every child planned + plan artifact — and names every failure.
Feed it hydrated children: start from `list_issues parentId=<parentId> limit=250`, then `get_issue`
each child when the list omits attachments or full fields. Its `action` is only ever `'noop'`
(enumerate + no-op) or `'abort'`:
**falling through to the single-ticket path is forbidden** — deliberately not even expressible in
that return type, because it would re-plan a finished or partial
epic as a normal buildable ticket. An **unplanned** parent can never satisfy the planned-parent
conjunct, so a corrupt spec attachment on one aborts every sweep until a human intervenes — that is deliberate
(re-decomposing would duplicate children), and the remediation is the same as the duplicate policy's:
delete the unreadable `Epic spec (…)` attachment, leaving the parent to re-decompose cleanly, or
repair its body. Accepted residual: the gate cannot detect a child deleted
outright — that failure is non-destructive (a partial epic is left alone, not corrupted). Only a
non-unplanned source with **neither** store present falls back to a single-ticket
`SUBSTANTIAL` plan (headless records the reason; interactive may re-ask). A non-unplanned parent
would sit in a non-queue state through the create→wire→expose window and, on a crash before the final
flip, be **invisible to the unplanned sweep and stranded** — recoverable only by manually re-running
that exact id. This precondition also means a well-formed epic parent never carries stale
`agent-friendly`/plan-link metadata; the strip in step 4 (below) is a defense-in-depth backstop, not
the primary guard.

**The spec attachment contract.** The decomposition spec is a **native tracker attachment carrying
plain JSON**, never a description marker:

| Field            | Value                                                                                                         |
| ---------------- | ------------------------------------------------------------------------------------------------------------- |
| filename         | `epic-spec.json` (`specAttachmentFilename()`)                                                                 |
| MIME type        | `application/json` (`SPEC_ATTACHMENT_MIME`)                                                                   |
| title            | `Epic spec (<ISSUE-ID>)` (`specAttachmentTitle(<ISSUE-ID>)`) — **must NOT start with `Implementation plan`**  |
| body             | `serializeEpicSpec(spec)` — plain JSON `{ schemaVersion, parentId, parent, children }`                        |
| read             | `readPlanAttachment` (the Phase 0 attachment-read op), by attachment id from `get_issue`                      |
| duplicate policy | exactly one is valid; **two or more ⇒ abort loudly**, never guess — a human deletes all but one, then re-runs |
| identity         | `validateSpecIdentity(spec, <ISSUE-ID>)` — `schemaVersion` + `parentId` must match, **not title alone**       |

Upload it with the **same** prepare → PUT → finalize mechanism a plan artifact uses
(`references/plan-storage.md` steps 1–5, `uploadRequest.headers` scratch-file discipline and its
immediate deletion after the PUT included, and the step-5 read-back), substituting this contract's
filename, MIME type and title. Never hand-roll a second upload path, and never claim the plan artifact's `text/markdown`
MIME or its `Implementation plan (…)` title: `bs-epic-lib.mjs`'s `normalizeTicket` recognizes a plan
by exactly that prefix, so a spec attachment titled that way is mistaken for the parent's plan
artifact. Title alone is not identity — a human can create an attachment with any name — so the
`schemaVersion` + `parentId` match is what makes it trustworthy.

The planner drafts a **decomposition spec**
`{ parentId:"<ISSUE-ID>", parent:{title,goal,keyChanges[]}, children:[{key,title,goal,keyChanges[],blockedByKeys[],estimate,priority,agentFriendly,openQuestions[]}] }`
(each `key` is a **stable** title-derived slug from `stableChildKey`, so a fresh-worktree retry
re-derives it identically and its resume marker still matches; **`parentId` is the source ticket's own
id and is not optional** — `serializeEpicSpec` omits an absent id rather than inventing one, and
`validateSpecIdentity` then refuses the attachment forever, so an unset `parentId` ships an
unbindable spec), then runs this **ordering discipline —
validate everything locally BEFORE the first Linear write** (the atomicity guard):

1. **Validate the spec.** `validateDecomposition` + `assertAcyclic`. On failure: interactive
   re-asks / falls back to a single `SUBSTANTIAL` plan; headless **falls back to a single-ticket
   plan and records the reason** (never emit a broken epic).
2. **Fully plan every child locally** to IDless scratch, each a **Phase 3** plan, drafted with
   **`allowEpic: false`** — the **recursion guard**:
   a child is never itself decomposed (depth cap = 1). The spec never carries plan bodies, so copy
   only each child plan's own `agentFriendly` verdict **and its `openQuestions` list** onto its spec
   entry — `serializeEpicSpec` derives the child's `agentQuestion` (⇒ the `agent-question` label) from
   a non-empty `openQuestions`, so a child left blank here silently loses that queue signal on
   resume. Then **re-run `validateDecomposition` on the completed spec before any
   write** — step 1 validated the spec _before_ those verdicts existed, so its non-boolean-`agentFriendly`
   guard (a malformed `"false"` string `serializeEpicSpec` would coerce to `true`) only bites when
   validation runs again after the copy. Run the Phase 4 **secret** and **image-parity**
   gates on every child plan _before_ any write.
3. **Confirm** (interactive only, via `AskUserQuestion`: create this epic / plan as one ticket /
   cancel); headless auto-creates.
4. **Persist the FULL spec FIRST.** The spec is an attachment now, so the old single atomic
   `save_issue` becomes an ordered write sequence, and that sequence is
   `epicPhase25WritePlan({parentId, spec, unplannedState, staleAttachmentIds, labelsToStrip})`
   (`labelsToStrip` = the `agent-friendly`/`needs-human` exposure roles; it is **parent-scoped**,
   stage 1's `stripLabels` and nothing else): execute its ops **in emitted order** — `label-strip`,
   then `spec-upload`, then `stale-delete`, then `create-children` — **minus any stage the
   preconditions below skip, and on a resume minus every child `reconcileEpicChildren` does NOT
   report `missing`** (it emits one `createChild` per SPEC child, never per missing child; executing
   those unfiltered on a resume duplicates every child that already exists), exactly as step 5
   executes `epicWiringPlan`. Each entry is `{stage, op, args, runtimeArgs}`, and **`args` is the
   statically-known subset only**, under the adapter's own key names; the created-id map passed to
   `epicWiringPlan` must include the reserved `parent` entry beside every child id — `runtimeArgs`
   names what only
   the executor can supply because it does not exist until the previous op ran (the prepare's `size`,
   the PUT's `file`/`uploadURL`/`headers`, the finalize's `assetUrl`). It owns the ordering (in
   particular that every destructive delete comes strictly after the spec upload); never re-derive
   that inline. It does **not** own the stage preconditions below, which stay prose, and it emits
   **no child `labels` field** — a child's label set is not derivable from the spec
   (`serializeEpicSpec` persists `agentFriendly`/`agentQuestion`, never a `labels` array), so the
   content labels + `agent-question` union below stays the caller's job:
   - **Stage 1 — label strip only.** The entry carries `stripLabels` OUTSIDE `args`, because
     `save_issue` has **no** "remove these labels" argument — its `labels` **replaces** the whole
     set. So read the parent's current labels (op `readLabels`), subtract `stripLabels`, and send
     the result as `labels`; spreading a `removeLabels` key into the call would either error or
     silently send `{id}` alone, leaving the parent exposed for the whole create→wire→expose
     window. Cheap, atomic, reversible, and sufficient on its own:
     `boss-build` selects a ticket that is planned **and** `agent-friendly` **and** carries a plan
     artifact, so breaking one conjunct makes the parent non-selectable from the FIRST tracker
     mutation onward rather than through the create→wire→expose window or after a crash (step 7's
     strip then only reaffirms it). This is the safety write; **it must not delete anything.**
   - **Stage 2 — upload the spec, exactly once.** **Read the parent's `attachments[]` AND its
     description FIRST — BOTH stores, the same dual-store rule detection uses.** Scoping this check
     to attachments would miss a legacy parent entirely, and the unplanned sweep that lands here is
     the **primary** resume route, not just the named-source branch above. Unlike the description
     marker it replaces — where a re-save simply overwrote the one marker —
     a finalize is **not** idempotent: it mints a NEW attachment row on every call, so a re-picked
     parent that already carries a spec must never upload a second one. Apply the same store-specific
     presence rule detection uses: an `Epic spec (…)` **attachment** counts when present; a
     **description** counts only when `parseEpicSpec(description)` returns a spec, never on a bare
     quoted `<!-- boss-plan-epic-spec:` substring. Take these in order: **two
     or more `Epic spec (…)` attachments ⇒ abort loudly** per the contract's duplicate policy;
     otherwise **either** store present ⇒
     **skip this stage entirely**, and skip stage 3 with
     it (step 7's flip re-runs that same prefix-scoped strip); discard the spec just drafted and
     continue on the idempotent resume
     path below against **the stored** one (a crash after stage 2 leaves precisely this state,
     and the parent is still unplanned, so the sweep re-picks it here). A legacy-sourced resume
     writes **no** attachment — it keeps its inline marker, carried verbatim through step 6's save. Otherwise upload — first **set `spec.parentId` to this
     ticket's id**, since only a bound spec can ever pass `validateSpecIdentity`. The PUT takes a
     **file**, so write
     `serializeEpicSpec(spec)` to `.linear-plans/<ISSUE-ID>.epic-spec.json` (Phase 5 deletes this
     scratch). **Then verify those bytes BEFORE the PUT:**
     `validateSpecIdentity(parseEpicSpec(<the file's contents>), <ISSUE-ID>)` must be `ok`. Nothing
     else catches an unbound spec — `serializeEpicSpec` omits an unset `parentId` silently rather
     than inventing one, and `validateDecomposition` never inspects it — so without this check the
     attachment uploads clean and only turns fatal much later, when a resume cannot bind it. `ok: false`
     ⇒ abort here, while zero children exist. Otherwise prepare → PUT → finalize the
     `epic-spec.json` attachment per the contract above; keep it for
     `epicSpecPaths` reverify when this stage ran. When this stage is skipped because a stored
     attachment or legacy marker already exists, write the stored spec to
     `.linear-plans/<ISSUE-ID>.epic-spec.json`, report it in `epicSpecPaths`, and do not upload.
     **Then read the
     finalized spec back — still inside this stage, BEFORE any child is created** (step 5 of that
     contract, one retry on a transport error): the read must return non-empty content **and**
     `validateSpecIdentity(parseEpicSpec(<the returned body>), <ISSUE-ID>)` must be `ok`. The pre-PUT
     check validates the bytes on disk; only this one proves what the tracker stored, which is what
     every resume reads. On a **confirmed-unreadable** or unbindable read-back,
     `deletePlanAttachment` that orphaned row — safe **only** here, with zero children — then take
     the SAFE branch. **Once children exist, a failed read-back aborts WITHOUT deleting the spec:**
     that would leave the parent with neither store, the state step 6 warns re-decomposes into
     DUPLICATE children. Reading back before the first child create is what keeps the delete safe.
     **Any** failure takes the SAFE branch: abort with
     **zero children created**, the parent still unplanned, so the next unplanned sweep re-picks it.
   - **Stage 3 — the deferred destructive strip.** Its `staleAttachmentIds` come from the same
     prefix-scoped sweep step 7 cites, in its **one-arg** form — no parent-overview attachment
     exists yet, so there is nothing to keep; an unscoped sweep would destroy the spec just
     uploaded. Any
     stale single-ticket `Implementation plan (…)` **link** is dropped here too. For
     `tracker-attachment`, `deletePlanAttachment` was already required **before the FIRST epic
     write** (this stage is skipped on every resume, so gating on its availability here would defer
     the check to step 7 — past child creation and exposure). A crash between stages 1 and 2 leaves only a removed label —
     recoverable, non-destructive, and resume just re-decomposes from scratch, nothing orphaned.

   No stage moves the ticket out of unplanned, so parent-repurpose-last still holds. **Crash-safety:**
   the description is never rewritten here, so after stage 2 Linear holds the **original notes +
   image URLs AND — unless stage 2 was skipped because a store already existed — the spec
   attachment**, and a fresh retry recovers the spec from whichever store holds it
   **and** reconstructs the verbatim `## Original notes` + runs the
   image-parity gate against the still-present original source. This durable record — surviving a fresh
   cron worktree — carries
   the parent overview **and every child's full metadata** (key, title, goal, keyChanges, blockedByKeys,
   estimate, priority, **`agentFriendly` call, and its `agentQuestion` decision** —
   `openQuestions` non-empty; it never carries plan bodies), so a retry completes the
   **original** epic from the parent alone rather than re-decomposing (a fresh LLM re-decomposition
   could build a different partial epic). Persisting `agentFriendly`/`agentQuestion` is what lets resume
   re-stamp the step-6 deferred-exposure label **and** re-apply `agent-question` to an ALREADY-created
   child correctly (below). Then **create children** as unplanned, unexposed shells so each returned
   id can receive its native plan attachment before its planned-state write. Each child shell
   carries `parentId` = original, each child spec's
   validated `estimate` and `priority` (so `boss-epic`, which orders ready/merge work by ticket
   priority, schedules children as the decomposition intended rather than by default/None), content
   labels **plus `agent-question` for any child whose plan recorded non-empty `openQuestions`** (the
   Phase 4 contract — union it into that child's labels at creation; it is independent of the
   agent-friendly/needs-human call and survives via the spec's `agentQuestion`), and a child plan
   **artifact titled exactly `Implementation plan (<child id>)`** (`boss-epic`'s
   `normalizeTicket` recognizes a plan only via a link/attachment whose title **starts with**
   `Implementation plan`; a child linked or attached under any other title is exposed `agent-friendly` yet silently
   skipped by `boss-epic` as missing a plan). On resume, inspect every adopted shell for that exact
   canonical attachment first. If absent, **always redraft** that child from its persisted spec
   metadata with `allowEpic:false`, re-run its secret and image-parity gates, and prepare, PUT and
   finalize the plan attachment before any planned-state or exposure write. Plan bodies are never
   persisted in the spec, so this unconditional redraft is the single documented path, not a
   size-triggered fallback; never expose an adopted child without its
   canonical plan artifact. Then save each child's contract description with
   `epicChildMarker(key)` embedded in that same write — canonical emitter only, never hand-written —
   but **not** `agent-friendly` yet (deferred exposure, step 6), in `topoOrderChildren` order,
   recording each new id against its `key`; later description saves must preserve that marker
   byte-for-byte. For `tracker-attachment`, now
   prepare, PUT, finalize **and read back** that child's attachment (`references/plan-storage.md`;
   use the parent epic id as the signed-header scratch prefix)
   step 5), and only then move its shell to the planned state — otherwise an unwritten child plan
   reaches the planned flip and a consumer selects that child on a row whose bytes do not exist. Any
   attachment **or read-back** failure takes the SAFE branch before that child's planned-state or
   label exposure, and **aborts the epic** rather than skipping that child: siblings already created
   stay unexposed and are adopted on resume. A non-`agent-friendly` child is
   not `boss-build`-selectable, so it cannot be picked up before its blockers exist.

5. **Wire the intra-epic DAG.** Execute `epicWiringPlan(spec, createdIdByKey)`: set each child's
   intra-epic `blockedBy` (append-only). These edges are internal to the epic — the children were all
   just created together and, on abort, stay unexposed together — so wiring them before the parent
   commit is safe. **Defer the Phase 4 step-5 external conflict links to step 6, after the parent
   overview commits.** Those outward edges mutate **non-epic** backlog tickets (a lower-priority active
   ticket saved `blockedBy` a child); writing them here — before the step-6 parent gate — would strand
   that backlog work behind a child that a **deterministic** parent-gate failure leaves permanently
   unexposed/unbuildable. Intra-epic edges come **exclusively** from `epicWiringPlan`.
6. **Commit the parent overview, THEN link external conflicts + expose the children (deferred exposure
   — makes an agent-friendly child `boss-build`-eligible).** Only now, after the intra-epic DAG wiring
   (step 5; the external links are deferred to here), **commit the parent overview before any external
   edge is written or any child is exposed:** after the children are created and moved to planned,
   re-assert the parent's configured unplanned state before composing the parent overview.
   Linear's sub-issue rollup can advance the parent on its own; without this re-assertion,
   parent-repurpose-last crash recovery silently stops working because the unplanned sweep can no
   longer find a partial epic. Then compose the parent overview, run step 7's
   three gates (secret + image-parity + plan-contract with `--mode epic-parent`), then attach it natively — **reading the finalized parent
   overview back before the save** (`references/plan-storage.md` step 5; deleting a
   confirmed-unreadable overview strands no spec store, which lives in its own attachment) — and
   save it onto the still-unplanned parent** (description-only; an **attachment-sourced** spec lives outside the description, so this
   description-replacing save cannot lose it — the old re-append-the-marker requirement is obsolete
   there, not dropped by accident. **A LEGACY-sourced resume is the exception:** that parent's spec
   _is_ the inline `<!-- boss-plan-epic-spec:… -->` marker, so carry that marker substring
   **verbatim** into the composed overview — this save would otherwise destroy the only store and
   leave the parent with neither, which the next sweep re-decomposes into DUPLICATE children. Carry
   it; never migrate it to an attachment instead — this phase only ever sweeps unplanned tickets, so
   a self-heal-on-read path would almost never fire and is not worth the second write. The unplanned → planned flip stays last — step 7). On a gate **or** attachment/read-back/save failure take the SAFE
   branch — **no external links, no exposure, no planned flip, abort**. All three epic upload sites —
   the stage-2 spec, each child plan in step 4, and this parent overview — are read back **before**
   step 7's planned flip, so the flip never exposes an epic standing on bytes never written. Because the failure-prone
   plan-store + Linear parent save happen **here, before any external edge or exposure**, a
   **deterministic** parent-gate failure never leaves a child `agent-friendly`/buildable, nor a non-epic
   backlog ticket blocked behind an unbuildable child, while the parent aborts unplanned; an exposed
   child is always backed by an already-finalized parent overview, never one that later aborts
   unplanned. Only after the parent overview is saved, **run the Phase 4 step-5 external conflict
   links** for each child against the active planned/in-progress/in-review backlog — but **exclude
   this epic's own child ids AND the epic parent id** from that comparison/backlog set (the siblings
   were just created in planned, so without this exclusion the "external" pass would add extra
   priority-oriented `blockedBy` edges between siblings on top of the intended intra-epic DAG, corrupting
   the decomposition order; the external linker only links each child against **non-epic** backlog
   tickets). Deferring these outward edges to here — past the parent commit — means a deterministic
   parent-gate abort writes **zero** external edges, so existing backlog work is never stranded behind a
   child that never becomes buildable. Then stamp each child with **its own
   plan's agent-friendliness call** (union, never clobber): a child whose plan concluded agent-friendly
   gets `agent-friendly`; a child whose plan concluded it **needs a human** (`agentFriendly: false`)
   gets `needs-human` instead — **never** `agent-friendly` — per the normal plan-contract convention.
   `boss-epic` treats a child as eligible only when it is planned **and** `agent-friendly` **and** has a
   plan artifact **and** is **not** `needs-human`, so honoring the per-child decision here keeps a
   human-blocked child from being handed to `boss-build`. By now every child already carries its blocker
   relations, so `boss-build`'s "skip a candidate whose blocker relations already exist" keeps blocked
   children from starting out of DAG order while an agent-friendly root child (no blockers) is correctly
   buildable. **Crash-safety:** any crash before this step leaves the children unexposed (no
   `agent-friendly`/`needs-human`, unbuildable), so a `boss-build` cron cannot pick a downstream child
   during the create→wire window; resume completes wiring **and** this exposure. **On resume the
   per-child call comes from the recovered spec:** an already-created-but-unexposed child adopted from
   the parent's spec has no `.linear-plans/` plan to re-read, so its persisted `agentFriendly` (step 4)
   is the authoritative source for whether resume stamps it `agent-friendly` or `needs-human`.
7. **Repurpose the parent (original-becomes-parent).** The epic overview (goal + child checklist with
   plan artifacts + verbatim `## Original notes`) was **composed, gated, stored + saved onto the
   still-unplanned parent in step 6**; the spec attachment is untouched by that description-replacing
   save (and a legacy parent's inline marker was carried through it verbatim), so idempotent resume
   still recovers the FULL original spec from a fully-built parent instead
   of re-decomposing into DUPLICATE children. The
   parent overview embeds the original description verbatim, so its step-6 gates are the **same three
   Phase 4 STOP gates**: the **secret gate** (read the composed parent overview; redact
   any credentials/PII before attachment finalization) and the **image-parity gate** (`$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs` —
   confirm every image URL in the ORIGINAL ticket description survives verbatim in the parent
   overview's `## Original notes`; on a drop take the SAFE branch — **no parent write**, abort), and
   the **plan-contract gate** (`$BOSS_PLAN_TOOLBOX/plan-contract-guard.mjs --mode epic-parent` —
   validate the parent overview's `## Summary`, `## Child tickets`, `## Planning`, `## Original notes`
   contract before attachment finalize or parent save; on a violation take the same SAFE branch —
   **no parent write**, abort). **These
   gates run in step 6, BEFORE child exposure**, so a deterministic parent-gate failure aborts with the
   children still unexposed/unbuildable. So step 7 is the final unplanned → planned
   flip (the last write), the overview already saved above.
   **Parent estimate, priority, and label:** resolve `EPIC_LABEL` through
   `labelName(config, 'epic')`, then union that result into the parent labels. The final flip writes
   `estimate = epicParentEstimate(spec)` and `priority = parent.priority`. The sum can be non-Fibonacci:
   if `estimate` is genuinely rejected, retry without `estimate` and warn, matching Phase 4. Do not
   rely on rejection alone: Linear was observed accepting a non-Fibonacci `15`, silently clamping it
   to `8`, and returning success. After the final flip, re-read the parent and compare its stored
   estimate with `epicParentEstimate(spec)`; warn on any difference.
   **Parent-label exception:** the parent carries
   **neither** `agent-friendly` **nor** `needs-human` (it is a `boss-epic` container, not a
   `boss-build` target); each **child** carries exactly one of `agent-friendly`/`needs-human`
   (**per its own plan's agent-friendliness call**), but **applied only after wiring** (step 6 deferred
   exposure), never at child-create time. **Strip stale build metadata as part of this flip**, for the step-4
   stage-1 reason (a state-only flip would leave an already-planned parent `boss-build`-selectable,
   defeating the parent-label exception): **remove any pre-existing
   `agent-friendly`/`needs-human` label from the parent and drop any stale single-ticket
   `Implementation plan (…)` link. For `tracker-attachment`, read the parent attachments and invoke
   `deletePlanAttachment` on exactly the ids
   `stalePlanAttachmentSweep(attachments, {keepAttachmentId: <the parent-overview attachment id>})` returns,
   before the final flip; **require that optional
   operation before the FIRST epic write** — not at stage 3, which is skipped on every resume, so
   gating on it there would defer the check to this final flip, i.e. until after children are created,
   wired and exposed `agent-friendly`. On an adapter lacking the op that ordering strands buildable
   children under an unplanned parent; checking up front takes the SAFE branch with nothing written. That helper's predicate is prefix-scoped and
   nothing else, deliberately: an unscoped sweep would silently destroy the `Epic spec (…)`
   attachment on the final flip. The parent overview attachment finalized in step 6 is
   retained as the epic artifact.**

**Guards (load-bearing — the trigger bar is low + headless auto-creates):** per-child estimate ceiling
`CHILD_MAX_ESTIMATE = 3` (a `5`/`8` child is rejected ⇒ decompose further; the producer-before-consumer
soft check `validateLayering` warns on a read/ui child not gated by its producer), child-count cap
`EPIC_MAX_CHILDREN = 12` (over ⇒ **`needs-human`**, **never** a single oversized ticket — the exact
monolith this avoids), minimum `EPIC_MIN_CHILDREN = 2`
(under ⇒ one ticket), **recursion guard** (`allowEpic: false`, no child recursion), **cycle safety**
(`assertAcyclic` rejects any `blockedByKeys` cycle before writes), **validate-before-write** (zero
Linear writes on any spec/gate failure), **parent-repurpose-last** (the write-atomicity guard:
children are created + wired **before** step 7 moves the parent unplanned → planned, so a crash or
malformed sentinel mid-create leaves the original ticket unplanned and the next sweep re-picks and
resumes it — a partial epic is never stranded), and **idempotent resume** (durable — survives a fresh
cron worktree where the `.linear-plans/` scratch is gone): first `get_issue` the parent and
decode the returned body first with `node "$BOSS_PLAN_TOOLBOX/plan-attachment.mjs" decode <in-file> <out-file>`, then
`parseEpicSpec` the decoded **body of its `Epic spec (<ISSUE-ID>)` attachment** (two or more
`Epic spec (…)` attachments ⇒ **abort loudly**, never guess) to recover the **FULL original
spec** (parent overview + every child's full metadata) — step 4 stage 2 wrote it before any child and
no later description save touches it, so it survives even a fully-built epic. **Then
`validateSpecIdentity(spec, <ISSUE-ID>)` it** — attachment-sourced specs only, here too and not just on
the named-source branch above: the attachment was selected by title, and title alone is not identity.
A failure takes the unreadable-spec recovery gate, never a silent resume against another epic's spec. **Legacy store (the one
description read that remains):** a parent written by an earlier build carries the spec inline as a
`<!-- boss-plan-epic-spec:… -->` description marker instead, and `parseEpicSpec` falls back to that
form, so such an epic is still recognised and recovered. Then enumerate the already-created children with `list_issues parentId=<parentId> limit=250`
(the op `boss-epic` uses; `get_issue` on the parent does not return child ids), hydrate each child
with `get_issue` so markers hidden by list-description truncation are present, then join them against
the spec with `reconcileEpicChildren(spec, hydratedLiveChildren)` — never by eye, never
by title — which matches each live child's `epicChildMarker(key)` marker to `spec.children[].key` and
reports `{adopted, missing, orphans, unmarked, repairs, errors}`. **Aligned** (no orphans): create
exactly what `missing` names. **Unambiguous rename** (`repairs` holds one `{specKey, liveKey, id}`):
adopt that child and rewrite **its own** description marker to `epicChildMarker(specKey)` — replacing
only the marker substring and **preserving the rest of that description's bytes verbatim**, since the
save replaces the description and would otherwise wipe the child's gated plan body; repair the
child, never the spec key, because `specKey` is the namespace `adopted` reports under and the one every
sibling's `blockedByKeys` and `epicWiringPlan` resolve through, so re-pointing the spec at `liveKey`
would strand those refs and throw mid-wire, after children already exist — create nothing for it. **Ambiguous
drift** (`ok:false` — multiple orphans, an unmarked child, duplicate live keys, or a non-array input):
take the SAFE branch — report `errors`, write nothing, create nothing, never guess; a refusal must never
be read as "no children exist" (that would duplicate the whole epic). Create only the spec keys
`missing` names —
**drafting each missing child from its persisted metadata and wiring it per the persisted
`blockedByKeys`, never a fresh re-decomposition** — then finish wiring + parent repurpose.
**Already-saved parent overview:** because step 6 saves the parent overview description-only while the
parent stays unplanned (the planned flip is step 7), a crash in that window re-picks an unplanned
parent whose description is **already the composed overview** (`## Original notes` + child checklist),
not the reporter's raw notes; on resume **detect this and reuse the saved overview verbatim** —
never recompose `## Original notes` from the transformed description (which would nest the overview or
trip image parity) — then **run the deferred step-6 external conflict links BEFORE stamping any child
buildable** (a crash could have landed after the parent save but before that pass, so the normal-flow
ordering — parent commit → external links → exposure — must hold on resume too, else an agent-friendly
root child is exposed without blocking overlapping active backlog work; the links are append-only, so
re-running is a safe no-op for edges already written) and finally finish the missing child exposure + the
unplanned → planned flip. A re-run
**adopts** existing children and completes only what is missing from the original spec; on a
fully-built epic it is a clean no-op (never duplicates), even from a fresh worktree.

## Phase 3 — Plan requirements (shared drafting spec)

Interactive or headless drafting (per `references/headless-drafting-brief.md`) produces
`.linear-plans/<ISSUE-ID>-<slug>.md` (gitignored; slug = issue id + hyphenated title; compute with
`node -e 'import(require("node:url").pathToFileURL(process.argv[3]+"/plan-slug.mjs").href).then(m=>console.log(m.issueSlug(process.argv[1],process.argv[2])))' <ISSUE-ID> "<title>" "${BOSS_PLAN_TOOLBOX:?}"` after running the toolbox preamble first).
The **full drafting spec** — body requirements and fill-in description-summary template — lives once
in **`references/headless-drafting-brief.md` § "Step 5"/"Step 7"**; both modes follow it.

The description-section contract governs `descriptionSummary` only. The plan attachment may retain
additional drafting-layer headings and its native structure. `## Original notes` remains the
terminal heading and its body runs to EOF. Every query-bearing upload URL anywhere in the plan file
must be query-stripped.

The producer-side plan-file floor is separate from the description contract: a single-ticket plan
file must include every required description-contract heading, plus the configured plan-file
headings `## Problem Frame`, `## Requirements`, and `## Implementation Units`, with at least one
heading outside `planContract.sections`. This gate reads only the plan attachment body supplied to
`plan-contract-guard.mjs`, and no consumer-side path requires this structure. Epic-parent overviews
and adopted-child redrafts are explicit exemptions; consumers use the description contract and plan
residue checks only.

The orchestrator keeps the versioned description section contract consumed by boss-build and
bs-sweep-plan: `descriptionSummary` MUST carry these `##` sections in order (`## Why this needs a
human` and `## Open Questions` conditional; all others always present), and stamps
`- Contract: v<N>` under `## Planning` (v1 today):

`## Summary` · `## Approach` · `## Key changes` · `## Testing` · `## Risks / unknowns` ·
`## Premises` · `## Acceptance criteria` · `## Required proof` · `## Why this needs a human` ·
`## Open Questions` · `## Planning` · `## Original notes`

Within `descriptionSummary`, `## Premises` and `## Proof harness analysis` are optional. Any other
description heading is off-contract: drop it, or register it in `planContract.sections`. The programmatic check is
**`validatePlanDescription(config, description)`** (`$BOSS_PLAN_TOOLBOX/skill-config.mjs`). The
epic-parent overview uses explicit `validatePlanDescription(config, description, {mode:'epic-parent'})`
with `## Summary`, `## Child tickets`, `## Planning`, and `## Original notes`. Unknown modes warn and fall back to child-plan.

When a ticket names a specific call site, construct, literal claim, or other mechanism that could
recur, record a repo-wide sibling-class enumeration before fixing scope. List every
site the search returns with verdict (`fix` or `not a defect`) and reason; adjudicate the class per
site rather than sweeping every match wholesale. The reason names the discriminator, such as where
the branch actually lives. A one-row "only named site found" table discharges it. An acceptance
criterion must not cap the number of changed files; scope comes from enumeration, not file count.

**config-first** order; the natural-reading `(description, config)` call throws a named
argument-order error. It returns `{ ok, version, missing, unknown, unsupportedVersion }`; `ok` covers
only `missing`/`unsupportedVersion`, and `unknown` is enforced by the Phase 4 contract gate.

**Headless open questions → `agent-question`.** The subagent records only genuinely **controversial**
forks (high bar — could-have-gone-either-way calls, never routine ones) as `openQuestions`; a
non-empty list drives the `agent-question` label (Phase 4) and the plan's `## Open Questions`
section. Interactive runs have a human answering each fork, so they produce none.

## Phase 3.5 — Extension plan-reviewers (additive, non-fatal)

Before upload, run any repo-local `boss-plan-*` **extension** plan-reviewers
(`discover --core boss-plan --role plan-reviewer`) over the drafted plan — strictly additive, a
documented no-op when none are installed (the default today), in both the interactive and headless
paths. Pass that exact role: inferring `--role review` from this phase's name rejects every
correctly-installed extension into `skipped` as `unknown requested role "review"`, which now reads
as a misinstallation in the ledger. Full protocol (discover → dispatch each as a fresh read-only
subagent → validate its envelope → fold or skip), against
[`docs/skills/extension-contract.md`](../../../docs/skills/extension-contract.md), lives in
[`references/extension-reviewers.md`](references/extension-reviewers.md).

## Phase 4 — Finalize the plan attachment and write back to the tracker

> **STOP — secret gate (mandatory, do not skip).** This runs before finalizing the native tracker
> attachment. Read the entire plan file (with special attention to the `## Original notes` verbatim
> block and anything pasted from the ticket or interview) and confirm it contains **zero** of: API
> keys, tokens, passwords, connection strings, private keys, session cookies, internal
> hostnames/IPs, or customer PII. If you find anything credential- or PII-shaped, **redact it in
> every persisted artifact with `[REDACTED]` or `[REDACTED: reference]`** (e.g. `[REDACTED: repo-root
.env]`) before attaching it. If you are unsure whether something
> is sensitive, treat it as sensitive and redact it. Do not finalize the attachment until this
> check passes. For credential-valued external-image query parameters, use `token=REDACTED`,
> `token=[REDACTED]`, or `token=[REDACTED:%20vault]`; these preserve the image
> reference without persisting its credential. The redacted source is the safe form used by the verbatim attachment checks below;
> the raw Phase 1 source is retained only in the ephemeral image-parity scratch file.
> A signed `uploads.linear.app` URL is an explicit carve-out: do **not** redact the reference away.
> Strip its signature query string and preserve the unsigned asset path instead, so the image-parity
> gate can retain the asset identity without carrying a credential-like signature.

> **STOP — image-parity gate (mandatory, mechanical, do not skip).** A rewritten description that
> silently drops the reporter's screenshots is "worse than none" (the Phase 0 edge rule), and the
> drafting LLM cannot be trusted to preserve them — so verify parity **mechanically** before any
> Linear write. Reuse the raw snapshot Phase 2 already wrote at
> `.linear-plans/<ISSUE-ID>.image-guard-orig.md`; do not rewrite it here. An **empty** or
> whitespace-only original is refused (exit 1); pass `--allow-empty-original` only if it truly is
> empty. Write the returned `descriptionSummary` to `.linear-plans/<ISSUE-ID>.image-guard-new.md`
> (per-issue paths avoid
> clobbering). Also write `.linear-plans/<ISSUE-ID>.attachment-guard-orig.md` as the same Phase 1
> source with **only** the mandatory secret/PII redactions and upload-signature stripping applied;
> do not derive it from either generated artifact. Both the returned `descriptionSummary` and the
> final attachment must preserve this safe source under `## Original notes`. Set `EXPECTED_IMAGES`
> to the number of distinct canonical upload identities observed in the Phase 1 description — the
> `uploads.linear.app` origin plus pathname, ignoring query strings — then run the guard:
>
> ```bash
> BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
> ORIG=".linear-plans/<ISSUE-ID>.image-guard-orig.md"; SAFE_ORIG=".linear-plans/<ISSUE-ID>.attachment-guard-orig.md"; NEW=".linear-plans/<ISSUE-ID>.image-guard-new.md"
> PLAN_FILE="${PLAN_FILE:-.linear-plans/<ISSUE-ID>-<slug>.md}"
> PLAN_REJECTED="$PLAN_FILE.rejected"
> EXPECTED_IMAGES="<distinct canonical upload identities observed in Phase 1>"
> cleanup_guard_scratch() {
>   rm -f "$ORIG" "$SAFE_ORIG" "$NEW" "$PLAN_FILE" "$PLAN_REJECTED" || echo "warning: guard scratch cleanup failed" >&2
> }
> # Keep scratch until all gates pass; every failing gate calls this helper before exiting.
> if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$ORIG" --rewritten "$NEW" \
>   --expect-images "$EXPECTED_IMAGES" --require-unsigned-uploads; then
>   echo "image-parity gate failed (guard message above) — no Linear write, aborting" >&2
>   cleanup_guard_scratch
>   exit 1
> fi
> if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$ORIG" --rewritten "$SAFE_ORIG" \
>   --require-safe-source; then
>   echo "safe-source gate failed (guard message above) — no Linear write, aborting" >&2
>   cleanup_guard_scratch
>   exit 1
> fi
> if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$SAFE_ORIG" --rewritten "$NEW" \
>   --require-verbatim --require-unsigned-uploads; then
>   echo "description safety gate failed (guard message above) — no Linear write, aborting" >&2
>   cleanup_guard_scratch
>   exit 1
> fi
> if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$SAFE_ORIG" --rewritten "$PLAN_FILE" \
>   --require-verbatim --require-unsigned-uploads; then
>   echo "plan-attachment safety gate failed (guard message above) — no attachment finalize, aborting" >&2
>   cleanup_guard_scratch
>   exit 1
> fi
> ```
>
> Phase 5 removes these files after success. A failed gate instead calls `cleanup_guard_scratch`
> before its non-zero exit, including the raw Phase 1 source which may contain sensitive content.
> `--require-verbatim` makes the tracker write reject rewritten Markdown in `## Original notes`, and
> `--require-safe-source` permits only image normalization and explicit redaction markers, never dropped prose.
> `--require-unsigned-uploads` rejects any query-bearing upload URL before it can persist a signature.
>
> On non-zero exit take the **SAFE branch** — identical to the dispatch-failure branch: **no Linear
> write**, a one-line stderr reason carrying the guard's own message (it prints each), discard the
> scratch (Phase 5 cleanup), and exit non-zero. The guard reuses the in-hand original description and
> returned `descriptionSummary`, so it adds no new Linear read.

> **STOP — plan-contract gate (mandatory, mechanical, do not skip).** "Exactly these
> `descriptionSummary` `##` sections, in order" was until now enforced only by the **consumer**, days after a malformed artifact had
> already been published: descriptions missing most required sections, a whole-field self-describing
> placeholder, an unsubstituted `<ATTACHMENT-ID>`-style token, an off-contract heading, and a plan
> file ending in literal tool-call scaffolding all passed every earlier gate. Verify
> **mechanically**, reusing the in-hand `descriptionSummary` and `PLAN_FILE` — **zero** extra tracker
> reads. Re-derive the toolbox dir here; blocks inherit nothing:
>
> ```bash
> BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
> ORIG=".linear-plans/<ISSUE-ID>.image-guard-orig.md"; SAFE_ORIG=".linear-plans/<ISSUE-ID>.attachment-guard-orig.md"; NEW=".linear-plans/<ISSUE-ID>.image-guard-new.md"
> PLAN_FILE="${PLAN_FILE:-.linear-plans/<ISSUE-ID>-<slug>.md}"
> PLAN_REJECTED="$PLAN_FILE.rejected"
> if CONTRACT_REPORT="$(node "$BOSS_PLAN_TOOLBOX/plan-contract-guard.mjs" --description "$NEW" --plan "$PLAN_FILE" 2>&1)"; then
>   :
> else
>   printf '%s\n' "$CONTRACT_REPORT" >&2
>   if printf '%s\n' "$CONTRACT_REPORT" | grep -q '\[plan-file-structure\]'; then
>     cp "$PLAN_FILE" "$PLAN_REJECTED" || echo "warning: failed to retain rejected plan structure artifact" >&2
>   fi
>   echo "plan-contract gate failed (guard message above) — no Linear write, aborting" >&2
>   rm -f "$ORIG" "$SAFE_ORIG" "$NEW" "$PLAN_FILE" || echo "warning: contract gate scratch cleanup failed" >&2
>   exit 1
> fi
> ```
>
> This is a **fifth** failed-gate exit, so it owes the same cleanup as the four above and removes all
> four ordinary scratch paths — `$ORIG` included, the raw Phase 1 source that may carry sensitive content.
> On a `plan-file-structure` violation only, the structure gate also retains the secret-checked plan
> bytes at `$PLAN_FILE.rejected` for triage; every normal cleanup path enumerates that retained path.
> "Discard the scratch (Phase 5 cleanup)" describes the SUCCESS path only: `exit 1` means Phase 5
> never runs, which is exactly why each failing gate deletes the scratch itself.
>
> One stderr line per violation, each tagged `missing-sections`, `unknown-section`, `section-order`,
> `placeholder-residue`, `not-a-description`, `plan-file-residue`, `plan-file-structure`, or
> `unreadable-input`; a missing
> or unreadable file is itself a violation, never a pass, and an `unknown-section` message names both
> the heading and its remedy. On non-zero exit take the **SAFE branch**: **no Linear write**, no
> attachment finalize, a one-line stderr reason carrying the guard's own message, discard the scratch
> as above, exit non-zero.

> **STOP — premise re-verification (mandatory, mechanical, do not skip).** The plan artifact and
> description may still be valid while the tracker premises the drafter relied on have moved. The
> drafting sentinel carries `premises: [{id, state}]`; immediately before the single tracker save,
> re-read those issue ids through the tracker adapter's `getIssue` capability, build a JSON object of
> live states, and run:
>
> ```bash
> BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
> PREMISES_FILE=".linear-plans/<ISSUE-ID>.premises.json"; LIVE_STATES_FILE=".linear-plans/<ISSUE-ID>.premise-states.json"
> PREMISE_REPORT="$(node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" premises "$PREMISES_FILE" "$LIVE_STATES_FILE" 2>&1)"
> PREMISE_RC=$?
> ```
>
> An empty `premises` array skips the reads. A `premise-limit` or unreadable premise is a SAFE
> branch before tracker writeback. A changed state does **not** abort: append an orchestrator-owned
> `- Premise drift: <ticket> was <state at recon>, is now <current state>` line parsed from
> `PREMISE_REPORT` under `## Planning` before the save, and name that annotation in the Phase 6
> report. This warning line is outside the description-section contract; `planContract.version` stays
> unchanged.

1. Finalize the native tracker attachment before tracker writeback (failure: no plan metadata/state write). Follow
   [`references/plan-storage.md`](references/plan-storage.md). Set
   `PLAN_FILE="${PLAN_FILE:-.linear-plans/<ISSUE-ID>-<slug>.md}"` and `TRACKER` before write-back.
   After `references/plan-storage.md` returns success, assert exactly one
   `Implementation plan (<ISSUE-ID>)` attachment remains; more than one exact-title attachment takes
   the SAFE branch: no plan metadata/state write, stderr naming duplicate ids, non-zero exit.

2. Read labels with op `readLabels` so you can **merge** (Linear `save_issue labels` replaces the
   set — never clobber existing labels like `feature`/`docs`). Compute the written label set as
   existing ∪ additions ∖ strips.
3. Decide the metadata (headless: derive from the subagent's returned metadata; interactive: from
   the interview):
   - **description**: the composed description-summary block (headless: the returned
     `descriptionSummary`, verbatim; interactive: composed per the drafting spec in
     `references/headless-drafting-brief.md` § "Step 7", matching the Phase 3 section contract).
   - **labels**: union of existing labels + relevant ones (`bug`/`feature`/`improvement`/`docs`) minus `stripLabels`. **Agent-friendly is the default:** add **`agent-friendly`** to every plan **unless** an autonomous agent genuinely could not complete the task (headless: `agentFriendly == false`) — in that case add **`needs-human`** **instead** (never both) and ensure the plan body carries the **## Why this needs a human** section (see Phase 3). Complexity alone is not a reason for `needs-human` — a large but well-specced ticket is still `agent-friendly`. Add **`agent-question`** (headless only) **if and only if** ≥1 open question was recorded (`openQuestions` non-empty); union it in, never clobber — it is independent of the agent-friendly/needs-human call. Set `stripLabels` to `agent-plan` on a successful plan. When there are no open questions, do not add `agent-question`; when it is already present, do not strip it.
   - **estimate** (Fibonacci): `0` trivial/minimal · `1`/`2`/`3` well-defined single-PR ticket, clear path · `5`/`8` too big for one PR ⇒ **triage EPIC** (Phase 2.5), never a single-ticket estimate (sole exception: a genuinely atomic, un-splittable `5` with a recorded `- Atomic-5:` justification under `## Planning`). Every planned ticket gets a non-null estimate.
   - **priority** (`1-4`): honor a reporter-set priority. Otherwise rank against the current config-resolved planned (`stateName(config, 'planned')`) backlog, considering urgency, simplicity, positive/business impact, and security (security concerns bias toward Urgent/High). A planned ticket should not stay `0=None`.
4. Single tracker save op (ops `moveState`/`setPriorityEstimate`; Linear uses `save_issue`) updating the issue by
   `id`:
   - `description`: the summary block above.
   - no plan link: the finalized attachment is the canonical plan.
   - `labels`: the merged-minus-stripped set (names).
   - `estimate`: the Fibonacci number.
   - `priority`: the chosen `1-4`.
   - `state`: the planned state.

   If the tracker save rejects `estimate` (Linear needs Fibonacci enabled), retry without
   `estimate`, complete the rest, and warn the user.

5. **Link conflicting dependencies (library-decided, cycle-safe).**
   Every decision comes from `$BOSS_PLAN_TOOLBOX/plan-deps-lib.mjs`; this step is I/O only. Never
   re-decide an edge in prose.

   a. **Fetch.** Op `selectPlanned` (planned — `trackerConfigFor(config).team`, `limit=250` — then
   in-progress and in-review) with an **explicit field list**: `description, labels, priority,
createdAt` plus the adapter's workflow-state/status fields (`stateName`/`stateType`,
   `state.{name,type}`, `status`/`statusType`, or equivalent). The default field set omits those, and an all-empty-description run returns
   zero links with no error, indistinguishable from a clean result. Prefilter on title + labels
   before reading 250 descriptions — but that prefilter is a **context-scale measure only, never an
   overlap decision**. Keep it inclusive: a candidate whose title and labels look unrelated can still
   list your files under `## Key changes`, and dropping it here is the missed-prerequisite defect
   re-entering through the filter instead of through fuzzy search. When a candidate is arguable, read
   its description and let the library decide. Then read this ticket's declared relations (op
   `getIssue` with relations) and fetch each related id **by id, regardless of state** — `selectPlanned` never
   returns a cleared ticket, so that is the only path by which a completed or canceled prerequisite
   is considered at all.

   b. **Judge logical dependency** per candidate (does either ticket need the other's feature? the
   one call no function can make) and pass it as `logicalDependencies[<candidate id>] = {direction,
note}`. **Direction is part of the verdict**, not something the library re-derives: `blockedBy`
   (the default, and what a bare `true` means — the candidate is the prerequisite THIS ticket needs)
   or `blocks` (the candidate needs this ticket). A logical basis is oriented by that verdict alone,
   because priority and creation order know nothing about it — so a mis-stated direction writes a
   real inverted edge, and an omitted one asserts the candidate is the prerequisite. Overlap is
   computed for you by `extractKeyChangeAreas` + `areasOverlap`: tracker full-text search is **fuzzy
   and must never decide overlap** — the oracle is each candidate's `## Key changes` section, or its
   whole description when it has none.

   c. **Classify once.** Build `subject`, `candidates`, `declaredRelatedIds`, `logicalDependencies`,
   `epicLabel` (`labelName(config, 'epic')`), `moduleRoots` and `stateRoles` (`stateRolesFor(config)`).
   Include `epicParentId` on the subject and every candidate when exposed: the library keeps epic
   parents/siblings on the planning-note path instead of adding external dependency edges. `moduleRoots` is this
   repo's top-level module/package directory names: area extraction drops every slash-free token
   without it, so a plan whose `## Key changes` names bare module names contributes no areas and its
   overlaps are missed in silence — the missed-prerequisite defect re-entering through the glue.
   `subject` needs the SAME fields as a candidate, including workflow state/status: it is blocked on
   inbound edges and blocks outbound ones, so missing state downgrades **every** edge rather than
   some. `stateRolesFor(config)` returns that role map. Omitting it is SILENT, not loud: every state
   resolves to unknown, every blocking edge downgrades to `relatedTo` under an `info` note, and a
   run that linked nothing reads exactly like one that found nothing to link. **Write that payload
   to a scratch JSON file yourself** and name it below — the block does not create it, because an
   empty `mktemp` file parses as nothing and throws.

   ```bash
   BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   DEPS_IN="<the scratch JSON file you just wrote>"
   node -e 'const u=require("node:url"),T=process.env.BOSS_PLAN_TOOLBOX,M=p=>import(u.pathToFileURL(T+p).href);Promise.all([M("/skill-config.mjs"),M("/plan-deps-lib.mjs")]).then(([c,d])=>{const g=c.loadSkillConfig({cwd:process.cwd()}),i=JSON.parse(require("node:fs").readFileSync(process.argv[1],"utf8")),a=x=>d.extractKeyChangeAreas(g,x.description,{moduleRoots:i.moduleRoots||[]}).areas;i.stateRoles=i.stateRoles||c.stateRolesFor(g);i.subjectAreas=a(i.subject);i.candidates=i.candidates.map(x=>({...x,areas:a(x)}));console.log(JSON.stringify(d.planDependencyEdges(i)))}).catch(e=>{process.stderr.write("boss-plan deps: "+(e&&e.message||e)+"\n");process.exitCode=1})' "$DEPS_IN"
   rm -f "$DEPS_IN"
   ```

   d. **Act on `{ edges, skipped, notes, questions, compared }`** — imperative branches, no implicit
   fallthrough:
   - edge with `write` non-null → save exactly that `{id, blockedBy}` with op `appendDependency`.
     Relations are **append-only**: only add, never clobber; v1 does not auto-prune.
   - edge with `edge: 'relatedTo'` → save it with op `appendRelatedTo`. Best-effort on BOTH branches,
     neither a stop condition: if the adapter does not declare `appendRelatedTo` (it is optional),
     record the relation as a `## Planning` note instead; if a declared one fails, log the reason and
     continue.
   - `notes[]` → `destination: 'planning'` under `## Planning`, `'risks'` under `## Risks / unknowns`.
     Never drop a `severity: 'warning'` note.
   - `skipped[]` `reason: 'same-epic-member'` → record the planning note; do not add an external
     edge. Intra-epic ordering is owned by the epic DAG.
   - `questions[]` → record under `## Open Questions` and add `agent-question`. Headless never asks;
     interactive mode may ask via AskUserQuestion.
   - `skipped[]` `expandChildren: true` → an epic parent, which never produces a PR of its own: fetch
     its active children and re-run (c) with those as candidates. Re-run at most twice, and add every
     parent id you have already expanded to `excludeIds`: the library's depth cap bounds ONE call, so
     a re-run loop that resets it walks a malformed parent/child graph forever.
   - `skipped[]` `reason: 'declared-related-unresolved'` → fetch that id and re-run (c), or record an
     Open Question. Never drop it silently.
   - `compared === 0` → nothing was evaluated. Report _could not evaluate_, never _no dependencies_.

   e. **Cycle safety — after (d)'s downgrade, over blocking writes only.** For each surviving
   `write`, op `getIssue` with relations on both ids; skip that write when the opposite relation
   already exists (a 2-cycle) or the proposed blocker is already blocked by the proposed blocked
   ticket. `relatedTo` is symmetric and non-blocking and cannot form a cycle — never gate it here,
   and never run this ahead of (d): a 2-cycle check before the downgrade skips the pair outright and
   silently suppresses the `relatedTo` edge and note the downgrade would have produced.

   e2. **Transitive-block warning** — only where a surviving `write` puts THIS ticket on the blocked
   side. Reuse (e)'s relations read on the blocker to inspect its own inverse `blocks` relations, and
   treat a blocker's blocker as **still blocking** unless its state type is cleared or canceled — the
   `DEFAULT_CLEARED_STATE_TYPES` / `DEFAULT_CANCELED_STATE_TYPES` rule in
   `toolbox/plan-deps-lib.mjs`, the single source of that definition (so prose and gate never
   diverge). If that payload lacks a nested blocker's own state, fetch it by id. When the blocker is
   itself open **AND** has ≥1 uncleared blocker, record a Transitive-block warning naming it and the
   immediate open ticket(s) blocking it. Detection only — never auto-prune, never via AskUserQuestion.

   f. Record what step 5 found — **whenever (d) produced ≥1 relation, note, or question**; skip only
   when it produced none of the three. A zero-relation run is not a quiet run: an arealess subject, an
   unresolved declared relation, an ambiguous orientation and a canceled prerequisite each write no
   edge and each raise a warning or a question, so gating this save on the relations alone throws away
   exactly the outcomes the plan's reader most needs — and `agent-question` never reaches the ticket.
   Step 4 saved the description first, so send a second tracker save with `id` + `description`
   (adding `labels` only to carry `agent-question`, when (d) produced a question — union it into the
   set Step 4 saved, because `labels` **replaces** the whole set; this is the run's last save, so a
   label deferred past it is a label never applied): re-send Step 4's
   description, including (d)'s notes and questions under the sections (d) named, plus — only when ≥1
   relation was written — `- Dependencies: blocks <BLOCKED-ID>; blocked by <BLOCKER-ID>` under
   `## Planning`. When (e2) found ≥1 warning, add a sibling conditional line next to
   `- Dependencies:` (omit it otherwise, mirroring how `- Dependencies:` is conditional):
   `- Transitive-block warning: blocked by <BLOCKER-ID>, which is itself open and blocked by <UPSTREAM-BLOCKER-ID>`. This
   line is orchestrator-owned — keep it out of the drafting subagent's returned template. This keeps
   Step 4's other fields and (d)'s relations intact.

## Phase 5 — Discard local artifacts

The plan now lives in the tracker attachment from Phase 4. Remove every local file this run created so
the worktree is left clean:

```bash
rm -f ".linear-plans/<ISSUE-ID>-<slug>.md"
rm -f ".linear-plans/<ISSUE-ID>-<slug>.md.rejected"
# `references/plan-storage.md` records this exact private path for every PUT. The
# normal path deletes it immediately after the PUT; repeat the removal here so a
# prepare/PUT/finalize abort cannot strand signed-upload request headers.
rm -f "${ATTACHMENT_HEADERS_FILE:-}"
# EPIC runs also wrote one full plan per child (.linear-plans/<ISSUE-ID>-child-*.md — the planned
# ticket is the parent) plus per-issue image-guard, attachment-guard, attachment-header and epic-spec-body scratch
# (step 4 stage 2's `.linear-plans/<ISSUE-ID>.epic-spec.json`), which can carry `## Original notes`,
# signed request data and the whole decomposition. Every `.linear-plans/` file this run writes begins
# with this run's issue id, the sweeper contract. ONE PATTERN PER LINE: under zsh and fish an unmatched
# glob aborts the WHOLE command line, so a single-ticket run — which writes no child plans — would
# skip every pattern sharing it. `find … -delete` exits 0 on no match, so each line stands alone.
# The `if` wrapper (not `… || true`, and not `2>/dev/null`) tolerates only the missing-directory
# case: a real deletion error — permission denied, I/O failure — still propagates. `-type f` keeps
# `rm -f` semantics by never removing a matching empty directory. Two separate masking bugs would
# let a naive block report success with scratch still on disk, so guard BOTH: (1) exit status is
# its LAST command's, so a failed child-plan delete followed by two no-match (exit 0) patterns
# vanishes — hence CLEANUP_RC accumulates instead of trusting per-line status; (2) BSD `find`
# (/usr/bin/find on macOS, where cron worktrees run) exits **0** even when `-delete` hits EACCES —
# only GNU find returns 1 — so the accumulator alone still misses it there. The residual `-print`
# sweep is the implementation-independent check: it asserts the post-condition we actually want
# (no matching scratch survives) rather than trusting any find's exit status.
CLEANUP_RC=0
rm -f .linear-plans/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json || CLEANUP_RC=1
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>-child-*.md.rejected' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.image-guard-*.md' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-guard-orig.md' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.attachment-headers-*.json' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ]; then find .linear-plans -maxdepth 1 -type f -name '<ISSUE-ID>*.epic-spec.json' -delete || CLEANUP_RC=1; fi
if [ -d .linear-plans ] && [ -n "$(find .linear-plans -maxdepth 1 -type f \( -name '<ISSUE-ID>-child-*.md' -o -name '<ISSUE-ID>-child-*.md.rejected' -o -name '<ISSUE-ID>*.image-guard-*.md' -o -name '<ISSUE-ID>*.attachment-guard-orig.md' -o -name '<ISSUE-ID>*.attachment-headers-*.json' -o -name '<ISSUE-ID>*.epic-spec.json' \) -print)" ]; then CLEANUP_RC=1; fi
[ "$CLEANUP_RC" = 0 ] || { echo "scratch cleanup failed — .linear-plans may still hold plan text, tracker state, signed upload headers or the epic spec" >&2; exit 1; }
```

In **interactive** mode also remove the seeded design doc (see "Interactive cleanup" in
`references/interactive-mode.md`); headless seeds none. Removal is best-effort (a missing file is
fine). In a `BOSS_CRON` run do this on every terminal path — including the Phase 2 dispatch-failure
abort, which also runs `bs-run-sentinel.mjs cleanup` — so an unattended run never leaves scratch.

## Phase 6 — Report

Print a concise summary: issue id + title, the finalized native plan attachment's **id** and exact
title `Implementation plan (<ISSUE-ID>)`, final labels, estimate, priority, and the status change
(unplanned → planned). When step 5 (e2) recorded any transitive-block warning, echo it
here too (e.g. `blocked by <BLOCKER-ID>, which is itself open and blocked by <UPSTREAM-BLOCKER-ID>`) so an unattended run
leaves a visible trail before the operator opens Linear. The plan is attached natively with no local copy
remaining (it is copied into `docs/plans/` at implementation time, per the plan's first dev step).

### Post-terminal notes extensions (repo opt-in)

**Caller suppression — check this before anything else.** A run another boss core dispatched as
part of its own larger run must not take its own notes: that caller already owns the single
post-terminal notes dispatch for the whole top-level run, so a nested phase here is exactly the
duplicate this gate exists to remove. A caller signals that ownership by setting
`BOSS_NOTES_SUPPRESSED=1` in the dispatched worker's environment. The marker **defaults to not
suppressed** — unset, empty, or any other value means this run owns its own notes, so a standalone
invocation still takes them.

A dispatched worker does not inherit that environment, so the caller also states the marker **in the
invocation**: bind it into the shell that runs the gate below (`BOSS_NOTES_SUPPRESSED=1`) before
reading it. Left unbound the gate reads a name nothing assigned, takes the not-suppressed branch,
and ships the duplicate this section exists to remove with both halves still reading as satisfied.

```bash
if [ "${BOSS_NOTES_SUPPRESSED:-}" = "1" ]; then
  echo "notes: suppressed (caller owns notes)"   # end the phase here: no discovery, no scratch, no dispatch
fi
```

Skipping is all that is due: suppression is never fatal and never changes the terminal outcome.

After the terminal outcome is decided and the report is emitted, resolve the extension helper and run:

```bash
BOSS_PLAN_ENV="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.claude/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || BOSS_PLAN_ENV="$HOME/.codex/skills/boss-plan/toolbox/boss-plan-env.sh"; [ -f "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
NOTES_JSON=$(node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" discover --core boss-plan --role notes --json)
```

Record every `NOTES_JSON.skipped` entry whose `deliberate` is `false` as
`extension <name>: skipped (<reason>)` in the ledger, before dispatching. Key that on the entry's
own `deliberate` field, never on the text of `reason`. A `deliberate: true` entry is a same-prefix
skill that is not an extension of this core — a markerless helper, or one extending another core —
and is never reported. Recording is all that is due: a discovery skip is never fatal and never
changes control flow; the phase still degrades exactly as documented below.

If `NOTES_JSON.extensions` is empty, do nothing and print nothing: a repo without a local notes
extension has not opted in. Create no scratch in that case.

**Sampling roll — one per run, shared by every reporting phase.** `notesDefaults.sampleRate` (a
number in `[0,1]`, default `1.0`; `0.33` is the recommended production setting) is the probability
that a run reports at all. Roll it **once per run** and carry the pair forward — a phase that
re-rolls turns one configured rate into two independent ones, so a run could pay for one reporting
phase and skip another. If an earlier phase in this run already rolled, reuse its values verbatim
and re-export them into this shell rather than rolling again:

```bash
if [ -z "${NOTES_SAMPLED:-}" ]; then
  NOTES_SAMPLE_RATE=$(export BOSS_PLAN_TOOLBOX; node --input-type=module -e 'import { pathToFileURL } from "node:url"; const { loadSkillConfig, notesSampleRate } = await import(pathToFileURL(process.env.BOSS_PLAN_TOOLBOX + "/skill-config.mjs").href); process.stdout.write(String(notesSampleRate(loadSkillConfig())))')
  NOTES_SAMPLED=$(awk -v r="${NOTES_SAMPLE_RATE:-1}" -v s="$$" 'BEGIN{srand(s);print (rand()<r)?"yes":"no"}')
  export NOTES_SAMPLE_RATE NOTES_SAMPLED
fi
if [ "$NOTES_SAMPLED" != "yes" ]; then
  echo "notes: sampled out (rate ${NOTES_SAMPLE_RATE:-1})"   # end the phase here: no scratch, no dispatch
fi
```

An unreadable or malformed rate resolves to `1.0` at both ends — the accessor's own fallback and
the `${NOTES_SAMPLE_RATE:-1}` default — so a broken config costs a few extra dispatches and never
silently switches reporting off. Seeding `srand` from the shell PID rather than the clock alone
keeps two runs that start in the same second from rolling identically. This gate sits **after** the
empty-`extensions` check, so a repo that never opted in still prints nothing at all.

Once both gates pass, create the terminal-only handoff:

```bash
NOTES_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-plan-notes.XXXXXX")
NOTES_OBSERVATIONS="$NOTES_RUN_TMP/observations.md"
```

Before dispatch, the orchestrator that still owns the completed run writes at most five
secret-scrubbed candidate observations to `NOTES_OBSERVATIONS`, with a maximum 8 KiB file size.
Keep each candidate to a short problem statement plus a file/skill/command pointer. Never copy a
transcript, command output, user-provided content, credentials, tokens, or other secrets; an empty
file is valid. This artifact is the only run-history source sent across the fresh-subagent boundary.

Dispatch descriptors in ascending `(order, name)` order as fresh, **awaited** subagents, each bounded
by `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms). Read each descriptor's `skillPath`; pass
it and `dir` in the worker brief. Resolve relative extension resources from `dir`, and use `SKILL.md`
as instructions — never its bare `name` through the Skill tool, which refuses `disable-model-invocation: true`.
Each receives:

```json
{
  "role": "notes",
  "core": "boss-plan",
  "context": {
    "mode": "<interactive if this run involved operator interaction; otherwise headless>",
    "core": "boss-plan",
    "outcome": "<resolved terminal outcome>",
    "repoId": "<BOSS_REPO_ID when present; otherwise null>",
    "observationPath": "<NOTES_OBSERVATIONS>"
  },
  "runTmp": "<NOTES_RUN_TMP>",
  "outPath": "<NOTES_RUN_TMP>/notes-<extension-name>.json"
}
```

Validate each result with `node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" validate --role notes --file
"<outPath>"` after running the toolbox preamble first. On success append one terminal-ledger line with the total persisted-note count. On a
discovery skip, timeout, missing output, malformed envelope, validation failure, or subagent failure,
append `extension <name>: skipped (<reason>)` and continue. Remove `NOTES_RUN_TMP` on every
post-opt-in terminal path. The phase cannot change the outcome, exit code, tracker or PR writes, and
is non-fatal in every case.

A **ledger write that itself fails** prints
`warning: ledger write failed (<reason>) — bookkeeping only, work state unaffected` and the run
continues. Ledger recording is advisory: it is history, never a terminal-state input.

## Privacy

Plans are stored only as native tracker attachments. The tracker may expose them to everyone with
access to the issue, and there is no server-side secret scanner, so **the agent running this skill is
the safeguard.** Never write secrets, tokens, credentials, private keys, session cookies, internal
hostnames/IPs, or customer PII into a plan — not in your prose, nor in the verbatim `## Original
notes` block (the likeliest place one sneaks in). Reference where a value lives instead (e.g. "the
deployment token in repo-root `.env`"); when in doubt, redact. This is enforced before attachment
finalization by the mandatory secret gate at the top of **Phase 4** — do not bypass it. Proof artifact
publishing is separate and continues to follow its configured proof adapter and privacy rules.

## Edge cases

- No unplanned issues / no ID match → report and stop.
- All unplanned tickets skipped at the Phase 1 confirmation (interactive) → report that the queue is exhausted and stop.
- Issue already past unplanned → warn and confirm before re-planning (headless: proceed if planned/in-progress, but stop on `Done`/`Canceled` — see Phase 1).
- Existing description → fold it into the interview/recon and preserve it verbatim under `## Original notes`.
- Estimate rejected → finish the other updates, warn about Fibonacci estimation setup.
- **Headless drafting dispatch fails** (missing/stale sentinel, or an `ok` sentinel with a missing/empty plan file) → `dispatch-failure`: **no Linear write**, non-zero exit with a one-line stderr reason, run-dir cleaned. A half-planned issue is worse than none.
- **Rewritten description drops a reporter image** (Phase 4 image-parity gate exits non-zero) → SAFE branch: **no Linear write**, non-zero exit naming the dropped URL(s), scratch discarded. A plan that silently destroys the reporter's screenshots is worse than none.

## Cron gate

When this skill is scheduled as a backlog-planning cron, register this **gate command** on
the job (scheduler UI, `GateCommand` — see PR #870) so the run only fires when the backlog
has something to plan, spending **zero** agent tokens otherwise:

```
node scripts/cron-gates/boss-plan.mjs
```

It exits `0` (run) iff at least one Linear issue is in the **unplanned** state — the
backlog this skill plans from — and non-zero (skip) otherwise. The gate is **fail-closed**:
a missing `LINEAR_API_KEY` (injected into the gate environment by bossd), network failure,
or API error exits non-zero with a one-line reason on stderr, captured in the scheduler's
`gate_output` log, so an unverifiable state skips the run rather than burning tokens. The
shared query logic lives in `scripts/linear-gate-lib.mjs` (unit-tested); this entry is a
thin I/O wrapper. (Only gate the **unattended/cron** use of this skill — interactive
`/boss-plan` runs are not gated.)

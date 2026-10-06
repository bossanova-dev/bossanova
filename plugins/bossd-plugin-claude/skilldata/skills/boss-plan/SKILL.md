---
name: boss-plan
description: Plan a tracker backlog ticket. Grabs the next unplanned issue by priority (or a ticket ID you provide), resolves drafting through boss-plan draft extensions or portable fallbacks, attaches the plan natively to the tracker issue, then writes a summary, labels, Fibonacci estimate, and priority before moving it from the unplanned to the planned state. Interactive by default; runs fully headless when BOSS_UNATTENDED=true (or legacy BOSS_CRON=true).
---

# boss-plan

Turn a vague tracker ticket into a **planned** ticket with an implementation-ready plan attached. Use
when asked to "plan a tracker ticket", "plan the next ticket", "boss-plan", or given a ticket ID. You
are a capable engineer: this document states what a planned ticket must look like, the helpers that
answer questions reliably, and the gates that protect the reporter's original notes and keep secrets
out of the tracker. How you research and write the plan is up to you.

**Modes.** Interactive by default (a draft extension may ask questions). With `BOSS_UNATTENDED=true`
or its legacy spelling `BOSS_CRON=true` — bossd sets both on every unattended run, including any
prompt-carrying `boss new`, so never fake the mode in prompt text — it is fully headless: **never**
call `AskUserQuestion`, dispatch one awaited drafting subagent, decide every fork yourself, and
record the controversial ones as open questions.

## What a planned ticket is

- **A plan attachment** titled exactly `Implementation plan (<ISSUE-ID>)` — free-form Markdown, no
  required headings, ending in `## Original notes` (the ticket as it read before planning, verbatim).
  It is the plan; nothing is committed to the repo.
- **A short description** that points at the plan rather than copying it: `## Summary`,
  `## Key changes` (the paths the change touches — dependency linking reads them), `## Why this needs
a human` and `## Open Questions` only when they apply, `## Planning` (with a `- Contract: v1`
  stamp, dependency notes, any `- Atomic-5:` justification), and `## Original notes` verbatim. The
  drafting brief has the template.
- **Labels**: the existing set, plus content labels that genuinely apply (`bug`, `feature`,
  `improvement`, `docs` — names via `optionalLabelName(config, '<role>')`, else the literal), plus
  exactly one of `agent-friendly` (the default) or `needs-human` (only when an agent genuinely could
  not do it — size alone is never the reason; then the plan explains why), plus `agent-question`
  when there are open questions (headless only). The planning-queue label (`agent-plan`) is removed.
  Pipeline label roles resolve through `labelName(config, '<role>')`, whose keys are: `agentFriendly`, `needsHuman`, `agentPlan`, `agentQuestion`, `epic`
  (it throws on an unknown role). Never create labels.
- **Estimate** (Fibonacci): 0 trivial; 1/2/3 one PR; 5/8 means it is an **epic**
  ([`references/epic.md`](references/epic.md)) unless a 5 is genuinely atomic.
- **Priority** 1–4 (`1=Urgent … 4=Low, 0=None`): keep a reporter-set priority, otherwise rank it
  against the planned backlog (security biases up). Not left at None.
- **State**: moved from the configured unplanned state to the planned state.
- **Dependencies**: `blockedBy` edges to tickets it genuinely conflicts with (Phase 4 step 5).

## Workspace facts

Load the config once — `loadSkillConfig({cwd})` (synchronous, options object) → `config`;
`trackerConfigFor(config)` gives the tracker server, team and workspace (never a project filter) and
the state roles `unplanned`, `planned`, `inProgress`, `inReview`. Reach the tracker only through the
resolved adapter. A blocker counts as cleared only when its state type is completed or canceled
(`plan-deps-lib.mjs`); boss-build will not start a ticket with an uncleared blocker.

## Helpers

Every block that uses the toolbox starts with this preamble (each Bash call is a fresh shell):

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
```

| Question                                     | Ask                                                                                |
| -------------------------------------------- | ---------------------------------------------------------------------------------- |
| Is this repo configured for planning?        | `isConfiguredForPlanning(config)` in `skill-config.mjs`                            |
| Is the ticket already planned?               | `node plan-run-guards.mjs idempotence <payload> --selected-id <id>`                |
| Read / write the description exactly         | `node tracker/cli.mjs read-description \| write-description …`                     |
| Upload / read / delete the plan attachment   | [`references/plan-storage.md`](references/plan-storage.md)                         |
| Normalise the drafter's metadata             | `node plan-run-guards.mjs adopt-metadata <file> <json>`                            |
| Did a secret slip in?                        | `node plan-secret-scan.mjs <files>`                                                |
| Did the reporter's images and notes survive? | `node plan-image-guard.mjs …`                                                      |
| Is the description a plan description?       | `node plan-contract-guard.mjs --description … --plan …`                            |
| Have referenced tickets moved since recon?   | `node plan-run-guards.mjs premises …`                                              |
| Which tickets does this conflict with?       | `planDependencyEdges` in `plan-deps-lib.mjs` (Phase 4 step 5)                      |
| Did the description land as written?         | `node plan-writeback-verify.mjs --intended … --stored …`                           |
| What scratch names may I use?                | `node plan-scratch-paths.mjs families`                                             |
| Is a tracker failure safe to retry?          | `node tracker/cli.mjs classify-outcome --observed "<err>" --operation read\|write` |

## Rules

1. **Never lose the reporter's notes.** `## Original notes` is the stored description, verbatim, in
   both the plan and the description; the image guard proves it. The tracker keeps no description
   history, so never overwrite a description you cannot verify.
2. **Keep secrets out.** Plans are visible to everyone with access to the issue. Never write tokens,
   keys, passwords, connection strings, session cookies, internal hostnames/IPs or customer PII —
   reference where a value lives instead (`[REDACTED: repo-root .env]`). The secret gate is mandatory.
3. **Validate before writing.** Every gate runs before the first tracker write it protects; a failed
   gate writes nothing and exits non-zero.
4. **Scratch lives in this run's directory only.** `.linear-plans/` is shared by concurrent runs:
   write only inside `.linear-plans/run-<RUN-SCRATCH-ID>/`, under names `plan-scratch-paths.mjs`
   declares, and delete only that directory. Never clean up by issue-id pattern (two runs can plan
   the same ticket) or tidy a peer's files.
5. **Leave nothing behind**, on every terminal path — except after a write-back `drift` verdict,
   where the scratch is the only copy of the intended bytes.
6. **Labels merge.** `save_issue` `labels` replaces the whole set: read, then write existing ∪
   additions ∖ strips.

## Phase 0 — Preflight

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
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
# Amortized self-heal for plan scratch — files and directories alike — orphaned by runs
# that abort before cleanup. It is TTL-gated, so a live peer run's scratch is never touched.
node "$BOSS_PLAN_TOOLBOX/plan-scratch-reap.mjs" .linear-plans ||
  echo "warning: stale plan-scratch reap failed (non-fatal)" >&2
# Mint this run's own scratch directory. `mktemp -d` creates it atomically, so two runs
# started at the same instant cannot collide on it even when they plan the same ticket.
mkdir -p .linear-plans
RUN_SCRATCH="$(mktemp -d .linear-plans/run-XXXXXXXX)" || { echo "boss-plan: cannot create run scratch directory" >&2; exit 1; }
echo "run scratch: $RUN_SCRATCH"
```

`<RUN-SCRATCH-ID>` is the suffix of the printed `run scratch:` directory; substitute it literally
everywhere below (it is not the sentinel `RUN_ID`). A repo with no planning config is a clean no-op
(exit 0); a present but broken config aborts.

Report installed-skill drift (a stale record warns; an absent capability stops):

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
  if O="$("$BOSS_BIN" skills check --gate 2>&1)"; then
    if [ -n "$O" ]; then printf '%s\n' "$O" >&2; fi
  else
    case "$O" in
      *--gate*) node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true ;;
      *)
        printf '%s\n' "$O" >&2
        printf '%s\n' "$O" | node "$BOSS_PLAN_TOOLBOX/skill-drift-verdict.mjs" classify --status 1 >&2 || exit 1
        ;;
    esac
  fi
elif [ -f "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" ]; then
  node "$BOSS_PLAN_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_PLAN_TOOLBOX" || true
else
  echo "boss-toolbox-drift: (drift helper not installed) — this install predates the check; drift is UNKNOWN, not clean." >&2
fi
```

Require the plan-attachment ops — `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" operations --require
preparePlanAttachment,finalizePlanAttachment,readPlanAttachment,deletePlanAttachment` (exit 2 names
what is missing: stop before any write) — and confirm the tracker answers a cheap read. A failed
read: classify it with `--operation read` and follow its action line (a retryable edge error is not
"tracker unreachable").

## Phase 1 — Select the issue

- **A named ticket**: `get_issue` it, whatever its state. Interactive re-planning follows
  `references/interactive-mode.md`. Headless stops on Done/Canceled.
- **Otherwise**: list the team's unplanned issues (`limit=250`), rank by priority (Urgent first,
  None last), then oldest. Interactive confirms with the user (plan / skip / pick another / cancel);
  headless takes the head. An empty queue reports and stops.

Then, in every case, the idempotence precheck — write the issue payload to
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json` and:

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
PRECHECK=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.precheck.json"
node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" idempotence "$PRECHECK" --selected-id "<ISSUE-ID>"
```

`action: "noop"` (already planned with a valid description and plan attachment) ⇒ delete the scratch,
print one line, exit 0 with no writes. `action: "plan"` ⇒ log its `reasons` and continue;
`fetched-issue-id-mismatch` means re-select.

## Phase 2 — Draft

Drafting resolves through the first tier that succeeds: a repo-local `boss-plan-*` extension with
`role: draft`, a host built-in, or the inline prompt. A dispatch succeeds only when its result is
valid **and** it wrote a non-empty plan at the path it alone was given; record
`extension <name>: skipped (<reason>)` for every one that did not.

**Interactive:** follow `references/interactive-mode.md`, then Phase 3.5 and Phase 4.

**Headless:** do not draft inline. Dispatch **one** awaited drafting subagent that keeps recon and the
plan in its own context and returns only a path and bounded metadata.

1. Create the sentinel context:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   RUN_SENTINEL="$BOSS_PLAN_TOOLBOX/bs-run-sentinel.mjs"
   test -f "$RUN_SENTINEL" || { echo "BLOCKED: bs-run-sentinel.mjs missing" >&2; exit 1; }
   DISPATCH_FAILURE="dispatch-failure"
   PLAN_PATH=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md"   # compute the slug with plan-slug.mjs issueSlug
   RUN="$(node "$RUN_SENTINEL" make-ctx boss-plan)"
   RUN_ID="${RUN%%$'\t'*}"; RUN_DIR="${RUN#*$'\t'}"
   RUN_SCRATCH=".linear-plans/run-<RUN-SCRATCH-ID>"   # Phase 0's directory; NOT $RUN_DIR, NOT $RUN_ID
   export RUN_SENTINEL DISPATCH_FAILURE PLAN_PATH RUN_ID RUN_DIR RUN_SCRATCH
   ```

2. Snapshot the stored description — the run's only description source, which the worker builds
   `## Original notes` from and Phase 4 gates against:
   `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" read-description --id <issue UUID> --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md`
   (exit 2 ⇒ byte-copy the Phase 1 `get_issue` description; exit 64 or a receipt for another id
   stops). Add nothing to it.

3. Dispatch a `general-purpose` subagent on the orchestrator's model (planning is judgement). Pass
   the **path** `references/headless-drafting-brief.md`, the ticket id and title, the snapshot path,
   `PLAN_PATH`, `RUN_SCRATCH`, and `RUN_SENTINEL`/`RUN_DIR`/`RUN_ID`. It returns only `planPath`,
   `labels`, `agentFriendly`, `estimate`, `priority`, `openQuestions` and
   `descriptionSummary: {path}` — never plan text. Hold it: write `$RUN_DIR/draft.dispatched-at`
   before each attempt and re-arm `node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" wait` while it
   exits 98 ([`references/headless-dispatch.md`](references/headless-dispatch.md) owns the hold and
   its failure rules).

4. Classify from the run file only, and re-verify every artifact the payload names by measuring it on
   disk:

   ```bash
   READ="$(node "$RUN_SENTINEL" read "$RUN_DIR" "$RUN_ID" draft)"
   AWAIT="${RUN_SENTINEL%/*}/bs-dispatch-await.mjs"
   # `disposition` demotes a provisional (never-upgraded) payload on EVERY kind.
   DISP="$(node "$AWAIT" disposition "$RUN_DIR" "$RUN_ID" draft --heartbeat .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.dispatch-heartbeat.json --dispatched-at "$(cat "$RUN_DIR/draft.dispatched-at")")"
   if [ "$(printf '%s' "$DISP" | jq -r '.publishable')" != true ]; then
     # SAFE branch — NO Linear write, non-zero exit.
     echo "$DISPATCH_FAILURE: no publishable sentinel ($(printf '%s' "$DISP" | jq -r '.reason')) — aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
     # Probe every artifact the payload DECLARES: an epic outcome carries no `planPath`, so a
     # plan-only probe finds nothing, authorises the removal below and eats finished child plans.
     set -- "$PLAN_PATH"
     while IFS= read -r A; do set -- "$@" "$A"; done <<<"$(printf '%s' "$READ" | jq -r '[.payload.epicSpecPaths,.payload.guardScratchPaths,(.payload.childPlanPaths|if type=="object" then [.[]] else . end)]|flatten|.[]|strings')"
     node "$AWAIT" guard-discard "$@" || exit 1   # read before discard; non-zero = retain
     # Abort skips Phase 5; delete the epic/guard/run-boundary scratch families now.
     CLEANUP_RC=0
     rm -rf .linear-plans/run-<RUN-SCRATCH-ID> || CLEANUP_RC=1
     if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi
     if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans/run-<RUN-SCRATCH-ID> may still hold plan text, tracker state or signed upload headers" >&2; fi
     exit 1
   fi
   node -e 'const f=require("fs"),p=require("path"),[r,L,F]=process.argv.slice(1),x=JSON.parse(r).payload||{},T=c=>c?.trim?.(),B="epicSpecPaths",H="guardScratchPaths",P="childPlanPaths",K=["planPath",H,P,B,"attachmentHeaderPaths"],v=k=>{const q=x[k]||[];return k===P&&q&&!Array.isArray(q)&&typeof q=="object"?Object.values(q):[].concat(q)},g=s=>s.toLowerCase().replace(/[^a-z\d]+/g,"-").replace(/^-+|-+$/g,""),n=(id,t)=>id.toUpperCase()+"-"+g(t);let b=0,E=c=>{console.error(`${F}: sentinel ok but artifact missing/empty or wrong path (${c}) — no Linear write, aborting`);b=1},S=v(B).filter(T),G=v(H).filter(T),D=p.resolve(".linear-plans/run-<RUN-SCRATCH-ID>");if(x.epic){const I=v("childIds").filter(T),M=typeof x[P]=="object"&&!Array.isArray(x[P])?x[P]:{},C=I.map(id=>M[id]).filter(T),R=T(x.epicParentId),A=[],U=new Set,O=p.resolve(D,`${R}.epic-spec.json`);for(const k of[H,B])if(!Array.isArray(x[k]))E(k);if(!S.length)E(B);for(const s of S){if(p.resolve(s)!==O){E(B);continue}try{const q=JSON.parse(f.readFileSync(s));if(T(q.parentId)!==R)E(B);for(const c of q.children||[])if(T(c.key)&&T(c.title))A.push([c.key,c.title])}catch{E(s)}}if(!R||I.some(id=>"image-guard-orig attachment-guard-orig image-guard-new".split` `.some(w=>!G.some(c=>p.basename(c)===`${R}.child-${id}.${w}.md`))))E(H);if(!R||!I.length||A.length!==I.length||C.length!==I.length||new Set(C.map(c=>p.resolve(c))).size!==I.length)E(P);for(const id of I){const c=T(M[id]);if(!c){E(`${P}.${id}`);continue}const j=A.findIndex(y=>p.basename(c)===`${R}-child-${y[0]}-${n(id,y[1])}.md`);if(j<0||U.has(j))E(c);else U.add(j)}if(U.size!==A.length)E(P)}else if(!v("planPath").some(T))E("planPath");const P0=p.resolve(L);for(const k of K)for(const c of v(k))if(T(c)){const z=p.resolve(c),a=z===P0||p.dirname(z)===D,m=a&&f.existsSync(z)&&f.statSync(z),s=m&&m.isFile()&&(m.size||k===H&&/-guard-orig[.]md$/.test(z));if(!s)E(c)}process.exit(b)' "$READ" "$PLAN_PATH" "$DISPATCH_FAILURE" ||
     {
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       # Artifact verification failure also skips Phase 5; remove the same scratch families.
       CLEANUP_RC=0
       rm -rf .linear-plans/run-<RUN-SCRATCH-ID> || CLEANUP_RC=1
       if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi
       if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans/run-<RUN-SCRATCH-ID> may still hold plan text, tracker state or signed upload headers" >&2; fi
       exit 1
     }
   EPIC="$(printf '%s' "$READ" | jq -r '.payload.epic // empty')"
   PREMISES="$(printf '%s' "$READ" | jq -c '.payload.premises // []')"
   if [ "$EPIC" = "true" ]; then
     # EPIC: inputs hydrated before this block (headless-dispatch.md).
     RC=0; RV="$(node "${RUN_SENTINEL%/*}/plan-run-guards.mjs" epic-reverify .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.epic-reverify.json)" || RC=$?
     # A crashed node exits 1 too: only a printed resumable verdict deletes scratch.
     case "$RC:$(printf '%s' "$RV" | jq -r .class 2>/dev/null)" in
       0:*) ;;
       1:resumable)
       echo "$DISPATCH_FAILURE: epic reverify failed, parent still unplanned — next sweep resumes it" >&2
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       CLEANUP_RC=0
       rm -rf .linear-plans/run-<RUN-SCRATCH-ID> || CLEANUP_RC=1
       if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi
       if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans/run-<RUN-SCRATCH-ID> may still hold plan text, tracker state or signed upload headers" >&2; fi
       exit 1 ;;
       *)
       echo "$DISPATCH_FAILURE: epic reverify exit $RC — needs a human; run scratch kept" >&2
       node "$RUN_SENTINEL" cleanup "$RUN_DIR"
       exit 1 ;;
     esac
     # PASSED: no single-ticket plan/metadata — SKIP Phase 3.5-4; Phase 5 + 6 report epicParentId, childIds.
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

   An epic outcome (`payload.epic`) already did every tracker write and is accepted only on
   `epic-reverify` exit 0 (`references/headless-dispatch.md` hydrates its inputs); skip Phase 3.5–4.
   A single-ticket outcome continues. Keep `PREMISES` for Phase 4.

5. Validate the **returned** metadata (never a same-named file):

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   METADATA=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.draft-metadata.json"
   if ! node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" adopt-metadata "$METADATA" "$RETURNED_METADATA"; then
     echo "$DISPATCH_FAILURE: draft metadata failed plan-run-guards.mjs adopt-metadata — no Linear write, aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
     # Read before discard: the plan already PASSED re-verify.
     node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" guard-discard "$PLAN_PATH" || exit 1
     # Abort skips Phase 5; remove this run's whole scratch directory, as every sibling abort does.
     CLEANUP_RC=0
     rm -rf .linear-plans/run-<RUN-SCRATCH-ID> || CLEANUP_RC=1
     if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi
     if [ "$CLEANUP_RC" != 0 ]; then echo "warning: scratch cleanup failed — .linear-plans/run-<RUN-SCRATCH-ID> may still hold plan text, tracker state or signed upload headers" >&2; fi
     exit 1
   fi
   ```

   The guard normalises labels, estimate and priority itself (its `warning:` lines say what changed)
   and fails only when the plan path or description is missing.

## Phase 2.5 — Epic

When the honest triage is EPIC, build a parent plus planned children instead of one plan: follow
[`references/epic.md`](references/epic.md). Headless, the drafting subagent does this itself;
interactive, a human confirms the shape first.

## Phase 3 — What the plan contains

The drafting brief ([`references/headless-drafting-brief.md`](references/headless-drafting-brief.md)
Steps 5 and 7) is the single drafting spec for both modes — plan body and description template. The
plan lives at `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md` (slug from
`plan-slug.mjs` `issueSlug`). Every query-bearing upload URL in it is query-stripped. Headless open
questions are only genuinely controversial forks; a non-empty list adds `agent-question` and an
`## Open Questions` section. Config-first helpers take `(config, description)`:
`validatePlanDescription`, `parseAcceptanceCriteria`, `parsePremises`, `validateVerifyOnlyEvidence`,
`extractKeyChangeAreas`.

## Phase 3.5 — Extension plan reviewers

Run any repo-local `boss-plan-*` extensions with `discover --core boss-plan --role plan-reviewer`
over the draft (additive, non-fatal, a no-op when none are installed) —
[`references/extension-reviewers.md`](references/extension-reviewers.md).

## Phase 4 — Gate, attach, write back

**Secret gate (mandatory).** `node "${BOSS_PLAN_TOOLBOX:?}/plan-secret-scan.mjs" "$PLAN_FILE" <the
description artifact>` — exit 1: redact the named lines and re-run; exit 2: fail closed. A clean scan
is a floor, not a pass: then read the whole plan (especially `## Original notes`) yourself and redact
anything credential- or PII-shaped in every persisted artifact. Credential query values in external
image URLs become `token=REDACTED`; a signed `uploads.linear.app` URL loses its signature query and
keeps its asset path.

**Premise re-check.** Tickets the plan cites may have moved since recon
([premise protocol](references/headless-dispatch.md#premise-drift-is-reconciled-not-appended)):

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
PLAN_FILE="${PLAN_FILE:-.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md}"
PREMISES_FILE=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.premises.json"; LIVE_STATES_FILE=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.premise-states.json"
PREMISES="${PREMISES:-[]}"
printf '%s\n' "$PREMISES" >"$PREMISES_FILE" || exit 1
if [ "$PREMISES" = '[]' ]; then
  printf '{}\n' >"$LIVE_STATES_FILE" || exit 1
fi
PREMISE_REPORT="$(node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" premises "$PREMISES_FILE" "$LIVE_STATES_FILE" --annotate .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md --annotate "$PLAN_FILE" 2>&1)"
PREMISE_RC=$?
```

Non-zero ⇒ no writes. Drift is annotated (`- Premise drift: …`) and named in the report.

**Image and notes gates.** Copy the description artifact to `image-guard-new.md`, write
`attachment-guard-orig.md` from the snapshot with only the mandatory redactions and signature
stripping, count the distinct `uploads.linear.app` assets in the snapshot, and:

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md"; SAFE_ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.attachment-guard-orig.md"; NEW=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md"
BODY=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md"   # the path descriptionSummary returned
cp "$BODY" "$NEW" || { echo "descriptionSummary artifact unreadable — aborting" >&2; exit 1; }
PLAN_FILE="${PLAN_FILE:-.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md}"
EXPECTED_IMAGES="<distinct canonical upload identities observed in the snapshot>"
cleanup_guard_scratch() {
  rm -f "$ORIG" "$SAFE_ORIG" "$NEW" || echo "warning: guard scratch cleanup failed" >&2
}
# Keep scratch until all gates pass; every failing gate calls this helper before exiting.
# It removes the SOURCE copies only. `$PLAN_FILE` is deliberately NOT removed — see below.
if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$ORIG" --rewritten "$NEW" \
  --expect-images "$EXPECTED_IMAGES" --require-unsigned-uploads; then
  echo "image-parity gate failed (guard message above) — no Linear write, aborting" >&2
  cleanup_guard_scratch
  exit 1
fi
if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$ORIG" --rewritten "$SAFE_ORIG" \
  --require-safe-source; then
  echo "safe-source gate failed (guard message above) — no Linear write, aborting" >&2
  cleanup_guard_scratch
  exit 1
fi
if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$SAFE_ORIG" --rewritten "$NEW" \
  --require-verbatim --require-unsigned-uploads; then
  echo "description safety gate failed (guard message above) — no Linear write, aborting" >&2
  cleanup_guard_scratch
  exit 1
fi
if ! node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$SAFE_ORIG" --rewritten "$PLAN_FILE" \
  --require-verbatim --require-unsigned-uploads; then
  echo "plan-attachment safety gate failed (guard message above) — no attachment finalize, aborting" >&2
  cleanup_guard_scratch
  exit 1
fi
```

A failing gate deletes the source copies (they may be sensitive) but keeps `$PLAN_FILE`, so the next
attempt edits rather than redrafts. Pass `--allow-empty-original` only when the stored description
really was empty.

**Contract gate.**

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md"; SAFE_ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.attachment-guard-orig.md"; NEW=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md"
PLAN_FILE="${PLAN_FILE:-.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md}"
if CONTRACT_REPORT="$(node "$BOSS_PLAN_TOOLBOX/plan-contract-guard.mjs" --description "$NEW" --plan "$PLAN_FILE" --module-roots "$(git ls-tree --name-only HEAD | paste -sd, -)" 2>&1)"; then
  if [ -n "$CONTRACT_REPORT" ]; then printf '%s\n' "$CONTRACT_REPORT" >&2; fi
else
  printf '%s\n' "$CONTRACT_REPORT" >&2
  echo "plan-contract gate failed (guard message above) — no Linear write, aborting" >&2
  rm -f "$ORIG" "$SAFE_ORIG" "$NEW" || echo "warning: contract gate scratch cleanup failed" >&2
  exit 1
fi
```

It fails only on a missing `## Summary` / `## Original notes`, text that is not a plan description,
an unsubstituted placeholder, or an empty/truncated plan; everything else is a `warning:`.

Then:

1. **Attach** the plan per [`references/plan-storage.md`](references/plan-storage.md) and assert
   exactly one `Implementation plan (<ISSUE-ID>)` attachment remains (duplicates ⇒ no further writes).
2. **Labels**: read with `readLabels` and compute the merged set (see "What a planned ticket is").
   Interactive runs strip a stale `agent-question` (a human answered every fork).
3. **Save** in one tracker write: the description from
   `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" write-description --id <ISSUE-ID> --body-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md`
   (execute the `{tool, args}` it emits for `descriptor-emitted`; fall back to an inline
   description only when stderr names a missing `writeDescription` op), plus labels, estimate,
   priority and the planned state. A rejected estimate: retry without it and warn. A failure
   `classify-outcome` calls `indeterminate`: read the issue back before any retry. If the read-back
   shows it did not land and the one retry fails the same way, split it: save everything except
   labels, then the merged labels in a labels-only write, and name the split in the run report.
4. **Link dependencies** — I/O only; `plan-deps-lib.mjs` decides every edge:

   a. Fetch candidates (planned, in-progress, in-review, plus every related id):
   `node "$BOSS_PLAN_TOOLBOX/tracker/cli.mjs" fetch-candidates --out-file .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.candidates.json --id <ISSUE-ID> --id <related id>…`.
   Read only the receipt and `jq -r '.[] | [.identifier, .title, .stateName] | @tsv'`; a
   non-zero exit means _could not evaluate_.
   b. Judge real logical dependencies yourself, with direction: `logicalDependencies[<id>] =
{direction: 'blockedBy' | 'blocks', note}`. Only these verdicts create blocking edges. File
   overlap is computed for you from each ticket's `## Key changes` (pass paths verbatim, never
   coarsened) and becomes a non-blocking `relatedTo` link — touching the same files is a rebase
   risk, not a prerequisite.
   c. Write the scan input to `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.deps-in.json`
   (`declaredRelatedIds`, `logicalDependencies`, optional `childrenByParentId`, plus any
   `moduleRoots`/`repoWideTokens`/`areaAliases` overrides — no `subject`) and classify:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   DEPS_IN=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.deps-in.json"
   CANDIDATES=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.candidates.json"
   node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" deps "$DEPS_IN" "$CANDIDATES" --subject <ISSUE-ID>
   # Remove the consumed scan input.
   rm -f "$DEPS_IN"
   ```

   Read the stderr line (`subjectAreas`, unresolved tokens, verdict) before writing anything.
   d. Act on the result: write each `edge.write` with `appendDependency` (append-only); `relatedTo`
   edges with `appendRelatedTo` when the adapter has it, else a `## Planning` note; `notes` under
   `## Planning`; `questions` under `## Open Questions` plus `agent-question`. A `skipped` entry
   with `expandChildren` true — add its children as `--id` and re-run (at most twice, excluding
   parents already expanded). `no-candidates` is a clean result: record nothing.
   `could-not-evaluate` is reported as such, never as "no dependencies"; an unresolved area is
   code-marked or declared in `moduleRoots` and re-run.
   e. Before each blocking write, read both tickets' relations and skip a write that would form a
   cycle. A blocker whose PR is merged (`gh pr view <url> --json state,mergeCommit`) gets
   `landed: {evidence}` and a re-run instead. Pass those reads to `transitiveBlockWarnings`:
   upstream ⇒ a `- Transitive-block warning:` line; downstream ⇒ a note (escalated also as a
   question). After each relation write, re-read **both** sides; record a missing edge, never
   re-write it.
   f. Only when `verdict.recordToDescription` is true or the second premise pass changed the text:
   read the stored description back into `image-guard-final.md`, re-read premise states, run the
   second pass:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   PREMISES_FILE=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.premises.json"; LIVE_STATES_FILE=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.premise-states.json"
   FINAL=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-final.md"
   PREMISE_BEFORE="$(shasum -a 256 "$FINAL")" || exit 1
   PREMISE_REPORT="$(node "$BOSS_PLAN_TOOLBOX/plan-run-guards.mjs" premises "$PREMISES_FILE" "$LIVE_STATES_FILE" --annotate .linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-final.md 2>&1)"
   PREMISE_RC=$?
   PREMISE_CHANGED=0
   if [ "$PREMISE_RC" = 0 ]; then
     PREMISE_AFTER="$(shasum -a 256 "$FINAL")" || exit 1
     [ "$PREMISE_BEFORE" = "$PREMISE_AFTER" ] || PREMISE_CHANGED=1
   fi
   ```

   then edit **that read-back** (never step 3's bytes — the tracker may have renormalised them)
   to add the notes, questions and `- Dependencies: blocks <ID>; blocked by <ID>` under the right
   sections, and save it with `write-description` (plus labels if `agent-question` was added).
   The description and the attachment then legitimately differ at `## Planning`.

5. **Verify the write-back, once, after the final description save.** `image-guard-final.md` must
   hold the exact bytes of that save; read the stored description into `image-guard-stored.md`:

   ```bash
   BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
   WB_FINAL=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-final.md"; WB_STORED=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-stored.md"
   if ! node "$BOSS_PLAN_TOOLBOX/plan-writeback-verify.mjs" --intended "$WB_FINAL" --stored "$WB_STORED"; then
     echo "write-back verification failed (verdict above) — the description is ALREADY stored; do NOT rewrite it" >&2
     exit 1
   fi
   [ "${PREMISE_RC:-0}" = 0 ] || { echo "$PREMISE_REPORT" >&2; exit "$PREMISE_RC"; }
   ```

   `byte-exact` / `normalized-equivalent` pass. `unattributed` passes but keeps the scratch and names
   the line in the report. `drift` fails: keep the scratch and **do not** rewrite the description.
   No verdict (a read failure) fails too.

## Phase 5 — Clean up

```bash
# Everything this run wrote — the plan text, any `.md.rejected` structure artifact, every epic child
# plan, the image-guard / attachment-guard scratch, the signed upload headers and the epic spec body —
# is inside this directory, so one recursive removal covers all of it. `references/plan-storage.md`
# already deletes each header file immediately after its PUT; this removal is what covers a
# prepare/PUT/finalize abort that stranded one.
CLEANUP_RC=0
rm -rf .linear-plans/run-<RUN-SCRATCH-ID> || CLEANUP_RC=1
# Assert the post-condition rather than trusting the exit status: BSD `find` and `rm` on macOS (where
# cron worktrees run) can exit 0 having failed to remove an entry, so re-check existence.
if [ -e .linear-plans/run-<RUN-SCRATCH-ID> ]; then CLEANUP_RC=1; fi
[ "$CLEANUP_RC" = 0 ] || { echo "scratch cleanup failed — .linear-plans/run-<RUN-SCRATCH-ID> may still hold plan text, tracker state, signed upload headers or the epic spec" >&2; exit 1; }
```

Interactive runs also remove the seeded design doc (`references/interactive-mode.md`). Every terminal
path cleans up, including dispatch failures — except a write-back `drift`. Never select a sentinel
run directory by grepping a ticket id; use the `RUN_ID` you were handed.

## Phase 6 — Report

Issue id and title; the attachment id and title; final labels, estimate, priority; the state change;
the write-back verdict and read route; the dependency verdict (`<verdict> compared=N edges=M`) and any
transitive-block warning; premise drift; for an epic, the parent and child ids.

**Notes** (skip when `BOSS_NOTES_SUPPRESSED=1`): discover `--role notes`; none ⇒ nothing. Roll
`notesSampleRate` once per run (reuse `NOTES_SAMPLED` if set); on a miss, stop. Otherwise write at
most five secret-free observations (≤ 8 KiB) to a temp `observations.md` and dispatch each extension
(instructions from its `skillPath`, bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`) with
`{"role":"notes","core":"boss-plan","context":{"mode","core","outcome","repoId","observationPath"},"runTmp","outPath"}`;
validate with `--role notes`. Never fatal.

## Phase 7 — Self-archive

The very last action on every terminal path, after the report and any notes dispatch:
`node "$BOSS_PLAN_TOOLBOX/session-self-archive.mjs" --outcome <planned|noop|epic|empty-queue|blocked|failed> --run-scratch .linear-plans/run-<RUN-SCRATCH-ID>`
— add `--suppressed` when a calling skill or your prompt owns the session. Act on its `verdict`:
`archive` — the archive is launched and will close this pane; end the turn with one line. `ask` —
ask whether to archive now; on yes re-run with `--confirmed`, on no stop. `skip` — print the
`reason` and stop; never an error.

## Cron gate

For an unattended planning cron, register `node scripts/cron-gates/boss-plan.mjs` as the job's gate
command: it exits 0 only when an unplanned issue exists, fails closed (missing `LINEAR_API_KEY`,
network or API error) and costs zero agent tokens otherwise. Interactive runs are not gated.

# Headless drafting brief (read by the dispatched subagent only)

You are drafting an implementation-ready plan for one ticket, unattended, inside the single awaited
subagent the boss-plan orchestrator dispatched. Nobody is watching: **never call
`AskUserQuestion`**. Decide every fork yourself from the ticket and the code, and record only the
genuinely controversial ones as open questions. Keep recon and drafting in your own context and
return only a small metadata object. On the single-ticket path you make **no** tracker writes — only
the plan file, the description artifact and the sentinel. Only the epic path writes to the tracker
([`epic.md`](epic.md)).

## Inputs

- `ISSUE-ID`, `title`, and `DESCRIPTION_SNAPSHOT_PATH`
  (`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md`) — the tracker's **stored**
  description, written before you were dispatched. It is the only source for `## Original notes`:
  copy from it, add nothing, and never re-read the tracker description (signed upload URLs rotate).
  It may be empty.
- `PLAN_PATH` — where the plan goes: `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md`.
- `RUN_SENTINEL`, `RUN_DIR`, `RUN_ID` — the run-file sentinel you write your outcome to. `RUN_DIR` is
  under `$TMPDIR` and is not where scratch goes.
- `<RUN-SCRATCH-ID>` — every local file you write goes in `.linear-plans/run-<RUN-SCRATCH-ID>/`,
  under a name `node "$BOSS_PLAN_TOOLBOX/plan-scratch-paths.mjs" families` declares. Never invent a
  scratch name or write scratch anywhere else.

Every block that uses `$BOSS_PLAN_TOOLBOX` starts with the toolbox preamble from SKILL.md (each Bash
call is a fresh shell).

## Heartbeat

The orchestrator's only sign that you are alive is
`HEARTBEAT_PATH=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.dispatch-heartbeat.json"`. Beat it at
every step boundary, and wrap anything that runs longer than a beat interval (the wrapper exits with
the wrapped command's status):

```bash
node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" heartbeat "$HEARTBEAT_PATH" "step 5: drafting"
```

```bash
node "$BOSS_PLAN_TOOLBOX/bs-dispatch-await.mjs" beat "$HEARTBEAT_PATH" --interval 30000 -- make test
```

Never beat from a background loop that outlives your work.

## Resuming

If `PLAN_PATH` already exists and is non-empty when you start, a previous dispatch died after
drafting. Normalise what is there — check it against this brief, fix only what fails, then do Steps
6–9 — rather than redrafting. A dead dispatch's returned prose is worth reading, but confirm every
claim in it against the file it names before relying on it.

## Step 1 — Triage

- **Trivial** (a copy tweak, one obvious line, no design decisions) — a light plan.
- **Substantial** — design choices, several files, unknowns.
- **Epic** — the honest estimate is ≥ 5, or the work is several independently mergeable PRs with at
  least two genuinely separable pieces. A single ticket is estimated 0/1/2/3; a truly atomic 5 stays
  one ticket with a `- Atomic-5:` justification under `## Planning`; an 8 is never one ticket. Build
  it by following [`epic.md`](epic.md) end to end — unless you were given `allowEpic: false` (you are
  drafting an epic's child), in which case plan it as one ticket, and a non-atomic honest ≥ 5 there
  gets estimate 5, a `- Oversized-child: <why not atomic; suggested sibling split>` bullet and
  `agentFriendly: false`.

Set depth from the estimate. Keep a reporter-set priority, otherwise rank against the planned
backlog.

## Step 2 — Recon

- **Read the ticket's evidence attachments** (notes, findings, logs, transcripts, specs) before
  fixing scope. A description is often a summary of a longer attached list; scope against the
  union, let the attachment win where it lists more, and say in the plan how many items you scoped.
- **View screenshots** with the tracker adapter's image-extract capability; markdown image text shows
  no pixels.
- **Read the code** the ticket touches, its module, conventions and tests, and any
  `docs/solutions/` or `CONCEPTS.md`. Ground claims in real symbols (`path:line`); invent nothing.
- **Confirm upstream contracts exist** in the merged tree (a field, column, method, config key,
  another ticket's output). Absent ⇒ bring it into scope or depend on the ticket that delivers it. A
  Done ticket is not proof its code landed.
- **Re-measure every number and coordinate** the ticket hands you (`path:line`, counts, sizes) in the
  current tree; drop or re-cite what no longer resolves.
- **Re-triage** if recon shows the real size is ≥ 5.

## Step 3 — Decide what a reviewer would ask

Work the questions an interviewer would have asked — scope (trim gold-plating), architecture and
boundaries, code idioms, tests, performance, and a skeptical second read — and record your answers as
decisions in the plan.

## Step 4 — Open questions

Record only genuinely controversial forks, where a reasonable planner could have gone the other way.
They become the `openQuestions` you return and the plan's `## Open Questions` section (that exact
casing), and add the `agent-question` label.

## Step 5 — Draft

Drafting resolves through the first tier that succeeds:

```bash
node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" discover --core boss-plan --role draft --mode headless --json
```

(missing helper ⇒ treat it as `{"extensions":[],"skipped":[]}`). Record each `skipped` entry whose
`deliberate` is `false` as `extension <name>: skipped (<reason>)`. Create a run temp dir first:

```bash
RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-plan-run.XXXXXX")
echo "$RUN_TMP"
```

- **Tier 1 — draft extensions.** Run each one **inline in this context**, following the `SKILL.md`
  read from its descriptor's `skillPath` (resources resolve from `dir`) — never as a nested
  dispatch, and never through the Skill tool. Give each its own plan target
  (`<runTmp>/draft-<extension-name>/<basename of PLAN_PATH>`) and the envelope
  `{role: "draft", core: "boss-plan", context: {mode: "headless", planPath, ticket}, runTmp, outPath}`.
  One **succeeds** only when its envelope is valid **and** it wrote a non-empty plan at its own
  target; promote the first success to `PLAN_PATH`. Record every failure as a skip. Any success
  suppresses tiers 2 and 3.
- **Tier 2** — a host-native drafting command, if there is one.
- **Tier 3** — draft it yourself from Steps 2–4.

Write the plan to `PLAN_PATH`, remove `$RUN_TMP`, and stop there — do not start implementing.

**The plan** is free-form Markdown, scaled to the triage: whatever the implementer needs — the
problem, the approach, the changes, how it will be tested, what "done" means, risks, decisions.
No headings are required, except that it **ends with `## Original notes`**, whose body is copied from
`DESCRIPTION_SNAPSHOT_PATH` and runs to the end of the file (`plan-image-guard.mjs --require-verbatim`
checks it). Some things help the implementer and the guards:

- **One ticket, one PR.** If the plan enumerates two or more independently mergeable tasks, it is an
  epic — re-triage.
- **Acceptance criteria** as checkboxes (`- [ ] …`), each demonstrable by a test or gate. A criterion
  that is only true because a file needed no change is written
  `` - [ ] (verify-only) <the claim> — check: `<runnable command>` `` — the command in backticks, each
  `;`/`|`/`&&` segment starting with something runnable (not a bare path, not prose).
- **Premises** the plan rests on, written the same way (`- [ ] <fact> — check: `<command>``, at most
  one marked `(central)`). A `path:line` citation is repo-relative and carries a backticked token
  from that line, so it can be re-checked.
- When the ticket names a pattern that may recur, list the other sites you found and say which you
  fix and why.
- **Proof**: what evidence would show the change works (test output, or — for UI — the screens or
  flows a reviewer should see). If the repo has a capture harness, name what it should capture.
- A note that an agent will probably implement this unattended, so nothing is left to "ask the
  user".
- **Agent-friendly by default.** Only when an agent genuinely could not do it (physical access,
  credentials only a human holds, a product call that cannot be made unattended) add
  `## Why this needs a human` and return `agentFriendly: false`. Size is never the reason.
- Query-strip every upload URL anywhere in the plan. No tool-call scaffolding, wrapper tags or
  commentary before or after the plan.

## Step 6 — No secrets

The plan is visible to everyone with access to the ticket. Write no secrets, tokens, credentials,
connection strings, private keys, cookies, internal hostnames/IPs or customer PII — including in
`## Original notes`. Reference where a value lives (`[REDACTED: repo-root .env]`) instead.

## Step 7 — The description

The tracker description is a short pointer to the plan. Don't add a `- Dependencies:` line (the
orchestrator does). Template:

```markdown
## Summary

<2-4 sentences: what will change and why. The plan attachment has the detail.>

## Key changes

- `<repo-relative path>`: <what>

## Why this needs a human

- <needs-human only: the specific blocker(s) that put this beyond an autonomous agent. Omit this entire `## Why this needs a human` heading when the plan is agent-friendly.>

## Open Questions

- <Headless only, and only when >=1 genuinely controversial decision was recorded (see Step 4): one bullet per fork — the decision, the option chosen, the alternative, and one line on why it was genuinely balanced. Omit this entire `## Open Questions` heading when there are none.>

## Planning

- Contract: v1
- Agent-friendly: <yes | needs-human (see "Why this needs a human")>
- Plan attachment: `Implementation plan (<ISSUE-ID>)`

## Original notes

<verbatim prior description if the ticket had one — preserved, never discarded>
```

Build it on disk and append the snapshot — only its path crosses the return channel (which
HTML-escapes `<`, `>`, `&`):

```bash
# The `description` family in $BOSS_PLAN_TOOLBOX/plan-scratch-paths.mjs. A bare `mktemp` here would
# land outside the scratch contract entirely, where no cleanup and no TTL reap can ever see it.
BODY=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md"
cat >"$BODY" <<'DESCRIPTION_SUMMARY_WITHOUT_ORIGINAL_NOTES'
## Summary

<all sections through `## Original notes`, ending with one blank line after the heading>
DESCRIPTION_SUMMARY_WITHOUT_ORIGINAL_NOTES
cat "${DESCRIPTION_SNAPSHOT_PATH:?DESCRIPTION_SNAPSHOT_PATH unset}" >>"$BODY"
```

Keep `## Original notes` byte-for-byte from the snapshot, except that upload URLs are written
query-stripped (a signed one must be). Never replace an image with a placeholder; the tracker keeps
no history, so a dropped URL is gone. Use a fenced block for any literal whose surrounding whitespace
matters.

## Step 8 — Verify, then write the sentinel

Write a redacted, signature-stripped copy of the snapshot to
`.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.attachment-guard-orig.md` and check it and the
description (pass `--allow-empty-original` only when the snapshot really was empty):

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
# Add --allow-empty-original ONLY when the ticket description handed to you was genuinely empty.
SAFE_ORIG=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.attachment-guard-orig.md"   # the safe source you just wrote
NEW=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md"                    # the composed description ($BODY from Step 7)
node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$DESCRIPTION_SNAPSHOT_PATH" \
  --rewritten "$SAFE_ORIG" --require-safe-source
node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original "$SAFE_ORIG" --rewritten "$NEW" \
  --require-verbatim --require-unsigned-uploads
```

Then the contract check, in its own block:

```bash
BOSS_PLAN_ENV=; for d in "${BOSS_SKILLS_HOME:-}" "$HOME/.claude/skills" "$HOME/.codex/skills"; do if [ -f "$d/boss-plan/toolbox/boss-plan-env.sh" ]; then BOSS_PLAN_ENV="$d/boss-plan/toolbox/boss-plan-env.sh"; break; fi; done; [ -n "$BOSS_PLAN_ENV" ] || { echo "BLOCKED: installed boss skills missing or stale - run 'boss skills install'"; exit 1; }; . "$BOSS_PLAN_ENV"
NEW=".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md"   # the same composed description as above
node "$BOSS_PLAN_TOOLBOX/plan-contract-guard.mjs" --description "$NEW" --plan "$PLAN_PATH" --module-roots "$(git ls-tree --name-only HEAD | paste -sd, -)"
```

Fix anything either reports before writing `ok`. Any size or count you report must be one you just
measured (`stat`, `wc -c`, an enumerating search); otherwise say `unmeasured`.

Single ticket — `premises` lists at most `PREMISE_LIMIT` `{id, state}` entries (the state you
observed at recon) for tickets the plan relies on, `[]` when none:

```bash
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" draft ok \
  "$(jq -nc --arg p "$PLAN_PATH" --argjson premises '[]' '{planPath:$p,premises:$premises}')"
```

Epic — no plan file; the payload carries the epic's ids and artifact paths ([`epic.md`](epic.md)
lists what each must hold). Set `ISSUE_ID` to the real id and replace every placeholder with the
real children, plan paths, spec path and guard scratch paths:

```bash
ISSUE_ID="<ISSUE-ID>"   # the id the orchestrator handed you (the actual issue id, not the literal <ISSUE-ID>); no shell export exists, so initialize it here before the write
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" draft ok \
  "$(jq -nc \
    --arg id "$ISSUE_ID" \
    --arg childId "<child-id>" \
    --arg childPlan ".linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>-<slug>.md" \
    --argjson childIds '["<child-id>"]' \
    --argjson epicSpecPaths '[]' \
    --argjson guardScratchPaths '[".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md",".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.attachment-guard-orig.md",".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-new.md",".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<child-id>.image-guard-orig.md",".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<child-id>.attachment-guard-orig.md",".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<child-id>.image-guard-new.md",".linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>-<slug>.md.rejected"]' \
    '{epic:true, epicParentId:$id, childIds:$childIds, childPlanPaths:{($childId):$childPlan}, epicSpecPaths:$epicSpecPaths, guardScratchPaths:$guardScratchPaths}')"
```

Write nothing if you could not produce the plan: an absent sentinel is the safe outcome.

## Step 9 — Return bounded metadata

Strict JSON, repo-relative paths, never plan text:

```json
{
  "planPath": ".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md",
  "labels": ["improvement"],
  "agentFriendly": true,
  "estimate": 3,
  "priority": 3,
  "openQuestions": [],
  "descriptionSummary": { "path": ".linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.description.md" }
}
```

`labels` are content labels (`bug`, `feature`, `improvement`, `docs` — any spelling; the guard maps
them). `estimate` is 0/1/2/3/5. `priority` is 1–4. `descriptionSummary` is the Step 7 artifact by
reference (an inline string is still accepted for older extensions). The orchestrator derives
`agent-friendly` / `needs-human` and `agent-question` itself.

Epic runs return instead:

```json
{
  "outcome": "epic",
  "epicParentId": "<ISSUE-ID>",
  "childIds": ["<ISSUE-ID>"]
}
```

with `childIds` listing every child in topological order.

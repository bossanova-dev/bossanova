# Headless drafting brief (read by the dispatched subagent only)

This reference is the **brief for the single awaited drafting subagent** that the boss-plan
orchestrator dispatches on the headless (`BOSS_CRON=true`) path. The orchestrator passes you the
**path** to this file, the ticket data, the target plan path, and the sentinel run context — it
never pastes this text into its own context. **Nothing here runs on the orchestrator's main
thread.** The default headless orchestrator path reads neither this brief nor
`references/interactive-mode.md`.

You are drafting a complete, implementation-ready plan for one Linear ticket, unattended. No human
is watching — **never call `AskUserQuestion`.** Decide every fork yourself with reasonable defaults
grounded in the ticket and the codebase, and record the controversial ones as open questions (see
below). Keep the full recon + drafting context inside your own context; you return only a small
metadata object.

## Inputs the orchestrator hands you

- `ISSUE-ID`, `title`, and the ticket `description` (verbatim, may be empty).
- `PLAN_PATH` — the exact file to write the plan to: `.linear-plans/<ISSUE-ID>-<slug>.md`
  (gitignored scratch; the slug is the issue id + hyphenated title). The orchestrator computed it
  with
  `node -e "import('./scripts/plan-upload.mjs').then(m=>console.log(m.issueSlug(process.argv[1],process.argv[2])))" <ISSUE-ID> "<title>"`.
- `RUN_SENTINEL`, `RUN_DIR`, `RUN_ID` — the run-file sentinel context you write your terminal
  decision to (see "Write the terminal sentinel" below).

## Step 1 — Triage triviality

Read the title + description and classify:

- **TRIVIAL** — copy/doc tweak, a single obvious one-liner, no design decisions (e.g. "Mention
  setup scripts on the home page"). Use a lightweight plan.
- **SUBSTANTIAL** — anything with design choices, multiple files, or unknowns.

This classification sets how deep your recon and plan go, and informs the estimate. If the ticket
is TRIVIAL, keep the plan proportionate; do not manufacture complexity.

## Step 2 — Codebase recon

Read the code the ticket touches: the files, the surrounding module, the existing conventions,
tests, and any relevant `docs/solutions/` or `CONCEPTS.md`. Ground every plan claim in real symbols
(`file:line`) — do not invent constructor signatures, structs, helpers, or styles. This recon is
the whole reason the drafting is isolated in a subagent: it accumulates here, in your context, and
never lands on the orchestrator's main thread.

**View the reporter's screenshots.** When the ticket `description` carries image markdown
(`![](…)`), an HTML `<img>` tag, or an `uploads.linear.app`/attachment URL, an agent reading that
markdown as text does **not** see the pixels. Call `mcp__bossanova-linear__extract_images` on the
description markdown to actually view the reporter's screenshots — they often disambiguate what the
words alone leave ambiguous (the BOS-364 web-vs-TUI mix-up would have been resolved on sight).

## Step 3 — Work the self-review dimensions yourself

The Bossanova interactive path gets these dimensions from its draft extension; headless has no
human to answer them, so you work the same dimensions and answer each with a reasonable default,
recording the decision you would otherwise have surfaced:

- **Scope challenge** — is the ticket asking for the right thing? Trim gold-plating.
- **Architecture** — where does the change belong; what are the module boundaries and contracts.
- **Code quality** — the idioms and patterns the implementer should follow.
- **Tests** — what unit / integration / e2e coverage proves the change.
- **Performance** — any hot paths, allocations, or query costs to call out.
- **Outside-voice sanity check** — a skeptical second read: what would a reviewer object to?

Keep depth proportionate to triage. Carry these self-made decisions into the plan exactly as the
interactive path carries the interview's.

## Step 4 — Record controversial open questions (high bar)

Because no human is steering, track any decision that was genuinely **controversial** — a real fork
where a reasonable planner could have chosen the other option with comparable merit. Keep the bar
**high**: record only could-have-gone-either-way calls, never routine or obvious ones (low-bar
noise defeats the signal). These become the `openQuestions` you return and the plan's
`## Open Questions` section, and drive the `agent-question` label.

## Step 5 — Resolve drafting, then write the polished plan

First run `node scripts/skill-extensions.mjs discover --core boss-plan --role draft --json`. If
that helper is missing in an installed public skill payload, treat discovery as
`{"extensions":[],"skipped":[]}` so the portable fallback tiers still run.

- **Tier 1:** if a draft extension exists, load each discovered extension by its returned descriptor
  `name` via the Skill tool with
  `{ role: "draft", core: "boss-plan", context: { mode: "headless", planPath: PLAN_PATH, ticket },
runTmp, outPath }`. The extension works the dimensions and writes the plan inside this single awaited
  drafting context.
- **Tier 2:** if no extension exists and the host exposes a native drafting command, use it and
  normalize the result to the planContract sections in Step 7.
- **Tier 3:** if neither exists, draft directly from Step 3 plus the self-contained plan-body
  requirements below. This tier has no external skill dependency.

Whichever tier runs, **write the plan to `PLAN_PATH`** and **stop after saving the plan file**. Do
not continue into subagent-driven-development or executing-plans. We only want the plan document
here.

This section is the **shared drafting spec** for both modes — the interactive path
(`references/interactive-mode.md`) points here too. Include, in the plan body, all of the following
(scaled to triage):

- A first development step: **"Copy this plan to `docs/plans/<ISSUE-ID>-<slug>.md` and commit it in
  the implementation PR."**
- A **## Acceptance criteria** section: concrete, testable pass/fail conditions.
- A **## Required proof** section: a checklist of artifacts the implementer must produce to pass
  review, each paired with what it must demonstrate. Proof is captured with the existing `boss-proof`
  skill (`node scripts/proof.mjs run`), which uploads **stills and video** to proof.bossanova.dev and
  comments them on the PR. boss-proof captures **both today** — web/marketing MP4 via Playwright
  (`"capture": "video"`), and TUI via a committed `proof/scenarios/*.scenario.json` replay plus
  agent-frame video — across the TUI / web / marketing / docs surfaces classified by
  `node scripts/proof.mjs plan`. For UI/TUI/web work, name the recipes/scenarios and the expected
  visual evidence; for backend-only work, specify test output/logs and note "no screenshot
  applicable." Where a still image cannot show the behaviour (animation, multi-step flow), name a
  **video** recipe/scenario — not a "future" proof type.
  Scope each proof bullet to ONE surface by naming it (TUI / web / marketing / docs), and for a
  multi-flow demonstration write one bullet per scene/flow, each pairing the flow with 1–3 SHORT
  literal on-screen evidence tokens. A fresh-context judge grades the captured proof against these
  bullets (BOS-141): a bullet whose evidence is not visible in the run's media downgrades the
  verdict, so write bullets an independent reviewer can check against the stills — never internal
  claims a screen cannot show.
- A **## Proof harness analysis** section: before finalizing the proof plan, analyze whether the
  current proof harness can already prove this change and record the result. Specifically:
  - **classify proof-applicability** with the _same_ gate the implementer uses —
    `node scripts/proof.mjs plan` selects recipes/scenarios from the changed paths against
    `proof/recipes/default.json` `pathRules`. The change is proof-applicable **iff** it touches a
    capturable surface (a TUI scenario — existing or buildable in-PR per the gap bullet below — /
    product web / marketing / docs); otherwise state "proof not applicable" (backend-only Go, scripts,
    proto, prompt/markdown, pure config/types, or test-only diffs). A TUI (`services/boss`) change is
    proof-applicable even when no scenario is committed yet — the missing scenario is the buildable
    affordance, not grounds to classify it "not applicable";
  - state the **surfaces touched** and the **existing recipe/scenario coverage** — name the recipe
    ids or `proof/scenarios/*.scenario.json` files that already cover the surface (from
    `proof/recipes/default.json` and `proof/scenarios/`);
  - **map each acceptance criterion to a concrete proof artifact** — a recipe id, a
    `proof/scenarios/*.scenario.json` scene + fixture preset, or a stated "no proof applicable —
    tests/diff are the evidence";
  - where a needed affordance is **missing but buildable** (a scenario, a fixture preset, a route, a
    `data-testid`, a recipe), **schedule it as IN-PR work** so the plan ships the means to prove
    itself — this is exactly the affordance boss-build Step 5 then builds;
  - record any **external blocker**: a surface no affordance can reach unattended.
    Keep it headless-safe: **never call `AskUserQuestion`** — decide defaults and record only
    genuinely controversial forks as open questions.
- **Autonomous framing**: state explicitly that an autonomous agent will likely implement this
  unattended, so steps, acceptance criteria, and proof must be unambiguous and self-contained — no
  "ask the user" gaps.
- **Agent-friendliness call**: by default this plan is **agent-friendly**. Only if you judge that an
  autonomous agent genuinely _could not_ complete the task, add a **## Why this needs a human**
  section naming the specific blocker(s) that put it beyond an agent (e.g. requires physical/hardware
  access, manual external-account or vendor setup, credentials only a human holds, a product/design
  judgment call that cannot be made unattended, or real-world verification an agent cannot perform).
  Complexity alone is **not** a reason — a large but well-specced ticket is still agent-friendly.
  Omit this section entirely for agent-friendly plans, and set `agentFriendly: true` in your returned
  metadata (set it `false` and include the section when you do add it).

## Step 6 — Plan-body secret hygiene

The orchestrator uploads this plan to `proof.bossanova.dev`, a **public** bucket, and runs a hard
secret gate before doing so. Do not put yourself on the wrong side of that gate: write **zero**
secrets, tokens, credentials, connection strings, private keys, session cookies, internal
hostnames/IPs, or customer PII into the plan — not in your own prose, and not in the verbatim
`## Original notes` block (the most likely place a secret sneaks in). If the work needs a secret,
**reference where it lives** (e.g. "the Cloudflare token in repo-root `.env`") instead of inlining
the value. When in doubt, redact.

## Step 7 — Compose the description summary (byte-identical template)

Compose the Linear description block the orchestrator will write back **verbatim** and return it as
`descriptionSummary`. This template is the **byte-identical external contract** boss-build and
bs-sweep-plan consume — do not rename or drop sections. Do NOT add the `- Dependencies:` line — the
orchestrator appends that itself when it links conflicting dependencies.

The `- Contract: v<N>` bullet under `## Planning` stamps the version of this description-section
contract so consumers (boss-build, bs-sweep-plan) can validate compatibility. Keep it equal to
`planContract.version` in the repo-root `.boss-skills.json` (v1 today); the sync gate
`skills-toolbox/plan-contract.test.mjs` fails if the stamp and the config version disagree.

```markdown
## Summary

<2-3 sentences: what & why>

## Approach

- <chosen implementation approach, bulleted>

## Key changes

- <module/area>: <what>

## Testing

<unit / e2e / eval coverage the plan calls for>

## Risks / unknowns

- <bullets>

## Acceptance criteria

- [ ] <testable pass/fail condition>

## Required proof

- [ ] (<surface: TUI / web / marketing / docs>) <boss-proof recipe / artifact> — the flow it must
      demonstrate, with 1–3 short literal on-screen evidence tokens a reviewer can check against the stills

## Proof harness analysis

- Applicability: <proof-applicable (capturable surface, per `node scripts/proof.mjs plan`) | not applicable — tests/diff are the evidence> · Surfaces touched: <TUI / web / marketing / docs / backend-only> · Existing coverage: <recipe ids / scenario files, or none> · Gap → in-PR affordances: <scenario / fixture preset / route / data-testid / recipe to build, or none> · External blockers: <none | the surface no affordance can reach unattended>

## Why this needs a human

- <needs-human only: the specific blocker(s) that put this beyond an autonomous agent. Omit this entire `## Why this needs a human` heading when the plan is agent-friendly.>

## Open Questions

- <Headless only, and only when >=1 genuinely controversial decision was recorded (see Step 4): one bullet per fork — the decision, the option chosen, the alternative, and one line on why it was genuinely balanced. Omit this entire `## Open Questions` heading when there are none.>

## Planning

- Contract: v1
- Complexity: <fib> · Priority: <label> · Agent-friendly: <yes | needs-human (see "Why this needs a human")>
- Full plan: [<ISSUE-ID>-<slug>.md](URL)
- On implementation: copy the plan to `docs/plans/<ISSUE-ID>-<slug>.md` and commit it in the PR.

## Original notes

<verbatim prior description if the ticket had one — preserved, never discarded>
```

**Preserve every image reference VERBATIM.** When composing `## Original notes`, copy every image
reference the ticket carried — inline markdown `![alt](…)`, HTML `<img …>` tags, and bare
`uploads.linear.app`/attachment URLs — **byte-for-byte**, URLs intact. **Never** replace an image
with a `[screenshot: …]` text placeholder or any paraphrase: Linear does not expose description
history to agents, so the rewritten description is the only surviving copy of those URLs, and a
paraphrase destroys them permanently (this is the exact BOS-364 data loss). You MAY _additionally_
list the images under a `## Screenshots` bullet list in the plan body for the implementer's
convenience, but the original URLs must stay intact inside `## Original notes`. A mechanical
orchestrator-side guard (`scripts/plan-image-guard.mjs`) aborts the Linear write if any source image
is missing from your `descriptionSummary`, so a dropped image fails the whole run — do not let it.

## Step 8 — Write the terminal sentinel

Once the plan file is written and non-empty, record your terminal decision to the run-file sentinel
so the orchestrator can classify the dispatch outcome **from the file only** (never from your
returned prose):

```bash
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" draft ok \
  "$(jq -nc --arg p "$PLAN_PATH" '{planPath:$p}')"
```

Write this **only after** the plan file exists. If you cannot produce the plan, do **not** write an
`ok` sentinel — leave it absent so the orchestrator reads `missing` and takes the safe branch.

Before writing the sentinel, **self-verify image parity**: confirm every image URL in the ticket's
original description (inline `![](…)`, `<img>`, `uploads.linear.app`/attachment URLs) survives
verbatim in your `descriptionSummary`'s `## Original notes` (run `scripts/plan-image-guard.mjs`
against the two, or eyeball the URL set). Fix any drop before writing the `ok` sentinel — the
orchestrator's mechanical guard will otherwise abort the whole run.

## Step 9 — Return only bounded metadata (never the plan content)

Return a single small object — **never the plan file's content** (returning content re-inflates the
caller, defeating the isolation). The `descriptionSummary` block is the only substantial string you
return, and it is bounded (a summary, not the plan):

```
{
  planPath:      "<PLAN_PATH>",              // path only, never content
  labels:        ["improvement", ...],       // relevant content labels to union (NOT agent-friendly/needs-human/agent-question)
  agentFriendly: true | false,               // false => needs-human; drives the mutually-exclusive label
  estimate:      <fib 0|1|2|3|5|8>,
  priority:      <1|2|3|4>,
  openQuestions: ["<one line per recorded controversial fork>", ...],  // may be empty
  descriptionSummary: "<the composed `## Summary … ## Original notes` markdown block>"
}
```

The orchestrator derives the mutually-exclusive `agent-friendly`/`needs-human` label from
`agentFriendly`, and unions `agent-question` iff `openQuestions` is non-empty — the label-application
contract stays orchestrator-owned. Choose the estimate and priority per the guidance in SKILL.md
Phase 6.

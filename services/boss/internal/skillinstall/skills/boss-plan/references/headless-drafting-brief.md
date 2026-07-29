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
  `node -e "import('file://'+process.argv[3]+'/plan-slug.mjs').then(m=>console.log(m.issueSlug(process.argv[1],process.argv[2])))" <ISSUE-ID> "<title>" "${BOSS_PLAN_TOOLBOX:?}"`
  — the toolbox dir is passed in as an argument, re-derived in the calling block, so the command
  never depends on an inherited export.
- `RUN_SENTINEL`, `RUN_DIR`, `RUN_ID` — the run-file sentinel context you write your terminal
  decision to (see "Write the terminal sentinel" below).

## Step 1 — Triage triviality

Read the title + description and classify:

- **TRIVIAL** — copy/doc tweak, a single obvious one-liner, no design decisions (e.g. "Mention
  setup scripts on the home page"). Use a lightweight plan.
- **SUBSTANTIAL** — anything with design choices, multiple files, or unknowns.
- **EPIC** — the honest Fibonacci estimate is **≥ 5**, or the work otherwise spans **multiple
  independently-shippable PRs**, each a coherent deliverable mergeable on its own. **Estimate is the
  forcing function:** a single ticket may be estimated only `0/1/2/3`; an honest `5` is EPIC unless
  it is genuinely atomic & un-splittable (then it survives as one ticket with a recorded `- Atomic-5:`
  justification under `## Planning`); an `8` is **never** a single-ticket estimate. An epic still
  requires **≥ 2** genuinely separable children; if the honest estimate is `≤ 3` and you cannot
  articulate ≥ 2 independent PR-sized pieces it is `SUBSTANTIAL`, not EPIC. When EPIC, follow the
  decompose-and-auto-create flow below **unless** `allowEpic: false` was passed in your context (you
  are drafting a child of an epic — the recursion guard: never decompose again; draft this child as a
  normal single-ticket plan).

Compute the honest estimate **first** — it drives the triage above — then let it set how deep your
recon and plan go. Every planned ticket gets a non-null estimate. Honor a reporter-set priority; otherwise rank the ticket (and an
epic parent) against the current config-resolved planned (`stateName(config, 'planned')`) backlog. If the ticket is TRIVIAL, keep the plan
proportionate; do not manufacture complexity.

### Epic decompose-and-auto-create (headless, triage = EPIC)

No human confirms, so you auto-create the epic **behind the hard guards** (SKILL.md Phase 2.5 owns
the guards + ordering discipline; the deterministic core is `$BOSS_PLAN_TOOLBOX/plan-epic-lib.mjs`):

**Precondition — the source ticket MUST be unplanned.** Parent-repurpose-last and idempotent resume
(re-pick a stranded partial epic via the unplanned sweep — `list_issues` filtered to the
config-resolved `trackerConfigFor(config).states.unplanned` value, never the literal role word
`unplanned`) both assume
the original starts unplanned. If a headless cron named a planned/in-progress source and it triages
EPIC, **first `parseEpicSpecMarker` on its description**: a `boss-plan-epic-spec` marker means it is
an existing epic parent (a fully-built epic is flipped to planned but keeps the marker), so route to the
idempotent resume/no-op path (complete only missing children, or no-op if already built) — **never**
the single-ticket fallback, which would re-plan a finished epic as a normal buildable ticket. Only a
non-unplanned source **with no epic marker** falls back to a single-ticket `SUBSTANTIAL` plan (record
the reason). A non-unplanned parent would be invisible to the unplanned sweep and stranded on a
crash before the final flip.

1. **Draft a decomposition spec.** Decompose the feature along its **architectural seams,
   producer-before-consumer**: `contract → persistence → producer → read → ui`. Tag each child with
   its `layer` and set every `read`/`ui` child `blockedBy` the **producer** child that writes its rows
   — a read/ui child is not plannable without a producer that populates it (its own sibling child, or,
   when the producer already exists in the merged tree, an external `blockedBy`; the Step 2 recon
   confirms which). Never bundle "persistence + read" such that the producer can be dropped. Each child
   estimate must be **≤ `CHILD_MAX_ESTIMATE`=3** (a `5`/`8` child is rejected — decompose it further).
   `{ parent:{title,goal,keyChanges[],priority}, children:[{key,title,goal,keyChanges[],blockedByKeys[],estimate,priority,layer,agentFriendly,openQuestions[]}] }`
   — set each child `key` with `stableChildKey(child, seen)` (a deterministic title-derived slug, so a
   fresh-worktree retry re-derives the same keys and its resume markers still match) — and validate it
   locally: `validateDecomposition` + `assertAcyclic`, then run `validateLayering` (the soft
   producer-before-consumer check: its **warnings** are advisory — confirm each read/ui child's rows
   are written by a producer, or add the missing producer — but they never block the epic).
   **A drafting-bug guard failure** (a `blockedByKeys` cycle or a dangling ref) ⇒
   **fall back to a single-ticket plan and record the reason** — never emit a broken epic.
   **A size failure is different:** more than `EPIC_MAX_CHILDREN`=12 children, a child you cannot get to
   ≤ `CHILD_MAX_ESTIMATE`=3, or an honest ≥ 5 that will not separate into ≥ `EPIC_MIN_CHILDREN`=2
   PR-sized children ⇒ **`needs-human`** ("too large to auto-plan; split by hand"), **never** a single
   oversized ticket — the exact monolith this flow exists to avoid.
2. **Fully plan every child** by drafting each as a synthetic single ticket **with `allowEpic:
false`** (the recursion guard — a child is never itself decomposed), writing a full
   planContract-v1 plan per child. **Copy each child's exact, gate-validated plan Markdown into
   `planMarkdown` on its spec entry**, as well as the child plan's own `agentFriendly` verdict AND its
   `openQuestions` list back onto its spec entry** — `epicSpecMarker` defaults an **absent**
   `agentFriendly` to `true`, so a `needs-human` (`agentFriendly:false`) child left blank here would be
   persisted as agent-friendly and become eligible for unattended build; and it derives the child's
   `agentQuestion` (⇒ the `agent-question` label) from a non-empty `openQuestions`, so a child left
   blank loses its `agent-question` queue signal. **Then re-run `validateDecomposition()` on the completed spec
   (now carrying each `agentFriendly`) before the first tracker write** — Step 1 validated the spec
   _before_ these verdicts existed, so the `plan-epic-lib.mjs` non-boolean-`agentFriendly` guard only
   catches a malformed value (e.g. the string `"false"`, which `epicSpecMarker` would coerce to `true`)
   if validation runs again _after_ the copy. On failure, fall back to a single-ticket plan. Run the
   secret + image-parity gates on every child plan **before the first Linear write**
   (validate-before-write: a gate failure aborts with zero Linear writes).
3. **Create + wire (parent repurpose LAST — the write-atomicity guard).** These tracker writes run
   here in the drafting subagent, so make them crash-safe by ordering. **First** persist the FULL spec
   durably: **append** the `epicSpecMarker(spec)` `<!-- boss-plan-epic-spec:… -->` marker (the parent
   overview + every child's full metadata — key, title, goal, keyChanges, blockedByKeys, estimate,
   priority, **the child plan's gate-validated `planMarkdown`, `agentFriendly` call, and its
   `agentQuestion` decision** — so resume
   re-applies `agent-question`) to the original ticket's **existing**
   description with `save_issue` — **preserve the
   original bytes** (append/prepend the marker, never replace; it is an HTML comment, invisible, adding/
   removing no images), description-only so it does NOT move the ticket out of unplanned (parent-
   repurpose-last still holds). **Defense-in-depth — in this SAME first `save_issue`, strip any
   pre-existing `agent-friendly`/`needs-human` label and stale single-ticket `Implementation plan (…)`
   link or attachment** from the parent. For `tracker-attachment`, require `deletePlanAttachment`
   before this first epic write and delete stale matching attachment ids. The unplanned-source
   precondition means a well-formed parent has none, but `boss-build` selects exactly a planned ticket
   carrying `agent-friendly` + a plan artifact
   (`skills/boss-build/SKILL.md`), so stripping at the FIRST write guarantees even a mis-selected parent
   is non-`boss-build`-selectable from the very first tracker mutation onward rather than through the
   create→wire→expose window or after a crash (the step-7 flip's strip then only reaffirms it).
   Preserving the original description text is
   crash-safety: on a crash after this marker
   write but before repurpose, Linear still holds the **original description + marker**, so resume
   recovers the spec from the marker **and** reconstructs the verbatim `## Original notes` + runs the
   image-parity gate against the still-present original source. Then attach and create
   children in
   `topoOrderChildren` order. **Create each child as an unplanned, unexposed shell first** with the
   `save_issue` contract SKILL.md Phase 2.5 step 4 spells out: `parentId` = the original ticket, the
   config-resolved **unplanned** state `trackerConfigFor(config).states.unplanned` value (never the
   literal role word `unplanned`), each child spec's validated `estimate` and `priority`, a
   `<!-- boss-plan-epic-child:<key> -->` resume marker embedded in its description, and its content
   labels (**plus `agent-question` when that child's `openQuestions` is non-empty** — the Phase 4
   contract, unioned in), but **neither** `agent-friendly` nor `needs-human`. A repo may map the role
   to a differently-named workflow state, so passing `unplanned` verbatim can make the tracker reject
   the child or land it in the wrong state. **The child plan attachment's title MUST be exactly
   `Implementation plan (<child id>)`** (matching the single-ticket convention): `boss-epic`'s
   `normalizeTicket` recognizes a plan only via a link/attachment whose title **starts with**
   `Implementation plan`, so an attachment under any other title is exposed `agent-friendly` yet
   silently skipped by `boss-epic` as "missing a plan". **On resume, first inspect every adopted
   shell for that exact canonical attachment. If it is missing, reconstruct the child plan file from
   the persisted `planMarkdown` and run prepare → PUT → finalize before any planned-state or exposure
   write. An old marker without `planMarkdown` must instead redraft that same synthetic child with
   `allowEpic:false`, re-run the child secret and image-parity gates, and persist the renewed marker
   before attaching. Never create only missing metadata or expose an adopted shell whose attachment is
   absent.** Wire the intra-epic DAG via `epicWiringPlan`
   (intra-epic edges must come **exclusively** from `epicWiringPlan`; the siblings were just created in
   the unplanned state). Use the shell's returned id for `preparePlanAttachment` → helper PUT →
   `finalizePlanAttachment` with the exact title, **then** transition it to the config-resolved planned
   state `trackerConfigFor(config).states.planned` (never the literal role word `planned`). Only this
   post-finalization transition gives `boss-epic` the exact parent/planned shape it re-verifies before
   ready/merge ordering. Never expose a child or move its shell to planned before that sequence
   succeeds. **Defer the external conflict links until after the parent overview commits** (below): those
   outward edges mutate **non-epic** backlog tickets, so writing them before the parent gate would
   strand existing backlog work behind a child that a deterministic parent-gate failure leaves
   unexposed. **Gate, then
   SAVE the parent overview BEFORE exposing any child:** compose the parent overview now and run its two
   Phase 4 gates (secret + image-parity) FIRST — on a gate failure take the SAFE branch (no exposure, no
   parent write, abort), so a **deterministic** parent-gate failure (e.g. a dropped image or a secret in
   the parent's `## Original notes`) never leaves a child `agent-friendly`/`boss-build`-buildable while
   the parent aborts unplanned. The parent overview embeds the original description verbatim; its
   **secret gate** (redact any credentials/PII) and **image-parity gate** (`$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs`
   — every original image URL must survive verbatim in the parent overview's `## Original notes`; on a
   drop, no parent write, abort) run here. Only after both parent gates pass, attach the parent overview
   natively + save it onto the original ticket,
   re-appending the hidden `<!-- boss-plan-epic-spec:… -->` marker,
   but keep it unplanned** (a description-only save — defer the unplanned → planned repurpose flip to the
   very last write below). This is the durable **parent commit**, and it runs **before any child is
   exposed**: because attachment finalization and saving to Linear are the failure-prone writes, doing them here
   means a failing configured plan-store operation or Linear parent save surfaces **before** a single child becomes
   `agent-friendly`, never after — so an exposed child is always backed by an already-published parent,
   never one that later aborts unplanned. Re-appending the marker matters because this save replaces
   the description, so without re-appending, the earlier `epicSpecMarker` write is lost and idempotent
   resume can no longer recover the FULL spec from a fully-built parent, re-decomposing into DUPLICATE
   children instead of a clean no-op. **Only after the parent overview is saved, run the external
   conflict links** for each child against the active backlog — **excluding this epic's own child ids
   AND the epic parent id** from the comparison set (the external pass links each child only against
   non-epic tickets) — so a deterministic parent-gate abort writes **zero** external edges and never
   strands backlog work behind an unbuildable child. **Then stamp
   each child with its OWN plan's agent-friendliness call** (union, never clobber) now that each carries
   its blocker relations: a child whose plan concluded agent-friendly gets `agent-friendly`; a child
   whose plan concluded it **needs a human** (`agentFriendly: false`) gets `needs-human` instead —
   **never** `agent-friendly`. `boss-epic` treats a child as eligible only when it is planned **and**
   `agent-friendly` **and** has a plan artifact **and** is **not** `needs-human`, so honoring the per-child
   decision here keeps a human-blocked child out of `boss-build`. This deferred exposure is the moment
   an agent-friendly child becomes `boss-build`-eligible, and `boss-build`'s "skip a
   candidate whose blocker relations exist" then keeps blocked children from starting out of DAG order
   (any crash before exposure leaves children unexposed — no `agent-friendly`/`needs-human`, unbuildable;
   resume completes wiring + exposure). **Only then**, as the very last write, resolve the `epic` label
   through `labelName(config, 'epic')`, union it into the parent, and flip unplanned → planned with
   `estimate = epicParentEstimate(spec)` (the sum of its children's estimates) and
   `priority = parent.priority`. If the tracker rejects that non-Fibonacci sum, retry without estimate
   and warn. The parent carries neither `agent-friendly` nor `needs-human`
   — the overview + marker were already saved above. **Strip stale build metadata with this flip:** a
   headless sweep can pick an explicitly-named planned/in-progress ticket that was **already planned**,
   so the original may already carry `agent-friendly` **and** a single-ticket `Implementation plan (…)`
   link or attachment; a bare state flip would leave the epic parent `boss-build`-selectable, so **remove any
   pre-existing `agent-friendly`/`needs-human` label and drop any stale single-ticket
   `Implementation plan (…)` link or attachment** from the parent with (or immediately before) the flip — its only
   plan artifact is the epic overview from above. Preserve the recorded new parent-overview attachment
   id while deleting stale matching attachments. Because this
   unplanned → planned flip is **last**, the parent stays
   unplanned until the epic is fully wired + exposed: a crash or malformed sentinel at any earlier
   point (partial child create, unsaved parent overview, or unexposed children) leaves the original
   ticket unplanned, so the next headless sweep re-picks it and resumes
   — the orchestrator's safe-branch abort is honest at the sweep level (no epic is stranded), even
   though this single run wrote to Linear before its sentinel landed.
   **Idempotent resume (durable — survives a fresh cron worktree where the `.linear-plans/` scratch is
   gone):** `get_issue` the parent and `parseEpicSpecMarker` its description to recover the **FULL
   original spec** (parent overview + every child's full metadata) from the
   `<!-- boss-plan-epic-spec:… -->` marker; then enumerate the parent's existing children with
   `list_issues parentId=<parentId> limit=250` (the same op `boss-epic` uses — `get_issue` on the
   parent does not return the children's descriptions where the marker lives) and adopt any
   already-created child by the `<!-- boss-plan-epic-child:<key> -->` marker in its description
   (preferred over title, which may collide). Create only the spec keys not yet present — **drafting
   each missing child from its persisted metadata and wiring it per the persisted `blockedByKeys`,
   never a fresh re-decomposition** — then finish wiring + **deferred exposure** + repurpose. For an
   adopted child that was created but not yet exposed (a crash in the create→expose window), its
   `.linear-plans/` plan is gone, so **read its `agentFriendly` from the recovered spec marker** to
   decide the exposure label — a persisted `agentFriendly:false` is re-stamped `needs-human`, never
   `agent-friendly`. **Already-saved parent overview:** because the parent-overview save above is
   description-only and keeps the parent unplanned (the unplanned → planned flip is the very last
   write), a crash in that window re-picks an unplanned parent whose current description is **already
   the composed overview** (`## Original notes` + child checklist + marker), not the reporter's raw
   notes. So on resume **detect an already-saved overview and reuse it verbatim** — never recompose
   `## Original notes` from the transformed description, which would nest the overview / re-embed the
   notes / trip the image-parity gate (the saved overview already embeds the verbatim original notes and
   already passed both gates on the first run); then **run the deferred external conflict links
   BEFORE stamping any child buildable** (a crash could have landed after the parent save but before
   that pass, so the normal-flow ordering — parent commit → external links → exposure — must hold on
   resume too, else an agent-friendly root child is exposed without blocking overlapping active backlog
   work; the links are append-only, so re-running them is a safe no-op for edges already written), and
   finally finish the missing child exposure and the unplanned → planned flip. Complete only what is
   missing from the original spec, never duplicate.

## Step 2 — Codebase recon

Read the code the ticket touches: the files, the surrounding module, the existing conventions,
tests, and any relevant `docs/solutions/` or `CONCEPTS.md`. Ground every plan claim in real symbols
(`file:line`) — do not invent constructor signatures, structs, helpers, or styles. This recon is
the whole reason the drafting is isolated in a subagent: it accumulates here, in your context, and
never lands on the orchestrator's main thread.

**View the reporter's screenshots.** When the ticket `description` carries image markdown
(`![](…)`), an HTML `<img>` tag, or an `uploads.linear.app`/attachment URL, an agent reading that
markdown as text does **not** see the pixels. Call the tracker adapter's image-extract capability on
the description markdown to actually view the reporter's screenshots — they often disambiguate what the
words alone leave ambiguous (a prior web-vs-TUI mix-up would have been resolved on sight).

**Verify every cited upstream contract exists.** For each upstream artifact this plan will build
against — a GraphQL/proto field, a DB column or migration, a service method, a config key, a data
pipeline stage, another ticket's output — confirm it **already exists in the merged tree** (grep/read
it). If it is absent, either pull it into this ticket's scope or add a hard `blockedBy` on the ticket
that must deliver it. **Never author a step against an artifact you did not confirm exists** — a
`Done` upstream ticket is not proof its contract actually landed; the code in the merged tree is. This
is what stops a consumer plan from being written against an assumed-but-absent column/field/service.

**Re-triage after recon.** Recon is where the true size surfaces. Recompute the honest estimate now;
if it is **≥ 5** and you are not already on the EPIC path, **re-triage to EPIC** (Step 1's
decompose-and-auto-create) — or, only for a genuinely atomic & un-splittable 5, record the
`- Atomic-5:` justification. A ticket that looked SUBSTANTIAL but recon proved is a 5+ must not be
planned as one oversized ticket.

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

First run `node "$BOSS_PLAN_TOOLBOX/skill-extensions.mjs" discover --core boss-plan --role draft --json`. If
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
(`references/interactive-mode.md`) points here too.

**One ticket = one atomic PR.** A single-ticket plan describes **one** atomically-shippable change.
If the plan you are drafting naturally enumerates **≥ 2** independently-landable tasks (a `Task 1..N`
list whose tasks could each merge on their own), that is the EPIC signal — stop and **re-triage to
EPIC** (Step 1's decompose-and-auto-create), do not emit the multi-task single ticket. Multi-task
scaffolding is legal only **across epic children**, never inside one ticket's plan. This is what stops
`boss-build` from landing the cheap slice of a "Task 1..N" ticket and closing it partial.

Include, in the plan body, all of the following (scaled to triage):

- A first development step: **"Copy this plan to `docs/plans/<ISSUE-ID>-<slug>.md` and commit it in
  the implementation PR."**
- A **## Acceptance criteria** section: concrete, testable pass/fail conditions.
- A **## Required proof** section: a checklist of artifacts the implementer must produce to pass
  review, each paired with what it must demonstrate. Proof is captured with the existing `boss-proof`
  skill (`node scripts/proof.mjs run`), which uploads **stills and video** to the configured public publish store and
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
  bullets: a bullet whose evidence is not visible in the run's media downgrades the
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

The orchestrator attaches this plan natively to the tracker ticket and runs a hard
secret gate before doing so. The attachment is visible to everyone with access to that ticket. Do not put yourself on the wrong side of that gate: write **zero**
secrets, tokens, credentials, connection strings, private keys, session cookies, internal
hostnames/IPs, or customer PII into the plan — not in your own prose, and not in the verbatim
`## Original notes` block (the most likely place a secret sneaks in). If the work needs a secret,
**reference where it lives** (e.g. "the deployment token in repo-root `.env`") instead of inlining
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
- Atomic-5: <ONLY when a single ticket is estimated `5` — the explicit reason it is atomic & un-splittable and cannot be an epic. Omit this bullet for `0/1/2/3` tickets.>
- Plan attachment: `Implementation plan (<ISSUE-ID>)` (finalized native attachment id: <ATTACHMENT-ID>)
- On implementation: copy the plan to `docs/plans/<ISSUE-ID>-<slug>.md` and commit it in the PR.

## Original notes

<verbatim prior description if the ticket had one — preserved, never discarded>
```

**Preserve every image reference VERBATIM.** When composing `## Original notes`, copy every image
reference the ticket carried — inline markdown `![alt](…)`, HTML `<img …>` tags, and bare
`uploads.linear.app`/attachment URLs — **byte-for-byte**, URLs intact. **Never** replace an image
with a `[screenshot: …]` text placeholder or any paraphrase: Linear does not expose description
history to agents, so the rewritten description is the only surviving copy of those URLs, and a
paraphrase destroys them permanently (this is the exact screenshot-dropping data-loss failure). You MAY _additionally_
list the images under a `## Screenshots` bullet list in the plan body for the implementer's
convenience, but the original URLs must stay intact inside `## Original notes`. A mechanical
orchestrator-side guard (`$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs`) aborts the Linear write if any source image
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

**Epic variant.** On an EPIC run (Step 1 triage = EPIC, decompose-and-auto-create completed) there is
NO single-ticket plan file — you already did every Linear write yourself. Write a **distinct** epic
sentinel: still `kind`/status `ok`, but the payload marks it an epic and carries **no** `planPath`, so
the orchestrator branches past Phase 3.5 / Phase 4 straight to cleanup + report:

```bash
ISSUE_ID="<ISSUE-ID>"   # the id the orchestrator handed you (the actual issue id, not the literal <ISSUE-ID>); no shell export exists, so initialize it here before the write
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" draft ok \
  "$(jq -nc --arg id "$ISSUE_ID" '{epic:true, epicParentId:$id}')"
```

`ISSUE_ID` is **not** a shell variable the orchestrator exports (its input is the placeholder
`<ISSUE-ID>`), so set it explicitly to the actual issue id above — otherwise `$ISSUE_ID` is unset,
the sentinel records `epicParentId:""`, and the orchestrator's epic reverify reads an empty parent
id and fails/cleans up as a failed run even though the parent was already repurposed. Write it only
after the epic is fully created + wired and the parent repurposed. If the epic guards
failed and you fell back to a single-ticket plan, write the single-ticket sentinel above instead.

Before writing the sentinel, **self-verify image parity**: confirm every image URL in the ticket's
original description (inline `![](…)`, `<img>`, `uploads.linear.app`/attachment URLs) survives
verbatim in your `descriptionSummary`'s `## Original notes`. Run the guard — re-deriving the toolbox
dir in the same block, because this Bash call inherits nothing:

```bash
BOSS_PLAN_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills/bossanova}/boss-plan/toolbox"
if [ ! -d "$BOSS_PLAN_TOOLBOX" ]; then BOSS_PLAN_TOOLBOX="$HOME/.codex/skills/bossanova/boss-plan/toolbox"; fi
# Add --allow-empty-original ONLY when the ticket description handed to you was genuinely empty.
node "$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs" --original <orig.md> --rewritten <new.md>
```

The guard **refuses** an empty or whitespace-only original (exit 1, `cannot verify image parity`)
rather than certify a comparison it cannot make. Your `description` input may legitimately be empty
(see Inputs), and that is the one case where the refusal is a false alarm: pass
`--allow-empty-original` then, and only then. If the description was **not** empty, an empty
`<orig.md>` means your own extraction broke — fix the extraction, never silence it with the flag.

Fix any drop before writing the `ok` sentinel — the orchestrator's mechanical guard will otherwise
abort the whole run.

## Step 9 — Return only bounded metadata (never the plan content)

Return a single small object — **never the plan file's content** (returning content re-inflates the
caller, defeating the isolation). The `descriptionSummary` block is the only substantial string you
return, and it is bounded (a summary, not the plan):

```
{
  planPath:      "<PLAN_PATH>",              // path only, never content
  labels:        ["improvement", ...],       // relevant content labels to union (NOT agent-friendly/needs-human/agent-question)
  agentFriendly: true | false,               // false => needs-human; drives the mutually-exclusive label
  estimate:      <fib 0|1|2|3; a bare 5 ONLY for a recorded atomic & un-splittable single ticket — an 8 is never a single ticket, it becomes an epic or needs-human>,
  priority:      <1|2|3|4>,
  openQuestions: ["<one line per recorded controversial fork>", ...],  // may be empty
  descriptionSummary: "<the composed `## Summary … ## Original notes` markdown block>"
}
```

The orchestrator derives the mutually-exclusive `agent-friendly`/`needs-human` label from
`agentFriendly`, and unions `agent-question` iff `openQuestions` is non-empty — the label-application
contract stays orchestrator-owned. Choose the estimate and priority per the guidance in SKILL.md
Phase 6.

**Epic outcome.** For an EPIC run, return the bounded **epic** object instead — NOT the single-ticket
shape (the single-ticket metadata does not apply: you already applied the per-child labels/estimate
on each child and left the parent deliberately unlabeled):

```
{
  outcome:      "epic",
  epicParentId: "<ISSUE-ID>",          // the repurposed original ticket
  childIds:     ["<ISSUE-ID>", ...]    // the created children, in topo order
}
```

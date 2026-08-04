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
the guards + ordering discipline; the deterministic core is `$BOSS_PLAN_TOOLBOX/plan-epic-lib.mjs`
plus this phase's own `$BOSS_PLAN_TOOLBOX/plan-epic-phase25.mjs` — `detectEpicParent`,
`epicSpecRecoveryGate`, `stalePlanAttachmentSweep`, `epicPhase25WritePlan` — and you never re-derive
either inline):

**Precondition — the source ticket MUST be unplanned.** Parent-repurpose-last and idempotent resume
(re-pick a stranded partial epic via the unplanned sweep — `list_issues` filtered to the
config-resolved `trackerConfigFor(config).states.unplanned` value, never the literal role word
`unplanned`) both assume
the original starts unplanned. If a headless cron named a planned/in-progress source and it triages
EPIC, **check BOTH spec stores before falling back** — one `get_issue(parent)` returns
`attachments[]` **and** `description`, so checking both costs **zero** extra calls. Hand that one
payload to `detectEpicParent(issue)`: it returns
`{isEpicParent, source, specAttachmentId, ambiguous, reasons}` and owns the store-specific presence
rule, the attachment-wins-over-legacy ordering, and the duplicate case — `ambiguous: true` means two
or more `Epic spec (…)` attachments ⇒ **abort loudly, never guess** which is current (a human
deletes all but one, then re-runs). Never re-derive that classification by hand. An `isEpicParent`
verdict means an existing epic parent (a fully-built epic is flipped to planned but keeps
its spec), so route to the idempotent resume/no-op path (complete only missing children, or no-op if
already built) — **never** the single-ticket fallback, which would re-plan a finished epic as a
normal buildable ticket. Read the spec with `parseEpicSpec` on the **attachment body**, never on a
description that merely contains it.
**Why the two stores are asymmetric:** an
`Epic spec (…)` attachment is created by nothing but this phase, so a present-but-unreadable one is
still proof; a **description** is reporter-writable prose, so a bare quoted
`<!-- boss-plan-epic-spec:` substring is NOT presence, and
treating it as such would brand a brand-new ticket an unreadable epic parent and abort it on every
sweep.
**Identity is checked on an attachment-sourced spec ONLY** — an
attachment body can be copied or mis-attached from another epic, so it must prove
`validateSpecIdentity(spec, <ISSUE-ID>)`. A legacy inline spec predates both `schemaVersion` and
`parentId`, so that check would reject it unconditionally; it is accepted on **provenance** instead —
it sits in the description of the ticket being resumed, the strongest binding that store offers.
That is weaker than it sounds (duplicating an issue copies its description), and is accepted only
because the legacy store is read-only, frozen and slated for removal. A legacy parse that succeeds is
trusted and recovered as-is; only a legacy parse _failure_ reaches the gate.
**Unreadable-spec recovery gate** — when the
spec cannot be read, or an attachment-sourced spec fails `validateSpecIdentity`, the decision is
`epicSpecRecoveryGate({parent, children, plannedState, epicLabel})`, which owns the ALL-of conjunct
set and names every failed conjunct separately for the abort log. Feed it the
`list_issues parentId=<parentId> limit=250` enumeration below; where that op omits each child's
attachments, read them per child — the one extra read this gate may make. Its `action` is only ever
`'noop'` (enumerate + no-op) or `'abort'`; **falling through to
the single-ticket path is forbidden** — deliberately not even expressible in that return type,
because it would re-plan a finished or partial epic as a normal
buildable ticket. An **unplanned** parent can never satisfy the planned-parent conjunct, so a corrupt spec
attachment on one aborts every sweep until a human intervenes — deliberate (re-decomposing would
duplicate children); the remediation is to delete the unreadable `Epic spec (…)` attachment so the
parent re-decomposes cleanly, or to repair its body. Accepted residual: the gate cannot detect a
child deleted outright, but that
failure is non-destructive (a partial epic is left alone, not corrupted). Only a non-unplanned
source with **neither** store present falls back to a single-ticket `SUBSTANTIAL` plan (record
the reason). A non-unplanned parent would be invisible to the unplanned sweep and stranded on a
crash before the final flip.

1. **Draft a decomposition spec.** Decompose the feature along its **architectural seams,
   producer-before-consumer**: `contract → persistence → producer → read → ui`. Tag each child with
   its `layer` and set every `read`/`ui` child `blockedBy` the **producer** child that writes its rows
   — a read/ui child is not plannable without a producer that populates it (its own sibling child, or,
   when the producer already exists in the merged tree, an external `blockedBy`; the Step 2 recon
   confirms which). Never bundle "persistence + read" such that the producer can be dropped. Each child
   estimate must be **≤ `CHILD_MAX_ESTIMATE`=3** (a `5`/`8` child is rejected — decompose it further).
   `{ parentId:"<ISSUE-ID>", parent:{title,goal,keyChanges[],priority}, children:[{key,title,goal,keyChanges[],blockedByKeys[],estimate,priority,layer,agentFriendly,openQuestions[]}] }`
   — `parentId` is the source ticket's own id and is **not optional**: `serializeEpicSpec` omits an
   absent id rather than inventing one, so an unset `parentId` ships a spec `validateSpecIdentity` can
   never bind. Set each child `key` with `stableChildKey(child, seen)` (a deterministic title-derived slug, so a
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
   planContract-v1 plan per child. The spec never carries plan bodies, so copy back only **the child
   plan's own `agentFriendly` verdict AND its
   `openQuestions` list onto its spec entry** — `serializeEpicSpec` defaults an **absent**
   `agentFriendly` to `true`, so a `needs-human` (`agentFriendly:false`) child left blank here would be
   persisted as agent-friendly and become eligible for unattended build; and it derives the child's
   `agentQuestion` (⇒ the `agent-question` label) from a non-empty `openQuestions`, so a child left
   blank loses its `agent-question` queue signal. **Then re-run `validateDecomposition()` on the completed spec
   (now carrying each `agentFriendly`) before the first tracker write** — Step 1 validated the spec
   _before_ these verdicts existed, so the `plan-epic-lib.mjs` non-boolean-`agentFriendly` guard only
   catches a malformed value (e.g. the string `"false"`, which `serializeEpicSpec` would coerce to `true`)
   if validation runs again _after_ the copy. On failure, fall back to a single-ticket plan. Run the
   secret + image-parity gates on every child plan **before the first Linear write**
   (validate-before-write: a gate failure aborts with zero Linear writes).
3. **Create + wire (parent repurpose LAST — the write-atomicity guard).** These tracker writes run
   here in the drafting subagent, so make them crash-safe by ordering. **First** persist the FULL spec
   durably — the spec is a **native attachment carrying plain JSON**, so what was one atomic
   `save_issue` is now an ordered write sequence, and that sequence is
   `epicPhase25WritePlan({parentId, spec, unplannedState, staleAttachmentIds, labelsToStrip})`
   (`labelsToStrip` = the `agent-friendly`/`needs-human` exposure roles; **parent-scoped** — it is
   stage 1's `stripLabels` and nothing else): execute its ops **in emitted order** — `label-strip`,
   then `spec-upload`, then `stale-delete`, then `create-children` — **minus any stage the
   preconditions below skip, and on a resume minus every child `reconcileEpicChildren` does NOT
   report `missing`** (it emits one `createChild` per SPEC child, never per missing child; executing
   those unfiltered on a resume duplicates every child that already exists). Each entry is
   `{stage, op, args, runtimeArgs}`, and **`args` is the statically-known subset only**, under the
   adapter's own key names — `runtimeArgs` names what only you can supply because it does not exist
   until the previous op ran (the prepare's `size`, the PUT's `file`/`uploadURL`/`headers`, the
   finalize's `assetUrl`). It owns that ordering
   (in particular that every destructive delete
   comes strictly after the spec upload); never re-derive it by hand. It emits **no child `labels`
   field**: a child's label set is not derivable from the spec (`serializeEpicSpec` persists
   `agentFriendly`/`agentQuestion`, never a `labels` array), so the content-labels +
   `agent-question` union at creation stays yours to apply. It does **not** own the stage
   preconditions below, which stay prose (SKILL.md Phase 2.5 owns the attachment contract:
   filename `epic-spec.json` / `specAttachmentFilename()`, MIME `application/json` /
   `SPEC_ATTACHMENT_MIME`, title `Epic spec (<ISSUE-ID>)` / `specAttachmentTitle(<ISSUE-ID>)` — which
   **must NOT start with `Implementation plan`**, or `bs-epic-lib.mjs`'s `normalizeTicket` mistakes
   it for the parent's plan artifact — body `serializeEpicSpec(spec)`, and identity by
   `validateSpecIdentity`, never title alone):
   - **Stage 1 — label strip only.** The entry carries `stripLabels` OUTSIDE `args`: `save_issue`
     has **no** "remove these labels" argument — its `labels` **replaces** the whole set — so read
     the parent's current labels (op `readLabels`), subtract `stripLabels`, and send the result as
     `labels`. Spreading a `removeLabels` key into the call would either error or silently send
     `{id}` alone, leaving the parent exposed. Removing any pre-existing
     `agent-friendly`/`needs-human` label from the parent is cheap, atomic, reversible, and
     enough on its own: `boss-build` selects a planned ticket carrying `agent-friendly` **and** a
     plan artifact (`skills/boss-build/SKILL.md`), so breaking one conjunct makes even a mis-selected
     parent non-selectable from the very first tracker mutation onward rather than through the
     create→wire→expose window or after a crash (the step-7 flip's strip then only reaffirms it).
     This is the safety write; **it must not delete anything.**
   - **Stage 2 — upload the spec, exactly once.** A
     finalize is **not** idempotent the way the description marker it replaces was — it mints a NEW
     attachment row every call — so **read `attachments[]` AND the description first, BOTH stores**
     (scoping this to attachments would miss a legacy parent, and the unplanned sweep landing here is
     the primary resume route, not just the named-source branch), applying detection's store-specific
     presence rule: an attachment counts when present, a description only when
     `parseEpicSpec(description)` returns a spec. Take these in order: **two
     or more `Epic spec (…)` attachments ⇒ abort loudly** (never guess which is current); otherwise
     **either** store present ⇒ **skip
     this stage**, and stage 3 with it (the step-7 flip re-runs that same prefix-scoped strip);
     discard the spec just drafted and continue on the idempotent resume path against **the stored**
     one (a crash after stage 2
     leaves exactly this state and the parent is still unplanned, so the sweep re-picks it here). A
     legacy-sourced resume writes **no** attachment — it keeps its inline marker, carried verbatim
     through the step-6 save. Otherwise upload — first **set `spec.parentId` to this ticket's id**,
     since only a bound spec can pass `validateSpecIdentity`. That PUT takes a **file**, so write `serializeEpicSpec(spec)` to
     `.linear-plans/<ISSUE-ID>.epic-spec.json` — the exact path SKILL.md's Phase 5 cleanup matches by
     name. **Verify those bytes BEFORE the PUT:**
     `validateSpecIdentity(parseEpicSpec(<the file's contents>), <ISSUE-ID>)` must be `ok` — nothing
     else catches an unbound spec, since `serializeEpicSpec` drops an unset `parentId` silently and
     `validateDecomposition` never inspects it, so an unbound attachment would upload clean and only
     turn fatal on a later resume; `ok:false` ⇒ abort while zero children exist. In all other
     respects it is prepare → PUT → finalize through
     the **same** mechanism `references/plan-storage.md` steps 1–4 define (including the
     `uploadRequest.headers` scratch file and its immediate deletion after the PUT), substituting
     that contract's filename, MIME type and title — never a second hand-rolled upload path. It
     carries the parent overview + every child's full metadata (key, title, goal, keyChanges,
     blockedByKeys, estimate, priority, **`agentFriendly` call and its `agentQuestion` decision** — so
     resume re-applies `agent-question`) and never a plan body. **Any** failure takes the SAFE
     branch: abort with **zero children created**, the parent still unplanned, so the next unplanned
     sweep re-picks it.
   - **Stage 3 — the deferred destructive strip.** Its `staleAttachmentIds` come from the same
     prefix-scoped sweep the final flip cites, in its **one-arg** form — no parent-overview
     attachment exists yet, so there is nothing to keep; any stale single-ticket
     `Implementation plan (…)`
     **link** is dropped here too. For
     `tracker-attachment`, `deletePlanAttachment` was already required **before the FIRST epic
     write** — not here: this stage is skipped on every resume, so gating on its availability at
     this point would defer the check to the final flip, past child creation and `agent-friendly`
     exposure, stranding buildable children under an unplanned parent on an adapter that lacks it.
     A crash between stages 1 and 2 leaves only a removed label — recoverable and
     non-destructive; resume just re-decomposes from scratch with nothing orphaned.

   None of the three rewrites the description or moves the ticket out of unplanned (parent-repurpose-
   last still holds), which is itself crash-safety: after stage 2 Linear holds the **original
   description AND — unless stage 2 was skipped because a store already existed — the spec
   attachment**, so resume
   recovers the spec from whichever store holds it **and** reconstructs the verbatim `## Original notes` + runs the
   image-parity gate against the still-present original source. Then attach and create
   children in
   `topoOrderChildren` order. **Create each child as an unplanned, unexposed shell first** with the
   `save_issue` contract SKILL.md Phase 2.5 step 4 spells out: `parentId` = the original ticket, the
   config-resolved **unplanned** state `trackerConfigFor(config).states.unplanned` value (never the
   literal role word `unplanned`), each child spec's validated `estimate` and `priority`, an
   `epicChildMarker(key)` resume marker — its canonical emitter, never a hand-written literal
   comment, so the writer here and the resume-side reader share one definition — embedded in its
   description, and its content
   labels (**plus `agent-question` when that child's `openQuestions` is non-empty** — the Phase 4
   contract, unioned in), but **neither** `agent-friendly` nor `needs-human`. A repo may map the role
   to a differently-named workflow state, so passing `unplanned` verbatim can make the tracker reject
   the child or land it in the wrong state. **The child plan attachment's title MUST be exactly
   `Implementation plan (<child id>)`** (matching the single-ticket convention): `boss-epic`'s
   `normalizeTicket` recognizes a plan only via a link/attachment whose title **starts with**
   `Implementation plan`, so an attachment under any other title is exposed `agent-friendly` yet
   silently skipped by `boss-epic` as "missing a plan". **On resume, first inspect every adopted
   shell for that exact canonical attachment. If it is missing, **always redraft** that synthetic
   child from its persisted spec metadata with `allowEpic:false`, re-run the child secret and
   image-parity gates, and run prepare → PUT → finalize before any planned-state or exposure write.
   Plan bodies are never persisted in the spec, so this unconditional redraft is the single
   documented path, not a size-triggered fallback. Never create only missing metadata or expose an
   adopted shell whose attachment is absent.** Wire the intra-epic DAG via `epicWiringPlan`
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
   but keep it unplanned** (a description-only save — defer the unplanned → planned repurpose flip to the
   very last write below). This is the durable **parent commit**, and it runs **before any child is
   exposed**: because attachment finalization and saving to Linear are the failure-prone writes, doing them here
   means a failing configured plan-store operation or Linear parent save surfaces **before** a single child becomes
   `agent-friendly`, never after — so an exposed child is always backed by an already-published parent,
   never one that later aborts unplanned. For an **attachment-sourced** spec the old
   re-append-the-marker requirement is **obsolete**,
   not dropped by accident: that spec lives outside the description, so this description-replacing save
   cannot lose it and idempotent resume still recovers the FULL spec from a fully-built parent
   instead of re-decomposing into DUPLICATE children. **A LEGACY-sourced resume is the exception** —
   its spec _is_ the inline `<!-- boss-plan-epic-spec:… -->` marker, so carry that marker substring
   **verbatim** into the composed overview or this save destroys the only store, leaving the parent
   with neither and the next sweep re-decomposing into duplicates. Carry it; never migrate it to an
   attachment instead. **Only after the parent overview is saved, run the external
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
   — the overview was already saved above. **Strip stale build metadata with this flip:** a
   headless sweep can pick an explicitly-named planned/in-progress ticket that was **already planned**,
   so the original may already carry `agent-friendly` **and** a single-ticket `Implementation plan (…)`
   link or attachment; a bare state flip would leave the epic parent `boss-build`-selectable, so **remove any
   pre-existing `agent-friendly`/`needs-human` label and drop any stale single-ticket
   `Implementation plan (…)` link or attachment** from the parent with (or immediately before) the flip — its only
   plan artifact is the epic overview from above. Delete exactly the ids
   `stalePlanAttachmentSweep(attachments, {keepAttachmentId: <the new parent-overview id>})` returns:
   its predicate is prefix-scoped and nothing else, deliberately, because an
   unscoped sweep would silently destroy the `Epic spec (…)` attachment on this final flip. Because this
   unplanned → planned flip is **last**, the parent stays
   unplanned until the epic is fully wired + exposed: a crash or malformed sentinel at any earlier
   point (partial child create, unsaved parent overview, or unexposed children) leaves the original
   ticket unplanned, so the next headless sweep re-picks it and resumes
   — the orchestrator's safe-branch abort is honest at the sweep level (no epic is stranded), even
   though this single run wrote to Linear before its sentinel landed.
   **Idempotent resume (durable — survives a fresh cron worktree where the `.linear-plans/` scratch is
   gone):** `get_issue` the parent and `parseEpicSpec` the **body of its `Epic spec (<ISSUE-ID>)`
   attachment** (two or more `Epic spec (…)` attachments ⇒ **abort loudly**, never guess) to recover
   the **FULL original spec** (parent overview + every child's full
   metadata), then **`validateSpecIdentity(spec, <ISSUE-ID>)` it** — attachment-sourced specs only,
   here too and not just on the
   named-source branch above: the attachment was selected by title, and title alone is not identity.
   A failure takes the unreadable-spec recovery gate, never a silent resume against another epic's
   spec. **Legacy store (the one description read that remains):** a parent written by an
   earlier build carries the spec inline as a `<!-- boss-plan-epic-spec:… -->` description marker
   instead, and `parseEpicSpec` falls back to that form, so such an epic is still recognised and
   recovered. Then enumerate the parent's existing children with
   `list_issues parentId=<parentId> limit=250` (the same op `boss-epic` uses — `get_issue` on the
   parent does not return the children's descriptions where the child markers live) and join them against
   the spec with `reconcileEpicChildren(spec, liveChildren)` — never adopt by eye, never by title —
   which matches each live child's `epicChildMarker(key)` marker in its description against
   `spec.children[].key` and returns `{adopted, missing, orphans, unmarked, repairs, errors}`. This
   exists because the per-child marker and the parent spec's `children[].key` are two independent
   stores that can drift apart: a child gets retitled after creation, and if the parent spec is later
   regenerated from the new title, resume finds no live child for the now-stale spec key and would
   otherwise create a duplicate — a second plan attachment and a second branch/PR downstream. The
   function's invariant is `liveKeys ⊆ specKeys` in an undrifted epic, so a live child whose marker key
   is NOT a spec key (an **orphan**) is a zero-false-positive drift signal, never a false alarm. Three
   outcomes: **(1) aligned** (no orphans) — create exactly the spec keys `missing` names; **(2)
   unambiguous rename** (`repairs` holds exactly one `{specKey, liveKey, id}`, reading
   `missing.length === 1 && orphans.length === 1` as one retitled child — reported in `repairs` for the
   run log rather than applied silently, since in principle it could be two unrelated facts) — adopt
   that child and rewrite **its own** description marker to `epicChildMarker(specKey)` — replacing only
   the marker substring and **preserving the rest of that description's bytes verbatim**, since the save
   replaces the description, so a bare `save_issue(id, description: epicChildMarker(specKey))` would
   wipe the child's `descriptionSummary` plan body — the artifact Phase 4's secret and image-parity
   gates produced and the one a downstream build agent reads. Repair the
   CHILD, never the spec key: `specKey` is the namespace `adopted` reports the repaired child under, and
   the one every sibling's `blockedByKeys` and `epicWiringPlan` resolve through, so re-pointing the spec
   at `liveKey` would leave those refs dangling and throw mid-wire, AFTER children already exist — the
   half-built state this phase's validate-before-write design exists to prevent. Rewriting the child's
   marker instead leaves the next resume aligned. Create nothing for it; **(3) ambiguous drift** (`ok:false` — multiple orphans, an unmarked child, duplicate live marker
   keys, or a non-array `liveChildren`) — take the SAFE branch: report `errors`, write nothing, create
   nothing, never guess. A refusal must never be read as "no children exist": that degrade would report
   the entire spec as missing and duplicate the whole epic. Create only the spec keys `missing` names —
   **drafting each missing child from its persisted metadata and wiring it per the persisted
   `blockedByKeys`, never a fresh re-decomposition** — then finish wiring + **deferred exposure** +
   repurpose. For an
   adopted child that was created but not yet exposed (a crash in the create→expose window), its
   `.linear-plans/` plan is gone, so **read its `agentFriendly` from the recovered spec** to
   decide the exposure label — a persisted `agentFriendly:false` is re-stamped `needs-human`, never
   `agent-friendly`. **Already-saved parent overview:** because the parent-overview save above is
   description-only and keeps the parent unplanned (the unplanned → planned flip is the very last
   write), a crash in that window re-picks an unplanned parent whose current description is **already
   the composed overview** (`## Original notes` + child checklist), not the reporter's raw
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

- **Tier 1:** if a draft extension exists, dispatch each discovered extension with
  `{ role: "draft", core: "boss-plan", context: { mode: "headless", planPath: PLAN_PATH, ticket },
runTmp, outPath }`. Load the extension by **reading the descriptor's `skillPath` from disk**
  (`dir` is its directory), passing both `skillPath` and `dir` in the worker brief, and requiring
  relative extension resources to resolve from `dir`. Pass that `SKILL.md` content into the dispatch
  as the extension's instructions. Never load a discovered extension by its bare descriptor `name` through the Skill tool:
  extension skills are dispatched explicitly, never model-matched, so they SHOULD declare
  `disable-model-invocation: true`, and the Skill tool refuses such a skill.
  The extension works the dimensions and writes the plan inside this single awaited
  drafting context.

  **Per-dispatch plan target.** The `planPath` you pass is **not** `PLAN_PATH` itself: give each
  dispatch its own path under `runTmp` (`<runTmp>/draft-<extension-name>/<basename of PLAN_PATH>`),
  unique to the dispatch you are about to classify, and create its parent directory before
  dispatching. You promote the winner yourself: copy the file produced by the **first** dispatch that
  succeeded under the predicate below to `PLAN_PATH`, which is the plan target every later step reads
  and the one tiers 2 and 3 write directly. A later sibling never overwrites a promoted plan.

  **Draft success predicate** — one definition, used by every tier gate below. A dispatched draft
  extension **succeeded** only when both of these hold: it returned a result envelope valid for the
  requested dispatch, **AND** the requested plan now exists and is non-empty at this dispatch's own
  `planPath`, the copy promoted to `PLAN_PATH`, **written by this dispatch**. Verify
  the second conjunct yourself by reading that `planPath` after the dispatch returns; a valid envelope
  that wrote no plan did **not** succeed. The per-dispatch target is what makes that read an
  attribution: hand every sibling the **same** `PLAN_PATH` and "a plan is there now"
  is a test of shared state, not of this extension, so the first extension to
  write a plan silently credits every sibling dispatched after it, which is the false success this
  predicate exists to prevent. Do not try to rescue a shared path by comparing it before and after
  instead — neither half of that comparison holds across arbitrary projects and filesystems, which is
  where these skills run. Identical bytes are the ordinary output of a deterministic redraft, so a
  byte comparison records a real dispatch as a skip and drops the run to a lower tier that overwrites
  its plan; and the modification time need not advance either, because a filesystem whose timestamp
  resolution is coarser than the rewrite stamps both writes the same. Attribution has to come from
  the target you chose. Anything else is a failed dispatch: record
  `extension <name>: skipped (<reason>)` for that extension as you classify it — every failed
  dispatch is recorded, including when a sibling succeeded, so the ledger shows the whole Tier-1
  outcome and not just the winner.
  If at least one extension succeeded under the draft success predicate, tiers 2 and 3 do not run.
  If **no** extension succeeded under the draft success predicate — whether it failed to load,
  returned no valid envelope, or returned a valid envelope without producing the plan —
  fall through to Tier 2, then Tier 3 — the drafting layer is never silently dropped.

- **Tier 2:** if no extension succeeded under the draft success predicate and the host exposes a native drafting command, use it
  and normalize the result to the planContract sections in Step 7.
- **Tier 3:** if no extension succeeded under the draft success predicate and no host built-in exists, draft directly from
  Step 3 plus the self-contained plan-body requirements below. This tier has no external skill
  dependency.

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

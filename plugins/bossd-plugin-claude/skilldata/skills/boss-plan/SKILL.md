---
name: boss-plan
description: Plan a Linear backlog ticket. Grabs the next Unplanned issue by priority (or a ticket ID you provide), runs a full plan-eng-review interview plus superpowers:writing-plans, hosts the plan on proof.bossanova.dev, then writes a summary, labels, Fibonacci estimate, priority, and a plan link back to the issue and moves it Unplanned -> Todo. Internal Bossanova project skill. Interactive by default; runs fully headless when BOSS_CRON=true.
---

# BS Plan

Turn a vague, one-line Linear ticket into a fully-planned **Todo** with an
implementation-ready plan attached. Use when asked to "plan a linear ticket",
"plan the next ticket", "boss-plan", or given a ticket ID like `BOS-12`.

This skill is **interactive by default** — it drives `AskUserQuestion` through the full
`plan-eng-review`. Under `BOSS_CRON=true` it runs **fully headless**, dispatching a single awaited
subagent for recon + drafting (Phase 2), so it is safe to schedule unattended.

- **Leave no local artifacts.** At every terminal state, discard the scratch you created (gitignored dirs, seeded design docs, `mktemp` files) so the worktree is clean — in all modes, headless (`BOSS_CRON=true`) especially.

**Headless mode.** If `BOSS_CRON=true`, no human can answer `AskUserQuestion`, so **never call it** —
in the orchestrator or the subagent, at any phase. The default path: preflight → select the
ranked-queue head → **dispatch ONE awaited `general-purpose` drafting subagent** (Phase 2) → classify
its run-file sentinel → upload + write back to Linear. Make selections with reasonable defaults from
the ticket and codebase, discard local artifacts, and never block waiting for input.

## On-demand references (read only when the mode calls for it)

Mode-exclusive prose lives in `references/*.md`, loaded **only** on the path that needs it. The
**default headless orchestrator path reads neither** — the resident body carries the whole skeleton.

| Reference                               | Read it when…                                                                             |
| --------------------------------------- | ----------------------------------------------------------------------------------------- |
| `references/interactive-mode.md`        | Interactive `/boss-plan` only — Phase 1 confirm loop, design-doc seed, `plan-eng-review`  |
| `references/headless-drafting-brief.md` | Passed (by **path**) to the Phase 2 drafting subagent — never read by the orchestrator    |
| `references/extension-reviewers.md`     | Phase 3.5 — repo-local `boss-plan-*` extension plan-reviewers (additive; no-op when none) |

Workspace facts (do not re-discover):

- Linear MCP server: `linear-bossanova`. Team **Bossanova** (key `BOS`). `bossanova-dev` is the workspace, NOT a project — never pass a `project` filter.
- Statuses by name: `Unplanned` (start) and `Todo` (end).
- Existing labels: `agent-friendly`, `needs-human`, `agent-plan`, `agent-question`, `docs`, `bug`, `improvement`, `feature`. Never create labels. `agent-friendly` and `needs-human` are mutually exclusive (every plan gets exactly one).
- Linear priority numeric: `1=Urgent, 2=High, 3=Medium, 4=Low, 0=None`.
- Dependency links use Linear `blocks`/`blocked by`. A blocker is "cleared" only
  when its state is `Done` or `Canceled` (PR merged / work dropped); the
  blocking-aware logic is unit-tested in `scripts/linear-deps-lib.mjs`. boss-implement
  will not start a ticket blocked by an uncleared blocker.
- Proof/R2: Wrangler reads `CLOUDFLARE_ACCOUNT_ID`/`CLOUDFLARE_API_TOKEN` from repo-root `.env`
  implicitly. `BOSS_PROOF_R2_BUCKET=bossanova-proof-production` and
  `BOSS_PROOF_PUBLIC_BASE_URL=https://proof.bossanova.dev` are non-secret constants (the R2
  adapter's config) the skill exports itself, NOT `.env` vars. Publish + tracker write-back use
  pluggable adapters (`scripts/publish/`, `scripts/tracker/`; default `r2`/`linear`).

## Phase 0 — Preflight

1. Set `PUBLISH_ADAPTER="${PLAN_PUBLISH:-r2}"`. Only `r2` loads repo-root `.env` and requires
   `CLOUDFLARE_ACCOUNT_ID`/`CLOUDFLARE_API_TOKEN`; non-R2 adapters own preflight. Print only
   `set`/`MISSING`:
   ```bash
   PUBLISH_ADAPTER="${PLAN_PUBLISH:-r2}"
   if [ "$PUBLISH_ADAPTER" = "r2" ]; then
     set -a; . ./.env; set +a
     A=${CLOUDFLARE_ACCOUNT_ID:+set}; echo "CLOUDFLARE_ACCOUNT_ID=${A:-MISSING}"
     T=${CLOUDFLARE_API_TOKEN:+set};  echo "CLOUDFLARE_API_TOKEN=${T:-MISSING}"
   fi
   ```
   If `PUBLISH_ADAPTER=r2` and either credential prints `MISSING`, stop before Linear writes.
2. For `r2`, export non-secret proof config constants:
   ```bash
   if [ "${PLAN_PUBLISH:-r2}" = "r2" ]; then
     export BOSS_PROOF_R2_BUCKET=bossanova-proof-production
     export BOSS_PROOF_PUBLIC_BASE_URL=https://proof.bossanova.dev
   fi
   ```
3. Confirm the Linear MCP is reachable with a cheap read (`list_issue_statuses team=Bossanova`).

## Phase 1 — Select the issue

- **If the user gave a ticket ID** (e.g. `BOS-12`): call `get_issue` with it. Respect that choice
  regardless of status.
  - **Interactive:** if it is already `Todo`/`In Progress`/`Done`/`Canceled`, warn and confirm
    before re-planning (see `references/interactive-mode.md`).
  - **Headless (`BOSS_CRON=true`):** do not ask. A cron job that names a ticket means to plan that
    ticket, so proceed regardless of status — but if it is `Done`/`Canceled`, log a warning and
    **stop** (re-planning finished work unattended is almost never intended) rather than blocking.
- **Otherwise**: `list_issues team=Bossanova state=Unplanned limit=250`. **Rank the whole queue** by
  **priority**, reading Linear's numbers correctly: Urgent(1) > High(2) > Medium(3) > Low(4)
  > None(0). Tie-break by **oldest `createdAt` first**. Keep this ranked list.
  - **Interactive:** show the head of the ranked queue and run the confirm loop (**plan this one /
    skip this one / pick a different one / cancel**) — see `references/interactive-mode.md`. `skip`
    walks down the ranked list.
  - **Headless (`BOSS_CRON=true`):** do not ask. Select the **head of the ranked queue** (highest
    priority, oldest tie-break) and proceed straight to Phase 2. If the Unplanned queue is empty,
    report that and stop.

## Phase 2 — Draft the plan

The plan itself — codebase recon, the review dimensions, and the polished write-up — is produced
per the **Phase 3 plan requirements** (the shared contract for what a plan must contain). The two
modes differ only in **who drafts**:

### Interactive (default `/boss-plan`)

Triage triviality, seed the design doc, run `plan-eng-review`, then invoke
`superpowers:writing-plans` inline — all in `references/interactive-mode.md`. Carry every interview
decision into the plan. Then continue to Phase 4 (upload + write back).

### Headless (`BOSS_CRON=true`) — dispatch ONE awaited drafting subagent

Do **not** draft inline. Recon + `superpowers:writing-plans` + the self-review dimensions are bulk
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
   RUN_SENTINEL="$(git rev-parse --show-toplevel)/.claude/skills/boss-plan/toolbox/bs-run-sentinel.mjs"
   test -f "$RUN_SENTINEL" || { echo "BLOCKED: bs-run-sentinel.mjs missing" >&2; exit 1; }
   DISPATCH_FAILURE="dispatch-failure"
   PLAN_PATH=".linear-plans/<ISSUE-ID>-<slug>.md"   # compute the slug with plan-upload.mjs issueSlug
   RUN="$(node "$RUN_SENTINEL" make-ctx boss-plan)"
   RUN_ID="${RUN%%$'\t'*}"; RUN_DIR="${RUN#*$'\t'}"
   export RUN_SENTINEL DISPATCH_FAILURE PLAN_PATH RUN_ID RUN_DIR
   ```

2. **Dispatch ONE awaited `general-purpose` subagent** (`subagent_type: general-purpose`,
   <!-- tier: opus --> plan drafting is judgment, so **tier: opus**; **await** the dispatch —

   **never** `run_in_background`). Pass it the **path** `references/headless-drafting-brief.md` (not
   its text), the ticket `id`/`title`/`description`, the target `PLAN_PATH`, and the sentinel context
   `RUN_SENTINEL`/`RUN_DIR`/`RUN_ID`. The brief tells it to recon, work the review dimensions, write
   the plan to `PLAN_PATH`, write the terminal sentinel with a `planPath` payload, and **return only**
   the bounded metadata object
   (`planPath`, `labels`, `agentFriendly`, `estimate`, `priority`, `openQuestions`,
   `descriptionSummary`) — **never the plan file's content** (returning content re-inflates the
   caller: codex fold).

   If the dispatch tool itself errors before the subagent starts, treat that as a dispatch failure:
   print one clear stderr line, clean up the sentinel context if it exists, make **no Linear write**,
   and exit non-zero. Do **not** draft inline in headless mode.

3. **Classify from the run-file sentinel only**, then re-verify (never trust the sentinel alone —
   epic D11):
   ```bash
   READ="$(node "$RUN_SENTINEL" read "$RUN_DIR" "$RUN_ID" draft)"
   RC_STATUS="$(printf '%s' "$READ" | jq -r '.status')"
   if [ "$RC_STATUS" != "ok" ]; then
     # status == missing (dead/failed subagent) OR stale (foreign leftover): a distinct
     # dispatch-failure. It routes to the SAFE branch — NO Linear write, non-zero exit — and the
     # plan NEVER completes. A half-planned issue is worse than none (the Phase 0 rule).
     echo "$DISPATCH_FAILURE: drafting subagent left no valid sentinel (status=$RC_STATUS) — no Linear write, aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
     exit 1
   fi
   PLAN_FILE="$(printf '%s' "$READ" | jq -r '.payload.planPath // empty')"
   # `ok` sentinel → re-verify the plan file is exactly the expected path and is non-empty.
   if [ "$PLAN_FILE" != "$PLAN_PATH" ] || [ ! -s "$PLAN_FILE" ]; then
     echo "$DISPATCH_FAILURE: sentinel ok but plan file missing/empty or wrong path ($PLAN_FILE) — no Linear write, aborting" >&2
     node "$RUN_SENTINEL" cleanup "$RUN_DIR"
     exit 1
   fi
   node "$RUN_SENTINEL" cleanup "$RUN_DIR"
   ```
   Only after an `ok` sentinel **and** a present, non-empty plan file at exactly `PLAN_PATH` do you
   proceed to Phase 4. The subagent's returned metadata `planPath` must also equal `PLAN_PATH`; its
   `descriptionSummary` becomes the Linear description. The orchestrator reads the plan **file** only
   for the secret gate.

## Phase 3 — Plan requirements (shared drafting spec)

Whoever drafts — the interactive orchestrator (via `superpowers:writing-plans`) or the headless
subagent (per `references/headless-drafting-brief.md`) — produces a plan file at
`.linear-plans/<ISSUE-ID>-<slug>.md` (gitignored scratch; the slug is the issue id + hyphenated
title, e.g. `BOS-5-add-an-unsubscribe-mechanism`; compute it with
`node -e "import('./scripts/plan-upload.mjs').then(m=>console.log(m.issueSlug(process.argv[1],process.argv[2])))" <ISSUE-ID> "<title>"`).
The **full drafting spec** — the plan-body requirements (first dev step, `## Acceptance criteria`,
`## Required proof`, autonomous framing, agent-friendliness call) **and** the fill-in
description-summary template — lives once in **`references/headless-drafting-brief.md` § "Step 5"/"Step 7"**;
both modes follow it, not repeated resident.

The orchestrator only needs the **versioned Linear description section contract** that boss-implement
and bs-sweep-plan consume — the drafter's `descriptionSummary` MUST carry exactly these `##` sections,
in order (`## Why this needs a human` and `## Open Questions` are conditional; all others always
present). Its machine-readable form lives in `.boss-skills.json` under `planContract`, and each
description stamps `- Contract: v<N>` under `## Planning` (v1 today):

`## Summary` · `## Approach` · `## Key changes` · `## Testing` · `## Risks / unknowns` ·
`## Acceptance criteria` · `## Required proof` · `## Why this needs a human` · `## Open Questions` ·
`## Planning` · `## Original notes`

**Headless open questions → `agent-question`.** The subagent records only genuinely **controversial**
forks (high bar — could-have-gone-either-way calls, never routine ones) as `openQuestions`; a
non-empty list drives the `agent-question` label (Phase 4) and the plan's `## Open Questions`
section. Interactive runs have a human answering each fork, so they produce none.

## Phase 3.5 — Extension plan-reviewers (additive, non-fatal)

Before upload, run any repo-local `boss-plan-*` **extension** plan-reviewers over the drafted plan —
strictly additive, a documented no-op when none are installed (the default today), in both the
interactive and headless paths. Full protocol (discover → dispatch each as a fresh read-only
subagent → validate its envelope → fold or skip), against
[`docs/skills/extension-contract.md`](../../../docs/skills/extension-contract.md), lives in
[`references/extension-reviewers.md`](references/extension-reviewers.md).

## Phase 4 — Publish the plan and write back to the tracker

> **STOP — secret gate (mandatory, adapter-agnostic pre-step, do not skip).** This runs **before
> any publish adapter**. The default R2 bucket (`proof.bossanova.dev`) is **public** — the
> unguessable URL is the ONLY access control (and no host is a licence to leak). There is no
> server-side secret scanner; **you are the only check.** Before invoking the publish
> adapter, read the entire plan file (with special attention to the `## Original notes` verbatim
> block and anything pasted from the ticket or interview) and confirm it contains **zero** of: API
> keys, tokens, passwords, connection strings, private keys, session cookies, internal
> hostnames/IPs, or customer PII. If you find anything credential- or PII-shaped, **redact it in the
> plan file and replace it with a reference to where the value lives** (e.g. "the Cloudflare token
> in repo-root `.env`") before publishing. If you are unsure whether something is sensitive, treat
> it as sensitive and redact it. Do not publish until this check passes. (This is the one place the
> headless orchestrator reads the plan **file** — the subagent already kept its body out of the
> orchestrator's context; the gate reads it once, deliberately, for safety.)

1. Publish the plan through `scripts/plan-publish.mjs` (resolves `PLAN_PUBLISH`, default `r2`, and
   owns object-key/URL derivation). Each Bash call is fresh; for `r2`, load `.env` and constants:

   ```bash
   PUBLISH_ADAPTER="${PLAN_PUBLISH:-r2}"
   if [ "$PUBLISH_ADAPTER" = "r2" ]; then
     set -a; . ./.env; set +a
     export BOSS_PROOF_R2_BUCKET=bossanova-proof-production
     export BOSS_PROOF_PUBLIC_BASE_URL=https://proof.bossanova.dev
   fi
   # Headless: carry forward the PLAN_FILE validated in Phase 2. Interactive: set it here.
   PLAN_FILE="${PLAN_FILE:-.linear-plans/<ISSUE-ID>-<slug>.md}"
   URL=$(node scripts/plan-publish.mjs --issue "<ISSUE-ID>" --file "$PLAN_FILE")
   echo "$URL"
   ```

   `$URL` is the public plan link. Set `TRACKER="${TRACKER:-linear}"` before write-back. Steps 2–5
   use tracker adapter ops; for `linear`, each `op` is a `linearOperationMap` key (`get_issue`,
   `save_issue`, or `list_issues`). Other trackers substitute their operation map without changing
   the sequence.

2. Read labels with op `readLabels` so you can **merge** (Linear `save_issue labels` replaces the
   set — never clobber existing labels like `feature`/`docs`).
3. Decide the metadata (headless: derive from the subagent's returned metadata; interactive: from
   the interview):
   - **description**: the composed description-summary block (headless: the returned
     `descriptionSummary`, verbatim; interactive: composed per the drafting spec in
     `references/headless-drafting-brief.md` § "Step 7", matching the Phase 3 section contract).
   - **labels**: union of existing labels + relevant ones (`bug`/`feature`/`improvement`/`docs`). **Agent-friendly is the default:** add **`agent-friendly`** to every plan **unless** an autonomous agent genuinely could not complete the task (headless: `agentFriendly == false`) — in that case add **`needs-human`** **instead** (never both) and ensure the plan body carries the **## Why this needs a human** section (see Phase 3). Complexity alone is not a reason for `needs-human` — a large but well-specced ticket is still `agent-friendly`. Add **`agent-question`** (headless only) **if and only if** ≥1 open question was recorded (`openQuestions` non-empty); union it in, never clobber — it is independent of the agent-friendly/needs-human call. When there are none, do not add it. Note: `bs-sweep-plan` strips only `agent-plan` after a successful plan and leaves `agent-question` in place — that persistence is intentional, so do not strip it.
   - **estimate** (Fibonacci): `0` trivial/minimal · `1`/`2`/`3` well-defined, clear path, no major unknowns · `5` some unknowns, factors discovered during implementation, larger effort · `8` many unknowns, vague/poorly-specced, large.
   - **priority** (`1-4`): start from the urgency discerned in the interview/recon, then modulate by simplicity (cheap, high-value wins can rank up), positive/business impact, and security (security concerns bias toward Urgent/High). A planned Todo should not stay `0=None`.
4. Single tracker save op (ops `moveState`/`setPriorityEstimate`; Linear uses `save_issue`, and the
   plan link uses `links` directly — no `linearOperationMap` plan-link op yet) updating the issue by
   `id`:
   - `description`: the summary block above.
   - `links`: `[{ url: "$URL", title: "Implementation plan (<ISSUE-ID>)" }]` (append-only).
   - `labels`: the merged set (names).
   - `estimate`: the Fibonacci number.
   - `priority`: the chosen `1-4`.
   - `state`: `"Todo"`.

   If the tracker save rejects `estimate` (Linear needs Fibonacci enabled), retry without
   `estimate`, complete the rest, and warn the user.

5. **Link conflicting dependencies (conservative, priority-oriented, cycle-safe).**
   Now that this ticket carries a `## Key changes` module/area list, link Linear
   blocking relations so two agents never work overlapping areas concurrently and
   churn on rebase (BOS-110: "build the sprint from the highest-priority unblocked
   tasks" without over-serializing the backlog).

   a. Fetch the active, not-yet-merged comparison set with op `selectPlanned` (Linear: `list_issues
team=Bossanova state=Todo limit=250`, then `In Progress` and `In Review`). These tickets' PRs
   could still collide with this one. Exclude `Done`/`Canceled`/`Unplanned` and this ticket itself.

   b. For each candidate, read its `## Key changes` section from its description
   (boss-plan writes one for every planned ticket). Extract the module/area tokens
   (e.g. `services/bossd`, `scripts`, `services/web`, a specific file). If a
   candidate has no `## Key changes` (e.g. an older ticket), fall back to its
   title + description text. Compare against THIS ticket's `## Key changes`.

   c. Decide whether to link. Add a relation only when there is **clear overlap**
   (a shared module/area or file that would realistically conflict on rebase)
   OR a **genuine logical dependency** (this ticket needs the other's feature, or
   vice versa). Do NOT link on speculative or whole-repo-wide overlap (e.g. both
   touch `docs/`); err toward fewer links.

   d. Orient the edge by priority, writing it with op `appendDependency` (Linear: `save_issue
blockedBy`). The **higher-priority** ticket is the blocker; on equal priority, the **older
   `createdAt`** is the blocker. So:
   - if THIS ticket outranks the candidate → save candidate blocked by THIS;
   - if the candidate outranks THIS ticket → save THIS blocked by candidate.
     Never block a higher-priority ticket behind a lower-priority one. Given **stable**
     priorities, priority+`createdAt` is a total order, so orienting edges this way cannot
     create a cycle (a later priority flip could in principle form a transitive cycle; v1
     accepts that low risk — see (e)).

   e. Cycle safety. Before adding an edge, use op `getIssue` with relations on both tickets (Linear:
   `get_issue includeRelations=true`). Skip if the opposite relation already exists (a 2-cycle) or the
   proposed blocker is already blocked by the proposed blocked ticket. `blocks`/`blockedBy`
   are **append-only** — only add, never clobber; v1 does not auto-prune stale relations.

   f. Record what you linked — **only when ≥1 link was added** (else skip). Step 4 saved the
   description first, so send a second tracker save with only `id` + `description`: re-send Step 4's
   description plus `- Dependencies: blocks BOS-X; blocked by BOS-Y` under `## Planning`. This keeps
   Step 4's other fields and the (d) relations intact.

   **Headless note:** a genuinely balanced link direction (equal priority + age + partial
   overlap) is recorded as an Open Question per the headless rules; interactive mode asks
   via AskUserQuestion.

## Phase 5 — Discard local artifacts

The plan now lives in Linear (the R2 link from Phase 4). Remove every local file this run created so
the worktree is left clean:

```bash
rm -f ".linear-plans/<ISSUE-ID>-<slug>.md"
```

In **interactive** mode also remove the seeded design doc (see "Interactive cleanup" in
`references/interactive-mode.md`); headless seeds none. Removal is best-effort (a missing file is
fine). In a `BOSS_CRON` run do this on every terminal path — including the Phase 2 dispatch-failure
abort, which also runs `bs-run-sentinel.mjs cleanup` — so an unattended run never leaves scratch.

## Phase 6 — Report

Print a concise summary: issue id + title, plan URL, final labels, estimate, priority, and the
status change (Unplanned -> Todo). The plan is hosted on R2 with no local copy remaining (it is
copied into `docs/plans/` at implementation time, per the plan's first dev step).

## Privacy

The proof bucket is **public** — the unguessable filename is the only access control and there is no
secret scanner, so **the agent running this skill is the sole safeguard.** Treat every plan as
world-readable the moment it uploads. Never write secrets, tokens, credentials, private keys, session
cookies, internal hostnames/IPs, or customer PII into a plan — not in your prose, nor in the verbatim
`## Original notes` block (the likeliest place one sneaks in). Reference where a value lives instead
(e.g. "the Cloudflare API token in repo-root `.env`"); when in doubt, redact. This is enforced at
upload time by the mandatory secret gate at the top of **Phase 4** — do not bypass it.

## Edge cases

- No Unplanned issues / no ID match → report and stop.
- All Unplanned tickets skipped at the Phase 1 confirmation (interactive) → report that the queue is exhausted and stop.
- R2 credentials missing (`CLOUDFLARE_ACCOUNT_ID` / `CLOUDFLARE_API_TOKEN` not in `.env`) → stop before any Linear write.
- Issue already past Unplanned → warn and confirm before re-planning (headless: proceed if `Todo`/`In Progress`, but stop on `Done`/`Canceled` — see Phase 1).
- Existing description → fold it into the interview/recon and preserve it verbatim under `## Original notes`.
- Estimate rejected → finish the other updates, warn about Fibonacci estimation setup.
- **Headless drafting dispatch fails** (missing/stale sentinel, or an `ok` sentinel with a missing/empty plan file) → `dispatch-failure`: **no Linear write**, non-zero exit with a one-line stderr reason, run-dir cleaned. A half-planned issue is worse than none.

## Cron gate

When this skill is scheduled as a backlog-planning cron, register this **gate command** on
the job (scheduler UI, `GateCommand` — see PR #870) so the run only fires when the backlog
has something to plan, spending **zero** agent tokens otherwise:

```
node .claude/skills/boss-plan/gate/gate.mjs
```

It exits `0` (run) iff at least one Linear issue is in the **`Unplanned`** state — the
backlog this skill plans from — and non-zero (skip) otherwise. The gate is **fail-closed**:
a missing `LINEAR_API_KEY` (injected into the gate environment by bossd), network failure,
or API error exits non-zero with a one-line reason on stderr, captured in the scheduler's
`gate_output` log, so an unverifiable state skips the run rather than burning tokens. The
shared query logic lives in `scripts/linear-gate-lib.mjs` (unit-tested); this entry is a
thin I/O wrapper. (Only gate the **unattended/cron** use of this skill — interactive
`/boss-plan` runs are not gated.)

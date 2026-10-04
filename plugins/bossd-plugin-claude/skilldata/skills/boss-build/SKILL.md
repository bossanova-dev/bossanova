---
name: boss-build
description: Use when asked to implement a planned Linear ticket on a schedule, "implement the next ticket", "boss-build", or given a ticket ID to implement. Unattended cron-safe sibling of boss-plan — it consumes agent-friendly planned tickets and ships review-ready PRs.
---

# boss-build

Implement exactly **one** planned ticket end to end, unattended, and hand off a review-ready PR. This
is the second half of the pair whose first half is `boss-plan` (which turns tickets into
`agent-friendly` planned tickets). You are a capable engineer: this document states the contract,
the helpers that answer questions reliably, and the few rules that protect the branch and the
truthfulness of the result. How you implement is up to you — **the plan is the specification**.

## Terminal states

Print exactly one, as the first token of its own line, followed by the ticket id, PR URL and a
summary (cost extraction matches a line-leading token).

- `REVIEW_READY` — PR pushed, green and ready; ticket moved to the **in-review** state; PR URL
  commented on the ticket; `please-review` applied. Open review findings do **not** prevent this: a
  round-capped review ships here with the findings **published** (PR comment, ticket comment, a
  `## Review findings` pointer in the body). Human review is the next gate.
- `PARTIAL` — branch green and pushed, at least one in-scope acceptance criterion satisfied **and**
  certified by the review, and everything left undone is an unmet in-scope criterion. Ticket stays
  **in-progress**; PR ready but marked do-not-merge; never `please-review`.
- `BLOCKED` — only when the run physically cannot finish: **(1)** quality gates are still red after
  the repair cap, or **(2)** the branch cannot be pushed. Ticket stays **in-progress** with a blocker
  comment (`file:line`, what was tried); PR left draft. Nothing else is BLOCKED — not open findings
  of any kind, not an unreadable review, not an uncertified criterion. boss-build builds what the
  plan says; judging the work belongs to the planner and the human reviewer.
- `NO_CHANGE` — no eligible candidate, claim lost with no runner-up, a foreign branch carrying other
  work, a peer already holding the worktree lock, or nothing committable after claiming (ticket
  restored to its entry state).

An existing PR/branch (bossd's bootstrap draft, an empty PR, or a prior run's work) is **adopted and
resumed**, not a stop condition.

## Workspace facts

- **Tracker**: `resolveTrackerAdapter(env)` (`toolbox/tracker/adapter.mjs`, default
  `TRACKER=linear`). Steps name **capabilities** — `selectPlanned`, `getIssue`, `moveState`,
  `readComments`, `writeComment`, `readLabels`, `readPlanAttachment`, `extractImages` (optional) —
  whose concrete MCP tools live in the adapter's operation map. Workspace, backlog team and states
  come from `trackerConfigFor(config)` (`toolbox/skill-config.mjs`); the backlog is a team, never a
  project filter. The three states are roles — `.planned`, `.inProgress`, `.inReview` — resolved to
  this workspace's names at runtime; never hard-code a state name.
- Priority numeric: `1=Urgent, 2=High, 3=Medium, 4=Low, 0=None`.
- **Plan**: an attachment titled like a plan (`Implementation plan (<ISSUE-ID>)` preferred);
  `selectImplementationPlanAttachment(ticket.attachments, issueID)` picks it.
- **Waiting on CI/PR state**: arm one-shot callbacks via `resolveCallbackAdapter(env)`
  (`toolbox/callback/adapter.mjs`) when `callbacksAvailable(env)` is true, and back every wait with
  the bounded poll either way — [`references/callback-watches.md`](references/callback-watches.md)
  Protocol steps 1 and 5. Never a fixed `sleep` of a minute or more, never a bare
  `gh pr checks --watch`, never `boss cron` (a cron fire starts a new blind session).

## Helpers

Every helper ships in `toolbox/`. Shell state does not survive between tool calls, and a
dispatched subagent inherits none of yours, so re-resolve the path in each block:

```bash
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
```

| Question                                            | Ask                                                                         |
| --------------------------------------------------- | --------------------------------------------------------------------------- |
| Is the worktree clean (hook-proof)?                 | `node worktree-state.mjs [--base <ref>]`                                    |
| Who owns this worktree?                             | `worktree-lock.sh acquire\|heartbeat\|release <run-id> [ticket]`            |
| Who won the ticket claim?                           | `node tracker/cli.mjs claim-verdict …`                                      |
| Which PR belongs to this branch?                    | `node pr-ownership.mjs number --pr-json <json>`                             |
| Which plan attachment?                              | `selectImplementationPlanAttachment` in `plan-attachment.mjs`               |
| Which repo-local extensions exist?                  | `node skill-extensions.mjs discover --core boss-build --role <role> --json` |
| Did the base move under me?                         | `node base-drift.mjs check …`                                               |
| Push the branch, with retry and a rescue ref        | `node finalize/push-branch.mjs --branch <name>`                             |
| Tag commits with the PR number                      | `node finalize/cli.mjs inject-pr-tag <pr>`                                  |
| Are the checks green / is the merge state blocking? | `node pr-check-state.mjs classify\|merge-state …`                           |
| Wait for CI without guessing                        | `node ci-wait.mjs run --pr <n>` (callback-watches.md Protocol step 5)       |
| May I stop watching CI?                             | `node callback/ci-watch.mjs classify …`                                     |
| Is the verify-only evidence well-formed?            | `validateVerifyOnlyEvidence(config, body)` in `skill-config.mjs`            |
| What is left before this terminal state is honest?  | `node finalize/route-contract.mjs assert --outcome <state> …`               |

## Rules

1. **One ticket per run.** No batching. Headless runs never ask questions: decide, record the
   decision and its rationale under `## Autonomous decisions` in the PR body, continue.
2. **Follow the plan.** Implement what it says — migrations, schema changes, configuration,
   dependency changes, whatever it calls for. Judging whether the work is wise is the planner's and
   the reviewer's job, not this run's. Where the ticket was edited after the plan, the ticket wins.
   Where a premise no longer holds, build what the plan is for against the code as it is and record
   the departure. Where a criterion cannot be checked from a worktree (production access, a deployed
   environment), implement the change and say in the PR body what a human must verify. Never print
   or commit secret values.
3. **Commit per task, never leave work behind.** Tagless conventional commits with a scope
   (`feat(scope): …`), path-scoped (`git commit --only -m "…" -- <files>`), never `git add -A`. Leave
   room in the subject for the `[#<PR>]` tag finalize prepends. A run's work is not done until it is
   pushed: every route that ends after implementation pushes first.
4. **Rebase, never merge.** Never merge the base into the branch, never `git pull`, never
   `--rebase-merges`; force-push only with `--force-with-lease` over this run's own rewrite.
5. **Never merge the PR.** Terminal success is review-ready.
6. **Await every subagent.** Dispatch with awaited `Task` (Claude) or `spawn_agent` + `wait_agent`
   (Codex); never background a dispatch and move on. Keep bulk output (diffs, CI logs, review
   transcripts) inside subagents that return short summaries.
7. **A step is done when its artifact exists** (`$PLAN_FILE` holds the plan; `gh pr view <n>`
   returns), not when it was attempted.
8. **Re-enter, don't stop early.** A prompt that arrives before this run printed a terminal state
   resumes the workflow: `route-contract.mjs assert --outcome <intended>` lists what is still owed —
   answer in one line and do it. Once a terminal state is printed, the run is over.
9. **Leave nothing behind.** Every terminal state that took the worktree lock goes through **Stop
   cleanly** (Step 12). Discard scratch files and temp dirs you created.
10. **Bookkeeping warns, capability blocks.** A drifted install, an incomplete route receipt, or a
    failed ledger write prints `warning: <what> — bookkeeping only, work state unaffected` and the
    run continues. A missing toolbox or helper script is a hard stop; tracker tools not yet in your
    tool list are not (see **Tracker**).

## Mode

```bash
if [ "${BS_HEADLESS:-}" = "1" ] || [ -n "${OPENCLAW_SESSION:-}" ] || [ ! -t 0 ]; then
  MODE=headless
else
  MODE=interactive
fi
```

A `--headless` argument forces headless; when unsure, choose headless.

## Preflight

```bash
git rev-parse --show-toplevel
git branch --show-current
command -v git; command -v gh; gh auth status
```

```bash
START_SHA="$(git rev-parse HEAD)"
# Clear a previous run's Step 6 review-verdict note (contract: Step 6).
rm -f "$(git rev-parse --git-dir)/boss-build-review-verdict"
SESSION_BRANCH="$(git branch --show-current)"
BASE_UPSTREAM="$(git rev-parse --abbrev-ref "$SESSION_BRANCH@{upstream}" 2>/dev/null)"
BASE_REMOTE="${BASE_UPSTREAM%%/*}"
BASE_BRANCH="${BASE_UPSTREAM#*/}"
if [ -z "$BASE_UPSTREAM" ] || [ "$BASE_REMOTE" = "$BASE_UPSTREAM" ] || [ "$BASE_BRANCH" = "$SESSION_BRANCH" ]; then
  BASE_BRANCH="$(gh repo view --json defaultBranchRef -q .defaultBranchRef.name)"
  BASE_REMOTE="origin"
fi
# BASE_BRANCH is the GitHub API/base-name value only. Git ownership and range decisions must use
# BASE_REF from its tracking remote, never a bare local branch ref that can be stale in a newly
# provisioned worktree. An untracked branch uses the default branch at origin.
BASE_REF="refs/remotes/$BASE_REMOTE/$BASE_BRANCH"
if ! git fetch "$BASE_REMOTE" "+refs/heads/$BASE_BRANCH:$BASE_REF"; then
  echo "NO_CHANGE: unable to resolve remote base ref $BASE_REF" >&2
  exit 0
fi
if ! BASE_SHA="$(git rev-parse --verify "$BASE_REF^{commit}")"; then
  echo "NO_CHANGE: remote base ref $BASE_REF is not a commit" >&2
  exit 0
fi

if [ -z "${BOSS_SKILLS_HOME:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$candidate"; break; fi
  done
fi
test -n "${BOSS_SKILLS_HOME:-}" || { echo "BLOCKED: installed boss skills not found"; exit 1; }
BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
export BOSS_SKILLS_HOME BOSS_BUILD_TOOLBOX
# Report installed skill drift before tracker writes or worktree mutation.
# skill-drift-verdict.mjs decides severity: a stale record warns, an absent
# capability stops. Without a boss CLI, use the warning helper so drift is not
# called clean.
if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
  if O="$("$BOSS_BIN" skills check --gate 2>&1)"; then
    if [ -n "$O" ]; then printf '%s\n' "$O" >&2; fi
  else
    case "$O" in
      *--gate*) node "$BOSS_BUILD_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_BUILD_TOOLBOX" || true ;;
      *)
        printf '%s\n' "$O" >&2
        printf '%s\n' "$O" | node "$BOSS_BUILD_TOOLBOX/skill-drift-verdict.mjs" classify --status 1 >&2 || exit 1
        ;;
    esac
  fi
elif [ -f "$BOSS_BUILD_TOOLBOX/toolbox-drift.mjs" ]; then
  node "$BOSS_BUILD_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_BUILD_TOOLBOX" || true
else
  echo "boss-toolbox-drift: (drift helper not installed) — this install predates the check; drift is UNKNOWN, not clean." >&2
fi
# BOSSD_MANAGED=1 iff a bossd daemon provisioned this worktree (references/standalone-mode.md):
if node "$BOSS_BUILD_TOOLBOX/bossd-present.mjs"; then BOSSD_MANAGED=1; else BOSSD_MANAGED=0; fi
if [ "$BOSSD_MANAGED" = "1" ]; then
  test -n "$SESSION_BRANCH" || exit 1
fi
```

A stale-but-present installed file warns and the run continues; an `absent`, `mode` or
`broken-symlink` row from the drift gate, or no toolbox at all, stops (rule 10).

**Tracker.** Make one cheap read through the adapter (e.g. the backlog team's statuses), then
classify it with `trackerMcpPreflight` (`toolbox/tracker/preflight.mjs`), passing your **own tool
list** — never a harness config file. MCP servers can still be connecting when a session starts: if
no tracker tools are in your tool list yet, wait about 20 seconds and look again, up to three times,
before classifying.

```bash
node --input-type=module -e '
  import{pathToFileURL as u}from"node:url"
  const { trackerMcpPreflight } = await import(u(process.env.BOSS_BUILD_TOOLBOX+"/tracker/preflight.mjs").href)
  process.stdout.write(JSON.stringify(trackerMcpPreflight({
    operationMap: ADAPTER_OPERATION_MAP, mcpServer: TRACKER_MCP_SERVER,
    agent: process.env.BOSS_AGENT || "", availableTools: AVAILABLE_TOOLS, probeOk: PROBE_OK,
  })))
'
```

On `ok: true` call tracker tools through `resolvedServer` — the server name this session actually
has, which may be spelled differently from the config (`linear`, `acme-linear`, `acme_linear`). On
`ok: false` stop `NO_CHANGE: <message>` with no writes: `absent` means the repo never declared the
server for this harness (fix the repo), `unreachable` means it did not answer (fix
credentials/network).

Also require, before any tracker write, that `trackerConfigFor(config).states` resolves all three
roles to non-empty names and that the adapter exposes `readPlanAttachment`; otherwise stop
`NO_CHANGE` naming what is missing.

**Boss transport.** This run's own session operations go through the `boss` CLI or the boss MCP
tools. Read `boss env --json` (`.capabilities.cli`, `.capabilities.mcp`) and compare against:

```bash
node --input-type=module -e '
  import{pathToFileURL as u}from"node:url"
  const m = await import(u(process.env.BOSS_BUILD_TOOLBOX+"/session/boss.mjs").href)
  process.stdout.write("tools:\n" + m.requiredBossToolsForEpic().join("\n") + "\n")
  process.stdout.write("cli:\n" + m.requiredBossCliCommandsForEpic().join("\n") + "\n")
'
```

`bossEpicTransportPreflight({availableTools, availableCliCommands})` returns `{ ok, transport,
missing, degraded, partial, inventoryHint }`; the CLI is preferred whenever its set is complete. On
`ok: false` stop `BLOCKED: no complete boss transport: <missing>; <inventoryHint>`. Otherwise report
`transport: <cli|mcp>` in your opening line, plus `cli-only mode (expected): <degraded>` and
`partial: <capability>(<fields>)` when non-empty. Under `BOSSD_MANAGED=0` there may be no transport
at all — see [`references/standalone-mode.md`](references/standalone-mode.md).

## Step 1: Take the worktree lock

```bash
if [ -z "${BOSS_BUILD_TOOLBOX:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-build/toolbox" ]; then BOSS_BUILD_TOOLBOX="$candidate/boss-build/toolbox"; break; fi
  done
fi
test -n "${BOSS_BUILD_TOOLBOX:-}" || { echo "BLOCKED: boss-build toolbox not found"; exit 1; }
LOCK="$BOSS_BUILD_TOOLBOX/worktree-lock.sh"
BLI_RUNID="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-token)"
"$LOCK" acquire "$BLI_RUNID" pending
```

The `pending` ticket is replaced in Step 2 (`"$LOCK" acquire "$BLI_RUNID" <TICKET-ID>`). Once you
hold the lock, create the route receipt every later step stamps (Step 9 lists the tokens) and carry
its path forward: `BOSS_BUILD_ROUTE_RECEIPT="$(mktemp -t boss-build-route.XXXXXX.json)"`.

- `ACQUIRED` / `TOOK_OVER_STALE` (exit 0) — you own the worktree. `TOOK_OVER_STALE` means a prior
  run here died: treat it as a resume candidate.
- `HELD_BY_PEER` (exit 3) — a live run owns it. Stop `NO_CHANGE` with zero writes; do **not** go
  through Step 12 (you hold nothing to release).

Refresh with `"$LOCK" heartbeat "$BLI_RUNID"` at step boundaries (at least at the start of Steps 5, 6
and 8).

## Step 2: Select one ticket

- **A named ticket** (e.g. `<ISSUE-ID>`): `getIssue` with relations. Naming it bypasses the
  `agent-friendly` label and estimate filters, and overrides — loudly — the `needs-human` and
  blocked-by skips:
  `WARNING: <ID> is labelled needs-human — implementing only because it was named explicitly` /
  `WARNING: <ID> is blocked by <BLOCKER-IDS> (unmerged) — implementing only because it was named explicitly`.
  It still needs a plan attachment; without one stop `NO_CHANGE` with no claim or state move.
- **Otherwise**: candidates from `selectPlanned` (backlog team, planned state, limit 250) — or, when
  `trackerConfigFor(config).selection` is set, from `node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs"
list-planned` (a non-zero exit stops `NO_CHANGE` quoting its stderr; never fall back to the
  unfiltered call). Keep `agent-friendly` tickets with a plan attachment, drop `needs-human`, rank by
  priority (Urgent first, None last), then lowest estimate, then oldest. Walk the ranking and take
  the first ticket that is unblocked (`readDependencies` / `isUnblocked`), not an epic parent, and
  has a plan. None ⇒ `NO_CHANGE` (`all agent-friendly planned tickets are blocked, ineligible, epic
parents, or missing plans`).

`agent-question` never blocks; copy the plan's open questions into the PR body. Selection has no side
effects: nothing is claimed or moved until Step 3. Detail:
[`references/claim-and-eligibility.md`](references/claim-and-eligibility.md).

Record the ticket in the lock (`"$LOCK" acquire "$BLI_RUNID" <TICKET-ID>`). Capture the ticket's
current tracker state as the **entry state** (in bossd-managed runs, the state from the
bootstrap payload, before bossd's session-start sync moved it). Standalone (`BOSSD_MANAGED=0`):
create a `boss-build/<ticket-id>` branch off the base first
([`references/standalone-mode.md`](references/standalone-mode.md)).

## Step 2.5: Ours to adopt, or foreign?

```bash
PR_JSON="$(gh pr list --head "$SESSION_BRANCH" --state open \
  --json number,title,body,headRefName,state)"
BOSS_BUILD_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-build/toolbox"
if [ ! -d "$BOSS_BUILD_TOOLBOX" ]; then BOSS_BUILD_TOOLBOX="$HOME/.codex/skills/boss-build/toolbox"; fi
PR_NUMBER="$(node "$BOSS_BUILD_TOOLBOX/pr-ownership.mjs" number --pr-json "$PR_JSON")"
```

Judge ownership from the branch name, `[<ISSUE-ID>]` in the PR title, `Linear issue: <url>` in its
body, and real commits ahead of `$BASE_REF` (ignoring bossd's empty bootstrap commit):

| what you find                           | route                                                   |
| --------------------------------------- | ------------------------------------------------------- |
| no PR and no real commits               | **fresh** — Step 7 creates the PR                       |
| bootstrap-only PR                       | **fresh** — Step 7 reuses that PR                       |
| this ticket's PR/branch with real work  | **resume** — Step 4.5 assesses it, Step 7 reuses the PR |
| someone else's PR/branch with real work | `NO_CHANGE` — never co-edit it; go straight to Step 12  |

## Step 3: Claim

```bash
if [ -z "${BOSS_BUILD_TOOLBOX:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-build/toolbox" ]; then BOSS_BUILD_TOOLBOX="$candidate/boss-build/toolbox"; break; fi
  done
fi
test -n "${BOSS_BUILD_TOOLBOX:-}" || { echo "BLOCKED: boss-build toolbox not found"; exit 1; }
TOKEN="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-token)"
BODY="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-comment --token "$TOKEN" --session-id "${BOSS_SESSION_ID:-}")" || { echo "BLOCKED: claim body"; exit 1; }
```

Before posting, `readComments` and route: a fresh `ACQUIRED` lock **and** zero peer claim comments is
the uncontended fast path (`UNCONTENDED=1`, no liveness evidence, no waits). Anything else runs the
full contended ceremony in [`references/claim-and-eligibility.md`](references/claim-and-eligibility.md)
(liveness evidence before posting, a 20 s wait after, malformed evidence is a hard error). Post `$BODY`
with `writeComment`, move the ticket to `.inProgress`, **re-read the comments** (both paths), then:

```bash
set --
if [ -z "${UNCONTENDED:-}" ]; then
  test -n "${BOSS_CLAIM_LIVENESS_JSON:-}" || { echo "BLOCKED: liveness"; exit 1; }
  set -- --liveness "$BOSS_CLAIM_LIVENESS_JSON"
fi
node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-verdict --me "$TOKEN" --comments "$COMMENTS_JSON" "$@"
```

- exit 0 (WON) — contended path: wait ~10 s and re-confirm with fresh liveness; proceed if still 0.
- exit 3 (LOST) — delete your claim, leave the state alone, take the next candidate; none ⇒
  `NO_CHANGE`.
- exit 4 (NO_WINNER) — delete your claim and repeat Step 3 once; again ⇒ `NO_CHANGE`.
- anything else ⇒ `BLOCKED: claim`.

Once WON, link the session per the reference (best-effort).

## Step 4: Read the plan

Select the plan attachment and read it with `readPlanAttachment` (by attachment **id**). The plan is
whatever it contains — no particular headings are required; work out what needs building and what
"done" means. Stop `BLOCKED` with a ticket comment only when there is no readable plan (nothing
selected, empty, or over 1 MiB). Save it outside the worktree — the plan lives in the tracker, never
in the repo:

```bash
PLAN_FILE="$(git rev-parse --git-dir)/boss-build/plan.md"
mkdir -p "$(dirname "$PLAN_FILE")"
# write the fetched plan bytes to "$PLAN_FILE"; hand its path to every subagent that needs it
```

When the ticket description contains images (markdown images, `<img>`, attachment URLs), view them
with `extractImages` — reading `![](url)` as text shows no pixels. Best-effort.

### Step 4.5: Assess adopted work (resume only)

Build a done-vs-remaining map from the branch diff, its log and the PR body (trust the diff), and set
Step 5's scope: **none** (everything already satisfied — skip to Step 6), **remaining**, or **fresh**.
Build on top of the existing work, never revert it.
[`references/resume-assessment.md`](references/resume-assessment.md) has the procedure, including
recovery for a restarted orchestrator.

### Step 4.6: Check premises and criteria against the code

Before Step 5, on every run: resolve the plan's cited `path:line`s by their **symbol**, re-derive
claimed sets, read the symbols it says are missing. A moved line whose symbol is still there is
drift — note the corrected location. A premise that genuinely no longer holds is handled by rule 2.
When the plan states no acceptance criteria, derive them from what it says done looks like. Ignore
the ticket's `## Original notes` (that is pre-planning history). An empty search result may be a
shell hook's fabrication; confirm absence by reading the file.

## Step 5: Implement

Implement the scope Step 4.5 left (the full plan on a fresh run) through the first methodology tier
that is available:

1. **Repo-local methodology extensions** —
   `node "$BOSS_BUILD_TOOLBOX/skill-extensions.mjs" discover --core boss-build --role methodology --json`.
   Dispatch each in ascending `order` as an awaited subagent whose instructions are the `SKILL.md`
   read from its descriptor's `skillPath` (pass `skillPath` and `dir`; never load an extension
   through the Skill tool — they declare `disable-model-invocation: true`). Hand it `$PLAN_FILE`, the
   current scope, rule 2, the task contract and the commit contract below.
2. **A host-native test-first/implementation affordance**, if this environment has one.
3. **The inline loop**: for each remaining task, a fresh focused subagent writes the failing test,
   runs the smallest covering command until it fails for the right reason, writes the minimal code,
   re-runs it green, refactors, and reviews its own task for spec compliance and quality.

A tier **ran successfully** when its work is on the branch (the commits it reported are in the log
range, or you recovered its residue) **and** the scope it was handed is implemented — check the
plan's criteria against the diff, never the dispatch's word. A tier that falls short (or an
extension that fails to load) is recorded as `extension <name>: skipped (<reason>)` / `tier <n>:
skipped (<reason>)` and the next tier gets **only what is still open**. Recompute that remainder
from the branch before every dispatch; where nothing remains, dispatch nothing and record
`not dispatched (scope already satisfied)`. One successful extension suppresses tiers 2 and 3.

**Task contract.** Each implementation subagent returns only: task id, files touched, tests
added/passing, interface signatures, residual risks (checked against the prior art it cited),
decisions made (with rationale), and commits made (short SHA + subject, or _no commit —
verification only_). Thread only that into the next dispatch; every decision reaches the PR body's
`## Autonomous decisions`.

**Commit contract** (every implementation brief carries it):

- Commit each task as it finishes, path-scoped to the files it touched; never one end-of-run commit.
  Run formatters/codegen **before** the commit they belong to.
- Never return with uncommitted work of your own: finish with `node <toolbox>/worktree-state.mjs`
  (`unknown` is not clean). If a hook rejects a message, fix exactly what the hook names and retry
  once; if it still fails, leave the work in the tree, report the paths, and never revert.
- After a rejected commit, check `git show --stat HEAD` before the next `git add` — the rejected
  files are still staged.
- Do not write to the PR or the tracker; report evidence in the contract.
- Re-read your commit messages before returning; correct any claim the run later disproved.

**Verify each dispatch.** Before dispatching, the tree is clean and you have recorded the starting
HEAD. After it returns, the tree is clean again and the log has advanced by the commits it reported.
Residue the contract names: commit it yourself (only those paths) and re-check the task. Residue
nobody can attribute: stop dispatching and go to Step 12 `BLOCKED`, naming the paths. An empty log
range with no _verification only_ claim: confirm from the diff whether the work is actually missing;
if it is, re-dispatch once with what you verified folded in. Exclude the daemon artifacts
`.claude/scheduled_tasks.lock` and `.claude/settings.local.json` from every such check. Mechanics:
[`references/resume-assessment.md`](references/resume-assessment.md#dispatch-snapshot-mechanics).

If the plan names web proof affordances, build them in this PR. For a TUI diff, author and commit a
`proof/scenarios/*.scenario.json` before Step 6 and get `node scripts/proof.mjs scenario validate`
and `scenario run --dry-run` green ([`references/proof-capture.md`](references/proof-capture.md)).

## Step 6: Review

**Baseline.** Fresh/bootstrap-only: `REVIEW_BASE="$START_SHA"`. Resume: `REVIEW_BASE="$BASE_REF"`.

**Anything to review?**

```bash
node "$BOSS_BUILD_TOOLBOX/worktree-state.mjs" --base "$REVIEW_BASE" \
  --exclude .claude/scheduled_tasks.lock --exclude .claude/settings.local.json -- .
```

`clean` ⇒ nothing was built: restore the entry state, delete the claim, Step 12 `NO_CHANGE`.
`unknown` ⇒ `BLOCKED`. `dirty` ⇒ commit what this run touched (path-scoped) so the review sees it.

**Base drift.** Immediately before the review, refresh the base and ask whether it moved under you:

```bash
REVIEW_BASE="$(git rev-parse --verify "$REVIEW_BASE^{commit}")" || exit 1   # pin: the fetch moves $BASE_REF
git fetch --no-tags "$BASE_REMOTE" "+refs/heads/$BASE_BRANCH:$BASE_REF" || FETCH_FAILED=--fetch-failed
node "$BOSS_BUILD_TOOLBOX/base-drift.mjs" check --repo "$(git rev-parse --show-toplevel)" \
  --base "$BASE_REF" --head "$(git rev-parse HEAD)" ${FETCH_FAILED:-}
```

Carry the pinned `REVIEW_BASE` forward (on a resume it named the ref the fetch moves). **Rebase** onto the moved base only when `behind` is a positive integer, `intersection` is non-empty
and `mergeTree` is `clean`, the tree is clean, and no rebase has happened yet this run; then re-bind
`REVIEW_BASE` to the new base tip. A failed rebase is aborted (confirm no `REBASE_HEAD` and a clean
tree; a worktree stuck mid-rebase is `BLOCKED` cause 2). `behind` or `mergeTree` = `unevaluated`, or
`mergeTree` = `conflicts`, is drift you record and tell the reviewer about, not something to rebase
over. Carry the detector's `note` verbatim to the reviewer and into `## Autonomous decisions`.

**Depth.** Full review unless the diff is small and touches no configured lens: quick when
`reviewDeltaDefaults(config).forceFull` is false, no changed file matches a lens
(`lensesForFile(config, path)` is empty for every file), and fewer than
`reviewDeltaDefaults(config).deltaFileThreshold` (default 20) files changed. An unreadable diff is
full. The quick tier disables the optional default rounds (`BOSS_REVIEW_DEFAULT_ROUNDS=0`) and gets two
legs of time instead of three; it still runs the whole-branch review, its fix loop and the
acceptance-criteria certification. Depth is chosen from the diff, never from a clock.

**Deadline and run file.**

```bash
leg_ms=${BOSS_SKILL_EXTENSION_TIMEOUT_MS:-300000}
case "$leg_ms" in '' | *[!0-9]*) leg_ms=300000 ;; esac
leg_ms=$(( 10#$leg_ms )); [ "$leg_ms" -gt 0 ] || leg_ms=300000
LEG=$(( (leg_ms + 999) / 1000 )); [ "$LEG" -ge 300 ] || LEG=300
LEGS=3                                   # 2 on the quick tier
STEP_6C_DEADLINE=$(( $(date +%s) + LEGS * LEG ))
FUNDING="$(node "$BOSS_BUILD_TOOLBOX/bs-review-caps.mjs" funding \
  "{\"allowanceSeconds\": $(( LEGS * LEG )), \"legSeconds\": $LEG, \"initialLegs\": $LEGS}")" ||
  FUNDING='{"reason":"funding-unpriced"}'
STEP_6C_FUNDING_REASON="$(printf '%s' "$FUNDING" | sed -n 's/.*"reason":"\([^"]*\)".*/\1/p')"
echo "STEP_6C_DEADLINE=$STEP_6C_DEADLINE STEP_6C_FUNDING_REASON=$STEP_6C_FUNDING_REASON"
```

```bash
RUN_SENTINEL="$BOSS_BUILD_TOOLBOX/bs-run-sentinel.mjs"
test -f "$RUN_SENTINEL" || { echo "BLOCKED: bs-run-sentinel.mjs missing"; exit 1; }
RUN="$(node "$RUN_SENTINEL" make-ctx boss-build)"
RUN_ID="${RUN%%$'\t'*}"; RUN_DIR="${RUN#*$'\t'}"
DISPATCH_FAILURE="dispatch-failure"   # byte-identical to the module's DISPATCH_FAILURE
export BOSS_SKILLS_HOME BOSS_BUILD_TOOLBOX RUN_SENTINEL RUN_ID RUN_DIR DISPATCH_FAILURE
# Seed a provisional pessimistic verdict: GENERATE the line; a hand-written literal is unmatchable.
node "$RUN_SENTINEL" write "$RUN_DIR" "$RUN_ID" review \
  "$(node "$BOSS_BUILD_TOOLBOX/bs-review-caps.mjs" sentinel capped 1)" '{"provisional":true}'rm -f "$(git rev-parse --git-dir)/boss-build/review-report.md"   # never post a previous run's report
```

**Dispatch exactly one review.** One fresh awaited subagent runs the `boss-review` skill over
`$REVIEW_BASE...HEAD` and nothing else — `boss-review` already carries the lenses, the repo's review
rounds, the cross-model second voice and its own fix loop, so never add a review of your own before
or after it. Its prompt's **first line is exactly `[bs-reviewer-dispatch]`** (an inert marker the
cost telemetry counts). State in the prompt, by these exact names: `REVIEW_BASE`,
`STEP_6C_DEADLINE`, `STEP_6C_FUNDING_REASON` (only when non-empty), `RUN_DIR`, `RUN_ID`,
`BOSS_NOTES_SUPPRESSED=1`, `BOSS_REVIEW_DEFAULT_ROUNDS=0` on the quick tier, the plan path and its
acceptance criteria (and required proof), the base-drift note, and on a resume the Step 4.5 map. It
must also beat `$RUN_DIR/review.heartbeat`
(`node "$BOSS_BUILD_TOOLBOX/bs-dispatch-await.mjs" heartbeat "$RUN_DIR/review.heartbeat"`) at every
phase boundary, write the rendered report to `$(git rev-parse --git-dir)/boss-build/review-report.md`
(the run dir is cleaned up after classification), and return only:

- the `## Cross-model review` token (from boss-review's second-voice ledger row): `clean` |
  `findings-fixed (<dispositions>)` | `skipped: <reason>` | `error: <reason>`;
- the `## Review coverage` token: `full` | `full (skipped: <rounds>)` | `quick: <reason>`;
- the base-drift note and a one-line summary of open findings.

`boss-review` writes the earned verdict into the run file itself. If the dispatch tool itself fails,
run the same single review inline. Hold it in the foreground:
`node "${RUN_SENTINEL%/*}/bs-dispatch-await.mjs" wait "$RUN_DIR" "$RUN_ID" review --heartbeat "$RUN_DIR/review.heartbeat" --while-live`
until the subagent returns or it exits 0, 96 or 97.

**Classify from the run file only** — never the returned prose:

```bash
DISP="$(node "${RUN_SENTINEL%/*}/bs-dispatch-await.mjs" disposition "$RUN_DIR" "$RUN_ID" review --heartbeat "$RUN_DIR/review.heartbeat")"
if [ "$(printf '%s' "$DISP" | jq -r '.publishable')" = "true" ]; then
  # matchSentinel classifies the byte-stable `bs-review clean:` / `bs-review capped:` prefixes.
  VERDICT="$(node "${RUN_SENTINEL%/*}/bs-review-caps.mjs" match "$(printf '%s' "$DISP" | jq -r '.kind')" | jq -r '.status // empty')"
  if [ -z "$VERDICT" ]; then VERDICT="$DISPATCH_FAILURE"; fi
  PROVISIONAL=
else
  # Every non-publishable disposition — a provisional payload on ANY kind, a missing sentinel (dead
  # subagent), a stale one (foreign leftover), a timed-out or abandoned dispatch — is a distinct
  # dispatch-failure that routes to the SAFE non-clean branch and is NEVER treated as clean.
  # `.reason` says which; only `provisional-payload` takes the coverage-unknown arm.
  VERDICT="$DISPATCH_FAILURE"
  PROVISIONAL="$(printf '%s' "$DISP" | jq -r 'if .reason == "provisional-payload" then "true" else empty end')"
fi
node "$RUN_SENTINEL" cleanup "$RUN_DIR"
case "$VERDICT" in clean|capped) REVIEW_VERDICT="$VERDICT" ;; *) REVIEW_VERDICT="none" ;; esac
printf 'REVIEW_VERDICT=%s\n' "$REVIEW_VERDICT" \
  >"$(git rev-parse --git-dir)/boss-build-review-verdict"
```

**Route.**

- `clean` → Step 6.5, then Step 7 (full coverage).
- `capped` → Step 7 with the findings to publish. Coverage keeps the token the review earned.
- `PROVISIONAL=true` (the seed was never upgraded — nothing settled a verdict) → Step 7 with
  `## Review coverage` = `none: review coverage unknown (review stack entered; provisional verdict never upgraded — <reason>)`
  and `## Cross-model review` = `error: <reason>`. Never `PARTIAL`.
- `dispatch-failure` → Step 7. Missing/stale run file: `none: review coverage unknown (<reason>)`.
  Present but unmatchable: keep the returned tokens annotated, else
  `none: review verdict unreadable (<reason>)`. Cross-model `error: <reason>`.
- `clean` with no returned coverage token → `none: review coverage unknown (<reason>)`; routing
  still follows the file.

`BOSS_BS_REVIEW=0` is the one way to skip review: dispatch nothing, write `sentinel capped 1`, and
publish `none: review stack did not run (disabled by BOSS_BS_REVIEW=0)` / cross-model
`skipped: disabled`.

An unreviewed or capped branch is never fatal: it ships, saying so in the PR. Only red gates and an
unpushable branch block.

## Step 6.5: Knowledge extensions (repo opt-in)

After a `clean` review: discover `--role knowledge`. None ⇒ nothing, no output. Otherwise dispatch
each (instructions from `skillPath`), validate with `skill-extensions.mjs validate --role knowledge
--file <outPath>`, and record `extension <name>: skipped (<reason>)` per failure. Extensions may
commit a knowledge artifact, so Step 7 captures the reviewed tip **after** this. Never fatal.
[`references/knowledge-extensions.md`](references/knowledge-extensions.md).

## Step 7: Push and publish the PR

Every route from Step 6 comes through here.

1. **Push** (record `REVIEWED_HEAD=$(git rev-parse HEAD)` first):

   ```bash
   PUSH_JSON="$(node "$BOSS_BUILD_TOOLBOX/finalize/push-branch.mjs" --branch "$SESSION_BRANCH")" || true
   printf '%s\n' "$PUSH_JSON"
   ```

   `pushed: yes` continues. `rescue` (the commits are on the `rescue` ref it names) or `no` (nothing
   left the worktree; name the SHAs) is `BLOCKED` cause 2: publish the blocker comment with both
   coverage tokens ([`references/publish.md`](references/publish.md)) and go to Step 12.

2. **Did the reviewed tree ship?** `git fetch -q origin "$SESSION_BRANCH"`; if `FETCH_HEAD` is not
   `REVIEWED_HEAD` (someone pushed on top, or the push rebased), either re-run Steps 5–6 on the new
   tip or publish `none: review coverage unknown (branch tip moved after review: <A> → <B>)`. Never
   claim coverage of a tree that was not reviewed.

3. **PR.** Compose the body in a temp file outside the worktree
   ([`references/publish.md`](references/publish.md) has the template), then create a **draft**
   (`gh pr create --draft --label agent-made --title "[<ISSUE-ID>] <issue title>" --body-file …`)
   when Step 2.5 said fresh with no PR, else `gh pr edit "$PR_NUMBER"` the existing one. Run
   `validateVerifyOnlyEvidence(config, body)` over the body before publishing it. Never put the
   phrase `do not merge` in a title or body except through the PARTIAL marker (boss-epic's merge
   gate matches it).

4. **Review comment.** Upsert exactly one `<!-- bs-review -->` comment: the review report
   (`$(git rev-parse --git-dir)/boss-build/review-report.md`), or an honest fallback note saying what ran and why there is no report.

## Step 8: Tag and get to green

```bash
# PR_NUMBER was captured in Step 7; re-derive if unset (resume / fresh shell).
PR_NUMBER="${PR_NUMBER:-$(gh pr list --head "$SESSION_BRANCH" --state open --json number -q '.[0].number // empty')}"
test -n "$PR_NUMBER" || exit 1
BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
test -f "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" || exit 1
BASE_BRANCH="$(gh pr view "$PR_NUMBER" --json baseRefName -q .baseRefName)"
git fetch origin "$BASE_BRANCH"
# Rebase all commits since the PR base and inject [#PR_NUMBER] into any missing it.
# Run with a 600s tool timeout. Redirect output to a file, never to head/tail; a
# SIGPIPE during rebase can strand HEAD between commits. If the tool is killed or
# times out, check $(git rev-parse --git-path rebase-merge) and rebase-apply before
# retrying, then follow add-pr-numbers.sh cleanup_temp guidance to continue or abort.
TAG_LOG="$(mktemp -t boss-build-inject-pr-tag.XXXXXX.log)"
# Capture HEAD BEFORE invoking. A non-zero exit does not mean nothing happened: the
# injector rewrites commit by commit, so it can fail having already tagged part of the
# range, and this is the commit that partially-applied rewrite started from.
PRE_INJECT_HEAD="$(git rev-parse HEAD)"
BASE_BRANCH="$BASE_BRANCH" node "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" inject-pr-tag "$PR_NUMBER" >"$TAG_LOG" 2>&1 ||
  echo "inject-pr-tag exited non-zero; HEAD was $PRE_INJECT_HEAD before it ran. See $TAG_LOG" >&2
git push --force-with-lease origin "$SESSION_BRANCH"
test "$(git rev-parse HEAD)" = "$(git rev-parse @{u})" || exit 1  # HEAD == upstream
```

A non-zero injector exit is a disclosure item, not a blocker — history may be partly rewritten, so
check before re-running (it is a no-op on tagged commits). A rejected amend names its reason (type,
missing scope, header or body-line length); fix exactly that. To rewrite a message without an
editor, use `git filter-branch --msg-filter` keyed on `$GIT_COMMIT`, back the range up first, and
prove the rewrite touched only messages (`git diff <backup> HEAD` empty, same commit count).

Then wait for CI (arm watches, bounded poll) and run **boss-repair** for failing checks, conflicts
and review comments, with `BOSS_NOTES_SUPPRESSED=1`, up to `policy.repairCap` (5) passes, re-arming
the CI wait after every push. Still red after the cap ⇒ `BLOCKED` cause 1: PR stays draft, blocker
comment names the failing check, `file:line`, and what was tried.

## Step 9: Decide the route and finalize

Re-inject the tag only if boss-repair added untagged non-empty commits (then push with lease and
wait for CI again). Green CI on the head is the full test run; do not repeat it locally. Then
decide:

- **Every in-scope criterion is met** (each `- [x]` demonstrated by the diff/tests, or a
  `(verify-only)` criterion carrying its recorded check) **and no review finding is open** ⇒
  `REVIEW_READY`.
- **Open review findings** (capped, provisional, unreadable, or an uncertified criterion) on a green
  branch ⇒ `REVIEW_READY` with findings published.
- **Only unmet in-scope criteria remain**, at least one criterion certified by a review that really
  ran, and nothing else open ⇒ `PARTIAL`.

Readying, on every non-BLOCKED route:

```bash
# Gate mergeability before readying. GitHub may report UNKNOWN briefly after a push, so poll with
# a bound; CONFLICTING or any dirty mergeStateStatus means rebase onto the base, re-run the tests
# relevant to the change (## Verification), push, wait for checks, and re-read mergeability.
for attempt in 1 2 3 4 5 6; do
  PR_STATE="$(gh pr view "$PR_NUMBER" --json isDraft,mergeable,mergeStateStatus)"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  if [ "$MERGEABLE" != "UNKNOWN" ]; then break; fi
  sleep 10
done
if [ "$MERGEABLE" != "MERGEABLE" ] || [ "$MERGE_STATE" = "DIRTY" ] || [ "$MERGE_STATE" = "BLOCKED" ]; then
  git rebase "origin/$BASE_BRANCH"
  # Re-run the relevant tests here (## Verification) before pushing.
  git push --force-with-lease origin "$SESSION_BRANCH"
  # The push re-opened the CI wait: arm (callback-watches.md Protocol step 1), then the bounded
  # poll (Protocol step 5) until CI_WAIT_STATE=settled; anything else goes back to Step 8.
  arm_ci_watches "$PR_NUMBER"
  ci_wait_bounded "$PR_NUMBER"
  PR_STATE="$(gh pr view "$PR_NUMBER" --json isDraft,mergeable,mergeStateStatus)"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  test "$MERGEABLE" = "MERGEABLE" || exit 1
  test "$MERGE_STATE" != "DIRTY" || exit 1
fi
# Ready the PR — the finalize adapter's readyPr capability (isDraft==true guard; command: gh pr ready).
if [ "$(printf '%s' "$PR_STATE" | jq -r .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
test "$(gh pr view "$PR_NUMBER" --json isDraft -q .isDraft)" = "false" || exit 1
# Readying is what STARTS the non-draft-only advisory bot, so the merge state degrades on a branch
# that has not changed. Decide that degraded value through the shared classifier, never by reading
# the raw token: --readied-this-run names the post-ready degrade, and its verdict is pending
# (reason advisory-unsettled), non-blocking. Only `blocking: true` routes back to Step 8.
POST_READY_STATE="$(gh pr view "$PR_NUMBER" --json mergeStateStatus -q .mergeStateStatus)"
POST_READY_VERDICT="$(node "$BOSS_BUILD_TOOLBOX/pr-check-state.mjs" merge-state \
  --merge-state "$POST_READY_STATE" --check-state green --check-reason ok \
  --unresolved-threads 0 --readied-this-run)"
test "$(printf '%s' "$POST_READY_VERDICT" | jq -r .blocking)" = "false" || exit 1
```

Then: `please-review` (not on `PARTIAL`), move the ticket `.inProgress → .inReview` (not on
`PARTIAL`), comment the PR URL on the ticket, and publish what the route owes — findings ledger, or
the PARTIAL title/body/marker — per [`references/publish.md`](references/publish.md). The CI reading
that makes a route green is `CI_WAIT_STATE=settled`; `timeout`/`unknown` is not green. A red reading
after readying unwinds first (body back to the plain form, `gh pr ready --undo`, remove
`please-review`), then reports `BLOCKED`.

Stamp each obligation into the route receipt (created in Step 1) as you complete it, on every route:
`node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token <token> --run-id "$BLI_RUNID"`.
Tokens:
`REVIEW_READY` — `verify-only-evidence-validated`, `premise-discharged`, `required-deferred-asserted`,
`pr-ready`, `please-review-added`; `PARTIAL` — `partial-gate-satisfied`, `pr-ready`,
`do-not-merge-marked`; every route — `claim-deleted`, `notes-before-lock-release`,
`stop-hooks-removed`, `lock-released`; optional — `blocked-pr-left-draft`, `entry-state-restored`,
`no-change-breadcrumb-written`.

## Step 10: Settle

Late reviews land after ready. Arm `checks_failed` and `checks_passed_ready` with `--on-transition`
(a state-matched `checks_passed_ready` fires immediately on an already-green PR), back it with the
bounded poll, capped by `policy.settleCap` (3) cycles. Then by source:

- **Bot reviews after a clean verdict** (author `isBot`, REST `"type": "Bot"`, or a `[bot]` login;
  read `REVIEW_VERDICT` from `$(git rev-parse --git-dir)/boss-build-review-verdict`) are advisory:
  fix what is real, push, re-verify, and post one grouped per-finding response per bot review
  ([`references/receiving-code-review.md`](references/receiving-code-review.md)). No settle cycle;
  at most one round per head SHA and three per run.
- **Human change requests and red CI** go back to Step 8 and spend a cycle. `UNSTABLE` with no
  failing check and no open thread is pending, not red.
- Feedback you cannot fix: respond per finding and stay `REVIEW_READY`. Re-quarantine (draft, remove
  `please-review`, blocker comment, `BLOCKED`) only when a `BLOCKED` cause actually holds.

## Step 11: Proof (REVIEW_READY only, never fatal)

Classify with `node scripts/proof.mjs plan` (read `recipes`, `surfaces`, `order`) and run the
change's browser recipes explicitly: `node scripts/proof.mjs run --recipe <id> …`. Its own PR
comment is the only proof channel — never hand-write "proof skipped". TUI proof is driven by the
Step 5 scenario. `node scripts/proof.mjs doctor` explains missing prerequisites. Every failure is
recorded and ignored. [`references/proof-capture.md`](references/proof-capture.md).

## Step 12: Stop cleanly

Every route that took the lock ends here, `foreign` included. Decide `OUTCOME` first; nothing below
may change it.

- Delete this run's claim comment if it still exists.
- On `NO_CHANGE` for a resolved ticket: restore the entry state with `moveState` unless another
  runner owns the ticket or the state is not one this run produced, and leave exactly one short
  breadcrumb comment naming the branch that fired and why (update it on a repeat; no transcripts,
  output or secrets). A failed restore is a warning.
- **Notes** (skip when `BOSS_NOTES_SUPPRESSED=1`): discover `--role notes`; none ⇒ nothing. Roll
  `notesSampleRate` once per run (reuse the roll Step 6.5 left in
  `$(git rev-parse --git-dir)/boss-build-notes-roll` if it is under 12 h old, consuming it). Write at
  most five secret-free observations (≤ 8 KiB) to a temp `observations.md` and dispatch **one**
  awaited worker that runs every extension in `(order, name)` order with the envelope
  `{"role":"notes","core":"boss-build","context":{"mode","core","outcome","repoId","observationPath"},"runTmp","outPath"}`,
  each bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`; validate with `--role notes`. Never fatal.
- Remove bossd's Stop hooks so it does not double-finalize:
  `node "$BOSS_BUILD_TOOLBOX/remove-bossd-stop-hooks.mjs"` (a no-op standalone).
- Release the lock: `"$BOSS_BUILD_TOOLBOX/worktree-lock.sh" release "$BLI_RUNID"`.
- Assert the route receipt (an incomplete receipt only warns; a missing helper is a hard stop):

```bash
# Keep the two streams APART. stdout carries only the honest outcome line; stderr carries the JSON
# detail plus node's own load-time noise, written BEFORE the outcome. Never merge them with 2>&1.
RC_ERR="$(mktemp -t boss-build-route-err.XXXXXX)"
RC_OUT="$(node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" assert --outcome "$OUTCOME" --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --run-id "$BLI_RUNID" 2>"$RC_ERR")" && RC_OK=yes || RC_OK=no
RC_VERDICT="$(printf '%s\n' "$RC_OUT" | head -n 1)"
case "$RC_VERDICT" in
  REVIEW_READY | PARTIAL | BLOCKED | NO_CHANGE | ROUTE_UNSATISFIED) ;;
  # No verdict line at all: the helper is absent, or was called wrong (its usage() exit 2 writes
  # nothing to stdout). That is an ABSENT CAPABILITY, not an accounting gap — it stays a hard stop.
  *)
    cat "$RC_ERR" >&2
    rm -f "$RC_ERR"
    echo "BLOCKED: route-contract helper unusable; no verdict on stdout" >&2
    exit 1
    ;;
esac
if [ "$RC_OK" != yes ]; then
  RC_DETAIL="$(tr '\n' ' ' <"$RC_ERR")"
  echo "warning: route receipt incomplete (${RC_DETAIL:-no detail}) — bookkeeping only, work state unaffected" >&2
fi
rm -f "$RC_ERR"
```

- On `REVIEW_READY` / `PARTIAL`, decide whether you may stop watching CI:

```bash
node "$BOSS_BUILD_TOOLBOX/callback/ci-watch.mjs" classify \
  --check-verdict "$CHECK_VERDICT_JSON" --pr-view "$PR_VIEW_JSON" --watches "$WATCH_LIST_JSON" \
  --target-chat "$BOSS_AGENT_SESSION_ID" --pr "$PR_NUMBER" \
  --triggers "$(
    node --input-type=module -e '
      import{pathToFileURL as u}from"node:url"
      const {resolveCallbackAdapter}=await import(u(process.env.BOSS_BUILD_TOOLBOX+"/callback/adapter.mjs").href)
      process.stdout.write(resolveCallbackAdapter(process.env).policy.watchTriggers.join(","))
    '
  )" \
  ${CALLBACKS_AVAILABLE:+--callbacks-available} --arm-attempts "$ARM_ATTEMPTS"
```

`settled`, `watched`, `polled` ⇒ print. `unwatched` ⇒ arm the `missingTriggers` it names, classify
once more, print. `unknown` (`unreadable-check-state`) ⇒ run the bounded poll, then print. Never
arm twice.

Then print the terminal state.

## Verification

Locally, run only the tests relevant to the change: `commands.testAffected` when the repo has one,
otherwise the tests covering what you changed, through the repo's own runner. A selection that ran
nothing is not a pass. Do not run the full suite locally: CI on the PR is the full check, and Step 8
waits for it. Only when the PR gets no CI checks at all, run `commands.test` once before readying.

## Cron gate

When scheduled as an unattended cron, register the gate command from
[`references/cron-gate.md`](references/cron-gate.md) so a run fires only when a candidate exists.

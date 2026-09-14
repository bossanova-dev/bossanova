---
name: boss-repair
description: Automated PR repair — fixes conflicts, failing checks, and review feedback
---

# PR Repair: Automated Fix Workflow

This skill is invoked automatically by the repair plugin when a PR enters a failing state (red status). It assesses the current PR state, identifies the root cause, and systematically repairs the issue.

---

## When This Skill is Invoked

The repair plugin automatically invokes this skill when:

- **Failing status (3)**: CI checks are failing
- **Conflict status (4)**: Merge conflicts with base branch
- **Rejected status (5)**: Review feedback requires changes

This skill normally runs **automatically** via the repair plugin and performs a focused repair pass per invocation: fix, push, wait for the resulting PR checks to finish, then report green/red/pending status. The repair plugin owns retrying additional attempts when checks remain red.

It may also be run **by hand** as `/boss-repair watch` to keep the whole wait-and-repair loop inside one manual invocation until the PR is green. See [Watch Mode](#watch-mode) below. The repair plugin always invokes this skill with no arguments, so automated runs never enter watch mode.

---

## Linear-History Invariant

**Always sync a branch with its base by rebasing. Never merge the base branch into the session branch.**

```bash
git fetch origin "$BASE_BRANCH"
git rebase "origin/$BASE_BRANCH"
```

A `git merge` of the base ref — and any `git pull` that records a merge — is **FORBIDDEN**. On a repo
whose merge strategy is rebase, a merge commit on the PR branch structurally breaks GitHub's
rebase-merge: GitHub refuses the merge no matter how green the checks are, and every later repair
round that merges again re-poisons the branch, deadlocking the PR. When a pull is unavoidable, use
`git pull --rebase`.

**Preflight — before any push that follows a base sync, assert zero merge commits:**

```bash
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 ||
  { echo "Merge commit(s) on this branch; linearize before pushing"; exit 1; }
```

Capture the count and compare it as a **string**. `test "$(…)" -eq 0` fails **open**: when
`$BASE_BRANCH` is unset or `origin/$BASE_BRANCH` has not been fetched, `git rev-list` errors and the
substitution is empty, and an empty operand compares equal to `0` under zsh — so the guard would pass
and the poisoned branch would be pushed. The form above fails closed in every shell.

**Diagnosis.** A count greater than `0` means one or more merge commits are on the branch — most
often a base merge. That is the one-line signal that GitHub's rebase-merge will refuse this PR. List
what you are about to flatten with `git rev-list --merges --oneline "origin/$BASE_BRANCH..HEAD"`.

**Linearize recovery.** Flattening **discards anything recorded only in a merge commit** — manual
conflict-resolution edits and files added directly in the merge. Run the Strategy A amendment guard
first (it lists those merges via `git show --remerge-diff`) and convert any amendment it reports into
a normal commit before continuing; otherwise this rewrite loses it silently. Then flatten, re-verify
the count is `0`, and force-push:

```bash
git fetch origin "$BASE_BRANCH"
git rebase --onto "origin/$BASE_BRANCH" "$(git merge-base "origin/$BASE_BRANCH" HEAD)"
# Resolve any conflicts in-rebase (Strategy A steps 2-4) until the rebase COMPLETES, then:
if [ -d "$(git rev-parse --git-path rebase-merge)" ] || [ -d "$(git rev-parse --git-path rebase-apply)" ]; then
  echo "Rebase still in progress; finish it or 'git rebase --abort' before pushing"; exit 1
fi
MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
test "$MERGE_COUNT" = 0 ||
  { echo "Branch still has merge commits after linearizing; resolve by hand"; exit 1; }
git push --force-with-lease
```

Each assertion must be `||`-guarded like the ones above — a bare `test` line prints nothing and gates
nothing, so the force-push on the next line would run regardless.

A plain `git rebase "origin/$BASE_BRANCH"` flattens merge commits too; the `--onto` form above is the
explicit equivalent for the linear-base case this recovery targets. Do **not** pass `--rebase-merges`
when linearizing — it recreates the very merge commits you are removing. Strategy A names it only to
explain what its amendment guard protects.

---

## Installed skill drift preflight

Before Phase 1, gate the installed skill tree this run is reading against this checkout's skill
source. Run this before repair writes, branch mutation, or any push. Resolve the toolbox path inside
the block because exported variables do not survive between tool calls.

```bash
if [ -z "${BOSS_SKILLS_HOME:-}" ]; then
  for candidate in "$HOME/.claude/skills" "$HOME/.codex/skills"; do
    if [ -d "$candidate/boss-repair/toolbox" ]; then BOSS_SKILLS_HOME="$candidate"; break; fi
  done
fi
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
  if O="$("$BOSS_BIN" skills check --gate 2>&1)"; then
    if [ -n "$O" ]; then printf '%s\n' "$O" >&2; fi
  else
    case "$O" in
      *--gate*) node "$BOSS_REPAIR_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_REPAIR_TOOLBOX" || true ;;
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
elif [ -f "$BOSS_REPAIR_TOOLBOX/toolbox-drift.mjs" ]; then
  node "$BOSS_REPAIR_TOOLBOX/toolbox-drift.mjs" --toolbox "$BOSS_REPAIR_TOOLBOX" || true
else
  echo "boss-toolbox-drift: (drift helper not installed) — this install predates the check; drift is UNKNOWN, not clean." >&2
fi
```

Drift is **bookkeeping**: both the CLI gate and the no-CLI helper path report it and neither is
terminal, because a drifted-but-present tree still repairs correctly and the helper can only compare
helper files and may itself be stale. A drift report never decides a terminal state; only the repair
work does.

---

## Boss transport preflight

Before Phase 1, decide **which carrier** this run will use for boss session operations — reading the
session's state, its check snapshots, its chats. There are two: the boss MCP tools, and the `boss`
CLI. Validate whichever the runtime actually exposes and BLOCK only when **neither** is complete; a
runtime with no MCP server but a working `boss` binary repairs perfectly well, and stopping it would
be a self-inflicted outage.

Resolve the toolbox in this block before reading the helper, because shell variables do not survive
between tool calls:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
```

Enumerate the available boss MCP tool names and `boss` subcommands from `boss env --json`:
read `.capabilities.mcp` for MCP tool names and `.capabilities.cli` for fully-qualified CLI
commands. `boss --help` is not a valid inventory source: its grouped help output has no stable
`Available Commands:` header and prints only bare top-level names, which cannot prove nested
commands such as `boss chat send`.

Diff both inventories at once with
`bossEpicTransportPreflight({availableTools, availableCliCommands})` from
`$BOSS_REPAIR_TOOLBOX/session/boss.mjs`, which returns `{ ok, transport, missing, degraded,
partial, inventoryHint }`:

The CLI is **preferred, not a fallback** — a complete CLI set wins even when the MCP set is also
complete. That preference is what made it safe to stop wiring the boss MCP server by default, so on
a managed spawn expect `cli`.

- `transport: 'cli'` — every `cli`-mapped capability is reachable, whether or not MCP is also
  complete. Proceed; substitute each capability's `cli` invocation for its MCP spelling.
- `transport: 'mcp'` — the CLI set is incomplete and the tool set is complete. Proceed on the
  richer carrier.
- `ok: false` — neither is complete. Stop `BLOCKED: no complete boss transport: <comma-separated
missing>; <inventoryHint when non-null>`, naming everything absent from both carriers in one line.

**Report the chosen transport in the repair run's opening line** — `transport: <mcp|cli>`, plus
`cli-only mode (expected): <comma-separated capabilities>` when the helper's `degraded` array is
non-empty, plus `partial: <capability>(<missing fields>)` for each entry in `partial`. The array
keeps the field name `degraded` in the return shape, but what it names are capabilities with no CLI
equivalent at all **and never will have one** — `resolveContext`, `getSessionStatuses` and
`createPlanningChat` (three, not two: `boss new` has no `--quick-chat` flag). Print them as
`cli-only mode (expected): resolveContext, getSessionStatuses, createPlanningChat`, and use their
documented fallbacks rather than guessing; saying so out loud is what stops a later reader believing
the run consulted them. That set is the expected steady state of a CLI run, not a fault, so save the
word `degraded:` for a capability missing from **both** transports — which today is exactly the
`ok: false` stop above.

A capability the CLI covers only _partially_ still has a transport, so it is neither `cli-only` nor
degraded, which is why `partial` is a separate list rather than an extension of that one. Today it holds `getSession`:
`boss show --json` carries the lifecycle state and `last_agent_activity_at`, but lacks
`repair_active`, `attention_status.reason`, `pr_mergeable` or `merge_block`. Where a `partial` entry
names a routing signal the CLI cannot supply, treat that signal as **not settled**, never as a pass
— `pr_mergeable` and `merge_block` in particular are how a repair run would otherwise mistake a
conflicting PR for a mergeable one.

Repair reaches for boss session state rarely — `gh` carries most of this workflow — so an incomplete
boss transport is usually a nuisance rather than a stop. Run the preflight anyway: discovering the
gap at the moment you need the value is how a repair run stalls with the PR half-fixed.

---

## GraphQL exhaustion: REST substitutions and degraded reads

GitHub's GraphQL and REST APIs have **separate quotas**, and GraphQL exhausts first. Most `gh pr`
subcommands are GraphQL-backed while `gh api repos/...` is REST, so a pass can be unable to read PR
metadata while still able to read — and write — review comments. This section is what keeps that
asymmetry from stranding a pass or, worse, turning it into a false clean.

**Route on the probe's printed failure class, never on stderr wording.** The review-feedback probe
classifies every `gh` failure it sees and prints the result as `probe_failure_class`. That field is
the definition; this document does not restate the signature list, because a second copy of a
decision ladder drifts from the first.

| `probe_failure_class` | Meaning                                                              | Routing                                                                       |
| --------------------- | -------------------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `rate_limited`        | Quota exhausted; the reset horizon is printed as `probe_retry_after` | Transient. Report a **residual**, retry after the horizon. Never a true stop. |
| `auth`                | 401 or bad credentials                                               | Permanent. **True stop.**                                                     |
| `not_found`           | The repository or PR does not exist or is unreadable                 | Permanent. **True stop.**                                                     |
| `environment`         | `gh` is missing, or the working directory is not a git repository    | Permanent. **True stop.**                                                     |
| `other`               | Unrecognised signature                                               | Treated as **unobserved**. Report a residual; never route it as clean.        |
| `none`                | Nothing failed                                                       | Read `repair_status` normally.                                                |

### REST substitutions for the GraphQL-backed reads

Each row names the GraphQL-backed command and a REST command answering the **same question**. The
GraphQL spelling stays primary — these are fallbacks for when it is blocked, not replacements.

| Question        | GraphQL-backed (primary)                        | REST substitute                                                                                    |
| --------------- | ----------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| PR head SHA     | `gh pr view --json headRefOid -q .headRefOid`   | `gh api repos/OWNER/REPO/pulls/PR_NUM -q .head.sha`                                                |
| PR base ref     | `gh pr view --json baseRefName -q .baseRefName` | `gh api repos/OWNER/REPO/pulls/PR_NUM -q .base.ref`                                                |
| Mergeability    | `gh pr view --json mergeable -q .mergeable`     | `gh api repos/OWNER/REPO/pulls/PR_NUM -q '.mergeable, .mergeable_state'`                           |
| Check results   | `gh pr checks --json name,bucket,state,link`    | `gh api repos/OWNER/REPO/commits/HEAD_SHA/check-runs`                                              |
| Review comments | (probe's GraphQL thread query)                  | `gh api repos/OWNER/REPO/pulls/PR_NUM/comments` — the probe already does this on its degraded path |

The check-runs substitute returns the whole payload; narrow it outside the table, where a `jq` pipe
is not a column separator:

```bash
gh api repos/OWNER/REPO/commits/HEAD_SHA/check-runs \
  --paginate -f per_page=100 \
  --jq '.check_runs[] | "\(.name) \(.status) \(.conclusion)"'
```

A check run whose `conclusion` is empty has not finished. That is pending, not passing — the same
unobserved-is-not-clean rule the mergeability row below states.

**The two APIs do not share a vocabulary, and the difference is where a false clean gets in.**
`gh pr view --json mergeable` returns GraphQL's `MERGEABLE`, `CONFLICTING`, or `UNKNOWN`. The REST
substitute returns a **boolean-or-null** `mergeable` plus a `mergeable_state` from a different
vocabulary entirely: `clean`, `dirty`, `blocked`, `unstable`, `behind`, `unknown`.

| GraphQL       | REST `mergeable` | REST `mergeable_state`                      |
| ------------- | ---------------- | ------------------------------------------- |
| `MERGEABLE`   | `true`           | `clean`, `unstable`, `behind`, or `blocked` |
| `CONFLICTING` | `false`          | `dirty`                                     |
| `UNKNOWN`     | `null`           | `unknown`                                   |

**A REST `mergeable` of `null`, or a `mergeable_state` of `unknown`, is an unobserved result and
must never be routed as "not conflicting".** GitHub computes mergeability asynchronously, so the
first read after a push routinely returns exactly that. Poll again; if it is still unobserved, report
it as a residual. Reading it as "not `CONFLICTING`" is the same false-clean this whole section
exists to prevent, reappearing in the substitute path.

### Which degraded reads continue, and which still block

Adding a fallback to a read that currently blocks relaxes a gate, so every case is adjudicated here.
A case not listed is unlisted, not permitted: treat it as the unrecognised row.

| Condition                                                    | Verdict                                                                       |
| ------------------------------------------------------------ | ----------------------------------------------------------------------------- |
| GraphQL rate-limited, REST answered                          | **Warn and continue, degraded.** The observation is partial but real.         |
| GraphQL rate-limited, REST also rate-limited                 | **Block.** Neither transport answered; there is no observation to degrade to. |
| `auth` on any transport                                      | **Block.** Not transient; continuing would report an unobserved PR.           |
| Genuine 404 on the repository or PR                          | **Block.** The subject is unreadable.                                         |
| Missing `gh`, or not a git repository                        | **Block.** The environment is unusable, and this must stay loud.              |
| Unrecognised failure text                                    | **Warn and continue, degraded, treated as unobserved.**                       |
| REST `mergeable` is `null` or `mergeable_state` is `unknown` | **Warn and continue, treated as unobserved.**                                 |

**A degraded read may downgrade an outcome; it may never upgrade one.** No degraded read may move a
verdict toward `clean`, `MERGEABLE`, or passing.

**Degraded is never silent.** Every degraded read emits one uniform line beginning with the token
`DEGRADED_READ`, so one search over a transcript finds every relaxed read in it. A relaxed gate whose
failures are no longer visible anywhere has been deleted, not relaxed — if you take a degraded path,
say so with that token.

---

## Repair Workflow

### Phase 1: Assess Current State

**1.1 Check PR Status**

Record the PR head SHA **first**, ahead of every other command here, per the round-freshness rule in the [Phase 2](#phase-2-execute-repair-strategy) lead-in:

```bash
gh pr view --json headRefOid -q .headRefOid   # record this value as ROUND_HEAD for the rest of the round
git status                    # Check for conflicts and uncommitted changes
git log --oneline -5          # Recent commits
gh pr view                    # PR details, checks, and review status
```

Both `gh pr view` reads above are GraphQL-backed. If either is blocked by quota, do not stop the
pass here — take the [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads)
for the head SHA, emit the `DEGRADED_READ` line, and continue.

**1.1a Read any work already in the tree**

When `git status` is not clean, read the actual change before authoring anything:

```bash
git status --porcelain
git diff
git diff --cached
```

An interrupted earlier round routinely leaves a complete, coherent fix behind, so for each
unresolved review thread you are about to repair, check whether that pre-existing diff **already
addresses it**.

- **It does.** Validate it — run the repo gates discovered in step 1.3 against it — and commit
  **that** work rather than authoring a replacement; authoring a parallel fix on top of an existing
  one duplicates or regresses it. Commit only the files that belong to the thread under repair, and
  say in the [Repair Summary](#repair-summary) that **pre-existing uncommitted work was adopted**,
  naming those files, so a reviewer can tell adopted work from work this pass authored.
- **It does not** — the diff is partial, incoherent, or unrelated to the thread. Then do **not**
  commit it and do **not** reset it: leave it exactly where it is and record it as a residual.
  Another agent may be writing in this worktree right now, so discarding it is destructive and
  committing it publishes a half-edit.

**1.2 Identify Problem Type**

Based on the output, categorize the issue:

- **Merge Conflict**: Git reports conflicts in files
- **Failing Checks**: PR checks show failures (tests, lint, build)
- **Review Feedback**: PR has requested changes or comments
- **Report-only finding**: a must-fix finding recorded only in the dispatching core's review report or in the PR body, matching none of the three probes above. Read both, and count a finding that is still **open**. The three probes above are places a finding can be written, not a definition of what a finding is — a finding opens a repair cycle because it is real and unresolved, never because of where it happened to be recorded.
- **No problem**: every signal is already clear — checks passing with none pending, `repair_status=clean`, `mergeable` not `CONFLICTING`, and no open report-only finding. `repair_status=not_evaluated` is not clean and never selects this category. This is a **valid categorization result**, not a failure to categorize: route straight to the **nothing to repair** outcome in [Terminal outcomes](#terminal-outcomes) and select no strategy.

The list above is what this round **probes**. What it **reports** is the helper's answer — do not pick
a `**Problem Identified**` value from the list by hand:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
# Write {"conflict":…,"failingChecks":…,"reviewThreads":…,"reportOnlyFindings":[…]} with your file
# tool, then substitute that file's path for <problem-sources-path>. It is a placeholder you fill
# in, NOT a variable this block sets: no shell state survives between the tool calls a pass is made
# of, so a variable would expand to the empty string and the command would exit on an unreadable path.
node "$BOSS_REPAIR_TOOLBOX/bs-repair-derivations.mjs" problem-sources --in "<problem-sources-path>"
```

| `sources` entry       | what this round does                                                        |
| --------------------- | --------------------------------------------------------------------------- |
| `merge-conflict`      | Strategy A.                                                                 |
| `failing-checks`      | Strategy B.                                                                 |
| `review-feedback`     | Strategy C.                                                                 |
| `report-only-finding` | Strategy C, reading the finding from the report or PR body that carries it. |
| `none`                | Route to **nothing to repair** and select no strategy.                      |

**`None` is unreportable while any source is open.** The helper returns `none` only when all four
sources are absent, and a probe answer it cannot read counts as **present** — so a round reports
`None` because it looked and found nothing, never because it did not look. Paste the printed `line`
into the [Repair Summary](#repair-summary)'s `**Problem Identified**` field.

**1.3 Identify Project Gate Commands**

Before running local checks, discover this repo's commands from project instructions, CI, and command files (`Makefile`, `justfile`, `Taskfile.yml`, `package.json`, `go.mod`, etc.). Use the smallest non-duplicative command set that covers the failing area.

### Phase 2: Execute Repair Strategy

For each triggered strategy below, the orchestrator runs that strategy's investigate-and-fix steps and ends with a short summary — files changed, what was fixed, and residual risk. **Where** those steps run is one decision, taken per strategy:

```
subagent tool permitted AND available  -> dispatch a fresh awaited subagent (the expected path)
tool absent, or a higher-priority      -> run the strategy inline (sanctioned)
  instruction prohibits calling it
dispatch attempted and failed          -> run the strategy inline (sanctioned)
```

Dispatch is the normal branch — a session that provisions the subagent tool should use it (`subagent_type: general-purpose`), because the subagent keeps the bulk material out of the orchestrator's context and gives the fix a second voice. The binding is agent-neutral: Claude uses awaited `Task` calls, while Codex uses `spawn_agent` and `wait_agent`. Inline is the **sanctioned** path for a session where the tool genuinely is not usable, not a lesser mode to choose by preference: read the permission actually in force rather than assuming, and record a ledger line naming the tier and reason. A blanket "do not call the subagent tool unless the user asked for it" instruction is one common instance of the second branch; `BOSS_CRON=true` by itself is **not** one — an unattended run may dispatch. Either way the strategy's steps and its reporting are identical; only the context they run in differs, so running inline is never a reason to skip a step or shorten the summary.

Read that blanket instruction against its own condition before routing on it. A managed or cron **dispatch grant** — a session preamble stating that launching the unattended session is the operator's standing request for the protocol-mandated dispatches this skill describes — **satisfies** the "unless the user requested it" condition, so a blanket no-subagent instruction is not an inline-routing trigger in such a session and the first branch of the table applies as normal. Absent a grant, that same blanket instruction still routes to the inline branch.

<!-- tier: opus (no override) because this dispatch runs whichever strategy triggered — A (merge-conflict resolution), B (fixing failing tests/build), or C (implementing review feedback) — all of which author or evaluate code, i.e. judgment. Not tiered down. -->

A dispatch stays on the orchestrator's model (Opus): conflict resolution, failing-check code fixes, and review-feedback reasoning are all judgment, so no cheaper `model:` override is applied. The subagent keeps the bulk material (diffs, CI logs, `gh run view` output, review threads) inside its own context; only the summary returns to the orchestrator, which stays thin. This is orchestration framing only: Strategy A/B/C below are unchanged and are exactly what runs, dispatched or inline. A dispatch is awaited (**never** `run_in_background`) through this core's vendored `toolbox/bs-dispatch-await.mjs` completion oracle, and its failure is a tool error, not a repair failure — it routes to the inline branch above and must never turn a would-be clean exit into a nonzero one.

**What the dispatch brief must carry.** A dispatched worker does not inherit this body — it inherits
the brief, and a rule the orchestrator has read is not a rule the child has read. So state the
scope-limiting rules **in the brief itself**, in the brief's own words rather than as a pointer back
here. The one that costs the most when it is omitted is the **residual-versus-repair rule**: a cause
confirmed to be present on the PR's base is an inherited failure and therefore a **residual** — it is
out of scope for this PR, and the worker reports it rather than fixing it. Omitting that rule does
not produce a worker who fails; it produces one who diagnoses correctly, verifies a real fix, and has
the whole change reverted afterwards because the fix was never in scope, with every hour of
fix-and-verify effort wasted and a less careful run shipping the out-of-scope change. Asserting that
the caller knows a rule proves nothing about the child, so bind the obligation into the dispatched
invocation.

**Gate summaries are quoted, not restated.** When a strategy claims it ran a gate, its short summary
must include the gate runner's **authoritative completion evidence** plus the **verbatim final
summary line** as printed — the test runner's own summary line, the linter's own summary line, or
the runner's closest equivalent. For a multi-command gate, the aggregate command's completed exit
status or status file is the authority; an absent status file is unknown rather than a pass; an early subcommand's passing summary is not. A gate figure
that is not backed by both the completion evidence and quoted line is **unverified** and must be
reported as unverified rather than as a result. The reason is mechanical: a restated number and a
fabricated number have the same shape, so quoting the line byte-for-byte and tying it to the
completed runner is the cheap check that separates a measurement from a guess. Carry those quoted
lines and the completion evidence into the [Repair Summary](#repair-summary)'s `**Gate results**`
field.

**Verdicts and cited evidence are trusted separately.** Before acting on a failed gate summary,
re-derive the failure identity from the test log itself: which tests failed, which package or file
owned the failure, and why the failure belongs to this branch. Assess the returned verdict and the
evidence it cites independently. A correct verdict supported by a mis-cited line is still a
mis-citation; a wrong verdict attached to a correctly quoted line still tells you what the log said.
Never promote narrative to authority when the log is available.

Adjudicate the cited evidence mechanically before any of it is published. Extract each checkable
claim into a JSON list of `{kind, claim}` objects — a cited `file` or `file:line` as `path`, a
quoted short or full SHA as `git-object`, a hash offered as the tree the gates ran on as `tree` —
and run

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
node "$BOSS_REPAIR_TOOLBOX/bs-dispatch-claims.mjs" verify --file <claims.json>
```

A `verified` claim may be cited in a PR reply. A `refuted` one is struck: never publish or forward
it, and treat the conclusion resting on it as unproven — re-derive it from the artefact. An
`unverifiable` one (no git, no repo root, a path outside it) is recorded and not cited; it is never
promoted to verified. Striking a claim does not discard the round: every other claim is adjudicated
on its own. Exit 2 is an operator error — bad usage, an unreadable claim list — and never a verdict:
nothing was adjudicated, so an empty record list there is not "nothing was refuted"; fix the
invocation and re-run before citing any of it.

**Round freshness — capture the head SHA before reading PR state.** The first thing a round does,
before reading review threads, check runs, or mergeability, is record the commit the **PR head**
points at — the commit every check run is attached to. That capture is
`gh pr view --json headRefOid -q .headRefOid`, and it is already the first command of
[Phase 1](#phase-1-assess-current-state) step 1.1, ahead of `gh pr view`.
**Do not run it again here** as the round's routine baseline: re-capturing in Phase 2 would
re-baseline onto a head that has already absorbed any mid-round push, and the comparison below could
never fire. The missing-baseline recovery below is the one exception, and it is safe only because it
re-reads the check runs alongside the re-capture.

The local worktree tip is **not** the value to compare. `git rev-parse HEAD` only moves when this
round itself commits, so it can never see the push by another actor that this rule exists to catch —
and it moves on a local commit that was never pushed and superseded no check run at all.

Carry the printed SHA in your own notes, not only in a shell variable: shell state does not survive
between tool calls, and an empty baseline must never be mistaken for a moved head. If you reach the
freshness test without a recorded baseline, do not cancel and do not proceed on what you already
read: re-capture the PR head and re-read the check runs against it, so the view you act on is
coherent with a baseline you hold. A missing baseline is never on its own a reason to cancel.

**Four baselines, four questions — no one of them substitutes for another.** A round holds more
than one piece of state, and each piece answers a different question about a different actor:

| name             | value                                                                      | question it answers                         |
| ---------------- | -------------------------------------------------------------------------- | ------------------------------------------- |
| `ROUND_HEAD`     | the PR head at round start (`gh pr view --json headRefOid -q .headRefOid`) | did the PR head move mid-round?             |
| `PUSHED_HEAD`    | the SHA this run authored and sent, captured before the push               | is this run's own work still on the branch? |
| `PREV_PASS_HEAD` | the PR head the previous [Watch Mode](#watch-mode) pass started from       | was the branch replaced since my last pass? |
| `$BEFORE`        | the local tip before a [Watch Mode](#watch-mode) pass                      | did _this pass_ make progress?              |

They are **four different questions**, so an answer to one is never an answer to another: a
`ROUND_HEAD` that still matches does not prove your commit survived, a local tip that moved does not
prove _this pass_ pushed, and a fast-forward since the last pass does not make an earlier pass's
verdict current. Record each one in your own notes as you obtain it, for the reason the paragraph
above already gives — shell state does not survive between tool calls.

**Handle review feedback before CI failures.** Thread content is stable: a reviewer's words mean the
same thing whether or not another commit has landed since they were written, so feedback can be
acted on exactly as read. Check runs are volatile — each describes one specific commit, and a push
that lands mid-round supersedes every check result read before it. Taking the stable half first
leaves only the volatile half needing a freshness test.

**Re-read the head SHA before acting on a CI failure, and skip the failure when it has moved.**
Re-read the PR head (`gh pr view --json headRefOid -q .headRefOid`) and compare it against
`ROUND_HEAD` at the moment you are about to act on a check
failure. If they differ, this round's CI view describes a superseded commit:
do not fix it and do not push a commit for it.
Check the head once more immediately before you push a CI fix, too: the window between deciding to
act and pushing is small but not empty, and a head that moved inside it supersedes the fix you are
about to send. Cancel the CI half then as well rather than force-pushing over the newer commit.
Keep the commit you already made — do not reset it — and name it in the residual as built but unpushed;
the branch being ahead of origin is expected in this one case, so Phase 3's clean-tree check,
Watch Mode's no-progress comparison, and the `PUSHED_HEAD` survival assertion below all read it as
the cancellation it is, not as progress and not as a clobber.
Skip only the CI half of the round — any conflict repair or review work this round still owes is
unaffected and is still due first; end the pass only once that work is done.
Report the superseded CI view as a residual and **exit zero**, per
[Residuals vs true stops](#residuals-vs-true-stops).

That skip is **cancellation, not error**. A superseded round is a normal outcome, not a failed one:
the push that superseded it raises its own signal and gets its own round, which reads a coherent
view of the new head. Nothing is broken and nothing is lost, so a cancelled round
must never exit non-zero. In Watch Mode the cancellation is not an exit at all: skip the superseded
CI view, return to the poll loop, and read the new head's state on the next pass, per
[Watch Mode](#watch-mode) — the exit-zero form applies to default mode only.

**A round's own push never marks that round stale.** The freshness test is applied only _before_ this
round acts on a check failure, and only against commits this round did not author. Once the round has
fixed a failure and pushed, the head is _expected_ to differ from `ROUND_HEAD` — that is the round
succeeding by its own hand, not going stale. After **every** push this round makes — including the
review-feedback push that the ordering above puts first —
re-baseline `ROUND_HEAD` to the commit you just pushed
(`git rev-parse HEAD` immediately after your own push is exactly that commit)
**and re-read the check runs**: a CI view read before any push, your own included, describes a
superseded commit and must be re-read rather than acted on. You never have to inspect authorship — because you
re-baseline after every push you make, any remaining difference from `ROUND_HEAD` is by construction
a commit this round did not author. Only a head that moved before this round acted, by a commit this
round did not author, cancels it.

Check runs for a commit you just pushed usually do not exist yet, so that re-read is a freshness
check inside the pass, not a second repair pass and not the Phase 3 poll. If it returns nothing, or
only pending runs, there is no fresh CI view to act on: leave it to Phase 3's post-push poll (Watch
Mode: to the next poll) rather than falling back on the pre-push results. `$BEFORE` in
[Watch Mode](#watch-mode) step 1 is a different baseline for a different question — it is the local
tip and detects whether _this_ pass made progress, while `ROUND_HEAD` is the PR head and detects a
push this round did not make. They are not interchangeable.

**Record `PUSHED_HEAD`, and verify this run's own commit survived.** `PUSHED_HEAD` is the SHA **this
run authored and sent** — capture it from your own commit (`git rev-parse HEAD` immediately after
`git commit` and **before** the `git push` that sends it) and record it in your own notes rather than
only in a shell variable. Do **not** take it from the post-push re-baselining above: that read is
right for `ROUND_HEAD` and wrong for this one, because a concurrent writer sharing this worktree can
move local HEAD onto its own commit before your push runs, and a post-push read would then record the
clobbering commit as this run's — so the survival assertion below would pass on precisely the failure
it exists to catch. The two values are equal whenever the push was genuinely this run's and diverge
the moment anyone else's commit is at the head, and it is `PUSHED_HEAD` — never `ROUND_HEAD` — that
answers whether this run's work is still on the branch.

**A commit you deliberately withheld never sets `PUSHED_HEAD`.** The stale-SHA cancellation above
keeps its commit and does not push it, so there is nothing sent for the survival check to be about:
leave `PUSHED_HEAD` unset there and report that commit as built but unpushed. Setting it anyway
would make the assertion below fail — the commit is not on origin, because you chose not to send it
— and print a concurrent writer that does not exist. The same holds when a push you did attempt was
**rejected**: nothing was sent, so unset `PUSHED_HEAD` and report the rejection, rather than letting
the assertion describe a commit that was never replaced because it never arrived.

`git push` printing `Everything up-to-date` is **not** proof your commit is on the branch. It proves
only that local HEAD already equals the remote ref, which is equally true when another writer
sharing this worktree moved local HEAD there — exactly what a force-push over this run's work looks
like from inside the worktree. Treat it as an unresolved signal, never as a successful push.

Before reporting a fix as landed in [Phase 3](#phase-3-verify-and-monitor), assert your work is
still reachable by **ancestry, not equality** — once for **every** SHA your notes recorded, not only
the most recent. A [Watch Mode](#watch-mode) round sends one per pass that pushed, and a clobber of
an earlier pass's commit is just as much a false report as a clobber of the last one.

**Re-hydrate the list from your notes before you read it.** No shell variable set by the push — or by
any earlier tool call in this round — is still in scope here, so the round-wide record lives in your
notes and is pasted back into `SENT_SHAS` at the point of use. A block that instead read a bare
`$PUSHED_HEAD` left over from the push would find it empty on every pass, take the "sent nothing"
arm, and report a clean survival for a round whose commit had in fact been replaced:

```bash
BRANCH=$(git branch --show-current) || exit 1
[ -n "$BRANCH" ] || exit 1
git fetch origin "$BRANCH" || exit 1
# Re-hydrated from your notes: every SHA this round sent, ONE PER LINE, oldest first.
# Spaces and tabs are tolerated too — the `tr` below normalises them to newlines.
SENT_SHAS="<paste the SHAs your notes recorded, one per line; empty if this round sent none>"
if [ -z "$SENT_SHAS" ]; then
  echo "this round sent nothing — nothing of this run's to verify (a commit withheld by the stale-SHA cancellation is reported as built but unpushed, not as a survival failure)"
else
  printf '%s\n' "$SENT_SHAS" | tr ' \t' '\n\n' | while IFS= read -r SENT_SHA; do
    [ -n "$SENT_SHA" ] || continue
    if git merge-base --is-ancestor "$SENT_SHA" "origin/$BRANCH"; then
      echo "this run's commit is still on the branch ($SENT_SHA)"
    else
      echo "RESIDUAL: a concurrent writer replaced this run's commit ($SENT_SHA is no longer an ancestor of origin/$BRANCH)"
    fi
  done
fi
```

**Paste the SHAs one per line, and iterate them with `read`, not with `for … in $SENT_SHAS`.** The
delimiter is the whole correctness of this check. Shells disagree about splitting an unquoted
parameter expansion — bash splits it into words, zsh with default options does not — so a
space-separated list read by a bare `for` arrives at `git merge-base --is-ancestor` as **one
argument** under zsh. Measured: that fails with `fatal: Not a valid object name`, the `else` arm
fires, and the block reports a **fabricated** `RESIDUAL` for commits that are in fact fine. Reading
one line at a time iterates the same items under either shell.

The paste is the one input in this block with **no machine producer**, so the reader does not trust
it to arrive newline-delimited. The `tr ' \t' '\n\n'` stage normalises spaces and tabs to newlines
before `read` ever sees them. Measured with `SENT_SHAS="deadbeef cafebabe"`: without that stage the
loop runs **once** with `deadbeef cafebabe` as a single value under **both** bash and zsh — the
expansion is quoted, so this one is not a zsh-only divergence — and `git merge-base --is-ancestor`
is handed the whole string, reproducing exactly the fabricated `RESIDUAL` this form exists to
prevent. With the stage it runs twice, `deadbeef` then `cafebabe`, under both shells. One per line
is still the form to paste; whitespace-separated is simply no longer a wrong answer.

Every substitution is checked, and both empty operands are handled rather than left to fall through:
`git branch --show-current` exits **zero with empty output** on a detached HEAD, so `|| exit 1` alone
does not catch it, and an empty `SENT_SHAS` would run the loop body **zero times** — the `[ -n ]`
guard discards the single blank line `printf` emits for an empty value — printing nothing at all,
which reads downstream as "no residual found" rather than as "nothing was checked". The explicit
`[ -z ]` arm is what makes a round that pushed nothing say so out loud.

**Every** entry is checked and reported, not just the newest. Stopping at the first survivor would
miss exactly the case the round-wide record exists for — pass 2 landing cleanly while a peer
force-pushed away what pass 1 sent — and a single residual anywhere in the list means the repair did
not fully land.

Ancestry rather than equality is load-bearing in both directions. Equality would fire on every
benign advance — a peer adding commits **on top** of yours is not a clobber — while a plain
head-SHA match cannot see that your commit is gone at all, because the commit that replaced it is a
perfectly valid new head. When the assertion fails: **do not re-push and do not force-push over the
newer commit.** Report it as a **residual** naming both SHAs, and do not claim the repair landed.

#### Strategy A: Merge Conflicts

**Symptoms**: Git reports conflicts, PR status shows conflict

**Resolution**:

1. Fetch and rebase onto the PR base branch — never merge it in, per the
   [Linear-History Invariant](#linear-history-invariant).

   The `gh pr view --json baseRefName` read below is GraphQL-backed, and its `2>/dev/null || true`
   swallows a quota failure into an empty `BASE_BRANCH` — which the guard then reads as "no
   resolvable base", a permanent-looking outcome from a transient cause. When the base comes back
   empty, take the [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads)
   row for the base ref before concluding there is no base:

   ```bash
   BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
   if [ -z "$BASE_BRANCH" ]; then
     CURRENT_BRANCH=$(git branch --show-current)
     # `head -1 | cut` and not an `awk` field reference: this file is also reachable as a slash
     # command, and the harness rewrites every positional parameter in the body — a dollar sign
     # followed by one digit — before any shell runs it. Invoked without arguments each becomes
     # nothing, so an awk program that selects a field arrives with an empty print list, which
     # awk accepts: it prints the whole line and hands back a wrong branch at exit 0. (This
     # comment spells no positional itself, or the substitution would eat the example too.) The
     # `test -n` guard below covers the unrelated case: no candidate branch at all. One behavioural
     # difference the rewrite does introduce: `head -1` exits as soon as it has the first line where
     # awk consumed all of the input. This block sets no `pipefail`, so the substitution's status is
     # unchanged today — but under a future `set -o pipefail` a SIGPIPE'd `sort` would fail it.
     BASE_BRANCH=$(
       git for-each-ref --format='%(refname:short)' refs/remotes/origin |
         sed 's#^origin/##' |
         grep -Fvx HEAD |
         grep -Fvx "$CURRENT_BRANCH" |
         while read -r branch; do
           git merge-base --is-ancestor HEAD "origin/$branch" && continue
           base=$(git merge-base HEAD "origin/$branch" 2>/dev/null) || continue
           printf '%s %s\n' "$(git show -s --format=%ct "$base")" "$branch"
         done |
         sort -nr |
         head -1 |
         cut -d' ' -f2
     )
     if [ -n "$BASE_BRANCH" ]; then echo "Using inferred git base branch: $BASE_BRANCH"; fi
   fi
   test -n "$BASE_BRANCH" || { echo "Could not determine PR base branch"; exit 1; }
   git fetch origin "$BASE_BRANCH"
   # Conflict-sizing preflight, BEFORE the rebase. merge-tree computes the merge in the object
   # database only: it writes no index entry, no worktree file and no ref, so it does not touch the
   # worktree at all. That is what makes it safe here — a peer session may be mid-edit in this
   # worktree, which is the same reason `git stash` is forbidden throughout this document. Record the
   # files it names as this round's conflict scope before anything starts rewriting history.
   CONFLICT_SCOPE=$(git merge-tree --write-tree --name-only "origin/$BASE_BRANCH" HEAD)
   CONFLICT_STATUS=$?
   if [ "$CONFLICT_STATUS" -eq 0 ]; then
     echo "conflict scope: none — the rebase is expected to apply cleanly"
   elif [ "$CONFLICT_STATUS" -eq 1 ]; then
     echo "conflict scope (only these files need resolving):"
     printf '%s\n' "$CONFLICT_SCOPE" | tail -n +2
   else
     echo "conflict scope: unreadable (merge-tree exited $CONFLICT_STATUS) — size it from the rebase itself, do not report it as none"
   fi
   MERGE_AMENDMENTS=$(
     git rev-list --merges "origin/$BASE_BRANCH"..HEAD |
       while read -r merge_commit; do
         if [ -n "$(git show --remerge-diff --format= "$merge_commit")" ]; then
           echo "$merge_commit"
         fi
       done
   )
   if [ -n "$MERGE_AMENDMENTS" ]; then
     echo "Merge commit amendments detected; automatic rebase could drop manual conflict-resolution edits:"
     echo "$MERGE_AMENDMENTS"
     echo "Resolve manually or convert those amendments into normal commits before rebasing."
     exit 1
   fi
   git rebase "origin/$BASE_BRANCH"
   ```

   Use the **amendment guard** (the `MERGE_AMENDMENTS` probe above — distinct
   from the merge-count preflight in the
   [Linear-History Invariant](#linear-history-invariant)) because a plain rebase
   flattens merge commits but does not preserve manual conflict-resolution edits
   or files added directly in merge commits. Only rebase automatically when the
   guard finds no merge-commit amendments. `--rebase-merges` would preserve that
   shape, but it recreates the merge commits the invariant forbids — so when the
   guard trips, lift those amendments out of the merge and re-run the plain
   rebase; never resolve it with a new merge.

   This block is a **separate tool call from the guard above, so it inherits none of its
   variables** — `MERGE_AMENDMENTS` and `BASE_BRANCH` are both empty here unless re-hydrated. Paste
   the SHAs the guard printed, the same way [Phase 2](#phase-2-execute-repair-strategy) re-hydrates
   `SENT_SHAS`, and re-read the base branch — and when that read comes back empty, take the
   [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads) row for the base
   ref before concluding there is no base. The `[ -n "$AMEND_LIST" ]` guard is what stops the
   block: capturing nothing and then rebasing anyway would flatten the very merge whose
   conflict-resolution edits this capture exists to save, and it would do it silently.

   ```bash
   # Re-hydrated from the guard's output: the merge SHAs it printed, ONE PER LINE (spaces and tabs
   # are tolerated — the `tr` below normalises them). This block inherits nothing from the guard.
   MERGE_AMENDMENTS="<paste the merge SHAs the guard printed, one per line>"
   BASE_BRANCH=$(gh pr view --json baseRefName -q .baseRefName 2>/dev/null || true)
   test -n "$BASE_BRANCH" || { echo "No base branch; re-run the base-branch resolution above"; exit 1; }
   # One commit per line, read one line at a time: a bare `for … in $MERGE_AMENDMENTS` would iterate
   # ONCE under zsh, which does not word-split an unquoted parameter expansion, and write every
   # amendment into a single patch file named after the whole list.
   AMEND_LIST=$(printf '%s\n' "$MERGE_AMENDMENTS" | tr ' \t' '\n\n' | grep -E '^[0-9a-fA-F]{7,40}$')
   # Fail CLOSED: an unpasted placeholder, an empty value, or a value that survived from nowhere all
   # arrive here as an empty list, and the rebase below would then flatten the merge having saved
   # nothing at all.
   test -n "$AMEND_LIST" || { echo "No amendment SHAs re-hydrated; do NOT rebase"; exit 1; }
   printf '%s\n' "$AMEND_LIST" | while IFS= read -r merge_commit; do
     [ -n "$merge_commit" ] || continue
     git show --remerge-diff --format= "$merge_commit" > "/tmp/amend-$merge_commit.patch"
   done
   git rebase "origin/$BASE_BRANCH"          # flattens; the amendments are now saved off-branch
   git apply "/tmp/amend-<sha>.patch"        # re-apply each captured amendment, oldest first
   git add -A && git commit -m "fix: re-apply conflict resolution from flattened merge"
   ```

   Confirm each `/tmp/amend-<sha>.patch` exists before trusting the rebase: the capture loop runs in
   a pipeline, and the two shells disagree about whether that body's assignments survive it, so a
   counter incremented inside it is not evidence either way. The list guard above is, because it is
   checked before the pipeline starts.

   If an amendment does not re-apply cleanly, stop and escalate per
   [Complex Conflicts](#complex-conflicts) rather than merging the base in.

2. Identify conflicting files:

   ```bash
   git diff --name-only --diff-filter=U
   ```

3. For each conflicting file:
   - Read the file to see conflict markers (`<<<<<<<`, `=======`, `>>>>>>>`)
   - Understand both versions. During rebase conflicts, Git labels can feel
     reversed from a merge: `ours` is the upstream/base branch being rebased
     onto, and `theirs` is the replayed PR commit.
   - If the conflict is in a generated artifact, resolve the source inputs and
     regenerate the artifact; never hand-edit the generated hunk as the fix.
   - If the conflict is additive-vs-additive in an append-only registry, keep
     BOTH sides unless a documented uniqueness rule says one entry supersedes
     the other.
   - Resolve by:
     - Keeping both changes if they're independent
     - Choosing the correct version if they conflict
     - Merging logic intelligently if needed
   - Use Edit tool to remove conflict markers and apply resolution

4. Continue the rebase:

   ```bash
   git add <resolved-files>
   GIT_EDITOR=true git rebase --continue
   ```

   Set `GIT_EDITOR=true` so automated repair accepts the existing replayed
   commit message instead of blocking or failing when no interactive editor is
   available.

   If `git rebase --continue` reports that the replayed commit became empty,
   inspect the resolution and confirm the base branch already contains the
   commit's intended change. When the patch is genuinely redundant, skip that
   replayed commit:

   ```bash
   git rebase --skip
   ```

   Do not preserve empty commits with `git commit --allow-empty` unless the PR
   intentionally needs an empty marker commit.

   Repeat steps 2-4 until the rebase completes. Do not create a merge commit.

5. Test the rebased branch with the repo's formatting and test gates:

   ```bash
   # Examples only; use the commands discovered for this repo
   pnpm lint && pnpm test
   go test ./...
   cargo test
   ```

   After the whole rebase completes, run the project's configured
   `commands.postRebase` check once. Do this after the final replayed commit,
   not at the individual conflicting commit: a later replay can silently stale
   a regenerated artifact again with no second conflict marker. If the branch
   refactored a call shape, grep the post-rebase tree for the OLD shape before
   trusting the clean rebase exit, explicitly including files the base added
   that this branch never touched, then run the affected module's tests.

6. Run the merge-commit preflight, then push the rebased branch:
   ```bash
   MERGE_COUNT=$(git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD) || exit 1
   test "$MERGE_COUNT" = 0 ||
     { echo "Merge commit(s) on this branch; linearize before pushing"; exit 1; }
   git push --force-with-lease
   ```
   A nonzero count means the branch is poisoned; run the linearize recovery in the
   [Linear-History Invariant](#linear-history-invariant) before pushing.

#### Strategy B: Failing Checks

**Symptoms**: PR checks show failures (tests, lint, build errors)

The A/B/C ordering here is presentational, not an execution order. If review feedback is also present, run [Strategy C](#strategy-c-review-feedback) first and apply the round-freshness rule from the Phase 2 lead-in before acting on anything below.

**Resolution**:

1. Identify which checks are failing:

   ```bash
   gh pr checks --json name,bucket,state,link
   ```

2. Get failure details:

   ```bash
   gh run view <run-id>     # View specific check run details
   ```

   If checks are still pending, do not block only on CI. First probe review threads and mergeability, then poll again as described in Phase 3 or Watch Mode.

   **An exit code is not a finding.** Before treating a red gate as a defect in this branch, read the failing output. A lock-contention warning from a tool that permits only one instance at a time, a signing or memory failure raised by a commit hook, and a failure in a file this change does not touch are infrastructure flakes, not findings; a failure already present on the PR's base is an inherited failure, not a finding — each produces a non-zero exit that says nothing about the branch. Re-run the affected target in isolation to confirm, compare the failure against the base when inheritance is plausible, and consult the repo's own agent instructions for the flake signatures it records. Only once the output names something this branch changed and the same cause is not already present on the base is there a failure to repair.

3. Triage failing checks by root cause before choosing a repair branch:
   - Derive a failure signature from each failing check's **output**, not from the check name.
   - Group the failing checks into a `cause -> checks it explains` table, and record that table in the Repair Summary.
   - Route by the number of distinct causes. The cause count drives routing, effort sizing, and whether fixes split into separate commits; the red-check count is only a symptom count and drives none of them.
   - Confirm each candidate cause against the PR's base before treating it as this branch's finding. Prefer reading the base branch's own recorded CI signal because that path is read-only and does not mutate the session worktree.
   - If the base has no recorded signal for that gate, reproduce the candidate cause at the merge-base in a bounded throwaway worktree. Never check out the base in the session worktree, and never use `git stash`; a concurrent writer may share this worktree, and stash operations can disturb unrelated work.
   - A cause confirmed to occur on the base is a **residual** with the confirming evidence recorded. Do not repair it in this pass, do not mint a new terminal outcome, and do not mint a new watch token.

4. For **test failures**:
   - Read test output to identify failing tests
   - Read the test file and implementation
   - Fix the root cause (not just the symptom)
   - Run the relevant test command locally to verify:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm test
     go test ./...
     cargo test
     ```
   - Commit the fix:
     ```bash
     git add <fixed-files>
     git commit -m "fix(tests): resolve failing test in <component>"
     git push
     ```

5. For **lint/format failures**:
   - Run the repo's formatter or lint fixer:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm lint --fix
     gofmt -w <files>
     cargo fmt
     ```
   - Commit if changes were made:
     ```bash
     git add .
     git commit -m "style: apply formatting fixes"
     git push
     ```

6. For **build failures**:
   - Read build output to identify error
   - Fix compilation/build issues
   - Verify locally with the repo's build command:
     ```bash
     # Examples only; use the command discovered for this repo
     pnpm build
     go test ./...
     cargo build
     ```
   - Commit and push the fix

#### Strategy C: Review Feedback

**Symptoms**: PR has requested changes or review comments

**Resolution**:

1. List all unresolved review threads and inline review comments.

   Do not rely on `gh pr view --comments` or `gh pr view --json comments` for this step. Those only cover PR conversation comments and can miss inline review comments with URLs like `#discussion_r...`.

   First, run the review feedback probe from the session worktree, using the installed skill script
   by absolute path. This script uses both required GitHub APIs and prints a compact summary, so a
   blank result cannot be mistaken for "no comments."

   ```bash
   BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
   if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
   node "$BOSS_REPAIR_PROBE"
   ```

   Probe interpretation rules:
   - The first line is a probe contract version. Treat an unrecognised contract version as
     `repair_status=not_evaluated`, never as a content verdict.
   - `probe_status=failed`: the probe failed. Fix the command or auth issue; do not report "no review feedback."
   - `probe_status=degraded`: GraphQL was exhausted, so the probe fell back to a REST-only read. It
     printed the REST-visible review comments clustered by reply parentage under
     `DEGRADED_COMMENT_CLUSTERS`, but REST cannot see thread resolution state, so this is an
     explicitly **partial** observation. Act on the printed comments; never read it as clean.
   - `probe_status=suspicious_zero`: `latestReviews` contains `COMMENTED`, but both probes found zero comments. Treat this as not evaluated for repair routing; do not conclude no feedback exists.
   - `probe_degraded`, `probe_failure_class`, and `probe_retry_after` are printed on **every** path,
     so an absent field is never ambiguous. `probe_failure_class` is what makes residual-versus-true-stop
     mechanical: route it through the class table in
     [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads) rather than by
     reading the error text yourself. `probe_retry_after` is the reset horizon in epoch seconds, or
     `unknown`, or `none` when nothing failed.
   - The probe's exit status distinguishes the same three cases: `0` observed, `1` hard failure,
     `2` suspicious zero, `3` degraded. A non-zero status is **not** by itself a true stop — read
     `probe_failure_class` before deciding.
   - Empty stdout, or stdout without a `probe_status=` line, is a probe failure, not a zero-comment result.
   - Trust `repair_status` as the normalized result only when `probe_status=ok`.
   - `repair_status=clean`: there are no unresolved review threads. Historical REST `inline_comments`, resolved GraphQL `review_threads`, and `COMMENTED` latest reviews do not require action by themselves.
   - `repair_status=needs_repair`: handle every printed unresolved thread.
   - `repair_status=parked`: every unresolved thread is waiting on a human. Do not re-dispatch repair work unless a later probe reports `needs_repair`.
   - `repair_status=not_evaluated`: review state was not successfully observed. It is never
     `clean`. **Decide residual versus true stop by reading `probe_failure_class`, not by judging
     the error text by eye**: `auth`, `not_found`, and `environment` are true stops; `rate_limited`
     and `other` are reported residuals, and `rate_limited` carries a `probe_retry_after` horizon to
     retry after. The full mapping is the class table in
     [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads).

     A park keys on the **reviewer's last-comment identity**, not on the branch head, so the branch can move underneath a park and address the complaint without ever unparking it. **Never carry a prior pass's parked verdict forward.** Before reporting a parked thread as a residual, **re-derive its premise against current HEAD**: grep the files the parked comment names for the feature keyword it disputes. That check is decisive in both directions — the same files and keyword that established the premise settle whether it still holds. If the premise no longer holds, clear the park, reply citing the file and line that now satisfies it, and resolve the thread:

     ```bash
     BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
     if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
     node "$BOSS_REPAIR_PROBE" mark --thread THREAD_ID --disposition open --repo OWNER/REPO --pr PR_NUM --host HOST
     ```

   - `repair_status=unknown`: REST and GraphQL disagree or thread state is unavailable after a
     successful observation. Do not treat reviews as clean.

2. Group the unresolved threads by the file each thread anchors to, then triage every thread in each group. A thread with no file anchor goes into a single `no file` group, so the rule is total. When the Phase 2 dispatch branch is available, dispatch one worker per file group for **verdict-only** analysis; that worker returns a **separate verdict per thread** in its group, using the four categories below, and may propose a patch shape but must not edit, commit, push, reply, or resolve. When the documented inline branch is active because the awaited subagent tool is absent or dispatch failed, the orchestrator triages each group itself using the same four categories and still records a separate verdict per thread. The orchestrator serializes all repository-mutating work after verdicts are known: apply any fixes, run gates, commit, push, reply, and resolve threads from one owner. The file is the first conflict unit, but shared helpers, generated artifacts, lockfiles, the Git index, and the branch tip are repository-wide; verdict-only workers keep parallel triage from becoming concurrent writers while still letting the orchestrator resolve or decline each thread independently.

   **Partition the threads by author before triaging — the source decides whether a repair cycle
   opens at all.** Identify an automated reviewer generically from the review author, never from a
   list of product names: the GraphQL author's `isBot` field, the REST author's `"type": "Bot"`
   value, or a login ending in the `[bot]` suffix. Any one signal is sufficient. When the
   originating build run recorded `REVIEW_VERDICT=clean` — read it from
   `$(git rev-parse --git-dir)/boss-build-review-verdict`, the run note that build wrote in this
   worktree — a bot-authored review is advisory. It gets exactly one grouped response comment per
   bot review, posted within the bot's own threads, carrying a per-finding reason for every finding
   it raised — never a blanket dismissal, and never silence. Answer the bot threads once and resolve
   what that response settles; do not open a repair cycle over a diff that run's own review already
   passed. Advisory is not ignored: a bot finding that names a real defect is still fixed — advisory
   means it does not mechanically open a fix cycle, not that the finding is dropped. Human
   changes-requested threads and red CI are unchanged: they triage and repair exactly as below. Read
   CI through the `pr-check-state.mjs` verdict — a PR that flips to `UNSTABLE` after being readied
   classifies as pending (`advisory-unsettled`), not red CI. The
   shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; `capped`,
   `none`, or an absent record means bot feedback is triaged exactly as today.

   For each thread, triage into one of four categories. The triage turns on two axes, in this order: the **premise** — is each factual claim the finding makes true against the tree, graded one premise at a time — and then the **remedy** — must each separable part of the suggested change be applied as written? A true premise does **not** by itself license implementing the suggestion, and grading only the premise is what leaves a correct finding with an unbuildable remedy homeless. Both axes decompose because a whole-finding verdict is either a wrong accept or a wrong reject the moment the parts disagree: a finding's premises are its distinct factual claims, each link of any causal chain it asserts, and each separable part of the remedy it proposes.

   **a) Actionable — fix it:**
   - Read the relevant code/files
   - Implement the requested change
   - Mark the thread as dispatched before acting on it:
     ```bash
     BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
     if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
     node "$BOSS_REPAIR_PROBE" mark --thread THREAD_ID --disposition dispatched --repo OWNER/REPO --pr PR_NUM --host HOST
     ```
   - Add a reply comment on the thread explaining what was fixed:
     Reply bodies are Markdown and must never be shell-interpolated. `-F` is selected here because
     the required local stub check proves its `@file` form passes the file's bytes through literally.
     First run the path-creation block by itself. It prints a concrete temporary path for the
     file-editing tool. Use the agent's file-editing tool to write the complete reply text to that
     path. Then put the same printed path in the submission block. Do not use shell input, redirection, `printf`, or a
     heredoc for the reply text: automatic repair has no interactive stdin, and shell source can
     reinterpret Markdown. The file must contain only the reply text.
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Use the agent's file-editing tool to write the exact reply text to the printed path, then:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY" -q .html_url; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Resolve the parent review thread:
     ```bash
     gh api graphql -f query='
       mutation {
         resolveReviewThread(input: {threadId: "THREAD_ID"}) {
           thread { isResolved }
         }
       }'
     ```
   - **If that resolve fails on GraphQL quota, defer it — do not re-post the reply.** Thread
     resolution is GraphQL-only; REST has no equivalent, so the fallback here is deferral, not
     substitution. The reply above has already landed, so record the owed resolve and move on:
     ```bash
     BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
     if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
     node "$BOSS_REPAIR_PROBE" defer --thread THREAD_ID --reply REPLY_URL --repo OWNER/REPO --pr PR_NUM --host HOST
     ```
     A deferred resolve is a **transient residual** with a retry horizon — never a permanent
     residual, and never a true stop. Report it as such and exit zero. A later pass drains it with
     `node "$BOSS_REPAIR_PROBE" drain --repo OWNER/REPO --pr PR_NUM --host HOST`, which retries the
     **resolve only**: the stored record holds no reply body and no reply endpoint, so a drain
     cannot double-post the comment that already landed.

   **b) Premise does not hold — decline and resolve:**
   The finding is by design, stale (references old code), a low-priority style suggestion, or already satisfied in the tree. For these:
   - An **already fixed** decline must cite the **file and line** in the current tree that satisfies the finding. A commit hash, a commit subject line, or "fixed in a later commit" is **not** sufficient: a subject states intent and names its scope loosely, so a commit reference alone cannot close a thread, and settling the decline against one can resolve a thread over a live bug.
   - Decline per premise, not per finding: a finding whose premises split — some refuted, some holding — is not a category (b) thread at all, and declining it wholesale discards the claims that held.
   - **Repair the prose that asserted the refuted premise.** When a premise is refuted because in-tree prose asserted it, the decline is not complete until that prose is corrected in the same pass. The sentence that made the reviewer's reading reasonable is what re-seeds the identical finding next pass, so declining the thread while leaving it standing ships a contradiction and guarantees a repeat. This is scoped to the sentence that asserted the premise — a comment, a doc line, a rationale paragraph — not to a general sweep.
   - Add a reply comment explaining why it won't be fixed:
     First create and print a temporary path, then write the reply to that exact printed path with
     the agent's file-editing tool:
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Submit that same path after the file-editing tool has written the reply:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY" -q .html_url; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Then resolve the thread:
     ```bash
     gh api graphql -f query='
       mutation {
         resolveReviewThread(input: {threadId: "THREAD_ID"}) {
           thread { isResolved }
         }
       }'
     ```
   - **If that resolve fails on GraphQL quota, defer it — do not re-post the reply.** Thread
     resolution is GraphQL-only; REST has no equivalent, so the fallback here is deferral, not
     substitution. The reply above has already landed, so record the owed resolve and move on:
     ```bash
     BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
     if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
     node "$BOSS_REPAIR_PROBE" defer --thread THREAD_ID --reply REPLY_URL --repo OWNER/REPO --pr PR_NUM --host HOST
     ```
     A deferred resolve is a **transient residual** with a retry horizon — never a permanent
     residual, and never a true stop. Report it as such and exit zero. A later pass drains it with
     `node "$BOSS_REPAIR_PROBE" drain --repo OWNER/REPO --pr PR_NUM --host HOST`, which retries the
     **resolve only**: the stored record holds no reply body and no reply endpoint, so a drain
     cannot double-post the comment that already landed.

   **c) Premise holds, remedy declined — affirm, record, resolve:**
   The finding is factually correct, but the change it suggests must not be applied as written. This is neither "fix it" nor "premise false", and it is not an escape hatch for a round that would rather not do the work: choosing it costs more reporting than fixing would. Three shapes recur:

   1. the correct fix is **outside the approved plan's scope**, and the plan already records it as a deliberate follow-up;
   2. the suggested change sits in the **wrong layer** and would not achieve what it claims;
   3. the remedy is **feature-sized** — several steps across several modules — and is not actionable within a repair pass.

   Grade each separable part of the remedy on its own feasibility: affirm the parts that hold, decline the parts that cannot be applied as written, and record a residual for each declined part separately. A remedy offering two branches is graded on which branch closes the defect class, never on which is the smaller diff — the cheaper branch is regularly the non-convergent one, and taking it converts a missing fix into a false guarantee the next round raises as a must-fix.

   The reply must do three things: **affirm the defect is real**, state precisely why the suggested change is not being applied, and **record a residual or follow-up instead of implementing it**. Post it through the same reply path as (b) — the same temporary-path block, the same submission block — then resolve the thread the same way. A reply that declines without affirming the defect is category (b) wrongly applied, and one that affirms without recording the residual loses the finding entirely.

   **The sink, and the order: record first, reply second.** The residual has a named destination, and
   the reply is inadmissible until that record exists and its identifier is in hand. A reply is
   posted once and cannot be un-posted, so one drafted before the record is written can promise a
   follow-up that never gets made — an unbacked claim on a public PR that nothing downstream
   re-checks. Resolve the destination first:

   ```bash
   BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
   if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
   node "$BOSS_REPAIR_TOOLBOX/bs-repair-derivations.mjs" residual-sink
   ```

   | `sink`       | `degraded` | what this round does                                                                                                                                                                                                                                                                   |
   | ------------ | ---------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
   | `boss-notes` | `false`    | Write the residual body to a file, substitute its path for the printed command's `<residual-body-path>`, and run that command. The note id it prints is the record id.                                                                                                                 |
   | `pr-comment` | `true`     | The same, with the printed fallback command; the comment URL is the record id. Say in the [Repair Summary](#repair-summary) that the residual sink was **degraded** — the fallback is a weaker, less searchable record, and reporting it is what stops it being a silent substitution. |

   Then gate the reply on that id, once per declined part:

   ```bash
   # Write {"residualRecordId":"<the id the write returned>"} with your file tool and substitute its
   # path for <decline-reply-path> — a placeholder, not a variable this block sets.
   node "$BOSS_REPAIR_TOOLBOX/bs-repair-derivations.mjs" decline-reply --in "<decline-reply-path>"
   ```

   | `reason`         | what this round does                                                                                                                                                                                                                     |
   | ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
   | `record-cited`   | The reply is admissible. Post it, citing that record id so a reader can check the follow-up exists.                                                                                                                                      |
   | `unbacked-claim` | **Do not post.** The record does not exist yet. Write the residual, take the id the write returned, and re-run the gate — never edit the reply to stop claiming a record, which loses the finding exactly as an unrecorded decline does. |

   A blank or whitespace-only id is refused, not accepted: the id arrives from a command
   substitution, and a write that failed most easily yields an empty line.

   **d) Unclear — ask for clarification:**
   - Add a reply comment asking for clarification:
     First create and print a temporary path, then write the reply to that exact printed path with
     the agent's file-editing tool:
     ```bash
     REPLY_BODY="$(mktemp)"
     printf '%s\n' "$REPLY_BODY"
     ```
     Submit that same path after the file-editing tool has written the reply:
     ```bash
     REPLY_BODY="/the/path/printed/above"
     if gh api repos/OWNER/REPO/pulls/PR_NUM/comments/COMMENT_ID/replies -F body=@"$REPLY_BODY" -q .html_url; then
       rm -f "$REPLY_BODY"
     else
       GH_STATUS=$?
       rm -f "$REPLY_BODY"
       exit "$GH_STATUS"
     fi
     ```
   - Do NOT resolve the thread — leave it open for the reviewer.
   - Mark it `needs-human` after posting the clarification, so later repair rounds park it until the
     reviewer's last-comment identity changes:
     ```bash
     BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
     if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
     node "$BOSS_REPAIR_PROBE" mark --thread THREAD_ID --disposition needs-human --repo OWNER/REPO --pr PR_NUM --host HOST
     ```
   - A thread left open this way is a **residual**: record it in the Repair Summary alongside the
     rest, per [Residuals vs true stops](#residuals-vs-true-stops). It does not fail the pass.

   **Required verifications before you reply.** Each costs a grep or two, and each is a required step, not a guideline. Run the ones that apply before the triage above is final, and state the result in the reply:

   - **Decompose the finding into its premises before any verdict, record a verdict and its evidence for each, and never act while one is unverified.** A premise is a distinct factual claim about the tree, a link of a causal chain, or a separable part of the remedy. The verdict vocabulary is `held`, `refuted`, `unverified`; evidence is the source substring read or the command run and its result, never a bare line number. An absent, unreadable, or out-of-vocabulary record is `unverified`, and a status, a severity, a `Done` upstream ticket, a bot's confidence, a `capped` review verdict, or a prior round's justification prose is never evidence that a premise holds — neither is the assertion of an in-tree contract nobody consumes, so grep for a cited registry's readers before accepting that it binds anything. The gate is the same for all three verbs: no premise may be `unverified` when the remedy is applied, declined, or published. The bullet below is the chain-shaped special case of this rule.
   - **Verify each link of a multi-step causal claim separately.** When a finding asserts a chain — this call does X, so Y follows, therefore Z is broken — check each link independently against the code instead of grading the comment as a whole. The reply must state which links held and which were restated or corrected. Answering wholesale goes wrong in both directions: a blanket accept commits the run to a false statement in the PR record, and a blanket reject discards the real defect the chain was built around.
   - **Settle a flagged documentation claim against the adjacent code comment and the nearest test.** When a finding says a documented claim is wrong, read the code comment beside the implementation — and the package doc comment — and the **name** of the nearest test before re-deriving the behaviour from the implementation or treating it as a code defect. When those two agree with each other and contradict the doc, the fix is prose-only and **no code change is in scope** — this is what stops a round "fixing" behaviour that was already correct.
   - **Run a sibling-class sweep before writing the fix.** Once you understand the finding's mechanism, search the repository for that mechanism before editing the cited site. Enumerate every site the search returns and record a verdict for each one: `fixed in this pass`, or `not a defect` with the reason; a one-row result is a complete discharge when the search finds only the cited site. The class is never fixed wholesale on the strength of the search alone: a predicate guarded by an error check can have many correct matches that render an error banner while the affected view remains interactive, and one defective match whose error branch replaces the view and swallows input; the discriminator is where the branch lives, not whether the pattern matched. The same sweep covers same-site siblings too: when one message drives both an observer and view state, relocating only the observer can leave the view half of the defect behind. **Each verdict answers the question the fix in hand raises, not the question that prompted the finding, and names the symbol it reasoned about rather than the file it lives in.** A sweep can examine exactly the right site and still clear it, because it re-answered the prompting thread's question: the site was right, the recorded reason was about something else, and the defect stayed standing. Naming only the file is the same failure one level up — clearing a file because a whitelist inside one function is correct says nothing about the different function in that same file the finding actually implicates, so a verdict that cannot name its symbol has not been reached. **Report each sibling's failure kind, not only the count.** A sweep can change a finding's kind rather than its count, because siblings can fail more quietly than the reported site: a reviewer cites one call site whose failure is visible, the sweep finds many, and the most serious of them surface no error at all — they continue past the failing call and leave the affected surface permanently blank. Record the kind beside every verdict and say so explicitly when a sibling's kind differs from the reported site's, because a silently-degrading sibling is the one nobody goes back for. Put the verdict table in the PR body or Repair Summary so the enumeration is reviewable.
   - **Find the sibling constant before designing a tunable-constant fix.** Before implementing a timeout, deadline, retry count, limit, or any other tunable constant in response to an open-ended suggestion, grep the containing package for sibling constants and follow the naming and test-seam shape already established there. An open-ended suggestion invites an invented mechanism; a sibling turns the fix into a mechanical, reviewable change with a ready-made test shape.
   - **Grep upstream before treating a skill-prose finding as single-site.** Before accepting that a finding quoting one line of skill prose is fixable at that line, grep the quoted remedy across the whole skills tree **and** the contract docs those skills are copied from. Treat the cited line as the symptom and fix the upstream contract passage in the same commit — it costs one grep, and a quoted-line-only fix leaves the contract re-seeding the identical prose into the next skill copied from it.
   - **Sweep the rationale, not only the restatements.** When the fix edits a prose contract rule, re-read the passages around it before you reply and correct any that cite the **old** rule as their **reason**, not only the ones that restate it. A restatement is greppable and a rationale is not, so the half that rots is the half no grep hands you — a fix scoped to the flagged sentence ships a contradiction one paragraph away.

   **IMPORTANT**: Every unresolved thread must be handled. Do not silently skip threads. Either fix and resolve, decline as premise-false and resolve with an explanation, affirm-and-record a declined remedy and resolve, or ask for clarification. Fixed, declined, and affirmed-but-declined threads must all be resolved before the PR is considered clean. Only true clarification requests may remain unresolved.

3. After implementing changes:
   - Because this checklist runs before commit, use only a zero-write scratch copy. Do not mutate the
     checkout. An in-place probe belongs after a commit, with its commit-first backup and exact-restore
     safeguards.
   - Materialize every scratch fixture through a read-only, `noatime` view of the checkout source. A
     plain host read, copy, or directory traversal can update checkout access metadata before the
     sandboxed command begins. If that atime-safe source view is unavailable, reject the probe.
   - A shell, interpreted-source, or test-gate invocation is zero-write only inside a filesystem sandbox
     whose only writable path is `"$PROBE_DIR"`, with an explicit read-only allowlist for the scratch
     fixture, required tool binaries and libraries, and any dependency cache. Start with a cleared
     environment (put `HOME`, `TMPDIR`, `PWD`, and XDG paths under `"$PROBE_DIR"`), do not expose host
     credentials or home paths, and deny writes outside the sandbox root. Network access is disabled,
     including loopback, link-local, and metadata endpoints. If filesystem or network confinement is
     unavailable, reject the probe; never execute the copied command on the host.
   - If a fix adds or changes a guard, gate, or assertion, prove it non-vacuous with this ordered
     checklist before committing the review-feedback result:
     1. **Name the property** the gate claims to forbid, including the unbounded direction for a
        one-sided bound.
     2. **Mutate the production feed, never the assertion**, using the zero-write scratch copy.
     3. **Prove the mutation landed** before reading the result; a no-op mutation proves nothing.
     4. **Require red for the right reason** and require the failure to name the property. A compile
        or harness error is not evidence that the gate detected the mutation.
     5. **Restore exactly, then prove the restore** and re-run the gate green.
   - **A fix that tightens a guard states what the guard now rejects, and covers that.** The
     checklist above proves a guard still catches what it should; a tightening carries the opposite
     risk, and it is the side nobody tests. When a fix narrows a guard, gate, or predicate, write
     down the inputs the narrowed form newly **rejects**, confirm each one deserves rejection, and
     add coverage for the boundary that moved — not only for the case that still passes. A round
     that tests a tightening solely on what it still admits ships the over-rejection undetected: an
     arm added to reject a non-positive value also rejected the zero-padded positive values the
     surrounding helper accepts, the round that wrote it saw green, and the next review round paid
     for the regression. Loosening and tightening are not the same review, so do not reuse one
     argument for both.
   - **When the diff touches markdown, read the rendered hunk before you commit** — including a
     hunk a dispatched worker handed back, because delegating the edit does not delegate this.
     Prettier's default `proseWrap: preserve` does not reflow prose, so a hand-split or inserted
     sentence leaves an orphan line mid-paragraph that `--check` reports as correctly formatted.
     Same class: run the formatter immediately after editing a markdown table cell and confirm the
     churn is padding-only, because an edited cell can re-pad every row around it. The formatter
     cannot be the only markdown check.
   - Run the repo's formatting and test gates:
     ```bash
     # Examples only; use the commands discovered for this repo
     pnpm lint && pnpm test
     go test ./...
     cargo test
     ```
   - **An exit code is not a finding.** Before treating a red gate as a defect in this branch, read the failing output. A lock-contention warning from a tool that permits only one instance at a time, a signing or memory failure raised by a commit hook, and a failure in a file this change does not touch are infrastructure flakes, not findings; a failure already present on the PR's base is an inherited failure, not a finding — each produces a non-zero exit that says nothing about the branch. Re-run the affected target in isolation to confirm, compare the failure against the base when inheritance is plausible, and consult the repo's own agent instructions for the flake signatures it records. Only once the output names something this branch changed and the same cause is not already present on the base is there a failure to repair.
   - Commit with reference to review feedback:

     ```bash
     git add <changed-files>
     git commit -m "$(cat <<'EOF'
     fix(review): address feedback on <component>

     - [Change 1 from review]
     - [Change 2 from review]

     Co-Authored-By: <the Co-Authored-By trailer your harness specifies>
     EOF
     )"
     ```

   - Push changes:
     ```bash
     git push
     ```

### Phase 3: Verify and Monitor

**3.1 Verify Fix**

After applying the repair:

1. Check that local state is clean:

   ```bash
   git status     # Should show clean working tree
   ```

   One case legitimately leaves the tree dirty: the pre-existing work
   [Phase 1](#phase-1-assess-current-state) step 1.1a found and deliberately did not touch. That step
   commits only the files belonging to the thread under repair and leaves partial, incoherent or
   unrelated changes exactly where they are, because a peer may be mid-edit in this worktree. A tree
   still dirty for that reason is **expected** here and is reported as the residual 1.1a already
   requires — do **not** commit it to make this check green, and do **not** reset it. Anything else
   dirty is this round's own unfinished work and must be resolved before reporting.

2. Verify the committed fix is pushed to origin. Re-derive this from a fresh fetch at the moment you
   need the answer — an `[ahead N]` count observed earlier in the pass is never sufficient, because
   it is computed against the remote-tracking ref, which is only as fresh as the last fetch, and a
   peer session sharing this branch may have already pushed the very commits this round produced:

   ```bash
   # Branch should not be left ahead of origin — derive that fresh, never from an earlier read.
   BRANCH=$(git branch --show-current) || exit 1
   [ -n "$BRANCH" ] || exit 1
   git fetch origin "$BRANCH" || exit 1
   LOCAL=$(git rev-parse HEAD) || exit 1
   REMOTE=$(git rev-parse "origin/$BRANCH") || exit 1
   # This block's only irreplaceable job is the two ancestry probes and the answers they produce.
   # The verdict, and its wording, are the helper's — see the routing table below. There is no
   # second, paraphrased rendering of the same decision here to drift out of step with it, and a
   # round that landed no commits still emits the field because the helper always answers.
   REMOTE_IS_ANCESTOR_OF_LOCAL=false
   LOCAL_IS_ANCESTOR_OF_REMOTE=false
   if [ "$LOCAL" = "$REMOTE" ]; then
     : # equal SHAs settle it without either probe; the helper reads `published` off the SHAs alone
   elif git merge-base --is-ancestor "$LOCAL" "$REMOTE"; then
     LOCAL_IS_ANCESTOR_OF_REMOTE=true
   elif git merge-base --is-ancestor "$REMOTE" "$LOCAL"; then
     REMOTE_IS_ANCESTOR_OF_LOCAL=true
   fi
   # WITHHELD is a value you PASTE, exactly as SENT_SHAS is — NOT a variable an earlier tool call
   # set: no shell state survives between the tool calls a pass is made of, so a defaulted read of
   # such a variable takes its default on every round, and `false` is the fail-OPEN direction here —
   # it routes a deliberately withheld commit to `push-owed`, whose row below says "Push." Left
   # unfilled, the placeholder makes the input invalid JSON and the helper exits non-zero, which is
   # the refusal this shape is for. Paste `true` only for the stale-SHA cancellation carved out below.
   WITHHELD="<true if the stale-SHA cancellation withheld this round's commit, else false>"
   BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
   if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
   PUSH_STATE_IN=$(mktemp)
   printf '{"localSha":"%s","remoteSha":"%s","remoteIsAncestorOfLocal":%s,"localIsAncestorOfRemote":%s,"withheld":%s}\n' \
     "$LOCAL" "$REMOTE" "$REMOTE_IS_ANCESTOR_OF_LOCAL" "$LOCAL_IS_ANCESTOR_OF_REMOTE" "$WITHHELD" >"$PUSH_STATE_IN"
   node "$BOSS_REPAIR_TOOLBOX/bs-repair-derivations.mjs" push-state --in "$PUSH_STATE_IN"
   rm -f "$PUSH_STATE_IN"
   ```

   Compare the two SHAs as **strings**, with `|| exit 1` on each substitution, per the fail-closed
   style the [Linear-History Invariant](#linear-history-invariant) already mandates: an empty
   substitution comparing equal under `-eq` is precisely the fail-open form that section forbids.

   **Both probes run, because "not equal" is three different situations.** Only one of them owes a
   push. Another is **divergence, not a routine push**: local and remote share nothing newer than an
   older base, because a concurrent writer rewrote the branch: do not push and do not force-push. A
   plain `git push` is rejected there and a force-push clobbers that writer's work, so this is the
   same concurrent-writer case the
   `PUSHED_HEAD` assertion in [Phase 2](#phase-2-execute-repair-strategy) forbids resolving by force
   — report it as a residual and re-derive the round from the new head. Collapsing the last two arms
   back into one — skipping the second probe and letting the remaining branch mean "ahead" — reads a
   clobber as ordinary unpushed work, which is the failure this whole section exists to prevent.

   One case legitimately leaves the branch ahead of origin: the stale-SHA cancellation above, where
   the CI half was cancelled and its commit was built but deliberately not pushed. A branch ahead of
   origin is **expected** there and is reported as that cancellation, not pushed — this check must
   read it the same way the clean-tree check does.

   **The helper owns the reported answer, and every round reports it.** Route on the printed `state`,
   and paste the printed `line` into the [Repair Summary](#repair-summary)'s **Push state** field
   verbatim — including on a round that landed no commits, which is exactly the round where "nothing
   to push" and "forgot to push" otherwise render identically.

   | `state`        | what this round does                                                                            |
   | -------------- | ----------------------------------------------------------------------------------------------- |
   | `published`    | Nothing owed — local and origin agree.                                                          |
   | `remote-ahead` | Do not push; re-derive the round from the new head.                                             |
   | `push-owed`    | Push.                                                                                           |
   | `diverged`     | Do not push and do not force-push; report a residual and re-derive the round from the new head. |
   | `withheld`     | Report it; do not push it.                                                                      |

   A `diverged` verdict whose `reason` is `unreadable-input` is the fail-closed arm, not an observed
   rewrite: a SHA or an ancestry answer did not resolve, so the round takes the same do-not-push
   action and says so rather than claiming a concurrent writer nobody saw. The helper never answers
   `published` from a read it could not make, because `published` is the one value that lets a round
   which forgot to push say nothing at all.

3. Poll the remote PR state, then report the final PR state (default mode performs one post-push poll; in Watch Mode you loop per the [Watch Mode](#watch-mode) section). Decide the check state through the shared classifier — this body states no green-or-red rule of its own:

   ```bash
   BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
   if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
   CHECK_DIR="$(mktemp -d)"
   HEAD_SHA="$(gh pr view --json headRefOid -q .headRefOid)"
   gh pr checks --json name,state,bucket > "$CHECK_DIR/checks.json"
   gh api "repos/OWNER/REPO/commits/$HEAD_SHA/check-runs?per_page=100" --paginate --slurp > "$CHECK_DIR/runs.json"
   node "$BOSS_REPAIR_TOOLBOX/pr-check-state.mjs" classify \
     --head-sha "$HEAD_SHA" --observed-sha "$HEAD_SHA" \
     --checks "$CHECK_DIR/checks.json" --check-runs "$CHECK_DIR/runs.json" \
     --prior "$CHECK_DIR/prior-contexts.json"
   BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
   if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
   node "$BOSS_REPAIR_PROBE"
   gh pr view --json mergeable -q .mergeable
   ```

   **Read the classifier's `state` and `reason`, never the buckets directly.** The bucket payload
   cannot separate a gate that ran and passed from one that never attached, which is why the
   SHA-keyed check-runs read is passed alongside it. `green` is the only pass, and only
   `provesGreen: true` is evidence the head actually cleared its gates. `failing` opens a repair
   cycle. `pending` never does — including reason `absent-gate`, which says a named gate the prior
   head carried is missing from this one, so waiting on it never resolves and the missing gate is
   reported rather than waited on. `unknown` (`unreadable`, `unclassified`, `stale-sha`,
   `no-gate-ran`) is neither green nor red: report it as unobserved.

   Write the pre-push head's context names to `$CHECK_DIR/prior-contexts.json` — a bare JSON array
   of names is enough — before pushing. Without that file the verdict reports `priorKnown: false`
   and `provesGreen: false`, which is the honest reading: a path-filtered push shrinks the check
   set, so the head set alone may simply be too small to prove anything.

   `gh pr checks` and `gh pr view --json mergeable` are both GraphQL-backed. A blocked read here is
   **not** "no failing checks" and **not** "not `CONFLICTING`" — take the
   [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads) for checks and
   mergeability, apply that section's null-is-unobserved rule, and emit the `DEGRADED_READ` line.

4. If `repair_status=needs_repair`, `repair_status=unknown`, or `repair_status=not_evaluated`, handle
   or report the review feedback before exiting; do not treat unknown or not-evaluated review status
   as clean. If checks are still pending, failed, or timed out after known review feedback is handled,
   note that in output. In default mode, still **exit cleanly (zero)** after the push even when checks
   are pending or failed — report the status but do not exit nonzero. The repair plugin only enters
   its in-session resume/retry loop after a clean exit; a nonzero exit makes it abandon that loop and
   fall back to a slower fresh sweep. (Watch Mode is the exception: it owns the loop and re-runs the
   matching repair strategy on failures itself, per the [Watch Mode](#watch-mode) section.)

   Dispatching Strategy A/B/C investigation into an awaited subagent (see the Phase 2 lead-in and the [Watch Mode](#watch-mode) section) is internal orchestration bookkeeping for a single repair pass only. It MUST NOT change this default-mode contract — one pass, push, a single poll, and a clean zero exit even when checks are pending, so the repair plugin keeps owning retries — nor the default-vs-watch distinction described above.

   ```
   ✓ Repair applied and pushed
   Checks finished: [passing | failing | pending | timed out]
   ```

**3.2 Report Results**

Provide a concise summary:

```
## Repair Summary

**Problem Identified**: [the `line` printed by `bs-repair-derivations.mjs problem-sources` — `None` only when every source is absent]

**Actions Taken**:
- [Action 1]
- [Action 2]
- [Action 3]

**Commits Created**:
- <commit-hash>: <commit-message>

**Status**:
- Changes pushed to origin
- [Checks are now passing | Checks are pending | Awaiting review]

**Push state**: [the `line` printed by `bs-repair-derivations.mjs push-state` — on every round, including one that landed no commits]

**Gate results**:
- [Each gate run with authoritative completion evidence plus its quoted final summary line | `unverified` for any gate missing either the completion evidence or quoted line | none]

**Root causes**:
- [Cause count and `cause -> checks it explains` table for failing-check triage | none]

**Repeating strategy**:
- [The strategy and primary file this pass repeated from the previous pass, with the consecutive-pass count | none]

**Residuals**:
- [What this pass could not resolve and why the round stopped short, each carrying its identity key, its ladder rung, and how many earlier rounds reported that key | none]
```

---

## Terminal outcomes

Every pass ends in exactly one of the outcomes below. Decide which one you are in before writing the
Repair Summary. These are **outcome names**, not new reason tokens:
the Watch Mode reason-line vocabulary is unchanged,
and each outcome records which existing token it maps to.

- **repaired** — a fix was committed and pushed. Exit zero. In Watch Mode the reason token is
  whatever the next poll reaches.
- **nothing to repair** — the pass arrived with every signal already clear: checks passing with none
  pending, `repair_status=clean`, and `mergeable` not `CONFLICTING`. Assess, confirm green,
  report **nothing to repair** with `**Residuals**: none`, and exit zero.
  This is a **legitimate terminal outcome, distinct from a failed pass** — a pass that finds nothing
  wrong has done its job in full, and there is no work owed.
  Manufacturing work in this state is scope creep and is **forbidden**:
  do not re-run gates this pass had no reason to invoke,
  do not re-read already-resolved threads,
  and do not make an unrelated improvement while you are here.
  Watch token: `green`.
- **parked no-op** — checks pass, `mergeable` is not `CONFLICTING`, and `repair_status=parked` with
  zero actionable threads, so no strategy fires and no mandated dispatch was skipped.
  The parked thread or threads are the residual. Exit zero. Watch token: `parked`.
- **residual** — the pass ran and something remains: pending CI, the bounded-pass limit, review
  feedback that arrived after the final push, `repair_status=not_evaluated` whose
  `probe_failure_class` is `rate_limited` or `other` (read the printed class — do not judge the
  error text by eye), a review thread whose reply landed but whose resolution was deferred on quota,
  a superseded CI view, a re-rolled flake, a concurrent
  writer that replaced the branch or this run's commit, or an escalated edge case.
  Report it and exit zero, per
  [Residuals vs true stops](#residuals-vs-true-stops).
  Watch tokens: `no-progress` / `max-attempts` / `blocked`.
- **true stop** — the worker could not run at all. Exit non-zero; the definition stays in
  [Residuals vs true stops](#residuals-vs-true-stops).

---

## Residuals vs true stops

Not every unfinished repair is a failure. Decide which of these two outcomes you are in **before**
choosing an exit code.

- **Residual** — a condition this pass legitimately could not resolve: conflicts too tangled to
  settle safely, a fix that needs a decision only a human can make, a red signal whose cause lives
  outside the branch. A residual is **not an error**. Report it explicitly — as a residual line in
  the Repair Summary, and in a PR comment when a human has to act — and then **exit zero**. A
  residual is always a _reported_ outcome, never a silent one; leaving it out of the report is the
  real failure.
- **True stop** — the worker could not run at all: required tooling is missing, the repository or PR
  is unreadable, `repair_status=not_evaluated` because auth or a real repository or PR 404 prevented
  observation, or an unexpected exception aborted the pass. There is no repair outcome to report, so
  **exit non-zero** and let the breakage surface loudly.

**Split the two by the printed failure class, not by eye.** When the probe reports
`repair_status=not_evaluated`, `probe_failure_class` decides: `auth`, `not_found`, and `environment`
are true stops; `rate_limited` and `other` are residuals, and a `rate_limited` one carries a
`probe_retry_after` horizon. A quota failure read as permanent is the misclassification this rule
exists to prevent — see
[REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads) for the full table.
A **deferred thread resolution** — a reply that landed while its GraphQL resolve was rate-limited —
is likewise a transient residual with a retry horizon, never a permanent residual and never a true
stop.

Both outcomes describe how the **pass itself** ends. The `exit 1` guards inside the command snippets
above are narrower: they abort that step — refusing to push a branch that would poison the PR, or a
rebase that would drop manual edits — rather than settling the pass's exit status. When a guard trips,
follow whatever the surrounding step says to do next — the merge-count preflights route to the
linearize recovery above, the amendment guard escalates to
[Complex Conflicts](#complex-conflicts) when its recovery will not re-apply, and a guard that leaves
nothing to work with (no resolvable base branch) is a true stop — then end the pass by the same rule:
a residual when there is an outcome to report, a true stop only when the pass could not run.

Retry, cooldown, and attempt caps belong to the **daemon-owned repair loop** (the repair plugin
described in [Integration with Repair Plugin](#integration-with-repair-plugin)), not to this worker.
The loop decides whether and when another pass runs, and a pass that exits zero with its residuals
reported gives it exactly what it needs to decide. Never simulate backoff by failing on purpose: a
non-zero exit that only means "something remains" is recorded as a **failed attempt**, so it does not
just disguise a normal outcome as a crash — it abandons the loop's in-session retry for a slower
fresh sweep, lengthens the backoff before the next pass, and leaves a consecutive-failure count
standing against a branch that was never broken. Failing does not "apply the cooldown" the branch
needs; it charges the branch for one it did not earn. Watch Mode is the exception the rest of this
document already names: a
manual `/boss-repair watch` run owns its own bounded loop and is not driven by the daemon loop's
retry schedule, per [Watch Mode](#watch-mode). The residual-versus-true-stop rule still decides how
that loop exits.

### Escalating a residual that re-fires

A residual nobody acts on comes back byte-identically on the next round. Re-reporting it unchanged
consumes a repair attempt and changes nothing, while the condition keeps blocking. So before you
write a residual into the Repair Summary, ask the ladder what rung it is on. **Do not re-decide an
escalation in prose:** the rung is the module's answer, and a round that reasons its way to a
different one has replaced a tested decision with an untested one.

The identity is the `[file, line, title]` tuple the review-side oscillation guard already keys on, so
"the same thing again" means the same thing in review and in repair — the helper mirrors that guard's
encoding down to the string `"null"` it gives a line-less finding. It additionally trims the file and
title, which the guard does **not**: that is a repair-side normalisation for a hand-typed tuple, not
part of the shared encoding, so for a file or title carrying stray whitespace the two keys still
differ — and where the guard would key a whitespace-only file, the helper refuses it. Type the tuple
exactly as the finding names it; the two agree only where the finding's own file and title carry no
leading or trailing whitespace. `<prior>` is how many **earlier**
rounds reported that identity — zero on a first sighting. `<actionable>` is `true` only when a
**different** strategy from the one already applied to this identity is available and you can name
it; when the only move left is the strategy that already ran, it is `false`.

**`<prior>` comes from the record, never from memory.** No pass inherits the previous pass's
material, so a round that does not read the record has exactly one defensible answer — `0` — and a
ladder every round enters at rung 0 is the pre-ladder behaviour with extra steps. Read the residual
entries the record carries and count the entries whose identity **key string** equals this one. That
count is `<prior>`. When no prior record is readable at all, `<prior>` is `0` and rung 0 is the
honest answer — say so in the residual line rather than inventing a count.

**The record is the watch-mode note record, and there is no other one.** Its entries are the
pass-number-keyed ones [Watch Mode](#watch-mode) step 1 writes, and they carry across passes because
that whole loop is a single session. **Outside watch mode nothing carries.** Default mode is one pass
per invocation, each a fresh session inheriting none of the previous pass's material, and this body
instructs no pass to persist its Repair Summary anywhere a later round could read it — printing a
summary into a transcript is not writing a record. So in default mode `<prior>` is `0` and the round
enters at rung 0, every time. That is a stated limit on where the ladder accumulates, not a gap to
paper over: do **not** answer from a remembered count, and do **not** invent a prior sighting from a
PR comment — the comments this body does instruct are prose notices for a human, carrying no
identity key and no count to read. A residual that keeps re-firing across default-mode
invocations is reported at rung 0 each time — say so plainly in the residual line, because an honest
`0` is what makes the missing carrier visible to whoever can build one.

**Reuse the recorded key string; do not re-derive a tuple from the moved file.** The count survives
only while the key does, and the line number does not survive an ordinary repair round: a fix that
inserts lines above the residual re-derives a different `line`, the lookup misses, and a fourth
sighting reads as a first one. Once you recognise a residual the record already names, copy that
entry's key verbatim instead of re-typing a tuple from the current file.

**Write the identity to a file and pass the path.** A finding title is arbitrary English prose: an
apostrophe in it ends a single-quoted shell argument early, and a double quote makes the JSON
unparseable — so a title spliced into a literal breaks the command roughly whenever the title
contains ordinary punctuation. Write the tuple with your file tool and pass `--in`, exactly as the
check-state helper takes its input by path:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
# Write ["<file>", <line-or-null>, "<title>"] with your file tool first, then substitute that
# file's path for <residual-json-path> below. It is a placeholder you fill in, exactly like
# <prior> and <actionable> — NOT a variable this block sets. A shell variable would be the one
# shape that fails silently: no shell state survives between the tool calls a pass is made of,
# so it would expand to the empty string and every classify would exit non-zero on an
# unreadable path, printing no rung at all.
node "$BOSS_REPAIR_TOOLBOX/bs-repair-escalation.mjs" classify \
  --in "<residual-json-path>" <prior> <actionable>
```

Route on the printed `rung` and `action`, never on the prose you would have written instead:

| `rung` | `action`            | what this round does                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| ------ | ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `0`    | `report`            | Report the residual the ordinary way. Nothing has re-fired.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `1`    | `report-repeating`  | Report it **and mark it repeating**, so the next round reads a repeat rather than re-deriving one.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `2`    | `escalate-strategy` | Do not re-run the strategy that already failed twice; change strategy for this residual, and say which one you moved to.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| `3`    | `blocked`           | Nothing this pass can do. Report it for a human, using the established `blocked` token. This is a **residual-level** label written into the Repair Summary. Its spelling is deliberately the shipped terminal token, imported rather than retyped so a caller that classifies a repair outcome already understands it — what is residual-level is the label's **scope**, not a second spelling. So do not write it into the Watch Mode reason line or the run sentinel on account of this rung: the pass still exits zero and still writes its own terminal token unless it was a true stop for its own reasons. |

Rung 3 is reached two ways, and the printed `reason` says which: this pass declaring it has no action
left, and the ladder's own **ceiling** overriding a pass that keeps claiming it has one. `<actionable>`
is your claim and the module cannot check it, so the ceiling is what stops a residual re-firing
forever on an optimistic answer. Both spellings of rung 3 are the same residual-level label.

A malformed identity makes the helper exit non-zero rather than answer. That is deliberate: a silent
default would classify every repeat as a first sighting, which is the exact failure this ladder
exists to end. **Fix the escaping or the file, never the wording:** a reworded title is a _different_
identity, so a round that edits the title until the command parses has silently reset the count it
was about to read. A non-zero exit naming a missing module or an unreadable path is a different thing
again — the verdict was never observed, so report it as unobserved rather than routing on a rung
nobody printed.

---

## Edge Cases and Error Handling

### Complex Conflicts

If conflicts are too complex to resolve automatically:

1. Add a PR comment explaining the situation:

   ```bash
   gh pr comment --body "Automatic repair detected complex merge conflicts that require manual review. Files affected: <list>. Please resolve manually."
   ```

2. Report the unresolved conflict as a **residual** in the Repair Summary — name the files still
   conflicting and why this round stopped short — and **exit zero**. This is not a true stop, so do
   not fail the pass to force a cooldown; whichever loop is driving this pass — the daemon-owned one,
   or watch mode's own — owns backoff and any further attempt.

### Cascading Failures

If fixing one issue causes another (e.g., fixing a conflict causes tests to fail):

1. Continue with the next repair strategy
2. Don't give up after the first fix
3. Iterate through strategies until stable
4. If the repair began by rebasing the PR branch, keep using
   `git push --force-with-lease` after any follow-up fixes. The branch history
   was already rewritten, so a plain `git push` will fail or leave the repair
   unpushed.

### Missing Context

If the repair requires information not available (e.g., design decisions, external dependencies):

1. Add a PR comment requesting clarification
2. Do NOT make assumptions
3. Report the open question as a **residual** in the Repair Summary — state what information is
   missing and who has to supply it — and **exit zero**. Waiting on a human is not a true stop, and
   failing the pass would neither get the question answered sooner nor tell the loop driving it
   anything it does not already track.

---

## Guidelines

1. **Root Cause Over Symptoms**: Fix the underlying issue, not just the visible error
2. **Minimal Changes**: Only change what's necessary to resolve the issue
3. **Test Locally**: Always run the repo's formatting and test gates before pushing
4. **Clear Commits**: Write descriptive commit messages that explain the fix
5. **Atomic Repairs**: Each repair attempt should be self-contained
6. **Stop Early, Not Loudly**: If this pass cannot fix the issue, report the residual and exit zero
   rather than grinding — see [Residuals vs true stops](#residuals-vs-true-stops)
7. **No raw bulk output in the main thread**: Never paste full diffs, CI logs, `gh run view` output, or
   review threads into the orchestrator's context — that bulk is re-charged on every later turn. The
   Phase 2 strategy subagent reads them in its own context and returns only a summary; when working
   inline, filter to the few relevant lines (`gh pr checks --json name,state,bucket`,
   `gh run view <run-id> --log-failed | tail`, `${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js`'s compact
   summary) instead of dumping.
8. **Treat an Omission-Justifying Comment as a Hypothesis**: A comment explaining why something
   was deliberately left undone may be over-conservative — the stated premise can be true while the
   conclusion is not forced. Check whether the trade-off is actually forced before trusting it; if
   it is not, report that rather than widening the fix.

---

## Anti-Patterns

| Anti-Pattern                                                      | Problem                             | Fix                                                               |
| ----------------------------------------------------------------- | ----------------------------------- | ----------------------------------------------------------------- |
| Accepting all "ours" or "theirs" blindly                          | Loses important changes             | Review each conflict individually                                 |
| Skipping tests after conflict resolution                          | Introduces bugs                     | Always run full test suite                                        |
| Commenting out failing tests                                      | Hides problems                      | Fix the root cause                                                |
| Merging the base branch in to resolve drift                       | Merge commit deadlocks rebase-merge | Rebase onto the base; keep `git rev-list --merges --count` at `0` |
| Plain force pushing                                               | Loses history, breaks collaboration | Use `--force-with-lease` after rebase                             |
| Making unrelated "improvements"                                   | Scope creep                         | Fix only the reported issue                                       |
| Retrying immediately after failure                                | Triggers cooldown loops             | Fix the root cause first                                          |
| Pasting raw diffs / CI logs / review threads into the main thread | Re-charged on every later turn      | Summarize in the strategy subagent; filter with `--json` / `grep` |

---

## Example Scenarios

### Scenario 1: Simple Rebase Conflict

```
Problem: PR shows conflict status, git reports conflicts in server.go

Resolution:
1. Fetch and rebase onto the PR base branch (never merge it in)
2. Read server.go, see conflict in import statements
3. Keep both imports (they're independent)
4. git add server.go && GIT_EDITOR=true git rebase --continue
5. Run repo formatting and test gates (passes)
6. Preflight: git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD → 0
7. git push --force-with-lease

Result: ✓ Conflict resolved, checks passing
```

### Scenario 2: Failing Test

```
Problem: PR checks show test failure in user_handler_test.go

Resolution:
1. gh pr checks → see "TestUserCreate" failing
2. gh run view <run-id> → read error: "expected status 201, got 400"
3. Read user_handler_test.go and user_handler.go
4. Found bug: missing validation check for email field
5. Add validation in user_handler.go
6. Repo test command passes
7. git add . && git commit -m "fix(user): add email validation in create handler"
8. git push

Result: ✓ Tests now passing
```

### Scenario 3: Review Feedback

```
Problem: PR has requested changes - "Extract this logic into a helper function"

Resolution:
1. gh pr view --comments → read reviewer's comment
2. Read the file mentioned in comment
3. Extract logic into new helper function
4. Update calling code to use helper
5. Repo formatting and test gates pass
6. git add . && git commit -m "refactor(handlers): extract validation logic to helper"
7. git push
8. gh pr comment --body "Extracted as requested. PTAL!"

Result: ✓ Review feedback addressed
```

---

## Integration with Repair Plugin

This skill is invoked by the repair plugin via:

```go
StartChatRun(ctx, &StartChatRunHostRequest{
    SessionId: sessionID,
    Command:   "boss-repair",
    Title:     "Repair: " + sessionName,
})
```

The plugin:

- Detects red status changes (Failing/Conflict/Rejected)
- Applies exponential backoff between attempts, persisted so a daemon restart
  cannot reset the schedule
- Prevents concurrent repairs for the same session (the guard is held across the
  whole wait-and-loop, not just a single run)
- After each run, waits for the PR checks to settle and loops — up to a bounded
  number of attempts — until the PR is clean, the failing signal stops changing,
  or the wait times out
- Calls `FireSessionEvent(FixComplete)` on success

This skill should:

- Focus on fixing the immediate problem
- Complete within a reasonable time (< 5 minutes typical)
- Exit zero once a pass completes, with any residual reported in the Repair Summary, and non-zero
  only on a true stop — see [Residuals vs true stops](#residuals-vs-true-stops)
- Provide actionable feedback via PR comments if unable to fix

---

## Watch Mode

**Manual `/boss-repair watch` only.** This mode is active **only** when the skill is invoked with the `watch` argument. The repair plugin always invokes `/boss-repair` with no arguments, so it never enters watch mode; default mode still waits for checks after its pushed fix, but the plugin owns additional repair attempts.

In default mode (no `watch` argument) you MUST behave exactly as Phases 1–3 describe: apply one repair pass, push, poll the resulting PR state once, report the result, and exit so the repair plugin can decide whether to retry. **Do not** start an additional repair loop in default mode. Default mode may exit while checks are pending, but it must not exit while known unresolved review feedback still needs repair.

In watch mode, after completing one normal repair pass (Phases 1–2) and pushing, do **not** exit. Instead poll the PR state directly, bounded to **5 repair passes total** (matching the plugin's own limit):

Each of these repair passes dispatches its own fresh awaited subagent (per the Phase 2 lead-in) to run the matching strategy's investigate-and-fix steps; the thin orchestrator running this loop only tracks the pass counter, the `$BEFORE` baseline commit, and poll state between passes — it does not carry a prior pass's diffs/logs/threads forward. This is internal bookkeeping only: it does not alter the default-mode contract described in Phase 3 §4, and the 5-pass bound plus the final reason line below stay byte-identical.

1. Record the current commit before each repair pass, and refresh this baseline after every pushed repair before returning to the poll loop:

   ```bash
   BEFORE=$(git rev-parse HEAD) || exit 1
   ```

   **Key every note entry to its pass number.** The record in your notes is the single
   carrier: **one entry per pass, written whether or not that pass pushed** — `pass <n>: <sha>` when
   it pushed, `pass <n>: (no push)` when it did not — never overwritten and never erased. It
   must keep **every SHA this round sent**, because Phase 2's
   survival assertion runs over all of them — pass 2 polling while a peer force-pushes away what pass
   1 landed is exactly the clobber that assertion exists to catch, and a round that kept only the
   current pass's value would report "sent nothing" and never look.

   **The per-pass entry carries more than the SHA.** Two later readers need state only this pass can
   supply, and neither is reconstructible afterwards, so write both into the same pass-number-keyed
   entry alongside the SHA: the **strategy and primary file** this pass ran, and the **identity key
   of every residual** this pass reported. Nothing else carries them — the orchestrator tracks the
   pass counter, the `$BEFORE` baseline and poll state and nothing more, and a dispatched pass
   inherits none of the previous pass's material — so an entry that omits them leaves the next pass
   deriving a first sighting for a residual on its fourth round, and a repeating-strategy comparison
   with nothing to compare against. An absent previous entry is a **first** pass, not a match.

   **Which is why the entry cannot be conditional on pushing.** A residual is by definition what a
   pass could **not** resolve, so the passes most likely to report one are the passes least likely to
   push — and a record kept only for passes that pushed drops exactly those sightings while the
   reading rule above turns the gap into a silent "first pass". One non-pushing pass in a 5-pass run
   caps the count at 3, below the ladder's ceiling, in precisely the run the ceiling was added for.
   So write the entry every pass. "This pass pushed nothing" is a **missing SHA field**, never a
   missing entry.

   **Do not carry a sent-SHA in a shell variable across the pass.** A shell variable does not survive
   between the tool calls a pass is made of, so a later step reading a bare `$PUSHED_HEAD` finds it
   empty however the pass went, and both readers below would be answering from an empty value rather
   than from the record. Both derive from the notes instead: step 9 reads **this pass's own entry**
   (no SHA recorded ⇒ this pass pushed nothing), and Phase 2's assertion reads **all** entries. Keying by pass
   number is what keeps those two readings apart without erasing anything — clearing a shell variable
   answered step 9 only by destroying the record Phase 2 needs.

   **Pass freshness — re-read the PR head at the start of every pass.** Record it as this pass's
   `ROUND_HEAD` and compare it against `PREV_PASS_HEAD`, the value the previous pass recorded. Then
   **record this pass's `ROUND_HEAD` as `PREV_PASS_HEAD` for the next pass** straight away — the
   baseline the next pass needs is the head _this_ pass started from — in your own notes rather than
   only in a shell variable, since no shell state survives between passes. Nothing else supplies that
   baseline: a pass that omits the hand-off leaves the next one with no way to see a rewrite. The
   **first** pass has no previous value, and an unset `PREV_PASS_HEAD` is **not** a rewrite:

   ```bash
   BRANCH=$(git branch --show-current) || exit 1
   [ -n "$BRANCH" ] || exit 1
   git fetch origin "$BRANCH" || exit 1
   ROUND_HEAD=$(gh pr view --json headRefOid -q .headRefOid) || exit 1
   if [ -z "$PREV_PASS_HEAD" ]; then
     echo "first pass — no previous baseline, nothing is invalidated"
   elif git merge-base --is-ancestor "$PREV_PASS_HEAD" "$ROUND_HEAD"; then
     echo "fast-forward since the previous pass"
   else
     echo "NON-FAST-FORWARD: the branch was rewritten since the previous pass; every earlier premise is void"
   fi
   PREV_PASS_HEAD=$ROUND_HEAD   # hand this pass's head to the next pass; record it in your notes too
   ```

   `gh pr view --json headRefOid` is GraphQL-backed. A pass blocked here has no freshness baseline
   at all, so take the [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads)
   row for the head SHA and emit the `DEGRADED_READ` line rather than abandoning the watch.

   The empty-`PREV_PASS_HEAD` arm is not decoration. Without it `merge-base` is handed an empty
   operand, errors, and falls into the `else`, so pass 1 would announce a rewrite that did not happen
   and order the agent to discard the triage it had just completed.

   A **non-fast-forward** move means another writer rewrote or rebased the branch, and every
   conclusion carried from an earlier pass is then **invalid outright** — not stale-but-reportable.
   Discard the prior triage, discard any carried `parked` verdict, and re-derive every premise
   (checks, threads, mergeability) against the new head before acting. Do **not** re-report an
   earlier pass's finding against a tree that no longer exists.

   Even a **fast-forward** move made by a writer other than this pass invalidates carried check
   verdicts; only re-derived reads may be acted on. This is the same head-scoping the
   `repair_status=clean` paragraph in step 8 already establishes for review probes, applied to the
   rest of the pass's premises.

2. Poll all repair signals before every wait, reading check state through the same classifier Phase 3 step 3 uses:

   ```bash
   BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
   if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
   node "$BOSS_REPAIR_TOOLBOX/pr-check-state.mjs" classify \
     --head-sha "$HEAD_SHA" --observed-sha "$HEAD_SHA" \
     --checks "$CHECK_DIR/checks.json" --check-runs "$CHECK_DIR/runs.json" \
     --prior "$CHECK_DIR/prior-contexts.json"
   BOSS_REPAIR_PROBE="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js"
   if [ ! -f "$BOSS_REPAIR_PROBE" ]; then BOSS_REPAIR_PROBE="$HOME/.codex/skills/boss-repair/scripts/review-feedback-probe.js"; fi
   node "$BOSS_REPAIR_PROBE"
   gh pr view --json mergeable -q .mergeable
   ```

   When quota blocks `gh pr checks` or `gh pr view --json mergeable`, poll through the
   [REST substitutions](#graphql-exhaustion-rest-substitutions-and-degraded-reads) rather than
   treating the blocked read as a passing signal, and emit the `DEGRADED_READ` line for each one.

3. Interpret the full PR state:

   - **Checks:** the `$BOSS_REPAIR_TOOLBOX/pr-check-state.mjs classify` verdict from step 2 — trust its `state`/`reason` and restate no rule here. `green` is the only pass (and only `provesGreen: true` proves the head cleared its gates); `failing` is the only red; `pending` is a wait, except reason `absent-gate`, which names a gate that vanished from this head and will never report; `unknown` is unobserved, never either.
   - **Review threads:** `${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/scripts/review-feedback-probe.js` — trust `repair_status` (`clean`, `parked`, `needs_repair`, `unknown`, `not_evaluated`) using the same interpretation rules as Strategy C above.
   - **Conflicts:** `gh pr view --json mergeable -q .mergeable` — `CONFLICTING` means a merge conflict appeared.

4. **Review work first:** if `repair_status=needs_repair`, repair every printed unresolved thread immediately. Partition those threads by author first, exactly as Strategy C's partition describes: after a build run that recorded `REVIEW_VERDICT=clean`, bot-authored threads are advisory — answer them once, resolve what that answer settles, and open no repair cycle — while human changes-requested threads repair as described here. For each valid comment, fix it, reply with what changed, resolve the parent review thread, push, then return to step 1 — re-baseline and re-run the pass-freshness check — before the next poll. For each invalid, stale, or already-handled comment, reply explaining why it is declined, resolve the parent review thread, push if the reply changed local state, then return to step 1 — re-baseline and re-run the pass-freshness check — before the next poll. Only true clarification requests may remain unresolved. If `repair_status=parked`, no action is due until a reviewer replies. If `repair_status=unknown` or `repair_status=not_evaluated`, do not treat reviews as clean; report the unreadable state or route true repository/PR unreadability as a true stop.

5. **Conflicts next:** if mergeable is `CONFLICTING`, repair the conflict, commit, push, then return to step 1 — re-baseline and re-run the pass-freshness check — before the next poll.

6. **Pending checks:** if checks are pending, **wait on a callback, not on a clock**. Never spend a
   fixed `sleep` of 60 seconds or longer waiting for CI to move: a guessed duration is not a reading,
   it wakes this loop on a schedule that has nothing to do with the checks, and it reports the same
   whether CI resolved, stalled, or never reported at all. Arm the one-shot watches for this PR when
   the callback gate is true, and fall back to a bounded poll only when it is not:

   ```bash
   BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
   if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
   BOSS_REPAIR_CALLBACK="$BOSS_REPAIR_TOOLBOX/callback/adapter.mjs"
   export BOSS_REPAIR_CALLBACK
   node --input-type=module -e '
     import{pathToFileURL as u}from"node:url"
     const {resolveCallbackAdapter,callbacksAvailable,callbacksUnavailableReason}=await import(u(process.env.BOSS_REPAIR_CALLBACK).href)
     const available=callbacksAvailable(process.env)
     if(!available){
       process.stdout.write("callbacks-unavailable: "+callbacksUnavailableReason(process.env)+"\n")
     }else{
       const a=resolveCallbackAdapter(process.env)
       process.stdout.write(JSON.stringify({triggers:a.policy.watchTriggers,expiresIn:a.policy.defaultExpiresIn})+"\n")
     }
   '
   ```

   When the gate is **true**, `registerWatch` one watch per `policy.watchTriggers` entry — read the
   trigger list from the adapter's policy, never retype it here — each non-exclusive trigger under
   its own group, resolving the CLI through the adapter rather than a bare binary name. On every wake
   **reconcile against real state before acting**: re-read checks, review threads, and mergeability
   exactly as steps 2 and 3 describe, dedup by callback id, and re-arm any trigger whose reconcile
   still reads false. When the gate is **false**, log `callbacksUnavailableReason(process.env)` — an unavailable gate is a clean
   degrade, never a failed wait — and drive the wait from the bounded poll alone.

   Either way the poll is what bounds the wait: a fixed number of reads at a fixed interval, with a
   cap on the reads rather than on wall time, routing an exhausted cap and an unresolvable rollup
   **identically** and never as green. The short interval between two reads of that bounded loop is
   pacing inside a wait that is already running, not a stand-in for the wait, and stays well under a
   minute. Do not wait on checks without probing reviews and mergeability first. Remove the live watches
   once this loop reaches its terminal state in step 8.

7. **Failed checks:** if checks failed, run the matching repair strategy from Phase 2 for the new failure, push, then return to step 1 — re-baseline and re-run the pass-freshness check — before the next poll.

8. **Done — green or parked:** the loop has reached a non-repair terminal state only when checks pass AND (`repair_status=clean` **or** `repair_status=parked`) AND mergeable is not `CONFLICTING` AND all fixed or declined review threads are resolved. `repair_status=not_evaluated` is terminal only as a non-green unreadable-review state: record it as a residual unless the reason shows the repository or PR itself is unreadable, in which case it is a true stop. Once all four hold, stop and exit zero.

   `repair_status=clean` is **head-scoped**: it is clean only for the head it was read against, so a clean probe at the end of one round predicts nothing about the next. A reviewer who re-reviews every push opens fresh threads against this round's own fix, which means **new threads since the previous round's head are an expected steady state** — not a regression, and not a fresh failure.

   That is **not licence to batch, defer, or wait for a quiet reviewer**: those fresh threads are routinely genuine defects in the fix just pushed, and deferring them ships the defect. What is bounded is the number of repair **rounds**, never the reviewer's cadence. So the **review re-poll runs after the CI-green check in every round** and is never skipped once checks pass — reaching green is the moment fresh feedback is most likely, not a reason to stop looking.

   Once those four conditions hold, the round that reaches them is one that **stops pushing** — and there are **two distinct terminators** wearing that same shape: a round that arrives green and does nothing, reported as `green`, and a round that replied and **parked without pushing**, reported as `parked`. Neither shortcuts the four conditions above; they name which terminal a satisfied round is reporting. The parked terminator did substantive work — it read, triaged and answered threads — so it is **not** the no-progress stop of step 9, which describes a round that could not move an unchanged failing signal.

9. **No-progress stop:** after a repair pass, if `git rev-parse HEAD` equals the `$BEFORE` value captured immediately before that pass (no new commit was pushed) and the failing signal is unchanged, stop and report — do not spin on an unfixable failure. This mirrors the plugin's duplicate-input guard.

   A raw `HEAD` comparison is not by itself the progress test, because HEAD moves for reasons this
   pass did not author: a peer push moves it without this pass pushing, which reads as progress and
   defeats the stop, and a peer force-push back to `$BEFORE` reads as no progress even though the
   pass did push. So a pass made progress only when **this pass's own note entry records a SHA** and
   that SHA is an ancestor of the current `origin/$BRANCH`. Read that entry by **this pass's
   number** from the record step 1 describes; an entry with no SHA field — step 1 writes one every
   pass, pushing or not — means this pass pushed nothing, which is the no-progress arm, and so does
   an entry that is missing altogether. Do **not** read a shell `PUSHED_HEAD` here: no shell state survives from
   the push to this point, so it is empty in every pass — including the ones that did push, where the
   stop would then fire on a pass that had just landed a commit. Re-derive `$BRANCH` here as step 1's
   fence does. A difference this pass did not author is neither progress nor no-progress —
   it is a re-derivation trigger, per the pass-freshness rule in step 1.

   **A repeating strategy is not a no-progress stop, and that stop cannot see it.** The progress test
   just described is satisfied by **this pass's own note entry existing with an ancestor SHA** — so a
   pass that pushed a real commit registers as progress however many times the same strategy has
   already been applied to the same file. Two consecutive passes running the identical strategy
   against the identical file therefore read as ordinary churn: each one pushed, each one did clear
   the signal it was aimed at, no guard fires, and the recurring structural cost is absorbed
   silently. Report it rather than stopping on it: when this pass's strategy **and** its primary file
   both match the previous pass's, add a `**Repeating strategy**` line to the Repair Summary naming
   the strategy, the file, and how many consecutive passes have now run that pair. This is a report
   and never a terminator — the pass continues, the bound in step 10 is unchanged, and the exit
   status is unaffected. Its value is that a structurally re-conflicting branch becomes visible to
   the next reader instead of costing a fresh diagnosis every round.

   **The comparison reads the record, not memory.** The pair it needs is the strategy and primary
   file step 1 writes into each pass's own entry; nothing supplies it implicitly, because a
   dispatched pass inherits none of the previous pass's material and no shell state survives between
   passes. Read the previous pass's entry for the baseline, and take the consecutive count as the
   length of the trailing run of entries carrying that same pair. An absent previous entry is a
   **first** pass, not a match.

10. **Bound:** never exceed 5 repair passes. After the 5th, report the remaining failures and exit.

11. **Ending short of green is a residual, not a true stop:** the no-progress stop, the 5-pass bound, and any escalation out of [Edge Cases and Error Handling](#edge-cases-and-error-handling) all end a loop that ran fine and left something behind. Record what is still red in the Repair Summary's `**Residuals**` line and **exit zero**; watch mode exits non-zero only on a true stop, per [Residuals vs true stops](#residuals-vs-true-stops).

    "What is still red" is not the whole residual. On the bound, it must name **pending CI checks** and **review feedback that arrived after the final push** as well — a pending check is not red, and a fresh unaddressed thread is not red either, so both fall outside that wording and are exactly what a bounded loop leaves behind.

    A round that ends because a failing check was **re-rolled and came back green** must name the concrete `file:line` hardening candidates that will stall again, or explicitly record that none was identified. A re-roll clears the signal without removing the fragility, and without that line the next occurrence pays the full triage cost from zero.

When watch mode exits (green, parked, no-progress, 5 attempts reached, or escalated to a human per an edge case), print the standard Repair Summary plus a final line stating why the loop ended (`green` / `parked` / `no-progress` / `max-attempts` / `blocked`). Use `blocked` for an edge-case escalation: it is the established token for "this needs a human", and callers that classify this line already understand it. Alongside that token the final line carries the **number of repair passes used** and the **pending-check state**, so an exhausted budget is visible without re-deriving it from the transcript.

---

## Checklist

Before completing the repair:

- [ ] Problem identified and categorized
- [ ] Appropriate repair strategy executed
- [ ] Local formatting and test gates passed
- [ ] Changes committed with descriptive message
- [ ] Base sync was a rebase; `git rev-list --merges --count "origin/$BASE_BRANCH"..HEAD` is `0`
- [ ] Changes pushed to origin; post-push waiting is delegated to the repair plugin
- [ ] Summary provided with actions taken
- [ ] Residuals reported in the summary, or explicitly recorded as `none`

A pass that ends on a residual without changing the branch skips the commit/push/gate boxes above —
there is nothing to commit — and is complete once the residual is reported.

A zero-diff pass records the gate boxes as **not run**.
Permission to skip a box is not permission to fill it in:
quoting a pass or fail status for a gate this pass never invoked is a **reporting defect**,
because a reviewer and the next round will both read it as evidence.

---

## Success Criteria

"Successful" here means the **run** did its job, not that the PR came out green. A pass that ends on
a reported residual is still a successful run — see [Residuals vs true stops](#residuals-vs-true-stops).
Criteria 2–4 apply only when the pass produced a change; a pass that ends on a residual without
touching the branch has nothing to gate, commit, or push and is judged on 1, 5, and 6 alone.
A zero-diff pass records the gate boxes as **not run**.
Permission to skip a box is not permission to fill it in:
quoting a pass or fail status for a gate this pass never invoked is a **reporting defect**,
because a reviewer and the next round will both read it as evidence.
One repair run is successful when:

1. ✅ The immediate issue was identified and fixed, or the reason it could not be is recorded as a residual
2. ✅ Local formatting and test gates passed
3. ✅ Changes were committed with a descriptive message
4. ✅ Changes were pushed to the PR branch
5. ✅ Review threads touched by this repair were resolved, declined with explanation, or replied to with a clarification question
6. ✅ The run exits cleanly so the repair plugin can wait for GitHub checks/reviews/conflicts to settle

The entire repair workflow is successful only when the repair plugin observes:

1. ✅ Checks have finished and are passing
2. ✅ No merge conflict remains
3. ✅ No unresolved actionable review feedback remains

Do not treat pending checks as final success. In **default mode**, pending checks mean the agent run should exit cleanly after pushing, and the repair plugin will wait; if checks fail, new review feedback appears, or a conflict appears, the plugin will start a fresh `boss-repair` run. In **Watch Mode** (`/boss-repair watch`), the skill itself waits and loops as described in [Watch Mode](#watch-mode) instead of exiting on pending checks.

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

After the terminal outcome is decided, resolve the extension helper and run:

```bash
BOSS_REPAIR_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-repair/toolbox"
if [ ! -d "$BOSS_REPAIR_TOOLBOX" ]; then BOSS_REPAIR_TOOLBOX="$HOME/.codex/skills/boss-repair/toolbox"; fi
NOTES_JSON=$(node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" discover --core boss-repair --role notes --json)
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
  NOTES_SAMPLE_RATE=$(export BOSS_REPAIR_TOOLBOX; node --input-type=module -e 'import { pathToFileURL } from "node:url"; const { loadSkillConfig, notesSampleRate } = await import(pathToFileURL(process.env.BOSS_REPAIR_TOOLBOX + "/skill-config.mjs").href); process.stdout.write(String(notesSampleRate(loadSkillConfig())))')
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
NOTES_RUN_TMP=$(mktemp -d "${TMPDIR:-/tmp}/boss-repair-notes.XXXXXX")
NOTES_OBSERVATIONS="$NOTES_RUN_TMP/observations.md"
```

Before dispatch, the orchestrator that still owns the completed run writes at most five
secret-scrubbed candidate observations to `NOTES_OBSERVATIONS`, with a maximum 8 KiB file size.
Keep each candidate to a short problem statement plus a file/skill/command pointer. Never copy a
transcript, command output, user-provided content, credentials, tokens, or other secrets; an empty
file is valid. This artifact is the only run-history source sent across the fresh-subagent boundary.

Dispatch descriptors in ascending `(order, name)` order as fresh, **awaited** subagents, each bounded
by `BOSS_SKILL_EXTENSION_TIMEOUT_MS` (default `300000` ms). Load each extension by **reading the
descriptor's `skillPath` from disk** (`dir` is its directory), passing both `skillPath` and `dir` in
the worker brief, and requiring relative extension resources to resolve from `dir`. Pass that `SKILL.md`
content into the dispatch as the extension's instructions — never by its bare descriptor `name` via the
Skill tool, which refuses a skill declaring `disable-model-invocation: true`.
Each receives:

```json
{
  "role": "notes",
  "core": "boss-repair",
  "context": {
    "mode": "<interactive if this run involved operator interaction; otherwise headless>",
    "core": "boss-repair",
    "outcome": "<resolved terminal outcome>",
    "repoId": "<BOSS_REPO_ID when present; otherwise null>",
    "observationPath": "<NOTES_OBSERVATIONS>"
  },
  "runTmp": "<NOTES_RUN_TMP>",
  "outPath": "<NOTES_RUN_TMP>/notes-<extension-name>.json"
}
```

Validate each result with `node "$BOSS_REPAIR_TOOLBOX/skill-extensions.mjs" validate --role notes --file
"<outPath>"`. On success append one terminal-ledger line with the total persisted-note count. On a
discovery skip, timeout, missing output, malformed envelope, validation failure, or subagent failure,
append `extension <name>: skipped (<reason>)` and continue. Remove `NOTES_RUN_TMP` on every
post-opt-in terminal path. The phase cannot change the outcome, exit code, tracker or PR writes, and
is non-fatal in every case.

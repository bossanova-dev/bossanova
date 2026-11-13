---
name: boss-build
description: Use when asked to implement a planned Linear ticket on a schedule, "implement the next ticket", "boss-build", or given a ticket ID to implement. Unattended cron-safe sibling of boss-plan — it consumes agent-build planned tickets and ships review-ready PRs.
---

# boss-build

Implement exactly **one** planned ticket end to end, unattended, and hand off a review-ready PR. This
is the second half of the pair whose first half is `boss-plan` (which turns tickets into
`agent-build` planned tickets). You are a capable engineer: this document states the contract,
the helpers that answer questions reliably, and the few rules that protect the branch and the
truthfulness of the result. How you implement is up to you — **the plan is the specification**.

## Terminal states

Print exactly one, as the first token of its own line, followed by the ticket id, PR URL and a
summary (cost extraction matches a line-leading token).

- `REVIEW_READY` — PR pushed, green and ready; ticket moved to the **in-review** state; PR URL
  commented on the ticket. Requires `ready-gate.mjs` → `ready` on the pushed head:
  a clean review and no commit after the reviewed head that changed its tree. Human review is the
  next gate. An opted-in, verified Step 12 completion merge keeps the token and prints
  `REVIEW_READY <ticket> <pr-url> merged <sha> — <summary>`; the ticket moves to `.done`.
- `PARTIAL` — branch green and pushed, at least one in-scope acceptance criterion satisfied on a
  branch whose review produced a verdict, and everything left undone is an unmet in-scope criterion. Ticket stays
  **in-progress**; PR ready but marked do-not-merge. Requires `ready-gate.mjs` → `ready` on the
  PARTIAL route (Step 9).
- `BLOCKED` — **(1)** quality gates are still red after the repair cap, **(2)** the branch cannot be
  pushed, or **(3)** the review is not clean — `ready-gate.mjs` is not ready: blocking findings left
  after the review's one fix pass, an unusable or never-run review, or commits after the reviewed
  head that no review saw. Ticket stays **in-progress** with a blocker comment (`file:line`, what was
  tried); PR left draft (`gh pr ready --undo` if this run readied it). Cause 3 lists every blocking
  finding at `file:line` in that comment and in the PR's `## Review findings`, and still upserts the
  `<!-- bs-review -->` comment ([`references/publish.md`](references/publish.md)). An unmet
  criterion alone is not BLOCKED.
- `NO_CHANGE` — no eligible candidate, a named ticket with nothing to build from, claim lost with
  no runner-up, a foreign branch carrying other work, a peer already holding the worktree lock, a
  named ticket verify would not admit, started without approval, an unplanned file-level overlap with an open
  PR (held as needs-human), or nothing committable after claiming (ticket
  restored to its entry state).

An existing PR/branch (bossd's bootstrap draft, an empty PR, or a prior run's work) is **adopted and
resumed**, not a stop condition.

## Workspace facts

- **Tracker**: `resolveTrackerAdapter(env)` (`toolbox/tracker/adapter.mjs`, `.boss-skills.json`
  `adapters.tracker`, with `TRACKER` as an override). Steps name **capabilities** — `selectPlanned`, `getIssue`, `moveState`,
  `readComments`, `writeComment`, `readLabels`, `readPlanAttachment`, `extractImages` (optional) —
  whose transports live in the adapter's operation map. Workspace, backlog team and states
  come from `trackerConfigFor(config)` (`toolbox/skill-config.mjs`); the backlog is a team, never a
  project filter. The three states are roles — `.planned`, `.inProgress`, `.inReview` — resolved to
  this workspace's names at runtime; never hard-code a state name. A failed capability call is
  classified with `node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" classify-outcome --observed "<err>" --operation read|write`
  and its action line followed, never the error body's own retry advice.
- Priority numeric: `1=Urgent, 2=High, 3=Medium, 4=Low, 0=None`.
- **Plan**: the uploaded plan attachment when the ticket has one (`Implementation plan (<ISSUE-ID>)`
  preferred; a linked PR or URL never counts), otherwise the ticket title and description, whatever
  their shape or length. `build-source` decides which, and refuses only a ticket with neither.
- **Waiting on CI/PR state**: arm one-shot callbacks via `resolveCallbackAdapter(env)`
  (`toolbox/callback/adapter.mjs`) when `callbacksAvailable(env)` is true, and back every wait with
  the bounded poll either way — [`references/callback-watches.md`](references/callback-watches.md)
  Protocol steps 1 and 5. Never a fixed `sleep` of a minute or more, never a bare
  `gh pr checks --watch`, never `boss cron` (a cron fire starts a new blind session).

Each capability is an operation in the resolved adapter's map. `tracker/cli.mjs operations` lists
its transport. An `mcp` operation is the named tool, called through `resolvedServer`; a `command`
operation is run with `tracker/cli.mjs invoke-operation <op> --args-file <file>`, whose `result` is
the answer. A descriptor verb (`create-issue`, `update-comment`, `write-description`) that prints
`outcome: operation-executed` has already performed the write: do not make an MCP call for it.

A `moveState` / `writeComment` response echoes the issue before the write; it is not
proof that the write landed or failed. Read claim visibility with `read-claim-comments` and
confirm state moves with `await-state`.

**State-move confirmation.** After the Step 6 entry-state restore, Step 9 move to `.inReview`,
Step 10 unwind to `.inProgress`, and Step 12 `NO_CHANGE` restore, run
`node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" await-state --id <issue-uuid> --state <resolved-target-name>`.
An unsettled receipt requires re-sending the same absolute `moveState` once and awaiting again.
If still unsettled, record
`node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger evidence-unknown --where "boss-build state-move read lag"`
(never fatal), name the lag in the run report and continue with the earned terminal state.
On exit 2, fall back to one `getIssue` read; a stale state is not proof of failure: re-send once
and continue. Restore warnings apply to an unsettled await, never the write echo.
Step 3's `.inProgress` move needs no await because claim arbitration does not read state.

## Helpers

Every helper ships in `toolbox/`. After Preflight, shell blocks and dispatched subagents
resolve the same private pin from the worktree git dir:

```bash
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
```

Once Preflight has pinned, read `references/…` and `agents/…` from `$BOSS_BUILD_PIN/`.

| Question                                                               | Ask                                                                                                                                                     |
| ---------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Remove a recorded scratch dir                                          | `node scratch-remove.mjs --record <file>`                                                                                                               |
| Rename an identifier across files                                      | `node rename-symbol.mjs --from <A> --to <B> [--path <p>] [--exclude <p>] [--dry-run]`                                                                   |
| Is the worktree clean (hook-proof)?                                    | `node worktree-state.mjs [--base <ref>]`                                                                                                                |
| Did a worker’s verification finish?                                    | `node verification-receipt.mjs run …` / `check …` / `clear …`                                                                                           |
| Who owns this worktree?                                                | `worktree-lock.sh acquire\|heartbeat\|release <run-id> [ticket]`                                                                                        |
| Is my claim visible yet?                                               | `node tracker/cli.mjs read-claim-comments …`                                                                                                            |
| Did the state move land?                                               | `node tracker/cli.mjs await-state …`                                                                                                                    |
| Who won the ticket claim?                                              | `node tracker/cli.mjs claim-verdict …`                                                                                                                  |
| Which PR belongs to this branch?                                       | `node build/pr-ownership.mjs number --pr-json <json>`                                                                                                   |
| What does this ticket build from?                                      | `node plan-attachment.mjs build-source <issue-json> [<plan-out>]`                                                                                       |
| Does an open PR collide with this plan?                                | `node build/open-pr-hold.mjs check --plan <plan> --issue-json <issue> [--pr <n>] [--apply]`                                                             |
| Would verify admit this named ticket?                                  | `node build/named-admission.mjs check\|apply …`                                                                                                         |
| Which repo-local extensions exist?                                     | `node skill-extensions.mjs discover --core boss-build --role <role> --json`                                                                             |
| Do the plan’s citations and premises still hold?                       | `node plan-recheck.mjs <plan> [--root <dir>]`                                                                                                           |
| Is a failed criterion only "checkout behind origin"? Re-run it rebased | `node build/acceptance-retry.mjs classify\|retry\|certify …`                                                                                            |
| Did the base move under me?                                            | `node base-drift.mjs check …`                                                                                                                           |
| Push the branch, with retry and a rescue ref                           | `node finalize/push-branch.mjs --branch <name> [--base <ref>] [--expect-remote <sha>]` <!-- ci-request: skip helper reference; routes below request --> |
| Request configured heavy CI once per verified draft head               | `node ci/ci-request.mjs request --pr <n> --head <sha>`                                                                                                  |
| Tag commits with the PR number                                         | `node finalize/cli.mjs inject-pr-tag <pr>`                                                                                                              |
| Are the checks green / is the merge state blocking?                    | `node pr-check-state.mjs classify\|merge-state …`                                                                                                       |
| Wait for CI without guessing                                           | `node ci-wait.mjs run --pr <n>` (daemon-first; callback-watches.md step 5)                                                                              |
| May I stop watching CI?                                                | `node callback/ci-watch.mjs classify …`                                                                                                                 |
| May this head be readied / receipted?                                  | `node build/ready-gate.mjs [--base <branch>]` (reads the Step 9 route file)                                                                             |
| Opt-in inline verify and merge a green REVIEW_READY PR                 | `node build/completion-gate.mjs envelope\|fast-path`                                                                                                    |
| Where does a follow-up go?                                             | `node build/followup-route.mjs route --candidates <file> --notes-out <file>`                                                                            |
| Update a published PR body without duplicating it                      | `node build/pr-body.mjs merge --body-file <live> --update-file <sections>`                                                                              |
| Is the local-verification record well-formed?                          | `validateLocalVerification(config, body)` in `skill-config.mjs`                                                                                         |
| Is the verify-only evidence well-formed?                               | `validateVerifyOnlyEvidence(config, body)` in `skill-config.mjs`                                                                                        |
| What is left before this terminal state is honest?                     | `node finalize/route-contract.mjs assert --outcome <state> …`                                                                                           |
| Post or read the boss/build commit-status receipt                      | `node commit-status.mjs post\|read …`                                                                                                                   |

## Rules

1. **One ticket per run.** No batching. Headless runs never ask questions: decide, record the
   decision and its rationale under `## Autonomous decisions` in the PR body, continue. An
   interactive run asks the operator a question it cannot decide; neither writes it into the PR.
2. **Follow the plan.** Implement what it says — migrations, schema changes, configuration,
   dependency changes, whatever it calls for. Judging whether the work is wise is the planner's and
   the reviewer's job, not this run's. Where the ticket was edited after the plan, the ticket wins.
   Where a premise no longer holds, build what the plan is for against the code as it is and record
   the departure. Where a criterion cannot be checked from a worktree (production access, a deployed
   environment), implement the change and pass what a human must verify to Step 7.3 as an
   `outside-worktree` follow-up candidate. An observation only possible after merge (the plan's
   `## After merge`, a scheduled run, a rollout) is a `post-merge` candidate. Never print
   or commit secret values.
3. **Commit per task, never leave work behind.** Tagless conventional commits with a scope
   (`feat(scope): …`), path-scoped (`git commit --only -m "…" -- <files>`), never `git add -A`. Leave
   room in the subject for the `[#<PR>]` tag finalize prepends. A run's work is not done until it is
   pushed: every route that ends after implementation pushes first.
4. **Rebase, never merge.** Never merge the base into the branch, never `git pull`, never
   `--rebase-merges`; force-push only with `--force-with-lease` over this run's own rewrite.
5. **Never merge by default.** Hand off to the verify chat through `/boss-verify`. Step 12's
   inline fast path (`completion-gate.mjs fast-path` → `verify-gate.mjs merge`) runs only with
   `buildDefaults.verifyFastPath: true`, an enabled `/boss-verify` cron job and a `pass` verdict.
6. **Await every subagent.** Dispatch with awaited `Task` (Claude) or `spawn_agent` + `wait_agent`
   (Codex); never background a dispatch and move on. Keep bulk output (diffs, CI logs, review
   transcripts) inside subagents that return short summaries. On Codex, after reading each result,
   release the completed subagent using `close_agent` when exposed or the documented host release
   primitive; completed agents may retain thread slots. An interrupt is not a release. If no
   release primitive is available, record that limitation and continue with bounded dispatch.
7. **A step is done when its artifact exists** (`$PLAN_FILE` holds the plan; `gh pr view <n>`
   returns), not when it was attempted.
8. **Re-enter, don't stop early.** A prompt that arrives before this run printed a terminal state
   resumes the workflow: `route-contract.mjs assert --outcome <intended>` lists what is still owed —
   answer in one line and do it. Once a terminal state is printed, the run is over.
9. **Leave nothing behind.** Remove a scratch dir whose path you recorded with
   `node "$BOSS_BUILD_TOOLBOX/scratch-remove.mjs" --record <file>` (or `rm -rf` a literal path), never
   `rm -r` on a `$(…)` target — Claude Code stops for a human on it even in bypass mode. Cleanup is
   its own Bash call, after the bookkeeping it must not take down. Every terminal state that took the
   worktree lock goes through **Stop cleanly** (Step 12). Discard scratch files and temp dirs you created.
10. **Bookkeeping warns, capability blocks.** A drifted install, an incomplete route receipt, or a
    failed ledger write prints `warning: <what> — bookkeeping only, work state unaffected` and the
    run continues. A missing pin, toolbox or helper script is a hard stop; tracker tools not yet in your
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

For pin recovery during a run, set `BOSS_BUILD_CONTINUE=1` and carry that run's own
`BLI_RUNID` into this block. Do not adopt a token from another entrant. This preserves review
bookkeeping and reenters the existing lock before replacing the missing pin.
The run record keeps its original `START_SHA` with the token; Step 1 reloads both in fresh shells.

A callback delivered after the terminal line starts a new reconciliation pass: unset `BLI_RUNID`,
set `BOSS_BUILD_CONTINUE=1`, and execute Preflight and Step 1 before reading pinned references or
running their helpers. This takes a new lock and pin while preserving the previous review and ready
route. Skip ticket selection, claiming and implementation; reconcile through
[callback watches](references/callback-watches.md), then use Steps 9–12 for the resulting state.
The new pass owns its fresh route receipt and removes its pin when it stops.

```bash
git rev-parse --show-toplevel
git branch --show-current
command -v git; command -v gh; gh auth status
```

```bash
START_SHA="$(git rev-parse HEAD)"
# A continuation needs the reviewed-head and route history it is reconciling.
if [ "${BOSS_BUILD_CONTINUE:-}" != 1 ]; then
  rm -f "$(git rev-parse --git-dir)/boss-build-review-verdict" "$(git rev-parse --git-dir)/boss-build-ready-route"
  rm -rf "$(git rev-parse --git-dir)/boss-build/review" "$(git rev-parse --git-dir)/boss-build/review-verdict.json"
fi
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
O=
if BOSS_BIN="$(command -v boss 2>/dev/null)"; then
  if O="$("$BOSS_BIN" skills check --gate --heal 2>&1)" ||
    { case "$O" in
        *"unknown flag: --heal"*) O="$("$BOSS_BIN" skills check --gate 2>&1)" ;;
        *) false ;;
      esac; }; then
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
# Own the worktree before replacing its pin. Keep a stable release helper if the shared tree moves.
if [ -z "${BLI_RUNID:-}" ]; then
  BLI_RUNID="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-token)" || exit 1
fi
PREFLIGHT_LOCK="$(mktemp -t boss-build-preflight-lock.XXXXXX)" || exit 1
cp "$BOSS_BUILD_TOOLBOX/worktree-lock.sh" "$PREFLIGHT_LOCK" || { rm -f "$PREFLIGHT_LOCK"; exit 1; }
LOCK_STATUS=0
bash "$PREFLIGHT_LOCK" acquire "$BLI_RUNID" pending || LOCK_STATUS=$?
if [ "$LOCK_STATUS" != 0 ]; then rm -f "$PREFLIGHT_LOCK"; exit "$LOCK_STATUS"; fi
trap 'rm -f "$(git rev-parse --absolute-git-dir)/boss-build-run-id"; bash "$PREFLIGHT_LOCK" release "$BLI_RUNID"; rm -f "$PREFLIGHT_LOCK"' EXIT
# Preserve the original review baseline when recovering this run's pin after implementation.
RUN_RECORD="$(git rev-parse --absolute-git-dir)/boss-build-run-id"
if [ "${BOSS_BUILD_CONTINUE:-}" = 1 ] && [ -f "$RUN_RECORD" ] &&
  [ "$(sed -n '1p' "$RUN_RECORD")" = "$BLI_RUNID" ]; then
  START_SHA="$(sed -n '2p' "$RUN_RECORD")"
  [ -n "$START_SHA" ] || { echo "BLOCKED: run record has no start SHA"; exit 1; }
fi
# A peer cannot replace either field without owning the lock.
printf '%s\n%s\n' "$BLI_RUNID" "$START_SHA" >"$RUN_RECORD" || exit 1
# Pin the healed skill before any later helper or reference is read.
node "$BOSS_BUILD_TOOLBOX/skill-pin.mjs" create --skill boss-build || exit 1
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
export BOSS_BUILD_PIN BOSS_BUILD_TOOLBOX
# BOSSD_MANAGED=1 iff a bossd daemon provisioned this worktree (references/standalone-mode.md):
if node "$BOSS_BUILD_TOOLBOX/bossd-present.mjs"; then BOSSD_MANAGED=1; else BOSSD_MANAGED=0; fi
if [ "$BOSSD_MANAGED" = "1" ]; then
  test -n "$SESSION_BRANCH" || exit 1
fi
trap - EXIT
rm -f "$PREFLIGHT_LOCK"
printf '%s\n' "${O:-}" | node "$BOSS_BUILD_TOOLBOX/skill-drift-verdict.mjs" reload --core boss-build --skill-md "${BOSS_BUILD_TOOLBOX%/toolbox}/SKILL.md" || exit "$?"
```

On `RELOAD:`, read the named file in full and restart this block under that body with
`BOSS_BUILD_CONTINUE=1` and this run's `BLI_RUNID`, once per run. The lock and run record stay owned.
A second reload means a peer install is racing; continue under the body just read and report it.

A stale record warns and the run continues. A blocking drift verdict, including config-validator
drift or an `absent`, `mode` or `broken-symlink` capability, or no toolbox at all, stops (rule 10).

When `tracker/cli.mjs operations` lists a `preflight` operation, run it with
`tracker/cli.mjs invoke-operation preflight` instead of the MCP preflight and team listing. A
non-zero exit is the stop reason, verbatim.

**Tracker.** Make one cheap read through the adapter (e.g. the backlog team's statuses; when config
names no team, the `listTeams` call with `limit=50` — an adapter without it gives
`visibleTeams: null`), then classify it with `trackerMcpPreflight` (`toolbox/tracker/preflight.mjs`),
passing your **own tool list** — never a harness config file. MCP servers can still be connecting when a session starts: if
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

**Team.** `--team <name>` is a skill argument, never a ticket id. Classify with
`resolveTrackerTeam(config, {preflight, visibleTeams: <raw list_teams result>, teamFlag})`
(`skill-config.mjs`): unconfigured stops `NO_CHANGE: <message>` with no writes; otherwise use `team`
for `selectPlanned` and pass `--team <team>` to `list-planned`.

Also require, before any tracker write, that `trackerConfigFor(config).states` resolves all three
roles to non-empty names; otherwise stop `NO_CHANGE` naming what is missing.

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

## Step 1: Confirm the worktree lock

Preflight acquired the lock before replacing the pin. Re-enter with its saved token; never
mint a new token here. A failed Preflight releases its lock through its EXIT trap. A peer
rejected by that acquisition stops `NO_CHANGE` without touching the pin or tracker.

```bash
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
LOCK="$BOSS_BUILD_TOOLBOX/worktree-lock.sh"
RUN_RECORD="$(git rev-parse --absolute-git-dir)/boss-build-run-id"
BLI_RUNID="$(sed -n '1p' "$RUN_RECORD")" || exit 1
START_SHA="$(sed -n '2p' "$RUN_RECORD")" || exit 1
[ -n "$BLI_RUNID" ] && [ -n "$START_SHA" ] || exit 1
"$LOCK" acquire "$BLI_RUNID" pending
```

The `pending` ticket is replaced in Step 2 (`"$LOCK" acquire "$BLI_RUNID" <TICKET-ID>`). Once you
hold the lock, create the route receipt every later step stamps (Step 9 lists the tokens) and carry
its path forward: `BOSS_BUILD_ROUTE_RECEIPT="$(mktemp -t boss-build-route.XXXXXX.json)"`.

- `ACQUIRED` / `TOOK_OVER_STALE` (exit 0) — you own the worktree. `TOOK_OVER_STALE` means a prior
  run here died: treat it as a resume candidate.
  On `TOOK_OVER_STALE`, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger stale-claim-taken-over --where "boss-build Step 2 worktree lock"` (never fatal).
- `HELD_BY_PEER` (exit 3) — a live run owns it. Stop `NO_CHANGE` with zero writes; do **not** go
  through Step 12 (you hold nothing to release).

Refresh with `"$LOCK" heartbeat "$BLI_RUNID"` at step boundaries (at least at the start of Steps 5, 6
and 8).

## Step 2: Select one ticket

- **A named ticket** (e.g. `<ISSUE-ID>`): `getIssue` with relations, saved as `$ISSUE_JSON`. Naming
  it bypasses the estimate filter and overrides — loudly — the blocked-by skip:
  `WARNING: <ID> is blocked by <BLOCKER-IDS> (unmerged) — implementing only because it was named explicitly`.
  Verify must still admit it: run `node "$BOSS_BUILD_TOOLBOX/build/named-admission.mjs" check "$ISSUE_JSON"`,
  with `--approve` when the arguments carry it (`split-args` puts it in `other`). Any non-zero
  exit stops `NO_CHANGE` quoting `reason`, with no claim or state move; on exit 3 an interactive run
  asks the operator instead, and a yes re-runs `check` with `--approve`.
  It needs something to build from: when `build-source` exits 3 (no readable plan attachment and no
  title or description text), stop `NO_CHANGE` quoting its `reason`, with no claim or state move.
- **Selection flags** (`--label`, `--exclude-label`, `--assignee`, `--creator`, `--project` and their
  `--exclude-` forms) are filters, never ticket ids: classify arguments with
  `node "$BOSS_BUILD_TOOLBOX/selection.mjs" split-args -- <args>`. With a named ticket they and
  `--max-open-prs` are ignored with a one-line warning.
- **Otherwise**: first run `node "$BOSS_BUILD_TOOLBOX/build/open-pr-limit.mjs" -- <args>`; a
  non-zero exit stops `NO_CHANGE` quoting its `reason`. Then read candidates with
  `node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" list-planned --team <team> <selectionArgs>`.
  On a non-zero exit, when `trackerConfigFor(config).selection` is present or `selectionArgs` is
  non-empty, stop `NO_CHANGE` quoting its stderr; never fall back to the unfiltered call. With no
  selection in effect, fall back to the `selectPlanned` operation descriptor (resolved team,
  planned state, limit 250); its nodes have no `blockerVerdict` and are treated as `unknown`.
  Keep `agent-build` tickets, with or without a plan attachment, and drop `needs-human`.
  `list-planned`'s order is the build order (each node's `rank` says why); never re-rank it, and
  no plan marker, estimate or predicted park changes it. Walk descriptor-fallback nodes by
  priority (None last), then oldest. Walk the ranking: for
  every candidate whose `blockerVerdict` is not `unblocked`, run
  `node "$BOSS_BUILD_TOOLBOX/build/blocker-verdict.mjs" --mode selection --id <id>`.
  Use its verdict: completed/canceled or merged blockers clear; a blocker in the configured
  in-review state also clears for selection when every open linked PR is non-draft, green and
  file-disjoint from the dependent's Key changes paths. Unknown facts or no paths fail closed.
  A helper failure is not an unblocked verdict. For each `softCleared` entry of the selected
  ticket, pass a `plan-question` candidate to Step 7.3 recording
  `built ahead of <ID> (PR #N, <configured in-review state>): planned files are disjoint; merge after it`,
  with `chosen` building on the base and `alternative` stacking on the blocker branch.
  Named-ticket overrides and epic merge ordering are unchanged. For each unblocked
  candidate, read its `getIssue` payload and take the first non-epic with something to build from
  (`build-source` does not exit 3). None ⇒ run the helper with `--mode selection`, each walked `--id` and `--summary`;
  stop `NO_CHANGE` quoting its top gating blockers (id, state, PR and dependents held), or the
  helper failure when unreadable. Print one selection line: the pick with its `rank` (`tierFrom`,
  `deadlineFrom`, leverage) and up to three higher-ranked candidates walked past, each with why
  (blocked by `<ID>` and its state or PR, epic, nothing to build from).

`agent-question` never blocks selection. Build the option the plan chose for each open question
(when it recorded none, decide per Rule 1) and pass each to Step 7.3 as a `plan-question` candidate
with `chosen` and `alternative`; it becomes an `## Autonomous decisions` line. Selection has no
side effects: nothing is claimed or moved until Step 3. Detail:
[`references/claim-and-eligibility.md`](references/claim-and-eligibility.md).

Record the ticket in the lock (`"$LOCK" acquire "$BLI_RUNID" <TICKET-ID>`). Capture the ticket's
current tracker state as the **entry state** (in bossd-managed runs, the state from the
bootstrap payload, before bossd's session-start sync moved it). Standalone (`BOSSD_MANAGED=0`):
create a `boss-build/<ticket-id>` branch off the base first
([`references/standalone-mode.md`](references/standalone-mode.md)).

## Step 2.5: Ours to adopt, or foreign?

```bash
# Stays on gh: the ownership judgement reads the PR title and body, which the daemon does not carry.
PR_JSON="$(gh pr list --head "$SESSION_BRANCH" --state open \
  --json number,title,body,headRefName,state)"
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
PR_NUMBER="$(node "$BOSS_BUILD_TOOLBOX/build/pr-ownership.mjs" number --pr-json "$PR_JSON")"
```

Judge ownership from the branch name, `[<ISSUE-ID>]` in the PR title, `Tracker issue: <url>` in its
body, and real commits ahead of `$BASE_REF` (ignoring bossd's empty bootstrap commit):

| what you find                           | route                                                   |
| --------------------------------------- | ------------------------------------------------------- |
| no PR and no real commits               | **fresh** — Step 7 creates the PR                       |
| bootstrap-only PR                       | **fresh** — Step 7 reuses that PR                       |
| this ticket's PR/branch with real work  | **resume** — Step 4.5 assesses it, Step 7 reuses the PR |
| someone else's PR/branch with real work | `NO_CHANGE` — never co-edit it; go straight to Step 12  |

## Step 3: Claim

```bash
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
TOKEN="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-token)"
LEASE_STATUS=0
LEASE_JSON="$(node "$BOSS_BUILD_TOOLBOX/issue-lease.mjs" acquire --scope build --issue '<TICKET-ID>' --token "$TOKEN")" || LEASE_STATUS=$?
```

Use the selected ticket id for `<TICKET-ID>`. Before `readComments`, posting a claim or moving state,
route on `LEASE_STATUS`:

- `0` (`acquired`) — continue with the comment claim below; both existing claim paths stay unchanged.
- `3` (`held`) — post nothing and move no state. Take the next candidate as for `LOST`, with no
  claim comment to delete. A named ticket, including an epic hand-off, stops `NO_CHANGE`, quoting
  the peer's `expiresAt` from `LEASE_JSON` (say unavailable when the helper has no expiry).
- `4` (`unavailable`) — record `LEASE_JSON.reason` in the run report and continue with the existing
  comment claim. Hosts without a daemon, older installs and service outages take this fallback.
- Anything else ⇒ `BLOCKED: issue lease`.

Keep the default 7200 s TTL and do not renew: after the claim moves the ticket to `.inProgress`,
planned selection no longer sees it. The lease adds a first ownership signal; it does not replace
comment arbitration. Release it best-effort whenever this run gives the ticket back:

```bash
node "$BOSS_BUILD_TOOLBOX/issue-lease.mjs" release --scope build --issue '<TICKET-ID>' --token "$TOKEN" || true
```

Use that release on `LOST`, before every `NO_WINNER` retry, the Step 6 nothing-built path, and every
Step 12 terminal path. Release only this run's token, never the peer's.

```bash
BODY="$(node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-comment --token "$TOKEN" --session-id "${BOSS_SESSION_ID:-}")" || { echo "BLOCKED: claim body"; exit 1; }
```

Before posting, read comments with `read-claim-comments --id <issue-uuid> --out-file <run-scratch-comments-file>`
(no condition means one read). On exit 2, fall back to one `readComments` operation and save its
comment array to the same scratch file; usage errors must be fixed. On an exit-4 retry, add
`--until-absent <deleted-comment-id>` for every claim deleted on the prior attempt to this
pre-post probe and the post-post read below. Then route: a fresh `ACQUIRED` lock **and** zero peer claim comments is
the uncontended fast path (`UNCONTENDED=1`, no liveness evidence, no waits). Anything else runs the
full contended ceremony in [`references/claim-and-eligibility.md`](references/claim-and-eligibility.md)
(liveness evidence before posting, a 20 s wait after, malformed evidence is a hard error). Post `$BODY`
with `writeComment`, move the ticket to `.inProgress`, **re-read the comments** (both paths) using
`node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" read-claim-comments --id <issue-uuid> --until-claim "$TOKEN" --out-file <run-scratch-comments-file>`.
Retain its `ownClaimId` for deleting this run's claim on `LOST`, `NO_WINNER` and Step 12.
On exit 2, fall back to one `readComments` operation, save the returned array to the scratch
file and obtain the own claim id from that read. If the budget expires without an own claim,
recover its id through a fresh read before deletion; never guess an id or use the write echo.
Set `COMMENTS_FILE` to the scratch file path, then:

```bash
set --
if [ -z "${UNCONTENDED:-}" ]; then
  test -n "${BOSS_CLAIM_LIVENESS_JSON:-}" || { echo "BLOCKED: liveness"; exit 1; }
  set -- --liveness "$BOSS_CLAIM_LIVENESS_JSON"
fi
node "$BOSS_BUILD_TOOLBOX/tracker/cli.mjs" claim-verdict --me "$TOKEN" --comments "$COMMENTS_FILE" "$@"
```

- exit 0 (WON) — contended path: wait ~10 s and re-confirm with fresh liveness; proceed if still 0.
- exit 3 (LOST) — delete your claim by `ownClaimId`, release your issue lease, leave the state alone, take the next candidate; none ⇒
  `NO_CHANGE`.
- exit 4 (NO_WINNER) — delete your claim by its read-derived id, record that deleted id, release
  your issue lease and repeat Step 3 once with `--until-absent` for that id on both reads;
  again ⇒ `NO_CHANGE`. A missing read-derived id is unknown evidence, never a guessed deletion.
- anything else ⇒ `BLOCKED: claim`.

Once WON on Step 2's `approve` route, run `node "$BOSS_BUILD_TOOLBOX/build/named-admission.mjs" apply "$ISSUE_JSON"`
before Step 4; a non-zero exit stops `NO_CHANGE` (Step 12) quoting its `reason`.

Once WON with `BOSS_SESSION_ID` set, link the session to the ticket so a run that dies before Step 9
still leaves bossd a session naming its ticket (best-effort; warn and continue on failure):
`"${BOSS_BIN:-boss}" session set-tracker "$BOSS_SESSION_ID" '<ticket-id>'`.
After that link attempt, when the selected ticket came from `list-planned` and carries `rank`,
store that candidate's rank object unchanged (best-effort; warn and continue on failure):
serialize it with the file tool to a recorded scratch file (`SELECTED_RANK_FILE`), then feed stdin:
`"${BOSS_BIN:-boss}" session set-rank "$BOSS_SESSION_ID" '<ticket-id>' --rank-json - < "$SELECTED_RANK_FILE"`.
Tracker provenance strings are data; never interpolate the JSON into shell source.
Skip for a named ticket or a descriptor fallback without a rank. The guarded write cannot change
an existing tracker link; later rank refresh fills sessions skipped here.

## Step 4: Read the plan

Save the `getIssue` payload as `$ISSUE_JSON`. The plan lives in the tracker, never the repo:

```bash
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
PLAN_FILE="$(git rev-parse --git-dir)/boss-build/plan.md"
mkdir -p "$(dirname "$PLAN_FILE")"
node "$BOSS_BUILD_TOOLBOX/plan-attachment.mjs" build-source "$ISSUE_JSON" "$PLAN_FILE"
```

`build-source` writes the title and description to `$PLAN_FILE` (`planFile`). `source: attachment`:
read the plan by `attachmentId`. For a command `readPlanAttachment`, invoke
`tracker/cli.mjs invoke-operation readPlanAttachment --args-file <args-file>` with
`{id: <attachmentId>, outFile: "$PLAN_FILE"}`; the command writes the bytes itself. For an MCP
operation, write its returned content bytes over `$PLAN_FILE`. A failed, empty or over-1 MiB read,
or no such capability, keeps the title-and-description plan (say so in the PR body); only with
`planFile: null` is there nothing to fall back to, which stops `NO_CHANGE`. Exit 3 stops `NO_CHANGE`
quoting `reason`, including when the issue reports duplicate plan records.
The plan is whatever it contains: no headings and no boss-plan description format are required.
Work out what needs building and what "done" means, and hand `$PLAN_FILE` to every subagent.

When the ticket description contains images (markdown images, `<img>`, attachment URLs), view them
with `extractImages` — reading `![](url)` as text shows no pixels. Best-effort.

### Step 4.5: Assess adopted work (resume only)

Build a done-vs-remaining map from the branch diff, its log and the PR body (trust the diff), and set
Step 5's scope: **none** (everything already satisfied — skip to Step 6), **remaining**, or **fresh**.
Build on top of the existing work, never revert it.
[`references/resume-assessment.md`](references/resume-assessment.md) has the procedure, including
recovery for a restarted orchestrator.

### Step 4.6: Check premises and criteria against the code

Before Step 5, run `node "$BOSS_BUILD_TOOLBOX/plan-recheck.mjs" "$PLAN_FILE"` against the current
worktree. Its JSON verdict determines the action: `current` → proceed; `drifted` → use the reported
locations and record them under `## Autonomous decisions`; `stale` → apply rule 2 and record the
departure in `## Premise discharge`; `unknown` → re-check the cited paths and symbols by hand and
record that fallback. Re-run every `not-evaluated` premise by hand. The citation verdict does not block implementation; the open-PR hold below is the exception.
When the plan states no acceptance criteria, derive them from what it says done looks like. When the
plan is an attachment, ignore the ticket's `## Original notes` (that is pre-planning history).

Re-check in-flight work once, after the citation recheck:

```bash
OWN_PR="${PR_NUMBER:-$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" session-pr --field number 2>/dev/null)}"
# Stays on gh: the daemon has no list-open-PRs-with-files read; the helper labels its gh call.
# Set FRESH_ROUTE=1 only for Step 2.5's fresh route; leave it empty on resume.
node "$BOSS_BUILD_TOOLBOX/build/open-pr-hold.mjs" check --plan "$PLAN_FILE" --issue-json "$ISSUE_JSON" \
  ${OWN_PR:+--pr "$OWN_PR"} ${FRESH_ROUTE:+--apply}
```

Record every overlap, planned or not, and every warning under `## Premise discharge`.
Exit 69 records `could not evaluate open PRs (<reason>)` and proceeds; a truncated snapshot
must carry its warning rather than being described as clean. Only an unplanned file-level overlap
holds a fresh run: exits 3 and 4 stop `NO_CHANGE` through Step 12 before implementation, even
when label writes failed (exit 4). On resume run without `--apply`, record overlaps and continue
even on exit 3. On a fresh hold whose writes landed, the helper's marker comment supplies the
Step 12 breadcrumb; if they did not land, that breadcrumb names the held PRs and shared files.

## Step 5: Implement

Report the phase: `node "$BOSS_BUILD_TOOLBOX/stage-chain.mjs" phase building` (never fatal).

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
   runs the smallest covering command through the verification receipt helper until it fails for the right reason, writes the minimal code,
   re-runs it green, refactors, and reviews its own task for spec compliance and quality.

A tier **ran successfully** when its work is on the branch (the commits it reported are in the log
range, or you recovered its residue) **and** the scope it was handed is implemented — check the
plan's criteria against the diff, never the dispatch's word. A tier that falls short (or an
extension that fails to load) is recorded as `extension <name>: skipped (<reason>)` / `tier <n>:
skipped (<reason>)` and the next tier gets **only what is still open**. Recompute that remainder
from the branch before every dispatch; where nothing remains, dispatch nothing and record
`not dispatched (scope already satisfied)`. One successful extension suppresses tiers 2 and 3.
For a methodology-extension skip or invalid result, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger extension-failed --where "<extension name>"` (never fatal).

**Task contract.** Each implementation subagent returns only: task id, files touched, tests
added/passing, interface signatures, residual risks (checked against the prior art it cited),
decisions made (with rationale), `verificationReceipts` (receipt ids for every command in `testsAddedOrPassing`), and commits made (short SHA + subject, or _no commit —
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
- Rename identifiers with `node <toolbox>/rename-symbol.mjs --from <Old> --to <New> --path <pathspec>`,
  never a shell loop feeding `$(…)`/`${(f)…}` output to `sed -i` — Claude Code stops for a human on it
  even in bypass mode.
- Do not write to the PR or the tracker; report evidence in the contract.
- Re-read your commit messages before returning; correct any claim the run later disproved.

**Verification contract** (every implementation brief carries it):

- The parent supplies a safe dispatch label (`task-N` or `ext-<name>`) and clears its receipts
  before dispatch. Run every verification command (tests, lint, gates) in the foreground through
  `node "$BOSS_BUILD_TOOLBOX/verification-receipt.mjs" run --dispatch <label> --id <id> -- <cmd>`.
- Reuse the same receipt id for the same test command across its completed expected-red and
  green runs, so the final receipt records the repaired gate. Await each run before replacing its
  receipt; distinct commands use distinct ids. Report the expected failure in the task summary.
- Await completion. Never return while a verification command is running. Report each receipt id
  in `verificationReceipts`; a command result without a receipt id is not evidence.

**Verify each dispatch.** Before dispatching, the tree is clean and you have recorded the starting
HEAD. After it returns, the tree is clean again and the log has advanced by the commits it reported.
Also run `node "$BOSS_BUILD_TOOLBOX/verification-receipt.mjs" check --dispatch <label> --id <each-reported-id> --wait-ms 540000`
inside a tool timeout of at least 600 seconds. Read every receipt, including unreported ones.
Exit 0 admits the verification; exit 1 means a command finished but failed: fold its `summary`
into the existing one re-dispatch allowance, and do not mark the task done. Exit 2 transfers gate
ownership to the parent: for a live `pending` receipt, repeat the bounded `check` until it becomes
terminal; a wait deadline never authorizes restarting that command or replacing its receipt.
Re-run only `absent` or `orphaned` commands, after establishing that their original gate process
is no longer running (a dead wrapper alone does not establish this). Use the recorded `command`
or, when absent, the closest covering tests from **Verification**, in the foreground through
`run` with the same receipt id. If the original process cannot be settled, keep verification
incomplete and do not admit the dispatch. Record these results under `## Local verification`,
then check again before admitting the dispatch. Clear receipts only after every gate has settled.
Never substitute a whole-suite rerun. Usage errors must be corrected, never admitted.
Residue the contract names: commit it yourself (only those paths) and re-check the task. Residue
nobody can attribute: stop dispatching and go to Step 12 `BLOCKED`, naming the paths. An empty log
range with no _verification only_ claim: confirm from the diff whether the work is actually missing;
if it is, re-dispatch once with what you verified folded in. Exclude the daemon artifacts
`.claude/scheduled_tasks.lock` and `.claude/settings.local.json` from every such check. Mechanics:
[`references/resume-assessment.md`](references/resume-assessment.md#dispatch-snapshot-mechanics).

## Step 6: Review

Report the phase: `node "$BOSS_BUILD_TOOLBOX/stage-chain.mjs" phase reviewing` (never fatal).

**Baseline.** Fresh/bootstrap-only: `REVIEW_BASE="$START_SHA"`. Resume: `REVIEW_BASE="$BASE_REF"`.

**Anything to review?**

```bash
node "$BOSS_BUILD_TOOLBOX/worktree-state.mjs" --base "$REVIEW_BASE" \
  --exclude .claude/scheduled_tasks.lock --exclude .claude/settings.local.json -- .
```

`clean` ⇒ nothing was built: restore the entry state and apply State-move confirmation above, delete the claim, release your issue lease,
Step 12 `NO_CHANGE`.
`unknown` ⇒ `BLOCKED`. `dirty` ⇒ commit what this run touched (path-scoped) so the review sees it.
Whenever `worktree-state.mjs` returns `unknown`, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger evidence-unknown --where "boss-build worktree-state"` (never fatal).

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

**Post-rebase audit.** Capture `PRE_REBASE_HEAD="$(git rev-parse HEAD)"` before any rebase in this
run; after it succeeds, ask what it silently changed:

```bash
node "$BOSS_BUILD_TOOLBOX/post-rebase-audit.mjs" check --repo "$(git rev-parse --show-toplevel)" \
  --pre-rebase-head "$PRE_REBASE_HEAD" --base "$BASE_REF"
```

Act on every list it reports, then record its stderr note in `## Autonomous decisions` beside the
base-drift `note`:

- `skippedCommits` ⇒ re-run the generators for those paths.
- `lostAdditions` ⇒ re-apply the branch change onto the base's new structure; re-verify its claim.
- `baseAddedTests` ⇒ run them by name.
- `retiredByBase`, `conventionSuspects`, `baseRelocations` ⇒ move the branch's writes to the base's
  convention (judge each suspect first).
- `deletedByBranch` ⇒ fix the base-side references.
- verdict `unevaluated` ⇒ not clean: run the module tests and record it.

**Which review.** `reviewSkillFor(config)` (`skill-config.mjs`) names it: `boss-review` by default,
or the repo's replacement skill — or chain of skills, run in order — from `review.skill`. Every one
receives the same inputs and must write the same verdict file (`review-verdict.mjs` is the contract);
boss-build reads nothing else from it.

**Dispatch.** Before dispatching review members, release every completed subagent this run still
holds, including Step 5 workers, using Rule 6's host capability check. Each Step 6 pass gets its own numbered directory under the run's review root
(preflight clears it), and each chain member a numbered directory inside that, created before the
dispatch so a member that never writes its verdict is counted as missing:

```bash
REVIEW_ROOT="$(git rev-parse --git-dir)/boss-build/review"; mkdir -p "$REVIEW_ROOT"
REVIEW_PASS="$(find "$REVIEW_ROOT" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')"
REVIEW_PASS=$((REVIEW_PASS + 1))
node --input-type=module -e '
  import{pathToFileURL as u}from"node:url"
  const m = await import(u(process.env.BOSS_BUILD_TOOLBOX+"/skill-config.mjs").href)
  process.stdout.write(m.reviewSkillFor(m.loadSkillConfig()).skills.join("\n") + "\n")
' >"$REVIEW_ROOT/skills.txt"
N=0
while IFS= read -r SKILL; do
  N=$((N + 1)); mkdir -p "$REVIEW_ROOT/$REVIEW_PASS/$N"; echo "member $N: $SKILL OUT_DIR=$REVIEW_ROOT/$REVIEW_PASS/$N"
done <"$REVIEW_ROOT/skills.txt"
```

For each skill in the chain, dispatch one fresh awaited subagent that runs it and nothing else —
never add a review of your own around it. Its prompt's **first line is exactly
`[bs-reviewer-dispatch]`**, and it states `BASE=$REVIEW_BASE`, `HEAD=<current HEAD>`,
`PLAN_FILE=$PLAN_FILE`, `OUT_DIR=` the path the block printed for that member,
`--fix`, the base-drift note and, on a resume, the Step 4.5 map. It returns one summary line; the
verdict file is the result. When `REVIEW_PASS > 1` and
`$(git rev-parse --git-dir)/boss-build/review-verdict.json` exists, also pass
`PRIOR_VERDICT=` that path to every member. It holds the previous pass's pooled verdict and is
replaced only after this pass's members finish. Preflight clears review state between runs.
Wait for each dispatch up to the hang guard
(`reviewTimeDefaults(config).hangGuardMs`); a dispatch that never writes its verdict is an error, not
a pass. If dispatch fails, release completed agents where supported and retry the member once.
If it still fails, run the review skill in this context once; it must dispatch its own finders and
verifiers through `review/dispatch-waves.mjs` and record unavailable workers as coverage gaps.
This context never reviews the diff itself. A replacement skill retains its independent-worker
contract; an unavailable skill with no fallback produces the missing verdict error.

**Classify** from the verdict files only — never the returned prose. Every pass this run is pooled,
so a later re-review never clears a blocking finding; a later pass may resolve earlier
non-blocking findings it confirmed gone:

```bash
BUILD_STATE="$(git rev-parse --git-dir)/boss-build"; REVIEW_ROOT="$BUILD_STATE/review"
VERDICT="$BUILD_STATE/review-verdict.json"
REVIEW_VERDICT="$(node "$BOSS_BUILD_TOOLBOX/review-verdict.mjs" pool --root "$REVIEW_ROOT" \
  --out "$VERDICT" --note "$(git rev-parse --git-dir)/boss-build-review-verdict")"
node "$BOSS_BUILD_TOOLBOX/review-verdict.mjs" render "$VERDICT" --report >"$BUILD_STATE/review-report.md"
```

A missing or invalid verdict reads `error`, so the note is `none` — never a silent pass. Route:
`clean` → Step 6.5, then Step 7. `capped` (blocking findings remain after the review's one fix pass)
and `none` go straight to Step 7 so the work is pushed and visible; their terminal route is
`BLOCKED` cause 3 at Step 9's gate. On `none`, run
`node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger dispatch-failed --where "boss-build Step 6 review"` (never fatal).

`BOSS_BS_REVIEW=0` is the one way to skip review: dispatch nothing, write `REVIEW_VERDICT=disabled`
(the ready gate waives the review, not the reviewed-tree check), and publish `## Review coverage`
`none: review did not run (disabled by BOSS_BS_REVIEW=0)` and `## Cross-model review`
`skipped: disabled`.

When the review has returned, `node "$BOSS_BUILD_TOOLBOX/stage-chain.mjs" phase building` (never fatal).

## Step 6.5: Knowledge extensions (repo opt-in)

After a `clean` review: discover `--role knowledge`. None ⇒ nothing, no output. Otherwise dispatch
each (instructions from `skillPath`), validate with `skill-extensions.mjs validate --role knowledge
--file <outPath>`, and record `extension <name>: skipped (<reason>)` per failure. Extensions may
commit a knowledge artifact, so Step 7 captures the reviewed tip **after** this. Never fatal.
[`references/knowledge-extensions.md`](references/knowledge-extensions.md).
For a knowledge-extension skip or invalid result, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger extension-failed --where "<extension name>"` (never fatal).

## Step 7: Push and publish the PR

Every route from Step 6 comes through here.

1. **Push** (record `REVIEWED_HEAD=$(git rev-parse HEAD)` first and persist it to
   `$(git rev-parse --git-dir)/boss-build-reviewed-head`):

   ```bash
   # ci-request: skip pre-tag head; Step 8 pushes the tagged head and requests CI
   PUSH_JSON="$(node "$BOSS_BUILD_TOOLBOX/finalize/push-branch.mjs" --branch "$SESSION_BRANCH" --base "$BASE_REF")" || true
   if [ "$(printf '%s' "$PUSH_JSON" | jq -r .refused)" = "superseded" ]; then
     # Step 2.5 established branch ownership; lease exactly the superseded tip just classified.
     REMOTE_TIP="$(printf '%s' "$PUSH_JSON" | jq -r .remoteTip)"
     PUSH_JSON="$(node "$BOSS_BUILD_TOOLBOX/finalize/push-branch.mjs" --branch "$SESSION_BRANCH" --base "$BASE_REF" --expect-remote "$REMOTE_TIP")" || true
   fi
   printf '%s\n' "$PUSH_JSON"
   ```

   `pushed: yes` continues. `rescue` (the commits are on the `rescue` ref it names) or `no` (nothing
   left the worktree; name the SHAs) is `BLOCKED` cause 2: publish the blocker comment with both
   coverage tokens ([`references/publish.md`](references/publish.md)) and go to Step 12.

2. **Did the reviewed tree ship?** `git fetch -q origin "$SESSION_BRANCH"`; if `FETCH_HEAD` is not
   `REVIEWED_HEAD` (someone pushed on top, or the push rebased), either re-review the new tip as
   Step 9's re-review does, or publish
   `none: review coverage unknown (branch tip moved after review: <A> → <B>)`. Never
   claim coverage of a tree that was not reviewed.

3. **PR.** Compose the body in a temp file outside the worktree
   ([`references/publish.md`](references/publish.md) has the template), then create a **draft**
   (`gh pr create --draft --title "[<ISSUE-ID>] <issue title>" --body-file …`)
   when Step 2.5 said fresh with no PR, else `gh pr edit "$PR_NUMBER"` the existing one with the body
   `pr-body.mjs merge` builds from its live body. Its review
   sections come from `node "$BOSS_BUILD_TOOLBOX/review-verdict.mjs" render <verdict> --pr-section`
   (`$(git rev-parse --git-dir)/boss-build/review-verdict.json`), pasted verbatim — except under
   `BOSS_BS_REVIEW=0`, where there is no verdict and Step 6's disabled sections are published. Run
   `validateVerifyOnlyEvidence(config, body)` and `validateLocalVerification(config, body)` over the
   body before publishing it. Fix local-verification findings in the body and validate again; they
   never change the route or yield BLOCKED. Write every candidate follow-up as `{text, source}` (plus
   the optional fields `FOLLOWUP_SOURCES` in the helper names per source: `outside-worktree`, `host`,
   `stage-evidence`, `plan-question`, `review-finding`, `post-merge`) to a JSON file — including every candidate
   `review-verdict.mjs followups <verdict>` prints — and run
   `node "$BOSS_BUILD_TOOLBOX/build/followup-route.mjs" route --candidates <file> --notes-out <notes-file>`.
   Paste its `sections` into the body, append its `decisions` to `## Autonomous decisions`, and
   settle each of its `questions` per Rule 1; each `warnings` entry names an item it rerouted or
   dropped for being unclassified or incomplete. When the notes file holds any
   entry, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger host-environment --from <notes-file>`
   (never fatal). Then run `node "$BOSS_BUILD_TOOLBOX/review/second-model-health.mjs" record <verdict>`
   (never fatal; it turns repeated second-model failures into one host health note). Only the PARTIAL
   marker in the body or title holds a merge.

4. **Review comment.** Upsert exactly one `<!-- bs-review -->` comment: the review report
   (`$(git rev-parse --git-dir)/boss-build/review-report.md`, rendered by Step 6 — it reports an
   unusable verdict honestly too). Under `BOSS_BS_REVIEW=0` there is no report: the comment body is
   `<!-- bs-review -->` and `review did not run (disabled by BOSS_BS_REVIEW=0)`, so no earlier
   run's verdict stays on the PR.

## Step 8: Tag and get to green

```bash
BOSS_BUILD_PIN="$(git rev-parse --absolute-git-dir)/boss-skill-pins/boss-build/${BOSS_AGENT_SESSION_ID:-local}"
BOSS_BUILD_TOOLBOX="$BOSS_BUILD_PIN/toolbox"
[ -f "$BOSS_BUILD_PIN/.boss-skill-pin.json" ] || { echo "BLOCKED: boss-build skill pin missing - re-run Preflight"; exit 1; }
test -f "$BOSS_BUILD_TOOLBOX/finalize/cli.mjs" || exit 1
# PR_NUMBER was captured in Step 7; re-derive if unset (resume / fresh shell). PR state reads go
# through the daemon-first reader; it runs labelled gh only when no daemon answers.
PR_NUMBER="${PR_NUMBER:-$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" session-pr --field number)}"
test -n "$PR_NUMBER" || exit 1
BASE_BRANCH="$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" view --pr "$PR_NUMBER" --field baseRefName)"
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
test "$(git rev-parse HEAD)" = "$(git rev-parse "refs/remotes/origin/$SESSION_BRANCH")" || exit 1  # HEAD == what was pushed
node "$BOSS_BUILD_TOOLBOX/ci/ci-request.mjs" request --pr "$PR_NUMBER" --head "$(git rev-parse HEAD)"
node "$BOSS_BUILD_TOOLBOX/build/ready-gate.mjs" remap-reviewed-head --base "$BASE_BRANCH"
```

A non-zero injector exit is a disclosure item, not a blocker — history may be partly rewritten, so
check before re-running (it is a no-op on tagged commits). A rejected amend names its reason (type,
missing scope, header or body-line length); fix exactly that. To rewrite a message without an
editor, use `git filter-branch --msg-filter` keyed on `$GIT_COMMIT`, back the range up first, and
prove the rewrite touched only messages (`git diff <backup> HEAD` empty, same commit count).

**CI request outcomes** (every request in this skill, after local verification): exit 0 continues.
`head-moved` re-reads the PR head and re-runs the tag-and-push block once, within branch ownership.
`dispatch-failed` or `unconfirmed` records the JSON under `## Autonomous decisions` and continues
to the CI wait. `skipped-conflicting` takes the existing conflict route: rebase, re-run the local
gates, push and request again. A CI wait ending `not-requested` retries the request once; a second
`not-requested` is `BLOCKED` cause 1, quoting the request JSON. Never call an unconfirmed request green.

Then wait for CI (arm watches, bounded poll) and run **boss-repair** for failing checks, conflicts
and review comments, with `BOSS_NOTES_SUPPRESSED=1`, up to `policy.repairCap` (5) passes, re-arming
the CI wait after every push. Still red after the cap ⇒ `BLOCKED` cause 1: PR stays draft, blocker
comment names the failing check, `file:line`, and what was tried.
When the repair cap leaves checks red, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger repair-exhausted --where "<failing check name>"` (never fatal).

## Step 9: Decide the route and finalize

If boss-repair added untagged non-empty commits, re-run Step 8's tag-and-push block and CI wait.
Add repair and readiness-rebase test re-runs to `## Local verification` as
phase-labelled bullets (repair row `<command>: <result>` ⇒ ``repair: `<command>` → <result>``); fix findings from
`validateLocalVerification(config, body)` and validate again before `gh pr ready`. Green CI on the head is the full test run; do not repeat it locally. Then
decide, asking the gate first on the head you would ready:
`node "$BOSS_BUILD_TOOLBOX/build/ready-gate.mjs"` (base: the PR's; exit 0 `ready`, 3 not ready with
`reasons`; any other exit is not ready). Before asking, persist the route: write `partial` to
`$(git rev-parse --git-dir)/boss-build-ready-route` only when the criteria point to the PARTIAL route
below, and remove that file otherwise. The gate reads it on every later ask, so no flag is carried.

- **Acceptance certification.** Before the route decision, run once on the head being readied:

  ```bash
  node "$BOSS_BUILD_TOOLBOX/build/acceptance-retry.mjs" certify --plan "$PLAN_FILE" --base "$BASE_REMOTE/$BASE_BRANCH" --reuse "$(git rev-parse --git-dir)/boss-build/review-verdict.json" --out "$(git rev-parse --git-dir)/boss-build/acceptance.json"
  ```

  The helper reuses the review's block only when its tree and criterion ids/checks match;
  otherwise it certifies fresh, including with `BOSS_BS_REVIEW=0` or a replacement review that
  omits the block. Each `met` criterion's `evidence` is its `## Local verification` bullet.
  A criterion without a command runs the repo's `acceptance.derivedChecks` runner for a test file
  it names; a full-gate criterion is met by this head's recorded green CI wait.
  `unmet` and `unchecked` are unmet in-scope criteria for PARTIAL, with the helper's `reason`.
  Certification folds classify/retry into the result: a passing scratch-tree retry leaves the
  pushed head, CI and reviewed head alone, follows the normal REVIEW_READY receipt and state
  move, and is recorded under `## Autonomous decisions`.

- **`ready`, and every in-scope criterion is met** (each `- [x]` demonstrated by the diff/tests, or
  a `(verify-only)` criterion carrying its recorded check) ⇒ `REVIEW_READY`; stamp
  `review-gate-passed`. On a `ready` head, a criterion PARTIAL cannot take is published with it.
- **Only unmet in-scope criteria remain**, at least one criterion met, the review produced a
  verdict, and nothing else open ⇒ `PARTIAL` when the gate, on the persisted PARTIAL route, says `ready` (T3 in
  [`references/publish.md`](references/publish.md)).
- **Re-review.** A commit after the reviewed head (a boss-repair fix, a conflict resolution, a
  Step 10 fix) is unreviewed, not a defect. When `tree-changed-since-review` is the gate's only
  reason, run `node "$BOSS_BUILD_TOOLBOX/build/ready-gate.mjs" remap-reviewed-head` (base: the PR's),
  record its `outcome` under `## Autonomous decisions`, and re-read the persisted `REVIEWED_HEAD`.
  Then re-run Step 6 over the new tip (`REVIEW_BASE` = that persisted `REVIEWED_HEAD` when it is an
  ancestor of `HEAD`, else the whole branch) as a new pass — Step 6 pools it with every earlier pass,
  never clearing a blocking finding; a later pass may resolve earlier non-blocking findings it
  confirmed gone. Re-publish the review sections and comment,
  persist the tip after that review's own fix commits as `REVIEWED_HEAD`, re-run Step 8's
  tag-and-push block and CI wait, and ask the gate again. At most two re-reviews per run; drift after that is `BLOCKED` cause 3 like any other
  not-ready reason.
- **Not `ready`** otherwise — the review or re-review is not clean, or the drift or verdict is
  unknown ⇒ `BLOCKED` cause 3: publish per [`references/publish.md`](references/publish.md), never
  ready.

Readying, on every non-BLOCKED route:

```bash
# Gate mergeability before readying. GitHub may report UNKNOWN briefly after a push, so poll with
# a bound; CONFLICTING or any dirty mergeStateStatus means rebase onto the base, re-run the tests
# relevant to the change (## Verification), push, wait for checks, and re-read mergeability.
# GitHub sends no webhook when it finishes computing mergeability, so a re-read is --fresh.
FRESH=""
for attempt in 1 2 3 4 5 6; do
  PR_STATE="$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" view --pr "$PR_NUMBER" ${FRESH:+--fresh})"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  if [ "$MERGEABLE" != "UNKNOWN" ]; then break; fi
  FRESH=1
  sleep 10
done
if [ "$MERGEABLE" != "MERGEABLE" ] || [ "$MERGE_STATE" = "DIRTY" ] || [ "$MERGE_STATE" = "BLOCKED" ]; then
  PRE_REBASE_HEAD="$(git rev-parse HEAD)" || exit 1
  git rebase "origin/$BASE_BRANCH"
  # Route the audit's findings by Step 6's post-rebase list, then re-run the relevant tests here
  # (## Verification) before pushing.
  node "$BOSS_BUILD_TOOLBOX/post-rebase-audit.mjs" check --repo "$(git rev-parse --show-toplevel)" \
    --pre-rebase-head "$PRE_REBASE_HEAD" --base "origin/$BASE_BRANCH"
  git push --force-with-lease origin "$SESSION_BRANCH"
  node "$BOSS_BUILD_TOOLBOX/ci/ci-request.mjs" request --pr "$PR_NUMBER" --head "$(git rev-parse HEAD)"
  # The push re-opened the CI wait: arm (callback-watches.md Protocol step 1), then the bounded
  # poll (Protocol step 5) until CI_WAIT_STATE=settled; anything else goes back to Step 8.
  arm_ci_watches "$PR_NUMBER"
  ci_wait_bounded "$PR_NUMBER"
  PR_STATE="$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" view --pr "$PR_NUMBER")"
  MERGEABLE="$(printf '%s' "$PR_STATE" | jq -r .mergeable)"
  MERGE_STATE="$(printf '%s' "$PR_STATE" | jq -r .mergeStateStatus)"
  test "$MERGEABLE" = "MERGEABLE" || exit 1
  test "$MERGE_STATE" != "DIRTY" || exit 1
  # Re-ask the gate on the rebased head. Not ready (a conflict resolution no review saw is
  # tree-changed-since-review) returns to the Step 9 decision above and is never readied.
  node "$BOSS_BUILD_TOOLBOX/build/ready-gate.mjs" || exit 1
fi
# Ready the PR — the finalize adapter's readyPr capability (isDraft==true guard; command: gh pr ready).
if [ "$(printf '%s' "$PR_STATE" | jq -r .isDraft)" = "true" ]; then gh pr ready "$PR_NUMBER"; fi
# Reads right after our own write are --fresh: a late webhook would still serve the draft state.
test "$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" view --pr "$PR_NUMBER" --fresh --field isDraft)" = "false" || exit 1
# Readying is what STARTS the non-draft-only advisory bot, so the merge state degrades on a branch
# that has not changed. Decide that degraded value through the shared classifier, never by reading
# the raw token: --readied-this-run names the post-ready degrade, and its verdict is pending
# (reason advisory-unsettled), non-blocking. Only `blocking: true` routes back to Step 8.
POST_READY_STATE="$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" view --pr "$PR_NUMBER" --fresh --field mergeStateStatus)"
POST_READY_VERDICT="$(node "$BOSS_BUILD_TOOLBOX/pr-check-state.mjs" merge-state \
  --merge-state "$POST_READY_STATE" --check-state green --check-reason ok \
  --unresolved-threads 0 --readied-this-run)"
test "$(printf '%s' "$POST_READY_VERDICT" | jq -r .blocking)" = "false" || exit 1
```

Then, on `REVIEW_READY`, post the `boss/build` receipt on this head (the Step 12 **Receipt** block) before the state move: verify judges every In Review head and parks one with no receipt. Then move the ticket `.inProgress → .inReview` (not on `PARTIAL`), apply State-move confirmation above, comment the PR URL on the ticket, and publish what the route owes — findings ledger, or
the PARTIAL title/body/marker — per [`references/publish.md`](references/publish.md). The CI reading
that makes a route green is `CI_WAIT_STATE=settled`; `timeout`/`unknown` is not green. A red reading
after readying unwinds first (body back to the plain form, `gh pr ready --undo`), then reports
`BLOCKED`.
When any CI wait ends `timeout` or `unknown`, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger gate-unknown --where "boss-build CI wait"` (never fatal).

Stamp each obligation into the route receipt (created in Step 1) as you complete it, on every route:
`node "$BOSS_BUILD_TOOLBOX/finalize/route-contract.mjs" stamp --receipt "$BOSS_BUILD_ROUTE_RECEIPT" --token <token> --run-id "$BLI_RUNID"`.
Tokens:
`REVIEW_READY` — `verify-only-evidence-validated`, `premise-discharged`, `required-deferred-asserted`,
`review-gate-passed`, `pr-ready`; `PARTIAL` — `partial-gate-satisfied`, `pr-ready`,
`do-not-merge-marked`; every route — `claim-deleted`, `notes-before-lock-release`,
`stop-hooks-removed`, `completion-phase-done`, `lock-released`; optional — `blocked-pr-left-draft`,
`entry-state-restored`,
`no-change-breadcrumb-written`, `pr-merged-by-completion`.

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
- Feedback you cannot fix: respond per finding and stay `REVIEW_READY`. Re-quarantine (draft, blocker
  comment, `BLOCKED`) only when a `BLOCKED` cause actually holds. Every Step 10 push re-asks
  `ready-gate.mjs` (it reads the persisted route); not ready ⇒ unwind (`gh pr ready --undo`, ticket
  back to `.inProgress`, followed by State-move confirmation above) and return to the Step 9 decision, whose re-review may clear it.

## Step 11: Evidence (verify stage)

boss-build captures no proof media. When the repo configures verify-role extensions, the verify
stage records evidence for the head it judges; boss-build neither waits for that evidence nor
reproduces it. Continue to Step 12.

## Step 12: Stop cleanly

Every route that took the lock ends here, `foreign` included. Decide `OUTCOME` first; nothing below
may change it.

- Delete this run's claim comment by the read-derived `ownClaimId` if it still exists.
- If Step 3 generated `TOKEN` for a resolved ticket, release its issue lease on every terminal path:
  `node "$BOSS_BUILD_TOOLBOX/issue-lease.mjs" release --scope build --issue '<TICKET-ID>' --token "$TOKEN" || true`.
  Use that ticket's id; this is best-effort and never fatal, including after a successful build.
- On `NO_CHANGE` for a resolved ticket: restore the entry state with `moveState` unless another
  runner owns the ticket or the state is not one this run produced, and leave exactly one short
  breadcrumb comment naming the branch that fired and why (update it on a repeat; no transcripts,
  output or secrets). On an open-PR hold, the helper's marker comment is the breadcrumb when its
  writes landed; otherwise name the held PRs and shared files in the breadcrumb. Apply State-move
  confirmation above; an unsettled restore is a warning.
- Unless `BOSS_NOTES_SUPPRESSED=1`, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" flush --core boss-build --outcome "$OUTCOME" --mode "<mode>"` once (unsampled, never fatal), then continue.
- **Notes** (skip when `BOSS_NOTES_SUPPRESSED=1`): discover `--role notes`; none ⇒ nothing. Run
  `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" roll --core boss-build --consume`; it reuses Step 6.5’s
  decision. `notesSampleRate(config)` in `skill-config.mjs` resolves `notesDefaults.sampleRate`
  (default `1`). If JSON `sampled` is `false`, print `notes: sampled out (rate <rate>)` and skip this notes dispatch. Otherwise write at
  most five secret-free observations (≤ 8 KiB) to a temp `observations.md` and dispatch **one**
  awaited worker that runs every extension in `(order, name)` order with the envelope
  `{"role":"notes","core":"boss-build","context":{"mode","core","outcome","repoId","observationPath"},"runTmp","outPath"}`,
  each bounded by `BOSS_SKILL_EXTENSION_TIMEOUT_MS`; validate with `--role notes`. Never fatal.
  For a notes-extension skip or invalid result, run `node "$BOSS_BUILD_TOOLBOX/notes-record.mjs" add --core boss-build --trigger extension-failed --where "<extension name>"` (never fatal).
- Remove bossd's Stop hooks so it does not double-finalize:
  `node "$BOSS_BUILD_TOOLBOX/remove-bossd-stop-hooks.mjs"` (a no-op standalone).
- On `BLOCKED` with `BOSS_SESSION_ID` set, tell bossd so its finalize keeps the PR draft and
  Blocks the session: `boss session finish --blocked --reason "<one line>" --json` (never fatal).
- **Receipt** (`REVIEW_READY` only, before Completion): post the `boss/build` commit status on the
  head this run pushed (again, when Step 10 moved the head after Step 9's post). The helper runs the
  ready gate itself and refuses (exit 3, `refused`, nothing posted) a head that is not ready.
  `posted` ⇒ done; a non-zero exit or a moved branch only warns — never certify a head this run did
  not push. `OUTCOME` never changes.

  ```bash
  SESSION_BRANCH="${SESSION_BRANCH:-$(git branch --show-current)}"
  PR_URL="$(node "$BOSS_BUILD_TOOLBOX/pr-state.mjs" session-pr --field url 2>/dev/null)"
  if git fetch -q origin "$SESSION_BRANCH" &&
    [ "$(git rev-parse HEAD)" = "$(git rev-parse "refs/remotes/origin/$SESSION_BRANCH")" ]; then
    node "$BOSS_BUILD_TOOLBOX/commit-status.mjs" post --context boss/build --state success \
      --sha "$(git rev-parse HEAD)" --description "boss-build REVIEW_READY run $(printf '%.12s' "$BLI_RUNID")" \
      --target-url "$PR_URL" || echo "warning: boss/build receipt not posted — bookkeeping only, work state unaffected" >&2
  else
    echo "warning: boss/build receipt not posted — fetch failed or HEAD is not origin/$SESSION_BRANCH" >&2
  fi
  ```

- **Completion**: after Stop-hook removal, before lock release, run the unsampled, non-fatal
  [verify fast path](references/verify-fast-path.md) exactly once per run. It ignores
  `BOSS_NOTES_SUPPRESSED`; no PR is a silent no-op. Inline verification requires
  `buildDefaults.verifyFastPath: true`; otherwise an authorized head records `skipped fast-path-off`
  and Hand-off arms the verify chat. Stamp `completion-phase-done` on every route. A verified merge also stamps
  `pr-merged-by-completion`, moves the ticket to `.done` (bookkeeping failures
  warn). `OUTCOME` never changes.
- On every route with a PR, first `boss callback remove --all --pr "$PR_NUMBER" --repo <owner/name> --json`; a non-zero
  exit is a failed cleanup to report (callback-watches.md step 6). Nothing is re-armed except below,
  and `$WATCH_LIST_JSON` must be listed after this cleanup, never reused from before it.
- **Hand-off** ([`references/stage-chaining.md`](references/stage-chaining.md)), after that cleanup: on `REVIEW_READY` with no `merged` completion record ⇒ `node "$BOSS_BUILD_TOOLBOX/stage-chain.mjs" arm-verify --pr "$PR_NUMBER" --completion "$(git rev-parse --git-dir)/boss-build-completion.json" --run-id "$BLI_RUNID"`. Print its line; never fatal; `OUTCOME` never changes. Set `HANDED_OFF=1` when its `action` is
  `armed` or `already-armed`: verify now holds the PR's forward-progress watch.

- When the completion record is `merged`, skip CI-watch classification; there is no open PR to
  watch. On other `REVIEW_READY` / `PARTIAL` runs, after the hand-off, decide whether you may stop
  watching CI:

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
  ${CALLBACKS_AVAILABLE:+--callbacks-available} ${HANDED_OFF:+--handed-off} --arm-attempts "$ARM_ATTEMPTS"
```

`handed-off`, `settled`, `watched`, `polled` ⇒ print. `unwatched` ⇒ arm the `missingTriggers` it
names, classify once more, print. `unknown` (`unreadable-check-state`) ⇒ run the bounded poll, then
print. Never arm twice.

- Remove recorded scratch in its own Bash call (Rule 9), after callback cleanup and hand-off.
- Finally remove the pin, release the lock, stamp `lock-released` and assert the route receipt.
  Keep the lock through pin removal so a peer cannot publish its next pin before this run deletes it.
  The final process loads the route module and release script before removing the pin; it needs no
  helper path afterwards. An incomplete receipt only warns; an unusable helper remains a hard stop.

```bash
# Keep the two streams APART. stdout carries only the honest outcome line; stderr carries the JSON
# detail plus node's own load-time noise, written BEFORE the outcome. Never merge them with 2>&1.
RC_ERR="$(mktemp -t boss-build-route-err.XXXXXX)"
RC_OUT="$(node --input-type=module - "$BOSS_BUILD_TOOLBOX" "$BLI_RUNID" "$OUTCOME" "$BOSS_BUILD_ROUTE_RECEIPT" 2>"$RC_ERR" <<'NODE'
import fs from 'node:fs'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
import { pathToFileURL } from 'node:url'
const [toolbox, runId, outcome, receipt] = process.argv.slice(2)
const route = await import(pathToFileURL(path.join(toolbox, 'finalize/route-contract.mjs')).href)
const lock = fs.readFileSync(path.join(toolbox, 'worktree-lock.sh'), 'utf8')
const gitDir = execFileSync('git', ['rev-parse', '--absolute-git-dir'], { encoding: 'utf8' }).trim()
try {
  fs.rmSync(path.join(gitDir, 'boss-build-run-id'), { force: true })
  execFileSync(process.execPath, [path.join(toolbox, 'skill-pin.mjs'), 'remove', '--skill', 'boss-build'])
} finally {
  process.stderr.write(execFileSync('bash', ['-s', '--', 'release', runId], { input: lock, encoding: 'utf8' }))
}
route.stampObligation(receipt, 'lock-released', { runId })
process.exitCode = route.runCli(['assert', '--outcome', outcome, '--receipt', receipt, '--run-id', runId])
NODE
)" && RC_OK=yes || RC_OK=no
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

Then print the terminal state, adding `merged <sha>` only for a verified completion merge.

## Verification

Locally, run only the tests relevant to the change: `commands.testAffected` when the repo has one,
otherwise the tests covering what you changed, through the repo's own runner. Record every local
command and its terminal receipt `summary` in `## Local verification`, including repair, rebase and no-CI runs.
When selection is unreliable (no affected selector or mapped tests, or it ran nothing), run the
closest covering tests you can identify and disclose what could not be selected and why with `gap:`.
Do not substitute a full local suite: CI on the pushed head owns it, and Step 8 waits for it. Only
when the PR gets no CI checks at all, run `commands.test` once before readying and record its result.

## Cron gate

When scheduled as an unattended cron, register the gate command from
[`references/cron-gate.md`](references/cron-gate.md) so a run fires only when a candidate exists.

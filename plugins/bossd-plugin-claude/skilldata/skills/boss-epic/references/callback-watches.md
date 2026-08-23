# One-shot CI/PR callback watches (per in-flight child)

Read this when the scheduling loop (Phase 3) is about to wait on an in-flight child's CI checks or
merge/close state. It replaces a naked blocking poll with a **one-shot GitHub callback** armed per
in-flight child, so the epic wakes the moment a child's CI resolves or its PR merges/closes — while
keeping the poll as a bounded fallback and authoritative reconciliation as the only thing that
actually decides. This is the boss-epic parity of boss-build's `references/callback-watches.md`.

**First decide whether callbacks are usable at all.** `callbacksAvailable(env)`
(`toolbox/callback/adapter.mjs`) is the single "callbacks usable" gate, and it is a **conjunction of
two checks — both must hold**:

1. the run is daemon-managed (`BOSS_SESSION_ID` is set), so something is behind the `boss callback`
   interface to answer; and
2. **the `boss` executable that issues the commands actually resolves here** —
   `resolveBossBinary(env)` (`toolbox/boss-binary.mjs`) tries `$BOSS_BIN`, then `boss` on `PATH`,
   then the repo build `./bin/boss`, and accepts a candidate only when it is an existing executable
   file. Executability is decided by a **stat, never by running the binary**, so a slow or wedged
   `boss` cannot hang the gate.

The second check is not redundant: a scheduled/cron environment can export `BOSS_SESSION_ID` while
leaving the CLI off its `PATH`, and a session-variable-only gate then reports callbacks usable and
arms per-child registrations that cannot run.

**Run the executable the gate resolved, not a bare `boss`.** This is the same `BOSS` the Operating
Contract already tells you to resolve once (`SKILL.md`, "**`boss` binary**") — the resolver is that
ladder as code. `resolveBossBinary(env)` returns the winning candidate as `path`, and two of its
three arms — an explicit `$BOSS_BIN` and the repo build `./bin/boss` — resolve to something a bare
`boss` in a shell would not find at all, or would find as a **different** binary (`$BOSS_BIN`
deliberately outranks `PATH`). So bind that `path` once and use `"$BOSS"` in place of the literal
`boss` at every call site below; the snippets spell `boss` only because that is the common case.
Taking the gate's verdict while discarding its `path` reintroduces the same failure from the other
side: a true gate followed by `command not found`.

```bash
# Each fenced block is a FRESH shell, so re-resolve the toolbox rather than assuming
# the startup block's export survived — unset, the bridge below imports
# `undefined/boss-binary.mjs`, dies, and hands back the bare `boss` this exists to avoid.
BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"
export BOSS_EPIC_TOOLBOX
BOSS="$(
  node --input-type=module -e '
    import{pathToFileURL as u}from"node:url"
    const { resolveBossBinary } = await import(u(process.env.BOSS_EPIC_TOOLBOX+"/boss-binary.mjs").href)
    process.stdout.write(resolveBossBinary().path ?? "")
  '
)"
# Empty means the gate is false OR this bridge itself failed — the two are not the
# same, so never read one as the other. `boss` is a last resort, not a substitute:
# under a true gate an empty result is a bug worth logging, alongside the resolver's
# own `reason`.
[ -n "$BOSS" ] || BOSS=boss
```

**The gate is deliberately CLI-only.** A host may expose the same three callback capabilities over an
MCP tool surface as well as over the `boss callback` CLI. This gate does **not** count that surface:
the protocol below is written against the CLI, so a host offering only the MCP tools is treated as
callbacks-unavailable and polls. That is a deliberate narrowing rather than an oversight — widening
it means giving the adapter a second transport signal, which is a separate change.

When the gate is **false**, **skip `registerWatch` entirely and let `policy.fallbackPoll` alone drive
Phase 3** — the clean, documented no-op below, never a failed wait — and **report why**:
`callbacksUnavailableReason(env)` returns the failing conjunct (`BOSS_SESSION_ID is unset`, or the
binary rejection naming `BOSS_BIN`/`PATH`/`./bin/boss`), so the epic log records "polling, because …"
rather than degrading silently. When it is **true**, arm per-trigger watches per in-flight child as
described. It is an up-front check, not a "did the CLI happen to fail at runtime" guess.

**Then select a verified callback target before any callback operation.** The driver records
`CHILD_PR_REPOSITORY` from the PR and the candidate chat/repository pairs from verified orchestrator
and child session records. It must not manufacture a repository identity from `BOSS_SESSION_ID`.
Use this runnable Node-to-shell JSON bridge inside the per-child Phase 3 loop:

```bash
# Leave an unknown candidate identity empty; selectEpicCallbackTarget then rejects it.
# Values come from verified records, never from BOSS_SESSION_ID alone.
CALLBACK_TARGET_JSON="$(
  CHILD_PR_REPOSITORY="$CHILD_PR_REPOSITORY" \
  ORCHESTRATOR_CHAT="${ORCHESTRATOR_CHAT:-}" \
  ORCHESTRATOR_REPOSITORY="${ORCHESTRATOR_REPOSITORY:-}" \
  CHILD_CHAT="${CHILD_CHAT:-}" \
  CHILD_REPOSITORY="${CHILD_REPOSITORY:-}" \
  node --input-type=module -e '
    const { selectEpicCallbackTarget } = await import(`${process.env.BOSS_EPIC_TOOLBOX}/callback/epic-target.mjs`)
    const callbackTarget = selectEpicCallbackTarget({
      childPrRepo: process.env.CHILD_PR_REPOSITORY,
      orchestrator: {
        chatId: process.env.ORCHESTRATOR_CHAT,
        repo: process.env.ORCHESTRATOR_REPOSITORY,
      },
      child: { chatId: process.env.CHILD_CHAT, repo: process.env.CHILD_REPOSITORY },
    })
    process.stdout.write(JSON.stringify(callbackTarget))
  '
)"
CALLBACK_CHAT="$(
  CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
    const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
    process.stdout.write(typeof target?.chatId === "string" ? target.chatId : "")
  '
)"
if [ -z "$CALLBACK_CHAT" ]; then
  echo "No verified callback target; retain cron/poll reconciliation. Continue to Phase 3b reconciliation and the bounded poll/session cron."
fi
```

A candidate is verified only when its chat and repository identities are non-empty and its repository
matches the child PR repository after both identities are normalized to a lowercased `owner/repo`
slug (so a canonical origin URL, an `scp`-like `git@host:owner/repo.git`, a trailing `.git` or `/`,
and mixed case all compare equal). A matching orchestrator wins; otherwise a matching child is used.
`BOSS_SESSION_ID` alone never verifies a target, so an unrelated managed chat cannot receive an epic
callback. If there is no verified target, skip callback registration, re-arm, list, and cleanup, and
retain the existing cron/poll reconciliation for that child. The bridge emits JSON, reads the selected
chat into `CALLBACK_CHAT`, and produces an empty value for no target. An empty chat disables only the
callback commands guarded below; it must fall through to the normal Phase 3b authoritative
reconciliation and bounded poll/session cron for that child. `CHILD_PR_REPOSITORY` remains the
verified selector input; callback CLI operations use the selected target's normalized repository.

The capability contract is the callback-notifier adapter (`toolbox/callback/adapter.mjs`, default
`CALLBACK=boss`). The boss reference (`toolbox/callback/boss.mjs`) maps three capabilities onto the
generic `boss callback` CLI and carries the watch policy:

| Capability      | `boss callback` command | Purpose                                                                       |
| --------------- | ----------------------- | ----------------------------------------------------------------------------- |
| `registerWatch` | `boss callback add`     | Arm a one-shot watch for one trigger; group only mutually exclusive triggers. |
| `listWatches`   | `boss callback list`    | Reconciliation read: enumerate live watches, dedup by id.                     |
| `removeWatch`   | `boss callback remove`  | Tear a stale/duplicate watch down by id when a child settles.                 |

`policy.defaultExpiresIn` = `24h`. `policy.fallbackPoll` = `gh pr checks --watch --fail-fast`.
`registerWatch` and `listWatches` require both `--chat "$CALLBACK_CHAT"` and
`--repo "$CALLBACK_REPO"`.
The generic `removeWatch` CLI accepts `--chat` but not `--repo`: use the prior scoped list to obtain
the callback id, then remove that returned id with `--chat`.

## Trigger policy: draft-aware, never bare `checks_passed`

`policy.watchTriggers` carries the generic default (`checks_passed`, `checks_failed`, `merged`).
**boss-epic overrides the green trigger** and arms the draft-aware set below instead. It does not
change the shared policy, because other drivers wait on PRs they did not open as drafts.

| Trigger               | Fires when                             | boss-epic use                                      |
| --------------------- | -------------------------------------- | -------------------------------------------------- |
| `checks_passed_ready` | checks green **and** PR is not a draft | **Yes** — the merge-eligibility moment.            |
| `checks_failed`       | checks red                             | Yes — wake a repair round.                         |
| `merged`              | PR merged                              | Yes — a child merged out from under the scheduler. |
| `ready_for_review`    | the draft → ready flip                 | Optional — wake on the un-draft event itself.      |
| `checks_passed`       | checks green, draft or not             | **Never** for a boss-build child PR (see below).   |

**Why bare `checks_passed` is wrong here.** boss-build opens its PR as a **draft** and CI runs on
drafts, so `checks_passed` is satisfied by the first green draft commit. Callbacks are one-shot and
are evaluated on PR **state**, not on transitions — so arming `checks_passed` against an
already-green draft fires on the very next evaluation, consuming the watch at a moment that can
never be merge-eligible (Phase 3c requires a non-draft PR). The driver then wakes, reconciles, finds
nothing to do, and must re-arm; repeat for every draft commit. `checks_passed_ready` collapses that
whole class of premature fires into the single wake that matters. Green on a draft PR is expected
CI noise, not merge-eligibility.

A callback remains a **wake signal, not proof**: the daemon merge gate plus the Phase 3b settled-chat
read are the authoritative filter, and both run again on every wake regardless of which trigger fired.

## Protocol

1. **Arm per child on flight entry** (gate `callbacksAvailable` true and `CALLBACK_CHAT`/`CALLBACK_REPO`
   non-empty). When a child enters `inFlight` (Phase 3a launch), register the three triggers against
   that child's PR with separate per-trigger groups. Group two triggers only when at most one of them
   can ever be satisfied for a given PR, such as `merged` versus `closed`; triggers that can be
   satisfied at different times or re-satisfied after a push each need their own group. The `--message`
   is the wake payload (a secret — never echoed by `list`); `--expires-in` bounds the watch so an
   abandoned run's watch self-expires. Record the returned callback/group ids alongside the child's
   `session_id` / `chat_id` for re-arm and cleanup.

   ```bash
   PR="$CHILD_PR_NUMBER"
   MSG="boss-epic: CI/PR state changed for child PR #$PR — reconcile and continue scheduling."
   CALLBACK_CHAT="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.chatId === "string" ? target.chatId : "")
     '
   )"
   CALLBACK_REPO="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.repo === "string" ? target.repo : "")
     '
   )"
   # checks_passed_ready, NOT checks_passed: the child PR is a draft until boss-build
   # readies it, and a bare green-on-draft fire would burn the one-shot watch.
   if [ -n "$CALLBACK_CHAT" ] && [ -n "$CALLBACK_REPO" ]; then
     for T in checks_passed_ready checks_failed merged; do
       boss callback add "$PR" "$T" --group "epicwait-$PR-$T" --message "$MSG" --expires-in 24h --chat "$CALLBACK_CHAT" --repo "$CALLBACK_REPO" --json
     done
   fi
   ```

   When `callbacksAvailable(env)` is false (no daemon behind the `boss callback` interface, or no
   resolvable `boss` executable — see the two conjuncts above), **skip
   registration**; the poll below is the sole wait mechanism. Consult the gate before arming rather
   than discovering unavailability from a CLI error. If a `registerWatch` call nonetheless errors at
   runtime under a **true** gate (e.g. an older host that lacks the `boss callback` subcommand), treat
   it exactly like the gate-false path — skip and let `policy.fallbackPoll` alone drive Phase 3, never
   a failed wait.

2. **Reconcile on wake — a callback is a nudge, not a verdict.** A wake (or a poll return) means
   _re-run the Phase 3b reconciliation_, not _act on the trigger name_. Read the child's real
   session/PR state (`get_session`, `list_check_snapshots`, `get_chat_statuses`, `gh pr checks`, and
   `gh pr view --json state,isDraft,mergedAt,mergeStateStatus`) and record which conjunct declined:

   - `ready` — the rollup was read successfully, is non-empty, every entry is terminal, none failed,
     the PR is open and not a draft, `mergeStateStatus == "CLEAN"`, and the tracked chat has
     **settled** (Phase 3b).
   - `not-yet` — the read succeeded, but one of the `ready` conjuncts is false. Keep waiting unless
     the PR is red or the child left this workflow's hands.
   - `could-not-evaluate` — the rollup could not be read, the rollup was empty, or
     `mergeStateStatus` is `UNKNOWN`/unreadable. Report this outcome by name; never fold it into
     `not-yet`, and never let it reach `ready`.

   A check count of zero is not a pass. An empty commit that skips CI can produce a head SHA with no
   merge workflow runs; a rollup containing only third-party checks can satisfy a bare non-empty
   check test while nothing required to merge has run. `ready` therefore needs both the non-empty
   all-terminal rollup and GitHub's `CLEAN` merge-state signal. A green is merge-eligible only once
   the tracked chat has **settled** (Phase 3b), never because a `checks_passed` wake arrived.

3. **Dedup — delivery is at-least-once.** Callbacks may deliver more than once. Guard every transition
   on real state (step 2) and on the callback id: a wake whose reconciliation shows the action already
   happened (child already merged, already failing-and-repairing, chat not yet settled) is a **no-op**.
   Never take an irreversible action (merge, fail-isolate, progress-comment claim) purely because a
   wake arrived.

4. **Re-arm while the child is still in flight.** A one-shot watch is consumed when it fires. Re-arm
   a trigger only when the reconcile in step 2 just read that trigger's condition as **false**.
   Re-arming a trigger whose condition still holds fires it immediately and burns the watch. When a
   trigger is skipped for this reason, record the skip by name, and state that the bounded
   `policy.fallbackPoll` is the sole wait mechanism for that trigger until a later reconcile reads its
   condition as false; arm it on that later reconcile. On `could-not-evaluate`, arm nothing and keep
   polling. This guard is inherently racy — the condition can flip between the read and the arm — but
   the consequence is bounded: a spurious immediate fire is absorbed by the dedup rule above, and the
   fallback poll still covers the wait. Use the same verified target to list the child repository's
   watches, then only re-arm missing, safe-to-arm triggers (avoids duplicate watches):

   ```bash
   CALLBACK_CHAT="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.chatId === "string" ? target.chatId : "")
     '
   )"
   CALLBACK_REPO="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.repo === "string" ? target.repo : "")
     '
   )"
   if [ -n "$CALLBACK_CHAT" ] && [ -n "$CALLBACK_REPO" ]; then
     LIVE_WATCHES="$(boss callback list --chat "$CALLBACK_CHAT" --repo "$CALLBACK_REPO" --json)"
     # For each missing trigger T, use the same scoped registration shape:
     boss callback add "$PR" "$T" --group "epicwait-$PR-$T" --message "$MSG" --expires-in 24h --chat "$CALLBACK_CHAT" --repo "$CALLBACK_REPO" --json
   fi
   ```

5. **Bounded fallback poll.** Whether or not watches are armed, back the wait with the bounded
   `policy.fallbackPoll` (`gh pr checks "$PR" --watch --fail-fast`) and the Phase 3 poll cadence
   (every 2–5 minutes), driven by a session cron — see the wait recipe below. When callbacks are
   available it is a safety net for a missed/expired delivery; when `callbacksAvailable` is false it
   is the sole wait mechanism. Keep it bounded by the per-ticket wall clock and repair-round caps so
   the loop never blocks unboundedly.

6. **Clean up when a child settles.** When a child leaves `inFlight` (folded into `merged` or
   `failed`), first obtain the group's ids from the prior scoped list, then remove each returned id
   with its chat scope (the generic CLI has no `remove --repo`):

   ```bash
   CALLBACK_CHAT="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.chatId === "string" ? target.chatId : "")
     '
   )"
   CALLBACK_REPO="$(
     CALLBACK_TARGET_JSON="$CALLBACK_TARGET_JSON" node --input-type=module -e '
       const target = JSON.parse(process.env.CALLBACK_TARGET_JSON ?? "null")
       process.stdout.write(typeof target?.repo === "string" ? target.repo : "")
     '
   )"
   if [ -n "$CALLBACK_CHAT" ] && [ -n "$CALLBACK_REPO" ]; then
     LIVE_WATCHES="$(boss callback list --chat "$CALLBACK_CHAT" --repo "$CALLBACK_REPO" --json)"
     # For each CALLBACK_ID returned in LIVE_WATCHES for this group's child PR:
     boss callback remove "$CALLBACK_ID" --chat "$CALLBACK_CHAT"
   fi
   ```

   Or let `--expires-in` reap them. Stale watches are harmless (their next fire just triggers another
   reconcile that finds nothing to do) but tearing them down avoids spurious wakes on a later run.

## The wait recipe: how a session-hosted driver actually sleeps

Phase 3 says "poll every 2–5 minutes". A driver hosted inside an agent session has no reliable way to
_sleep_ across that interval on its own — the turn ends. Use these two mechanisms, in this order.

**1. Primary — per-PR draft-aware callbacks.** Armed as above, they deliver a prompt into the driver
chat the moment a child's PR reaches `checks_passed_ready` / `checks_failed` / `merged`. This is the
low-latency path and the only one that costs nothing while nothing is happening.

**2. Fallback — a session cron.** Register a scheduled prompt (whatever the host calls a recurring
job / scheduled prompt) that re-enters the Phase 3b poll cycle on a 2–5 minute cadence. It covers a
missed or expired delivery, and it is the sole wait mechanism when `callbacksAvailable(env)` is false.
Two caveats, both learned the hard way:

- **Step syntax may be rejected.** Not every host accepts `*/N` in the minute field. If `*/7` is
  refused, **enumerate the minutes** instead: `4,11,18,25,32,39,46,53 * * * *`.
- **Avoid the herd minutes.** Do not schedule on `:00` or `:30` — every naively-configured job in the
  world fires there. Offset by a few minutes (the `4,11,18,…` set above is already offset).

Tear the cron down when Phase 4 posts the final report unless a fail-isolated session is still live;
in that case keep only the cron/callback teardown needed to observe or resume it, and remove settled
child watches. A cron outliving a fully settled run wakes a driver with no epic to schedule.

**3. Anti-pattern — backgrounded watchers.** Do **not** hold the wait with a backgrounded shell loop
(`… &`, a host's "run this in the background" affordance, `sleep` / `while true` polling). Session
hosts may reap such
processes within the turn that spawned them, and the failure is **silent**: the driver believes a
watcher is running, no wake ever arrives, and the epic stalls until its wall clock expires. Callbacks
and the cron are the only two mechanisms that survive the end of a turn.

**4. Every wake runs the same cycle.** Callback wake, cron tick, or manual re-invocation — the driver
re-runs the identical idempotent Phase 3b reconciliation and never branches on _why_ it woke. That is
what makes the two mechanisms safe to run at once: a duplicate wake is a no-op, not a double action.

## Invariants

- **Reconcile before act, always** (`policy.reconcileBeforeAct`). No merge, repair, or fail-isolate is
  driven by a callback trigger name alone — only by the Phase 3b authoritative state read.
- **Draft-aware green only.** Arm `checks_passed_ready`, never bare `checks_passed`, on a child PR
  opened as a draft — a green-on-draft fire consumes the one-shot watch for nothing.
- **The wait survives the turn.** Callbacks and a session cron are the only two wait mechanisms;
  a backgrounded watcher or sleep loop may be killed within its turn and stalls the epic silently.
- **Idempotent under duplicate/late delivery** (`policy.dedupById`). Re-delivery is a no-op.
- **Group only mutually exclusive triggers.** Sharing a group across triggers that can both hold
  makes the first fire cancel a still-needed watch.
- **Graceful degradation gated on `callbacksAvailable`.** Gate false ⇒ skip `registerWatch`, use
  `fallbackPoll` — an explicit no-op, never a failed wait. The gate, not a runtime CLI failure,
  decides. The gate **verifies the `boss` executable** (an existing executable file, by stat) as
  well as the managed-session variable, and a false gate **reports its reason**
  (`callbacksUnavailableReason(env)`) so a fallback poll is always explained.
- **Project-agnostic.** Only the generic `boss callback` interface and `gh` are named; no host- or
  tracker-specific identifiers appear here.

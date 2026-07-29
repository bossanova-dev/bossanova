# One-shot CI/PR callback watches (per in-flight child)

Read this when the scheduling loop (Phase 3) is about to wait on an in-flight child's CI checks or
merge/close state. It replaces a naked blocking poll with a **one-shot GitHub callback** armed per
in-flight child, so the epic wakes the moment a child's CI resolves or its PR merges/closes — while
keeping the poll as a bounded fallback and authoritative reconciliation as the only thing that
actually decides. This is the boss-epic parity of boss-build's `references/callback-watches.md`.

**First decide whether callbacks are usable at all.** `callbacksAvailable(env)`
(`toolbox/callback/adapter.mjs`, keyed on `BOSS_SESSION_ID`) is the single "callbacks usable" gate.
When it is **false**, **skip `registerWatch` entirely and let `policy.fallbackPoll` alone drive
Phase 3** — the clean, documented no-op below, never a failed wait. When it is **true**, arm a group
per in-flight child as described. It is an up-front check, not a "did the CLI happen to fail at
runtime" guess, and the one place to extend if the usability signal ever diverges from raw in-boss.

The capability contract is the callback-notifier adapter (`toolbox/callback/adapter.mjs`, default
`CALLBACK=boss`). The boss reference (`toolbox/callback/boss.mjs`) maps three capabilities onto the
generic `boss callback` CLI and carries the watch policy:

| Capability      | `boss callback` command | Purpose                                                       |
| --------------- | ----------------------- | ------------------------------------------------------------- |
| `registerWatch` | `boss callback add`     | Arm a one-shot watch for one trigger (grouped per child).     |
| `listWatches`   | `boss callback list`    | Reconciliation read: enumerate live watches, dedup by id.     |
| `removeWatch`   | `boss callback remove`  | Tear a stale/duplicate watch down by id when a child settles. |

`policy.defaultExpiresIn` = `24h`. `policy.fallbackPoll` = `gh pr checks --watch --fail-fast`.

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

1. **Arm per child on flight entry** (gate `callbacksAvailable` true). When a child enters `inFlight`
   (Phase 3a launch), register the three triggers against that child's PR as a single **group** so the
   first to fire cancels its siblings — the epic wakes exactly once whether the child's CI went green,
   went red, or its PR merged out from under it. The `--message` is the wake payload (a secret — never
   echoed by `list`); `--expires-in` bounds the watch so an abandoned run's watch self-expires. Record
   the returned callback/group ids alongside the child's `session_id` / `chat_id` for re-arm and
   cleanup.

   ```bash
   PR="$CHILD_PR_NUMBER"
   MSG="boss-epic: CI/PR state changed for child PR #$PR — reconcile and continue scheduling."
   # checks_passed_ready, NOT checks_passed: the child PR is a draft until boss-build
   # readies it, and a bare green-on-draft fire would burn the one-shot watch.
   for T in checks_passed_ready checks_failed merged; do
     boss callback add "$PR" "$T" --group "epicwait-$PR" --message "$MSG" --expires-in 24h --json
   done
   ```

   When `callbacksAvailable(env)` is false (no daemon behind the `boss callback` interface), **skip
   registration**; the poll below is the sole wait mechanism. Consult the gate before arming rather
   than discovering unavailability from a CLI error. If a `registerWatch` call nonetheless errors at
   runtime under a **true** gate (e.g. an older host that lacks the `boss callback` subcommand), treat
   it exactly like the gate-false path — skip and let `policy.fallbackPoll` alone drive Phase 3, never
   a failed wait.

2. **Reconcile on wake — a callback is a nudge, not a verdict.** A wake (or a poll return) means
   _re-run the Phase 3b reconciliation_, not _act on the trigger name_. Read the child's real
   session/PR state (`get_session`, `list_check_snapshots`, `get_chat_statuses`, `gh pr view`) and
   decide from that — a green is merge-eligible only once the tracked chat has **settled** (Phase 3b),
   never because a `checks_passed` wake arrived.

3. **Dedup — delivery is at-least-once.** Callbacks may deliver more than once. Guard every transition
   on real state (step 2) and on the callback id: a wake whose reconciliation shows the action already
   happened (child already merged, already failing-and-repairing, chat not yet settled) is a **no-op**.
   Never take an irreversible action (merge, fail-isolate, progress-comment claim) purely because a
   wake arrived.

4. **Re-arm while the child is still in flight.** A one-shot watch is consumed when it fires. If
   reconciliation says the child must keep running (woke on an intermediate signal, or a repair round
   is in progress and the next CI run is pending), re-register the child's group before blocking again.
   Use `boss callback list` to see which triggers are still live and only re-arm the missing ones
   (avoids duplicate watches).

5. **Bounded fallback poll.** Whether or not watches are armed, back the wait with the bounded
   `policy.fallbackPoll` (`gh pr checks "$PR" --watch --fail-fast`) and the Phase 3 poll cadence
   (every 2–5 minutes), driven by a session cron — see the wait recipe below. When callbacks are
   available it is a safety net for a missed/expired delivery; when `callbacksAvailable` is false it
   is the sole wait mechanism. Keep it bounded by the per-ticket wall clock and repair-round caps so
   the loop never blocks unboundedly.

6. **Clean up when a child settles.** When a child leaves `inFlight` (folded into `merged` or
   `failed`), remove its group's live watches where practical (`boss callback remove <id>`, or let
   `--expires-in` reap them). Stale watches are harmless (their next fire just triggers another
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

Tear the cron down when Phase 4 posts the final report; a cron outliving its run wakes a driver with
no epic to schedule.

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
- **Graceful degradation gated on `callbacksAvailable`.** Gate false ⇒ skip `registerWatch`, use
  `fallbackPoll` — an explicit no-op, never a failed wait. The gate, not a runtime CLI failure,
  decides.
- **Project-agnostic.** Only the generic `boss callback` interface and `gh` are named; no host- or
  tracker-specific identifiers appear here.

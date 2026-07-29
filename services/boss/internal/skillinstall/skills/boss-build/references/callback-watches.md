# One-shot CI/PR callback watches

Read this when a step blocks on a PR's CI checks or merge/close state (Step 8's green gate, Step 9's
re-inject wait). It replaces a naked blocking poll with a **one-shot GitHub callback** that wakes the
run the moment CI resolves or the PR merges/closes — while keeping the poll as a bounded fallback and
authoritative reconciliation as the only thing that actually decides.

**First decide whether callbacks are usable at all.** `callbacksAvailable(env)`
(`toolbox/callback/adapter.mjs`, keyed on `BOSS_SESSION_ID`) is the single "callbacks usable" gate.
When it is **false** (a standalone run with no daemon behind the `boss callback` interface), **skip
`registerWatch` entirely and use `fallbackPoll`** — the clean, documented no-op below, never a failed
wait. When it is **true**, arm the group as described. This is an up-front check, not a "did the CLI
happen to fail at runtime" guess, and it is the one place to extend if the usability signal ever
diverges from raw in-boss.

The capability contract is the callback-notifier adapter (`toolbox/callback/adapter.mjs`, default
`CALLBACK=boss`). The boss reference (`toolbox/callback/boss.mjs`) maps three capabilities onto the
generic `boss callback` CLI and carries the watch policy:

| Capability      | `boss callback` command | Purpose                                                       |
| --------------- | ----------------------- | ------------------------------------------------------------- |
| `registerWatch` | `boss callback add`     | Arm a one-shot watch for one trigger (grouped with siblings). |
| `listWatches`   | `boss callback list`    | Reconciliation read: enumerate live watches, dedup by id.     |
| `removeWatch`   | `boss callback remove`  | Tear a stale/duplicate watch down by id when the wait ends.   |

`policy.watchTriggers` = `checks_passed`, `checks_failed`, `merged`. `policy.defaultExpiresIn` = `24h`.
`policy.fallbackPoll` = `gh pr checks --watch --fail-fast`.

## Protocol

1. **Arm on wait entry.** When a step begins waiting on CI/PR state, register the three triggers as a
   single **group** so the first to fire cancels its siblings — the run wakes exactly once whether CI
   went green, went red, or the PR merged out from under it. The `--message` is the wake payload
   (a secret — never echoed by `list`); `--expires-in` bounds the watch so an abandoned run's watch
   self-expires. Record the returned callback/group ids in your working notes for re-arm and cleanup.

   ```bash
   PR="$PR_NUMBER"
   MSG="boss-build: CI/PR state changed for PR #$PR — reconcile and continue."
   for T in checks_passed checks_failed merged; do
     boss callback add "$PR" "$T" --group "buildwait-$PR" --message "$MSG" --expires-in 24h --json
   done
   ```

   When `callbacksAvailable(env)` is false (no daemon behind the `boss callback` interface), **skip
   registration and fall through to the bounded poll** — never fail the wait because callbacks are
   missing. Consult the gate before arming rather than discovering unavailability from a CLI error. If
   a `registerWatch` call nonetheless errors at runtime under a **true** gate (e.g. an older host that
   lacks the `boss callback` subcommand), treat it exactly like the gate-false path — skip and rely on
   the bounded poll below, never a failed wait.

2. **Reconcile on wake — a callback is a nudge, not a verdict.** A wake (or a poll return) means
   _look_, not _act_. Query real state before changing course:

   ```bash
   gh pr checks "$PR" --json name,state -q '[.[]|{name,state}]'   # actual check rollup
   gh pr view  "$PR" --json state,isDraft,mergedAt,mergeStateStatus
   ```

   Decide from that real state, not from the trigger name: green + mergeable → continue to ready;
   red → route back to **Step 8 (boss-repair)**; `merged`/`closed` → the PR left this workflow's
   hands, stop via **Stop cleanly** without re-opening or re-pushing.

3. **Dedup — delivery is at-least-once.** Callbacks may deliver more than once. Guard every state
   change on real PR state (step 2) and on the callback id: a wake whose reconciliation shows the
   action already happened (PR already ready, already merged, checks already green) is a **no-op**.
   Never take an irreversible action (ready, comment, status move) purely because a wake arrived.

4. **Re-arm while still waiting.** A one-shot watch is consumed when it fires. If reconciliation says
   the wait must continue (e.g. woke on a partial/intermediate signal, or repaired red and are
   waiting for the next CI run), re-register the group before blocking again. Use `boss callback list`
   to see which triggers are still live and only re-arm the missing ones (avoids duplicate watches).

5. **Bounded fallback poll.** Whether or not a watch is armed, back the wait with a bounded
   `gh pr checks "$PR" --watch --fail-fast`. When callbacks are available it is a safety net for a
   missed/expired delivery; when they are not, it is the sole wait mechanism. Keep it bounded (the
   wall-clock breaker in Preflight, plus the Step 8 `policy.repairCap` and Step 10 `policy.settleCap`)
   so the run never blocks unboundedly.

6. **Clean up on wait exit.** When the wait phase ends (green + readied, or routed to BLOCKED/Stop),
   remove the group's live watches where practical (`boss callback remove <id>`, or let
   `--expires-in` reap them). Stale watches are harmless (their next fire just triggers another
   reconcile that finds nothing to do) but leaving them tidy avoids spurious wakes on a later run.

## Invariants

- **Reconcile before act, always** (`policy.reconcileBeforeAct`). No terminal action is driven by a
  callback trigger name alone.
- **Idempotent under duplicate/late delivery** (`policy.dedupById`). Re-delivery is a no-op.
- **Graceful degradation gated on `callbacksAvailable`.** Gate false ⇒ skip `registerWatch`, use
  `fallbackPoll` — an explicit no-op, never a failed wait. The gate, not a runtime CLI failure,
  decides.
- **Project-agnostic.** Only the generic `boss callback` interface and `gh` are named; no host- or
  tracker-specific identifiers appear here.

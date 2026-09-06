# One-shot CI/PR callback watches

Read this when a step blocks on a PR's CI checks or merge/close state (Step 8's green gate, Step 9's
re-inject wait).

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

**Run the executable the gate resolved, not a bare `boss`.** `resolveBossBinary(env)` returns the
winning candidate as `path`. Bind that `path` once and use `"$BOSS"` in place of the literal `boss`
at every call site below; the snippets spell `boss` only because that is the common case.

```bash
# Each fenced block is a FRESH shell, so re-resolve the toolbox rather than assuming
# the startup block's export survived.
BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
export BOSS_BUILD_TOOLBOX
BOSS="$(
  node --input-type=module -e '
    import{pathToFileURL as u}from"node:url"
    const { resolveBossBinary } = await import(u(process.env.BOSS_BUILD_TOOLBOX+"/boss-binary.mjs").href)
    process.stdout.write(resolveBossBinary().path ?? "")
  '
)"
# Empty means the gate is false OR this bridge failed — never read one as the other.
# Under a true gate an empty result is a bug worth logging, with the resolver's `reason`.
[ -n "$BOSS" ] || BOSS=boss
```

**The gate is deliberately CLI-only.** A host that exposes the callback capabilities only over an
MCP tool surface, and not over the `boss callback` CLI, is treated as callbacks-unavailable and
polls.

When the gate is **false**, **skip `registerWatch` entirely and use `fallbackPoll`** — the clean,
documented no-op below, run as Protocol step 5's bounded loop and never as a bare `--watch`, never a
failed wait — and **report why**: `callbacksUnavailableReason(env)` returns the failing conjunct
(`BOSS_SESSION_ID is unset`, or the binary rejection naming `BOSS_BIN`/`PATH`/`./bin/boss`). When it
is **true**, arm the per-trigger watches as described.

The capability contract is the callback-notifier adapter (`toolbox/callback/adapter.mjs`, default
`CALLBACK=boss`). The boss reference (`toolbox/callback/boss.mjs`) maps three capabilities onto the
generic `boss callback` CLI and carries the watch policy:

| Capability      | `boss callback` command | Purpose                                                                       |
| --------------- | ----------------------- | ----------------------------------------------------------------------------- |
| `registerWatch` | `boss callback add`     | Arm a one-shot watch for one trigger; group only mutually exclusive triggers. |
| `listWatches`   | `boss callback list`    | Reconciliation read: enumerate live watches, dedup by id.                     |
| `removeWatch`   | `boss callback remove`  | Tear a stale/duplicate watch down by id when the wait ends.                   |

`policy.availableTriggers` is the CLI vocabulary: `merged`, `closed`, `checks_passed`,
`checks_failed`, `ready_for_review`, `checks_passed_ready`. `policy.watchTriggers` is the default
wait set: `checks_passed`, `checks_failed`, `merged`. `policy.defaultExpiresIn` = `24h`.
`policy.fallbackPoll` = `gh pr checks --watch --fail-fast`. That value names the **command** the
adapter reports, not the wait protocol, and the raw form has **no timeout of its own** — so never
run it unwrapped. Run the fallback poll as the bounded loop in Protocol step 5, which is the only
form this reference sanctions; every `fallbackPoll` mention below means that loop.

## Protocol

1. **Arm on wait entry.** When a step begins waiting on CI/PR state, register the three triggers with
   separate per-trigger groups. Group two triggers only when at most one of them can ever be
   satisfied for a given PR, such as `merged` versus `closed`; triggers that can be satisfied at
   different times or re-satisfied after a push each need their own group. The `--message` is the
   wake payload (a secret — never echoed by `list`); `--expires-in` bounds the watch so an abandoned
   run's watch self-expires. Record the returned callback/group ids in your working notes for re-arm
   and cleanup.

   ```bash
   PR="$PR_NUMBER"
   MSG="boss-build: CI/PR state changed for PR #$PR — reconcile and continue."
   BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}"
   if [ ! -d "$BOSS_SKILLS_HOME/boss-build/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi
   BOSS_BUILD_TOOLBOX="$BOSS_SKILLS_HOME/boss-build/toolbox"
   export BOSS_BUILD_TOOLBOX
   WATCH_TRIGGERS="$(
     node --input-type=module -e '
       import{pathToFileURL as u}from"node:url"
       const {resolveCallbackAdapter}=await import(u(process.env.BOSS_BUILD_TOOLBOX+"/callback/adapter.mjs").href)
       process.stdout.write(resolveCallbackAdapter(process.env).policy.watchTriggers.join(" "))
     '
   )"
   for T in $WATCH_TRIGGERS; do
     boss callback add "$PR" "$T" --group "buildwait-$PR-$T" --message "$MSG" --expires-in 24h --json
   done
   ```

   When `callbacksAvailable(env)` is false, **skip registration and fall through to the bounded
   poll** — never fail the wait because callbacks are missing. If a `registerWatch` call nonetheless
   errors at runtime under a **true** gate (e.g. an older host that lacks the `boss callback`
   subcommand), treat it exactly like the gate-false path — skip and rely on the bounded poll below,
   never a failed wait.

2. **Reconcile on wake — a callback is a nudge, not a verdict.** A wake (or a poll return) means
   _look_, not _act_. Query real state before changing course:

   ```bash
   gh pr checks "$PR" --json name,state -q '[.[]|{name,state}]'   # actual check rollup
   gh pr view  "$PR" --json state,isDraft,mergedAt,mergeStateStatus
   ```

   Decide from that real state, not from the trigger name, and record which conjunct declined:

   - `ready` — the rollup was read successfully, is non-empty, every entry is terminal, none failed,
     the PR is open and not a draft, and `mergeStateStatus == "CLEAN"`.
   - `not-yet` — the read succeeded, but one of the `ready` conjuncts is false. Keep waiting unless
     the PR is red or left this workflow's hands.
   - `could-not-evaluate` — the rollup could not be read, the rollup was empty, or
     `mergeStateStatus` is `UNKNOWN`/unreadable. Report this outcome by name; never fold it into
     `not-yet`, and never let it reach `ready`.

   A check count of zero is not a pass. An empty commit that skips CI can produce a head SHA with no
   merge workflow runs; a rollup containing only third-party checks can satisfy a bare non-empty
   check test while nothing required to merge has run. `ready` therefore needs both the non-empty
   all-terminal rollup and GitHub's `CLEAN` merge-state signal. Red routes back to **Step 8
   (boss-repair)**; `merged`/`closed` means the PR left this workflow's hands, so stop via
   **Stop cleanly** without re-opening or re-pushing.

3. **Dedup — delivery is at-least-once.** Callbacks may deliver more than once. Guard every state
   change on real PR state (step 2) and on the callback id: a wake whose reconciliation shows the
   action already happened (PR already ready, already merged, checks already green) is a **no-op**.
   Never take an irreversible action (ready, comment, status move) purely because a wake arrived.

4. **Re-arm while still waiting.** A one-shot watch is consumed when it fires. Re-arm a trigger only
   when the reconcile in step 2 just read that trigger's condition as **false**. Re-arming a trigger
   whose condition still holds fires it immediately and burns the watch. When a trigger is skipped
   for this reason, record the skip by name, and state that the bounded `policy.fallbackPoll` is the
   sole wait mechanism for that trigger until a later reconcile reads its condition as false; arm it
   on that later reconcile. On `could-not-evaluate`, arm nothing and keep polling.
   Use `boss callback list` to see which triggers are still live and only re-arm the
   missing, safe-to-arm ones (avoids duplicate watches).
   State-matching is the default. When a consumer needs a new unsatisfied → satisfied edge instead,
   pass the CLI's `--on-transition` flag through the adapter's `onTransition` argument.

5. **Bounded fallback poll — and `--watch` is not the bound.** Whether or not a watch is armed, back
   the wait with the bounded poll below. When callbacks are available it is a safety net for a
   missed/expired delivery; when they are not, it is the sole wait mechanism.

   Do **not** reach for `gh pr checks "$PR" --watch --fail-fast` here. It has **no timeout of its
   own**; the bound has to come from the caller, which is what this loop is:

   ```bash
   CI_WAIT_ATTEMPTS=${CI_WAIT_ATTEMPTS:-60}     # outer cap on READS, not on wall time — see below
   CI_WAIT_INTERVAL=${CI_WAIT_INTERVAL:-30}     # seconds between reads
   CI_WAIT_STATE=timeout
   i=0
   while [ "$i" -lt "$CI_WAIT_ATTEMPTS" ]; do
     # Emit BOTH the node's `status` and its conclusion/state: `.status` is what makes an
     # in-progress node visible, and `"UNKNOWN"` keeps an unreadable node out of an empty token.
     ROLLUP=$(gh pr view "$PR" --json statusCheckRollup -q \
       '[.statusCheckRollup[]|(.status//empty),(.conclusion//.state//"UNKNOWN")]|join(" ")' \
       2>/dev/null) || ROLLUP=""
     case "$ROLLUP" in
       "")                                   : ;;                       # unreadable: keep waiting
       *PENDING*|*IN_PROGRESS*|*QUEUED*|*EXPECTED*|*REQUESTED*|*WAITING*) : ;;
       *FAILURE*|*ERROR*|*CANCELLED*|*TIMED_OUT*|*ACTION_REQUIRED*) CI_WAIT_STATE=failed; break ;;
       *UNKNOWN*)                            CI_WAIT_STATE=unknown; break ;;
       *SUCCESS*)                            CI_WAIT_STATE=settled; break ;;
       *)                                    CI_WAIT_STATE=unknown; break ;;
     esac
     i=$(( i + 1 ))
     sleep "$CI_WAIT_INTERVAL"
   done
   ```

   Two reasons this reads the rollup rather than filtering `gh pr checks` output: that output has
   summary header lines a filter misreads as check rows, and `gh pr checks` collapses same-named
   checks, so a `FAILURE` can hide behind a `SUCCESS` of the same name. Read the rollup states.

   **The catch-all is fail-CLOSED, and that is the point.** Read the arms as what they are: they
   match the **whole joined string**, in order, not each node separately. So a rollup reaches
   `settled` exactly when it carries a `SUCCESS` token and no pending, failure or `UNKNOWN` token —
   the pending arm is matched first, so a single still-running node holds the whole rollup at
   _keep waiting_ however many `SUCCESS` tokens sit beside it — and it still does when other nodes
   are `SKIPPED`, `NEUTRAL` or `STALE` beside that `SUCCESS`. What fails closed is the shape with
   **no `SUCCESS` at all** — an all-`SKIPPED` or all-`NEUTRAL` set where no gate ran, a state this
   list does not classify, a node whose state was unreadable — every one of which lands on
   `unknown`. Route `timeout` and `unknown` **exactly as each other**: neither is green, neither may
   satisfy a green-branch check, and both take the same unknown route a missing reading takes.

   **What the outer cap bounds, stated honestly.** `CI_WAIT_ATTEMPTS` x `CI_WAIT_INTERVAL` bounds the
   **sleeping**, not the whole wait: each `gh` read is itself unbounded, so the true worst case is
   that product **plus** the time those reads take, and a single hung read still blocks this loop
   indefinitely. At the shipped defaults the sleeping is 60 x 30s = 30 minutes. Where `timeout` /
   `gtimeout` is present, wrapping the `gh` call (`timeout 60 gh pr view …`) is what closes that
   residual; it is absent on a stock macOS, so this loop must not be described as a hard wall-clock
   bound on hosts that lack it.

   On `CI_WAIT_STATE=timeout` the wait expired with the checks still unsettled; on
   `CI_WAIT_STATE=unknown` a read landed on a rollup this loop will not call green. **Both are
   unknown, never green, and both route identically**: do not ready the PR, do not report green, and
   do not silently loop again. Route them the way the caller's step routes an unreadable check set —
   Step 8 to `policy.repairCap`, Step 10 to `policy.settleCap`, and a cap reached with the state
   still unknown to BLOCKED naming the PR and the elapsed allowance. Only `settled` is green and only
   `failed` is red; treating either unknown state as a third, softer outcome is how a fail-open green
   re-enters through the consumer after the poll itself was made fail-closed.

6. **Clean up on wait exit.** When the wait phase ends (green + readied, or routed to BLOCKED/Stop),
   remove every live watch returned by the scoped list where practical (`boss callback remove <id>`,
   or let `--expires-in` reap them).

## Invariants

- **Reconcile before act, always** (`policy.reconcileBeforeAct`). No terminal action is driven by a
  callback trigger name alone.
- **Idempotent under duplicate/late delivery** (`policy.dedupById`). Re-delivery is a no-op.
- **Group only mutually exclusive triggers.** Sharing a group across triggers that can both hold
  makes the first fire cancel a still-needed watch.
- **Graceful degradation gated on `callbacksAvailable`.** Gate false ⇒ skip `registerWatch`, use
  `fallbackPoll` — an explicit no-op, never a failed wait. The gate, not a runtime CLI failure,
  decides, and a false gate **reports its reason** (`callbacksUnavailableReason(env)`) so a fallback
  poll is always explained.
- **Project-agnostic.** Only the generic `boss callback` interface and `gh` are named; no host- or
  tracker-specific identifiers appear here.

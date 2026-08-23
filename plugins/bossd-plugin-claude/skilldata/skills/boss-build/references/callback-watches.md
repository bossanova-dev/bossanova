# One-shot CI/PR callback watches

Read this when a step blocks on a PR's CI checks or merge/close state (Step 8's green gate, Step 9's
re-inject wait). It replaces a naked blocking poll with a **one-shot GitHub callback** that wakes the
run the moment CI resolves or the PR merges/closes — while keeping the poll as a bounded fallback and
authoritative reconciliation as the only thing that actually decides.

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
leaving the CLI off its `PATH`, and a session-variable-only gate then reports callbacks usable, arms
a registration that cannot run, and falls back to polling with the attempt already burned.

**Run the executable the gate resolved, not a bare `boss`.** `resolveBossBinary(env)` returns the
winning candidate as `path`, and two of its three arms — an explicit `$BOSS_BIN` and the repo build
`./bin/boss` — resolve to something a bare `boss` in a shell would not find at all, or would find as
a **different** binary (`$BOSS_BIN` deliberately outranks `PATH`). So bind that `path` once and use
`"$BOSS"` in place of the literal `boss` at every call site below; the snippets spell `boss` only
because that is the common case. Taking the gate's verdict while discarding its `path` reintroduces
the same failure from the other side: a true gate followed by `command not found`.

```bash
# Each fenced block is a FRESH shell, so re-resolve the toolbox rather than assuming
# the startup block's export survived — unset, the bridge below imports
# `undefined/boss-binary.mjs`, dies, and hands back the bare `boss` this exists to avoid.
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

When the gate is **false**, **skip `registerWatch` entirely and use `fallbackPoll`** — the clean,
documented no-op below, never a failed wait — and **report why**: `callbacksUnavailableReason(env)`
returns the failing conjunct (`BOSS_SESSION_ID is unset`, or the binary rejection naming
`BOSS_BIN`/`PATH`/`./bin/boss`), so the log records "polling, because …" rather than degrading
silently. When it is **true**, arm the per-trigger watches as described. This is an up-front check,
not a "did the CLI happen to fail at runtime" guess.

The capability contract is the callback-notifier adapter (`toolbox/callback/adapter.mjs`, default
`CALLBACK=boss`). The boss reference (`toolbox/callback/boss.mjs`) maps three capabilities onto the
generic `boss callback` CLI and carries the watch policy:

| Capability      | `boss callback` command | Purpose                                                                       |
| --------------- | ----------------------- | ----------------------------------------------------------------------------- |
| `registerWatch` | `boss callback add`     | Arm a one-shot watch for one trigger; group only mutually exclusive triggers. |
| `listWatches`   | `boss callback list`    | Reconciliation read: enumerate live watches, dedup by id.                     |
| `removeWatch`   | `boss callback remove`  | Tear a stale/duplicate watch down by id when the wait ends.                   |

`policy.watchTriggers` = `checks_passed`, `checks_failed`, `merged`. `policy.defaultExpiresIn` = `24h`.
`policy.fallbackPoll` = `gh pr checks --watch --fail-fast`.

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
   for T in checks_passed checks_failed merged; do
     boss callback add "$PR" "$T" --group "buildwait-$PR-$T" --message "$MSG" --expires-in 24h --json
   done
   ```

   When `callbacksAvailable(env)` is false (no daemon behind the `boss callback` interface, or no
   resolvable `boss` executable — see the two conjuncts above), **skip
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
   on that later reconcile. On `could-not-evaluate`, arm nothing and keep polling. This guard is
   inherently racy — the condition can flip between the read and the arm — but the consequence is
   bounded: a spurious immediate fire is absorbed by the dedup rule above, and the fallback poll
   still covers the wait. Use `boss callback list` to see which triggers are still live and only
   re-arm the missing, safe-to-arm ones (avoids duplicate watches).

5. **Bounded fallback poll.** Whether or not a watch is armed, back the wait with a bounded
   `gh pr checks "$PR" --watch --fail-fast`. When callbacks are available it is a safety net for a
   missed/expired delivery; when they are not, it is the sole wait mechanism. Keep it bounded (the
   wall-clock breaker in Preflight, plus the Step 8 `policy.repairCap` and Step 10 `policy.settleCap`)
   so the run never blocks unboundedly.

6. **Clean up on wait exit.** When the wait phase ends (green + readied, or routed to BLOCKED/Stop),
   remove every live watch returned by the scoped list where practical (`boss callback remove <id>`, or let
   `--expires-in` reap them). Stale watches are harmless (their next fire just triggers another
   reconcile that finds nothing to do) but leaving them tidy avoids spurious wakes on a later run.

## Invariants

- **Reconcile before act, always** (`policy.reconcileBeforeAct`). No terminal action is driven by a
  callback trigger name alone.
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

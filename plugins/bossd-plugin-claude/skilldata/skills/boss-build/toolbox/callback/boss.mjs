// skills-toolbox/callback/boss.mjs
// Boss reference implementation of the callback-notifier-adapter interface.
// One-shot GitHub PR-event callbacks (register/list/remove a durable watch) over
// the generic `boss callback` CLI. Like the session-runner adapter
// (the session reference) this is DECLARATIVE: it records the `boss callback`
// sub-command + the flag/JSON-field shape for each capability rather than issuing
// the calls itself — the agent runs the CLI; the map is the single source of truth
// the SKILL prose reads. node builtins only (the cron worktree is dependency-free —
// mirrors the session reference).

// capability -> { command, args:[...], response:[...] } over `boss callback`.
export const bossCallbackOperationMap = {
  registerWatch: {
    command: 'boss callback add',
    // `<pr> <trigger>` are positional. --group is safe only for mutually exclusive
    // triggers, where at most one can ever hold for the PR, such as merged vs closed.
    // Non-exclusive waits such as green / red / merged use a separate group per
    // trigger so one state does not cancel the other still-needed watches.
    // A per-trigger group is DELIBERATE here, and the daemon cannot tell it apart
    // from the mistake it also produces: two mutually exclusive triggers armed
    // under different groups are two groups of one, so neither cancels the other
    // and the losing leg stays armed until it expires. The daemon warns on that
    // shape. Pass independentWatch (the CLI's --independent-watch) on a fan-out
    // that is meant to outlive its siblings, to record the intent and silence a
    // warning that is correct in general and wrong here.
    // Triggers are state-matched by default. Pass onTransition (the CLI's
    // --on-transition flag) only when the watch must be transition-matched.
    // --message is the wake payload delivered to the target chat; it is a SECRET —
    // never echoed back by `list`, so it is deliberately absent from every response
    // below. --expires-in bounds the durable watch; --repo/--chat scope it; --json
    // emits the stable githubCallbackJSON row.
    args: [
      'pr',
      'trigger',
      'group',
      'message',
      'expiresIn',
      'onTransition',
      'independentWatch',
      'repo',
      'chat',
      'json',
    ],
    // The generic CLI supports both scopes for registration. The caller must
    // use the verified target's chat and the child PR's repository together.
    scope: { chat: true, repo: true },
    response: ['id', 'group_id', 'trigger', 'state'],
  },
  listWatches: {
    command: 'boss callback list',
    // Reconciliation read: enumerate the live watches for this PR to dedup by id
    // and decide what still needs re-arming. The message body is intentionally NOT
    // a field here (it is a secret).
    args: ['repo', 'trigger', 'state', 'chat', 'json'],
    // List must use the same two scopes as registration before it is used for
    // reconciliation, re-arm decisions, or cleanup id discovery.
    scope: { chat: true, repo: true },
    response: ['id', 'group_id', 'pr_number', 'trigger', 'state', 'last_event'],
  },
  removeWatch: {
    command: 'boss callback remove',
    // Tear a stale or duplicate watch down by id when the wait phase ends.
    args: ['callbackId', 'chat'],
    // The generic CLI accepts --chat but deliberately has no --repo on remove.
    // Discover callback ids with the prior chat+repo scoped list, then remove
    // each returned id with this chat scope.
    scope: { chat: true, repo: false },
    response: [],
  },
}

// Callback-watch policy constants the spine references instead of magic values, so
// the register/reconcile/re-arm/fallback behaviour is named in one place rather than
// re-derived in prose. Frozen so a consumer can read but not mutate it.
export const bossCallbackPolicy = Object.freeze({
  // Every trigger accepted by the generic boss callback CLI, in its stable order.
  availableTriggers: Object.freeze([
    'merged',
    'closed',
    'checks_passed',
    'checks_failed',
    'ready_for_review',
    'checks_passed_ready',
  ]),
  // The default one-shot triggers registered when a PR/CI wait begins. Group only
  // mutually exclusive triggers; checks_passed, checks_failed, and merged are not
  // mutually exclusive over a PR's lifetime, so callers register them in separate groups.
  watchTriggers: Object.freeze(['checks_passed', 'checks_failed', 'merged']),
  // Draft-aware waits replace bare checks_passed with the merge-eligibility trigger.
  draftAwareTriggers: Object.freeze(['checks_passed_ready', 'checks_failed', 'merged']),
  // The draft-aware set an EPIC arms per in-flight child. It is draftAwareTriggers plus
  // `ready_for_review` (the un-draft flip itself, so the epic hears about the transition and not
  // only about the green that follows it) and `closed`, which an epic must treat as a terminal
  // failure path: a child PR closed without merging leaves a ticket that will never go green, and
  // a run without this trigger waits on it until the wall clock expires.
  epicChildTriggers: Object.freeze([
    'checks_passed_ready',
    'ready_for_review',
    'checks_failed',
    'merged',
    'closed',
  ]),
  // Bare `checks_passed` is NEVER the merge-ready signal for a draft-producing child. CI runs on
  // drafts, so it fires on the first green draft commit and burns the one-shot watch at a moment
  // that can never be merge-eligible. Named here so a caller asserts the prohibition rather than
  // re-deriving it from prose.
  forbiddenDraftTriggers: Object.freeze(['checks_passed']),
  // Closed-without-merge is a required failure path for an epic child (see epicChildTriggers), so
  // the policy says so explicitly rather than leaving each caller to decide.
  closedIsFailure: true,
  // Durable-watch lifetime; bounded so an abandoned run's watch self-expires rather
  // than lingering for the 30d hard cap.
  defaultExpiresIn: '24h',
  // Every callback wake is advisory: reconcile against real PR state (gh pr checks /
  // gh pr view) before changing course — a callback is a nudge, not a verdict.
  reconcileBeforeAct: true,
  // A one-shot watch is consumed when it fires; re-arm it only after reconciliation
  // reads that trigger's condition as false. A still-true state fires immediately
  // and burns the replacement watch.
  rearmWhileWaiting: true,
  // At-least-once delivery: dedup by callback id and guard every state change on real
  // state, so a duplicate delivery is a no-op.
  dedupById: true,
  // Bounded fallback when the callback interface is unavailable (no daemon, older
  // host). fallbackPoll's only sanctioned invocation is the consuming workflow's
  // Protocol step 5 bounded loop (or equivalent bounded watch loop); never run it unbounded.
  fallbackPoll: 'gh pr checks --watch --fail-fast',
})

/**
 * Build the boss reference callback-notifier adapter. Documents the `boss callback`
 * register/list/remove capabilities + the callback-watch policy; issues no calls.
 * @returns {{notifier: string, operationMap: object, policy: object}}
 */
export function createBossCallbackAdapter() {
  return {
    notifier: 'boss',
    operationMap: bossCallbackOperationMap,
    policy: bossCallbackPolicy,
  }
}

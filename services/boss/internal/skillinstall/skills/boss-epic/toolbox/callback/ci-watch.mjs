#!/usr/bin/env node

// ci-watch.mjs — the single agent-callable verdict on "is this PR's CI actually being observed".
//
// Arming a callback watch was a 100% prose obligation. The consuming cores carry correct hard
// rules telling a run to arm before it hands off, and their callback-watch protocol spells out
// five steps, and runs skipped all of it anyway: a run that armed nothing reached
// REVIEW_READY byte-identically to one that armed correctly, because nothing ever read the result.
// This module is the read. It answers one question — may this run stop looking at CI? — so the
// answer is a checked verdict rather than a remembered step.
//
// Five states. Exactly ONE of them blocks:
//
//   settled    — every required trigger's condition already holds. There is nothing left to watch.
//   watched    — a live watch covers every required trigger that is not already satisfied.
//   polled     — the bounded fallback poll is the observation mechanism. Absorbs every degrade:
//                callbacks unavailable, no verified target, an arm that did not take, and an
//                unreadable check state once the poll has run.
//   unwatched  — BLOCKING. Checks are still moving, callbacks are available, and a required trigger
//                is neither live nor satisfied. The run must arm before it may stop looking.
//   unknown    — the check state could not be evaluated AND the bounded poll has not run. Reported,
//                never blocking (see below).
//
// WHY `unknown` MUST NOT BLOCK. The protocol forbids arming on an unreadable state
// (the callback-watch protocol: "On could-not-evaluate, arm nothing and keep polling"). A blocking
// `unknown` therefore could not reach `watched` by arming, could not reach `settled` (checks are not
// terminal), and could not reach `polled` before the poll ran — an unbounded hang in a headless cron
// run at the exact moment PR state is unreadable. The same reference already rules that `timeout`
// and `unknown` route identically and that treating either as a third, softer outcome is the
// fail-open bug. So `unknown` is a label for the caller, never a gate.
//
// WHY SATISFACTION IS PER-TRIGGER. A global "checks are terminal, so stop watching" is wrong,
// because `merged` is not a check at all. Two failures it would cause, in both directions:
// a PR merged while a check is still in flight (merge queue, admin merge) reads pending, so a global
// gate arms `merged` into an already-true condition and burns the one-shot watch instantly; and
// checks green on an unmerged PR reads terminal, so a global gate stops looking while boss-epic is
// still waiting on the merge. Each trigger is judged against its own condition, and a trigger left
// unarmed BECAUSE its condition already holds is returned by name in `skippedTriggers` — which is
// what the protocol's step 4 already demands be recorded.
//
// Contract:
//   - Pure and offline. No network, no child processes, no `gh`, no `boss`. Callers pass in payloads
//     they already fetched, which is what makes this unit-testable from fixtures alone — the same
//     contract as the sibling `pr-check-state.mjs`.
//   - Node builtins only, so it runs from any installed skill toolbox in any repository.
//   - Every time comparison parses both sides to epoch milliseconds. A lexicographic compare of two
//     ISO strings written in different UTC offsets silently matches nothing, which is
//     indistinguishable from a real empty result.

import { readFileSync } from 'node:fs'

import { isMainModule } from '../main-module.mjs'

export const CI_WATCH_STATES = Object.freeze({
  SETTLED: 'settled',
  WATCHED: 'watched',
  POLLED: 'polled',
  UNWATCHED: 'unwatched',
  UNKNOWN: 'unknown',
})

// The one blocking state. Kept as an exported set rather than an inline comparison so a caller can
// assert the blocking surface rather than re-deriving it, and so widening it is a visible edit.
export const BLOCKING_STATES = Object.freeze([CI_WATCH_STATES.UNWATCHED])

export const CI_WATCH_REASONS = Object.freeze({
  ALL_SATISFIED: 'all-triggers-satisfied',
  LIVE_WATCHES: 'live-watches-cover-required-triggers',
  MISSING_WATCHES: 'required-triggers-unwatched',
  CALLBACKS_UNAVAILABLE: 'callbacks-unavailable',
  TARGET_UNVERIFIED: 'callback-target-unverified',
  ARM_FAILED: 'arm-failed-degraded-to-poll',
  UNREADABLE_POLLED: 'unreadable-check-state-polled',
  UNREADABLE: 'unreadable-check-state',
  // The caller asked "is CI observed" without naming what has to be observed. Its own verdict,
  // because the alternative is a universally-quantified predicate answered over an empty set: the
  // `watched` arm tests `missingTriggers.length === 0`, which is trivially true when nothing is
  // required, so "live watches cover the required triggers" held over nothing and the empty
  // `liveTriggers` beside it was the proof no watch had ever been read.
  NO_REQUIRED_TRIGGERS: 'no-required-triggers-supplied',
})

// The action the caller takes per verdict. Named so the skill prose can state one action per row and
// restate no rule of its own.
export const CI_WATCH_ACTIONS = Object.freeze({
  PROCEED: 'proceed',
  ARM: 'arm-then-reclassify',
  POLL: 'bounded-poll',
})

// `active` is the ONLY value that means "armed and waiting". The daemon's lifecycle is
// active -> leased -> triggered -> delivered, with canceled/expired terminal
// (lib/bossalib/models/github_callback.go). The delivery worker leases only from `triggered`, so a
// `leased` row is reachable only from one that ALREADY FIRED — a fired one-shot watch is not
// observation, and counting it would report `watched` over a PR nobody is watching.
const LIVE_STATE = 'active'

// Expiry is swept by the daemon at the top of every list read (ExpireOverdueCallbacks in
// services/bossd/internal/server/github_callback.go), so a listed `active` row is already guaranteed
// not overdue and this module never has to decide "is it expired" against its own clock. What
// remains is a different question the daemon cannot answer: will it still be alive when CI finishes.
// That is `minRemaining`.
//
// The floor is tied to the bounded poll's own documented budget (CI_WAIT_ATTEMPTS 60 ×
// CI_WAIT_INTERVAL 30s = 30 minutes) because that is this repo's stated idea of how long a CI wait
// runs. A LARGER floor is actively harmful, not merely conservative: it marks a watch with real
// runway as missing, the run arms a replacement, and the daemon rejects a co-satisfiable re-arm in
// the same group — so an inflated floor manufactures the very livelock the arm bound exists to stop.
const DEFAULT_MIN_REMAINING_MS = 30 * 60 * 1000

// Trigger -> the condition that makes a watch for it pointless because it already holds.
// `checks_passed_ready` is the merge-eligibility trigger: green AND out of draft AND still open.
const SATISFACTION = Object.freeze({
  checks_passed: (checks) => checks === 'green',
  checks_failed: (checks) => checks === 'failing',
  checks_passed_ready: (checks, pr) => checks === 'green' && pr.isDraft === false && pr.open,
  merged: (_checks, pr) => pr.merged,
  closed: (_checks, pr) => pr.open === false,
  ready_for_review: (_checks, pr) => pr.isDraft === false,
})

export function epochMs(value) {
  if (value === null || value === undefined || value === '') return null
  if (typeof value === 'number') return Number.isFinite(value) ? value : null
  const parsed = Date.parse(String(value))
  return Number.isNaN(parsed) ? null : parsed
}

function parseDurationMs(value, fallback) {
  if (value === null || value === undefined || value === '') return fallback
  if (typeof value === 'number') return Number.isFinite(value) ? value : fallback
  const match = /^(\d+)(ms|s|m|h|d)$/.exec(String(value).trim())
  if (!match) return fallback
  const scale = { ms: 1, s: 1000, m: 60000, h: 3600000, d: 86400000 }[match[2]]
  return Number(match[1]) * scale
}

// Normalise the PR half into the three facts the trigger table needs. Deliberately tolerant: a
// caller that cannot read the PR passes null, and every PR-derived trigger then reads UNSATISFIED —
// which routes toward arming or polling, never toward "nothing to watch".
function normalizePrView(prView) {
  if (!prView || typeof prView !== 'object') {
    return { merged: false, isDraft: null, open: true, known: false }
  }
  const state = typeof prView.state === 'string' ? prView.state.toUpperCase() : ''
  const merged = Boolean(prView.mergedAt) || state === 'MERGED'
  return {
    merged,
    isDraft: typeof prView.isDraft === 'boolean' ? prView.isDraft : null,
    open: state === '' ? !merged : state === 'OPEN',
    known: true,
  }
}

function normalizeWatches(watches) {
  if (!Array.isArray(watches)) return []
  return watches.filter((row) => row && typeof row === 'object')
}

/**
 * Is this row a live watch for THIS chat and THIS pull request, with enough runway left?
 *
 * Both scoping checks are load-bearing rather than defensive. `boss callback list` does not default
 * `--chat` (it filters only when the flag was explicitly passed) while `boss callback add` DOES
 * default the chat to $BOSS_AGENT_SESSION_ID — so an unscoped list returns other sessions' watches,
 * and without the chat check a peer's watch on the same PR would report this run as `watched` while
 * this chat gets no wake at all. The CLI also exposes no `--pr` filter, so a repo-scoped list
 * returns sibling PRs' watches and the pr_number check is the only thing keeping them out.
 */
function isLive(row, { targetChatId, prNumber, nowMs, minRemainingMs }) {
  if (row.state !== LIVE_STATE) return false
  if (targetChatId && row.target_chat_id !== targetChatId) return false
  if (prNumber !== null && Number(row.pr_number) !== Number(prNumber)) return false
  const expiresMs = epochMs(row.expires_at)
  if (expiresMs === null) return false
  return expiresMs - nowMs >= minRemainingMs
}

/**
 * Classify whether this run may stop looking at a PR's CI.
 *
 * @param {object} options
 * @param {{state?: string}|null} options.checkVerdict `classifyChecks` output from pr-check-state.mjs.
 * @param {{state?: string, isDraft?: boolean, mergedAt?: string|null}|null} options.prView
 *   `gh pr view --json state,isDraft,mergedAt`. Required for the `merged` trigger, which is not a check.
 * @param {boolean} options.callbacksAvailable `callbacksAvailable(env)` from ./adapter.mjs.
 * @param {string} [options.unavailableReason] `callbacksUnavailableReason(env)`, surfaced so a poll is explained.
 * @param {string} options.targetChatId The chat the wake must reach. Rows for other chats are invisible.
 * @param {number|string} options.prNumber The PR under observation.
 * @param {boolean} [options.targetVerified] boss-epic's `selectEpicCallbackTarget` result. False degrades to poll.
 * @param {Array<object>} options.watches Rows from `boss callback list --json`.
 * @param {string[]} options.requiredTriggers Usually `policy.watchTriggers` or `policy.draftAwareTriggers`.
 * @param {string|number} [options.now] Evaluation instant.
 * @param {string|number} [options.minRemaining] Runway floor, default 30m.
 * @param {boolean} [options.fallbackPollCompleted] Whether the bounded poll has already run.
 * @param {number} [options.armAttempts] How many times this run has already armed. >=1 forbids blocking.
 * @param {string|null} [options.lastArmError] Surfaced in `reason` when an arm did not take.
 * @returns {{state: string, reason: string, blocking: boolean, action: string,
 *            missingTriggers: string[], skippedTriggers: string[], liveTriggers: string[]}}
 */
export function classifyCiObservation(options = {}) {
  const {
    checkVerdict = null,
    prView = null,
    callbacksAvailable = false,
    unavailableReason = '',
    targetChatId = '',
    prNumber = null,
    targetVerified = true,
    watches = [],
    requiredTriggers = [],
    now = Date.now(),
    minRemaining = null,
    fallbackPollCompleted = false,
    armAttempts = 0,
    lastArmError = null,
  } = options

  const nowMs = epochMs(now) ?? Date.now()
  const minRemainingMs = parseDurationMs(minRemaining, DEFAULT_MIN_REMAINING_MS)
  const checks = typeof checkVerdict?.state === 'string' ? checkVerdict.state : 'unknown'
  const pr = normalizePrView(prView)
  const rows = normalizeWatches(watches)
  const required = Array.isArray(requiredTriggers) ? requiredTriggers.filter(Boolean) : []

  const live = new Set()
  for (const row of rows) {
    if (isLive(row, { targetChatId, prNumber, nowMs, minRemainingMs })) live.add(row.trigger)
  }

  const skippedTriggers = []
  const missingTriggers = []
  const liveTriggers = []
  for (const trigger of required) {
    const satisfied = SATISFACTION[trigger]?.(checks, pr) === true
    if (satisfied) skippedTriggers.push(trigger)
    else if (live.has(trigger)) liveTriggers.push(trigger)
    else missingTriggers.push(trigger)
  }

  const verdict = (state, reason, action) => ({
    state,
    reason,
    blocking: BLOCKING_STATES.includes(state),
    action,
    missingTriggers,
    skippedTriggers,
    liveTriggers,
  })

  // Answered nothing, so it says so. This arm sits ahead of every other — including the degrades —
  // because a degrade is a statement about the observation MECHANISM, and with no required trigger
  // there is no question for that mechanism to settle: `polled`/`proceed` over an empty required
  // set is the same invented permission in a different costume.
  //
  // It stays NON-BLOCKING, and deliberately: `unwatched` is this module's entire blocking surface,
  // and the header states at length why a blocking `unknown` hangs a headless run. The refusal
  // lands on `mayStopObserving` and on the CLI's non-zero exit instead — both of which have the
  // caller's own remedy immediately to hand, namely supplying the trigger list.
  if (required.length === 0) {
    return verdict(
      CI_WATCH_STATES.UNKNOWN,
      CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS,
      CI_WATCH_ACTIONS.POLL,
    )
  }

  // Nothing left to watch: every required trigger's own condition already holds.
  if (required.length > 0 && missingTriggers.length === 0 && liveTriggers.length === 0) {
    return verdict(
      CI_WATCH_STATES.SETTLED,
      CI_WATCH_REASONS.ALL_SATISFIED,
      CI_WATCH_ACTIONS.PROCEED,
    )
  }

  // Covered: whatever is not already satisfied has a live watch pointed at it.
  if (missingTriggers.length === 0) {
    return verdict(CI_WATCH_STATES.WATCHED, CI_WATCH_REASONS.LIVE_WATCHES, CI_WATCH_ACTIONS.PROCEED)
  }

  // Degrades, in precedence order. Each lands on the bounded poll, which the skills already sanction
  // as a clean documented mechanism rather than a failed wait.
  if (!callbacksAvailable) {
    const reason = unavailableReason
      ? `${CI_WATCH_REASONS.CALLBACKS_UNAVAILABLE}: ${unavailableReason}`
      : CI_WATCH_REASONS.CALLBACKS_UNAVAILABLE
    return verdict(CI_WATCH_STATES.POLLED, reason, CI_WATCH_ACTIONS.POLL)
  }
  if (!targetVerified) {
    return verdict(
      CI_WATCH_STATES.POLLED,
      CI_WATCH_REASONS.TARGET_UNVERIFIED,
      CI_WATCH_ACTIONS.POLL,
    )
  }

  // An unreadable check state cannot be armed against, so it polls rather than blocking. Before the
  // poll has run it is reported as `unknown` — still non-blocking, because the caller's next move is
  // the poll either way and a gate here would hang a headless run.
  if (checks === 'unknown') {
    return fallbackPollCompleted
      ? verdict(
          CI_WATCH_STATES.POLLED,
          CI_WATCH_REASONS.UNREADABLE_POLLED,
          CI_WATCH_ACTIONS.PROCEED,
        )
      : verdict(CI_WATCH_STATES.UNKNOWN, CI_WATCH_REASONS.UNREADABLE, CI_WATCH_ACTIONS.POLL)
  }

  // The arm bound. One arm, one re-classify, then degrade — a second attempt is provably useless:
  // either the first arm took (and the daemon rejects a co-satisfiable re-arm in the same group), or
  // it failed for a reason a retry will not change. Without this bound a classifier that says
  // `unwatched` while a watch genuinely exists loops forever.
  if (armAttempts >= 1) {
    const reason = lastArmError
      ? `${CI_WATCH_REASONS.ARM_FAILED}: ${lastArmError}`
      : CI_WATCH_REASONS.ARM_FAILED
    return verdict(CI_WATCH_STATES.POLLED, reason, CI_WATCH_ACTIONS.POLL)
  }

  return verdict(CI_WATCH_STATES.UNWATCHED, CI_WATCH_REASONS.MISSING_WATCHES, CI_WATCH_ACTIONS.ARM)
}

/**
 * May the caller stop looking at CI and print a terminal state?
 *
 * Keyed on the verdict's own `reason` before its `blocking` flag, in the same idiom the sibling
 * `provesGreenReason` uses to name a remedy ahead of the generic state it is an instance of. The
 * no-required-triggers verdict is non-blocking by design, so `blocking === false` alone would hand
 * this predicate exactly the permission the verdict exists to withhold.
 */
export function mayStopObserving(verdict) {
  if (verdict?.reason === CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS) return false
  return verdict?.blocking === false
}

function parseFlags(argv) {
  const flags = {}
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i]
    if (!arg.startsWith('--')) continue
    const name = arg.slice(2)
    const next = argv[i + 1]
    if (next === undefined || next.startsWith('--')) flags[name] = true
    else {
      flags[name] = next
      i += 1
    }
  }
  return flags
}

function readPayload(flags, name) {
  const value = flags[name]
  if (typeof value !== 'string' || value === '') return null
  let raw
  try {
    raw = value === '-' ? readFileSync(0, 'utf8') : readFileSync(value, 'utf8')
  } catch (err) {
    // An absent payload is an ABSENT INPUT, not a reason to abort with no verdict. Degrading to null
    // keeps the fail-closed reading — no watches means none are live, an unreadable PR means no
    // PR-derived trigger is satisfied — and never reports the run as observed. Mirrors the same arm
    // in pr-check-state.mjs, and is deliberately narrow: only a path that is not there.
    if (err?.code === 'ENOENT') {
      process.stderr.write(`ci-watch: --${name} payload not found: ${value}\n`)
      return null
    }
    throw new Error(
      `ci-watch: --${name} — cannot read payload ${JSON.stringify(value)}: ${err?.code ?? err?.message ?? err}`,
    )
  }
  if (raw.trim() === '') return null
  return JSON.parse(raw)
}

// Every flag `classify` reads, and nothing else. `parseFlags` accepts any `--name value` pair, and
// `main` used to read only the keys it knew — so an unrecognised name landed in the bag, was never
// looked at, and made a typo byte-indistinguishable from omitting the flag. For `--triggers` that
// omission routed straight into the permissive verdict `classifyCiObservation` now refuses; naming
// the offending flag on a non-zero exit is the only thing that tells the two apart.
//
// Kept as a literal list rather than derived from the reads below: a derived set would grow
// silently with a new read, which is the same unchecked widening in the other direction.
const CLASSIFY_FLAGS = Object.freeze([
  'check-verdict',
  'pr-view',
  'callbacks-available',
  'unavailable-reason',
  'target-chat',
  'pr',
  'target-unverified',
  'watches',
  'triggers',
  'now',
  'min-remaining',
  'poll-completed',
  'arm-attempts',
  'arm-error',
])

// Message form is `<module>: <verb>(<expected shape>) — <what was actually passed>`, matching the
// sibling `pr-check-state.mjs` guards. The top-level catch already turns a raised error into a
// non-zero exit with the message on stderr and no verdict on stdout.
// The flags that carry no value. Everything else in `CLASSIFY_FLAGS` takes one, and `parseFlags`
// renders a value-less flag as boolean `true` — which the reads below type-test away to an empty
// default, making a LOST VALUE byte-indistinguishable from an omitted flag exactly as a misspelt
// NAME once was. `--triggers --now <t>` is the case that matters: the trigger list silently empties.
const CLASSIFY_BOOLEAN_FLAGS = Object.freeze([
  'callbacks-available',
  'target-unverified',
  'poll-completed',
])

function assertKnownFlags(verb, flags, accepted, valueless = []) {
  const shape = accepted.map((name) => `--${name}`).join(', ')
  for (const [name, value] of Object.entries(flags)) {
    if (!accepted.includes(name)) {
      throw new Error(`ci-watch: ${verb}(${shape}) — unrecognised flag --${name}`)
    }
    if (value === true && !valueless.includes(name)) {
      throw new Error(`ci-watch: ${verb}(${shape}) — --${name} needs a value`)
    }
  }
  return shape
}

export function main(argv) {
  const [cmd, ...rest] = argv
  const flags = parseFlags(rest)

  if (cmd === 'classify') {
    const shape = assertKnownFlags('classify', flags, CLASSIFY_FLAGS, CLASSIFY_BOOLEAN_FLAGS)
    const triggers =
      typeof flags.triggers === 'string'
        ? flags.triggers
            .split(',')
            .map((t) => t.trim())
            .filter(Boolean)
        : []
    // The CLI refuses what `classifyCiObservation` merely reports. An in-process caller gets the
    // non-blocking `no-required-triggers-supplied` verdict and its own `mayStopObserving` refusal;
    // a shell caller has no predicate to consult and reads `$?`, so the exit code is its gate.
    if (triggers.length === 0) {
      throw new Error(
        `ci-watch: classify(${shape}) — --triggers named no trigger; a verdict over no required triggers is not an answer`,
      )
    }
    return classifyCiObservation({
      checkVerdict: readPayload(flags, 'check-verdict'),
      prView: readPayload(flags, 'pr-view'),
      callbacksAvailable: flags['callbacks-available'] === true,
      unavailableReason:
        typeof flags['unavailable-reason'] === 'string' ? flags['unavailable-reason'] : '',
      targetChatId: typeof flags['target-chat'] === 'string' ? flags['target-chat'] : '',
      prNumber: flags.pr === undefined ? null : Number(flags.pr),
      targetVerified: flags['target-unverified'] !== true,
      watches: readPayload(flags, 'watches') ?? [],
      requiredTriggers: triggers,
      now: typeof flags.now === 'string' ? flags.now : Date.now(),
      minRemaining: typeof flags['min-remaining'] === 'string' ? flags['min-remaining'] : null,
      fallbackPollCompleted: flags['poll-completed'] === true,
      armAttempts: Number(flags['arm-attempts'] ?? 0),
      lastArmError: typeof flags['arm-error'] === 'string' ? flags['arm-error'] : null,
    })
  }

  throw new Error(`unknown command: ${cmd ?? '(none)'} (expected "classify")`)
}

if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(`${JSON.stringify(main(process.argv.slice(2)))}\n`)
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`)
    process.exitCode = 1
  }
}

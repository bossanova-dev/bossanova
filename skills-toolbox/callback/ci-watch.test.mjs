import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  BLOCKING_STATES,
  CI_WATCH_ACTIONS,
  CI_WATCH_REASONS,
  CI_WATCH_STATES,
  classifyCiObservation,
  mayStopObserving,
} from './ci-watch.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./ci-watch.mjs', import.meta.url))

const NOW = '2026-09-17T12:00:00Z'
const CHAT = 'chat-abc'
const PR = 278
const TRIGGERS = ['checks_passed', 'checks_failed', 'merged']

// A live row: active, this chat, this PR, a day of runway.
function row(overrides = {}) {
  return {
    id: 'cb-1',
    state: 'active',
    target_chat_id: CHAT,
    pr_number: PR,
    trigger: 'checks_passed',
    expires_at: '2026-09-18T12:00:00Z',
    ...overrides,
  }
}

function classify(overrides = {}) {
  return classifyCiObservation({
    checkVerdict: { state: 'pending' },
    prView: { state: 'OPEN', isDraft: false, mergedAt: null },
    callbacksAvailable: true,
    targetChatId: CHAT,
    prNumber: PR,
    watches: [],
    requiredTriggers: TRIGGERS,
    now: NOW,
    ...overrides,
  })
}

test('only `unwatched` blocks — the blocking surface is exactly one state', () => {
  assert.deepEqual(BLOCKING_STATES, [CI_WATCH_STATES.UNWATCHED])
})

test('pending checks with no watches is unwatched, and names every missing trigger', () => {
  const v = classify()
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.equal(v.blocking, true)
  assert.equal(v.action, CI_WATCH_ACTIONS.ARM)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
  assert.equal(mayStopObserving(v), false)
})

test('live watches covering every required trigger is watched', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger, i) => row({ id: `cb-${i}`, trigger })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
  assert.equal(v.blocking, false)
  assert.deepEqual(v.missingTriggers, [])
  assert.deepEqual(v.liveTriggers, TRIGGERS)
  assert.equal(mayStopObserving(v), true)
})

test('one missing trigger among live ones still blocks, and names only that one', () => {
  const v = classify({
    watches: [row({ trigger: 'checks_passed' }), row({ id: 'cb-2', trigger: 'checks_failed' })],
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, ['merged'])
  assert.deepEqual(v.liveTriggers, ['checks_passed', 'checks_failed'])
})

// --- Liveness: only `active` counts -----------------------------------------------------------
// The daemon lifecycle is active -> leased -> triggered -> delivered. Every non-active value means
// the one-shot watch has already fired or is gone; counting one as observation reports `watched`
// over a PR nobody is watching.
for (const state of ['leased', 'triggered', 'delivered', 'canceled', 'expired']) {
  test(`a ${state} row is not a live watch — it has already fired or is gone`, () => {
    const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, state })) })
    assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
    assert.deepEqual(v.missingTriggers, TRIGGERS)
  })
}

// --- Scoping: the two holes in `boss callback list` --------------------------------------------

test("a peer session's watch on the same PR does not satisfy this chat", () => {
  // `boss callback list` filters by chat only when --chat was explicitly passed, while
  // `boss callback add` defaults the chat. An unscoped list therefore returns other sessions' rows,
  // and counting them reports `watched` while THIS chat gets no wake.
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, target_chat_id: 'chat-someone-else' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test("a sibling PR's watch does not satisfy this PR", () => {
  // The CLI exposes no --pr filter, so a repo-scoped list returns sibling PRs' watches.
  const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, pr_number: 999 })) })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test('pr_number compares numerically, so a string row still matches', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, pr_number: String(PR) })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

// --- Expiry runway ------------------------------------------------------------------------------

test('a watch expiring inside the runway floor does not count as live', () => {
  // The reported incident: a 30-minute expiry against a longer CI run. 20 minutes of runway is not
  // observation, even though the daemon has not swept the row yet.
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T12:20:00Z' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, TRIGGERS)
})

test('the default runway floor is 30m — a watch just past it counts', () => {
  const v = classify({
    watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T12:31:00Z' })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

test('minRemaining is caller-tunable for a slow repo', () => {
  const watches = TRIGGERS.map((trigger) => row({ trigger, expires_at: '2026-09-17T14:00:00Z' }))
  assert.equal(classify({ watches, minRemaining: '1h' }).state, CI_WATCH_STATES.WATCHED)
  assert.equal(classify({ watches, minRemaining: '4h' }).state, CI_WATCH_STATES.UNWATCHED)
})

test('a row with no expiry is not live', () => {
  const v = classify({ watches: TRIGGERS.map((trigger) => row({ trigger, expires_at: '' })) })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
})

// --- Per-trigger satisfaction: the burn case ----------------------------------------------------

test('a merged PR with checks still pending does not ask for a `merged` watch', () => {
  // The burn case. A global "checks are terminal" gate reads pending here and arms `merged` into an
  // already-true condition, which fires instantly and consumes the one-shot watch. `merged` must be
  // judged against the PR, not the checks.
  const v = classify({
    checkVerdict: { state: 'pending' },
    prView: { state: 'MERGED', isDraft: false, mergedAt: '2026-09-17T11:00:00Z' },
    watches: [row({ trigger: 'checks_passed' }), row({ id: 'cb-2', trigger: 'checks_failed' })],
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
  assert.deepEqual(v.skippedTriggers, ['merged'])
  assert.deepEqual(v.missingTriggers, [])
})

test('green checks on an unmerged PR still require the merge watch', () => {
  // The mirror failure: a global "checks are terminal" gate stops looking here, while boss-epic is
  // still waiting on the merge that never gets observed.
  //
  // `checks_failed` stays REQUIRED rather than skipped, and that is deliberate: green is not a
  // permanent property of a head. A readied PR can flip to UNSTABLE afterwards, which is the whole
  // reason boss-build has a Step 10 settle loop. Arming it cannot burn the watch either, because its
  // condition (failing) does not currently hold — only a trigger whose condition ALREADY holds is
  // skipped.
  const v = classify({ checkVerdict: { state: 'green' } })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.skippedTriggers, ['checks_passed'])
  assert.deepEqual(v.missingTriggers, ['checks_failed', 'merged'])
})

test('every required condition already holding is settled, with nothing to arm', () => {
  const v = classify({
    checkVerdict: { state: 'green' },
    prView: { state: 'MERGED', isDraft: false, mergedAt: '2026-09-17T11:00:00Z' },
    requiredTriggers: ['checks_passed', 'merged'],
  })
  assert.equal(v.state, CI_WATCH_STATES.SETTLED)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.PROCEED)
  assert.deepEqual(v.missingTriggers, [])
})

test('failing checks satisfy checks_failed, not checks_passed', () => {
  const v = classify({ checkVerdict: { state: 'failing' }, requiredTriggers: ['checks_failed'] })
  assert.equal(v.state, CI_WATCH_STATES.SETTLED)
  assert.deepEqual(v.skippedTriggers, ['checks_failed'])
})

test('checks_passed_ready needs green AND out-of-draft AND open', () => {
  const draftAware = ['checks_passed_ready']
  assert.equal(
    classify({
      checkVerdict: { state: 'green' },
      prView: { state: 'OPEN', isDraft: true, mergedAt: null },
      requiredTriggers: draftAware,
    }).state,
    CI_WATCH_STATES.UNWATCHED,
    'a green DRAFT is not merge-eligible, so the watch is still needed',
  )
  assert.equal(
    classify({
      checkVerdict: { state: 'green' },
      prView: { state: 'OPEN', isDraft: false, mergedAt: null },
      requiredTriggers: draftAware,
    }).state,
    CI_WATCH_STATES.SETTLED,
  )
})

test('an unreadable PR leaves every PR-derived trigger unsatisfied', () => {
  const v = classify({
    checkVerdict: { state: 'green' },
    prView: null,
    requiredTriggers: ['merged'],
  })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.missingTriggers, ['merged'])
})

// --- `unknown` never blocks ---------------------------------------------------------------------

test('an unreadable check state is reported, never blocking', () => {
  // The deadlock this avoids: the protocol forbids arming on could-not-evaluate, so a blocking
  // `unknown` could reach neither `watched` nor `settled` nor (before the poll) `polled` — an
  // unbounded hang in a headless run at the exact moment PR state is unreadable.
  const v = classify({ checkVerdict: { state: 'unknown' } })
  assert.equal(v.state, CI_WATCH_STATES.UNKNOWN)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
  assert.equal(mayStopObserving(v), true)
})

test('an unreadable check state after the bounded poll is polled, and proceeds', () => {
  const v = classify({ checkVerdict: { state: 'unknown' }, fallbackPollCompleted: true })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.reason, CI_WATCH_REASONS.UNREADABLE_POLLED)
  assert.equal(v.action, CI_WATCH_ACTIONS.PROCEED)
})

test('a missing check verdict is treated as unreadable, not as settled', () => {
  const v = classify({ checkVerdict: null })
  assert.equal(v.state, CI_WATCH_STATES.UNKNOWN)
  assert.equal(v.blocking, false)
})

// --- Degrades -----------------------------------------------------------------------------------

test('callbacks unavailable degrades to polled and carries the reason', () => {
  const v = classify({
    callbacksAvailable: false,
    unavailableReason: 'not a bossd-managed session: BOSS_SESSION_ID is unset',
  })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.blocking, false)
  assert.match(v.reason, /BOSS_SESSION_ID is unset/)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
})

test('an unverified callback target degrades to polled, never blocks', () => {
  // boss-epic: no verified target means registration is skipped by contract. Blocking here would
  // hang every epic whose target cannot be resolved.
  const v = classify({ targetVerified: false })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.reason, CI_WATCH_REASONS.TARGET_UNVERIFIED)
  assert.equal(v.blocking, false)
})

test('callbacks-unavailable outranks the arm bound', () => {
  const v = classify({ callbacksAvailable: false, armAttempts: 1 })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.match(v.reason, new RegExp(CI_WATCH_REASONS.CALLBACKS_UNAVAILABLE))
})

// --- The arm bound ------------------------------------------------------------------------------

test('after one arm attempt the verdict degrades instead of blocking again', () => {
  // The livelock bound. A second arm is provably useless: either the first took (and the daemon
  // rejects a co-satisfiable re-arm in the same group), or it failed for a reason a retry will not
  // change. Without this the run arms and re-classifies forever.
  const v = classify({ armAttempts: 1 })
  assert.equal(v.state, CI_WATCH_STATES.POLLED)
  assert.equal(v.blocking, false)
  assert.equal(v.action, CI_WATCH_ACTIONS.POLL)
  assert.equal(mayStopObserving(v), true)
})

test('the arm error is surfaced in the degraded reason', () => {
  const v = classify({ armAttempts: 1, lastArmError: 'group already holds checks_passed' })
  assert.match(v.reason, /group already holds checks_passed/)
})

test('arming once and succeeding reaches watched, not the degrade', () => {
  const v = classify({
    armAttempts: 1,
    watches: TRIGGERS.map((trigger, i) => row({ id: `cb-${i}`, trigger })),
  })
  assert.equal(v.state, CI_WATCH_STATES.WATCHED)
})

// --- Shape ---------------------------------------------------------------------------------------

test('an empty required-trigger list is unknown, never a pass', () => {
  // This used to report `watched` — the `watched` arm's test is `missingTriggers.length === 0`,
  // which is trivially true over an empty required set, so "live watches cover every required
  // trigger" held over nothing and the empty `liveTriggers` beside it was the proof no watch had
  // ever been read. A verdict may report a permissive state only from inputs it received.
  const v = classify({ requiredTriggers: [] })
  assert.equal(v.state, CI_WATCH_STATES.UNKNOWN)
  assert.equal(v.reason, CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS)
  assert.deepEqual(v.liveTriggers, [])
  // Still non-blocking: the module's blocking surface is exactly `unwatched`, and widening it
  // would hang a headless run. The refusal lands on `mayStopObserving` and the CLI exit instead.
  assert.equal(v.blocking, false)
  assert.deepEqual(BLOCKING_STATES, [CI_WATCH_STATES.UNWATCHED])
})

test('an absent required-trigger list reaches the same unknown verdict as an empty one', () => {
  for (const requiredTriggers of [undefined, null, 'checks_passed', [null, '', false]]) {
    const v = classify({ requiredTriggers })
    assert.equal(v.state, CI_WATCH_STATES.UNKNOWN, JSON.stringify(requiredTriggers ?? null))
    assert.equal(v.reason, CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS)
  }
})

test('mayStopObserving refuses the no-required-triggers verdict despite it not blocking', () => {
  // The predicate is the in-process caller's gate, and `blocking` alone would hand it a pass: an
  // unevaluated trigger set is not permission to stop looking at CI.
  const v = classify({ requiredTriggers: [] })
  assert.equal(v.blocking, false)
  assert.equal(mayStopObserving(v), false)
})

test('the no-required-triggers arm wins over every degrade, which cannot answer either', () => {
  // Callbacks being unavailable is a statement about the MECHANISM; with no required triggers there
  // is still no question for the poll to settle, so `polled`/`proceed` would be the same invented
  // permission in a different costume.
  for (const overrides of [
    { callbacksAvailable: false },
    { targetVerified: false },
    { checkVerdict: { state: 'unknown' } },
    { armAttempts: 1 },
  ]) {
    const v = classify({ requiredTriggers: [], ...overrides })
    assert.equal(v.state, CI_WATCH_STATES.UNKNOWN, JSON.stringify(overrides))
    assert.equal(v.reason, CI_WATCH_REASONS.NO_REQUIRED_TRIGGERS)
  }
})

test('malformed watch rows are ignored rather than throwing', () => {
  const v = classify({ watches: [null, 'nonsense', 42, row({ trigger: 'checks_passed' })] })
  assert.equal(v.state, CI_WATCH_STATES.UNWATCHED)
  assert.deepEqual(v.liveTriggers, ['checks_passed'])
})

test('a non-array watches payload is treated as no watches', () => {
  assert.equal(classify({ watches: null }).state, CI_WATCH_STATES.UNWATCHED)
})

test('every verdict carries the full reporting shape', () => {
  for (const v of [
    classify(),
    classify({ watches: TRIGGERS.map((t) => row({ trigger: t })) }),
    classify({ callbacksAvailable: false }),
    classify({ checkVerdict: { state: 'unknown' } }),
    classify({ requiredTriggers: [] }),
  ]) {
    assert.ok(Object.values(CI_WATCH_STATES).includes(v.state), `state: ${v.state}`)
    assert.equal(typeof v.reason, 'string')
    assert.equal(typeof v.blocking, 'boolean')
    assert.ok(Array.isArray(v.missingTriggers))
    assert.ok(Array.isArray(v.skippedTriggers))
    assert.ok(Array.isArray(v.liveTriggers))
    assert.equal(v.blocking, BLOCKING_STATES.includes(v.state))
  }
})

// ---------------------------------------------------------------------------
// CLI. The flag bag used to be read for known keys only, so a misnamed argument was silently
// equivalent to omitting it — and omitting `--triggers` routed straight into the permissive
// verdict the unit cases above now refuse.

function runCli(args) {
  return spawnSync(process.execPath, [SCRIPT_PATH, ...args], { encoding: 'utf8' })
}

test('CLI classify — a real trigger list prints one JSON line and exits 0', () => {
  const result = runCli(['classify', '--triggers', 'checks_passed,checks_failed'])
  assert.equal(result.status, 0, result.stderr)
  assert.equal(result.stdout.trimEnd().split('\n').length, 1, 'exactly one JSON line')
  const parsed = JSON.parse(result.stdout)
  assert.deepEqual(parsed.missingTriggers, ['checks_passed', 'checks_failed'])
})

test('CLI classify — every flag the verb reads is accepted', () => {
  // The guard is only as good as its accepted set: a name left out of it turns a working shipped
  // invocation into a hard failure. Each flag is probed on top of a known-good baseline.
  const dir = mkdtempSync(path.join(tmpdir(), 'ci-watch-'))
  const payload = (name, body) => {
    const file = path.join(dir, name)
    writeFileSync(file, JSON.stringify(body))
    return file
  }
  const probes = [
    ['--check-verdict', payload('verdict.json', { state: 'pending' })],
    ['--pr-view', payload('pr.json', { state: 'OPEN', isDraft: false, mergedAt: null })],
    ['--watches', payload('watches.json', [])],
    ['--callbacks-available'],
    ['--unavailable-reason', 'daemon unreachable'],
    ['--target-chat', CHAT],
    ['--pr', String(PR)],
    ['--target-unverified'],
    ['--now', NOW],
    ['--min-remaining', '30m'],
    ['--poll-completed'],
    ['--arm-attempts', '1'],
    ['--arm-error', 'group already holds checks_passed'],
  ]
  for (const probe of probes) {
    const result = runCli(['classify', '--triggers', 'checks_passed', ...probe])
    assert.equal(result.status, 0, `${probe[0]}: ${result.stderr}`)
    assert.ok(Object.values(CI_WATCH_STATES).includes(JSON.parse(result.stdout).state), probe[0])
  }
})

test('CLI classify — an unrecognised flag exits non-zero and names the offending flag', () => {
  // `--requiredTriggers` is the in-process option spelling, and it is the exact mistake this
  // closes: it used to land in the bag, go unread, and print a permissive verdict at exit 0.
  const result = runCli(['classify', '--requiredTriggers', 'checks_passed'])
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /--requiredTriggers/)
  assert.match(result.stderr, /unrecognised flag/)
  assert.equal(result.stdout, '', 'no verdict is printed for a rejected invocation')
})

test('CLI classify — a recognised flag that lost its value is refused, not silently emptied', () => {
  // `parseFlags` renders a value-less flag as boolean `true`, which the reads type-test away to an
  // empty default — so `--triggers --now <t>` swallowed the trigger list and left it empty.
  const result = runCli(['classify', '--triggers', '--now', '2026-01-01T00:00:00Z'])
  assert.notEqual(result.status, 0)
  assert.match(result.stderr, /--triggers needs a value/)
  assert.equal(result.stdout, '', 'no verdict is printed for a rejected invocation')
  // The genuinely value-less flags are unaffected.
  const ok = runCli([
    'classify',
    '--triggers',
    'checks_passed',
    '--callbacks-available',
    '--target-chat',
    'c1',
    '--pr',
    '1',
  ])
  assert.equal(ok.status, 0, ok.stderr)
})

test('CLI classify — an absent or empty trigger list exits non-zero naming --triggers', () => {
  for (const args of [
    ['classify'],
    ['classify', '--triggers'],
    ['classify', '--triggers', ' , , '],
    ['classify', '--triggers', '', '--pr', '278'],
  ]) {
    const result = runCli(args)
    assert.notEqual(result.status, 0, args.join(' '))
    assert.match(result.stderr, /--triggers/, args.join(' '))
    assert.equal(result.stdout, '', args.join(' '))
  }
})

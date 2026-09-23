// skills-toolbox/tracker/outcome.test.mjs
// Every verdict in the frozen set is produced here at least once — that is the contract the
// vocabulary hangs on, so it is asserted mechanically at the bottom rather than trusted.

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  TRACKER_OPERATION_KINDS,
  TRACKER_OUTCOME_REASONS,
  TRACKER_OUTCOME_REASON_SET,
  TRACKER_RETRY_CAPS,
  TRACKER_VERDICTS,
  TRACKER_VERDICT_SET,
  classifyTrackerOutcome,
  formatOutcomeLine,
  readEvidence,
  withBoundedRetry,
} from './outcome.mjs'

const V = TRACKER_VERDICTS
const R = TRACKER_OUTCOME_REASONS

// Every verdict this suite actually produced, so the "test suite produces every one" claim is
// measured rather than asserted by eye.
const produced = new Set()
function expectVerdict(actual, verdict, reason, retry) {
  assert.equal(actual.verdict, verdict)
  assert.equal(actual.reason, reason)
  assert.equal(actual.retry, retry)
  produced.add(actual.verdict)
  return actual
}

test('the verdict set is frozen and is exactly the five verdicts', () => {
  assert.ok(Object.isFrozen(TRACKER_VERDICTS))
  assert.deepEqual([...TRACKER_VERDICT_SET].sort(), [
    'false-empty',
    'indeterminate',
    'ok',
    'permanent',
    'retryable',
  ])
  assert.throws(() => {
    'use strict'
    TRACKER_VERDICTS.SIXTH = 'sixth'
  })
})

test('the reason vocabulary is closed, frozen and disjoint from the verdicts', () => {
  assert.ok(Object.isFrozen(TRACKER_OUTCOME_REASONS))
  for (const reason of TRACKER_OUTCOME_REASON_SET) {
    assert.equal(TRACKER_VERDICT_SET.has(reason), false, `${reason} must not double as a verdict`)
  }
  assert.deepEqual(TRACKER_OPERATION_KINDS, ['read', 'write'])
})

// --- retryable signatures, each under its OWN reason slug -------------------------------------

test('a bare "fetch failed" is retryable under transport-failed', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('fetch failed')),
    V.RETRYABLE,
    R.TRANSPORT_FAILED,
    true,
  )
  // The sandbox signature the source notes recorded: no status, no cause, just the two words.
  expectVerdict(classifyTrackerOutcome('fetch failed'), V.RETRYABLE, R.TRANSPORT_FAILED, true)
})

test('a request timeout on a read is retryable under request-timeout', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('The operation timed out')),
    V.RETRYABLE,
    R.REQUEST_TIMEOUT,
    true,
  )
  expectVerdict(
    classifyTrackerOutcome('connect ETIMEDOUT 1.2.3.4:443'),
    V.RETRYABLE,
    R.REQUEST_TIMEOUT,
    true,
  )
})

test('an HTTP 5xx is retryable under server-error, from the text or an explicit status', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('Linear API HTTP 500')),
    V.RETRYABLE,
    R.SERVER_ERROR,
    true,
  )
  expectVerdict(classifyTrackerOutcome({ status: 503 }), V.RETRYABLE, R.SERVER_ERROR, true)
})

test('an HTTP 429 is retryable under its own rate-limited slug, not server-error', () => {
  const v = expectVerdict(
    classifyTrackerOutcome(new Error('Linear API HTTP 429')),
    V.RETRYABLE,
    R.RATE_LIMITED,
    true,
  )
  assert.notEqual(v.reason, R.SERVER_ERROR)
  expectVerdict(classifyTrackerOutcome({ status: 429 }), V.RETRYABLE, R.RATE_LIMITED, true)
})

// --- permanent signatures, each under its OWN reason slug -------------------------------------

test('a missing API key is permanent under missing-credential and is never retried', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('LINEAR_API_KEY is not set')),
    V.PERMANENT,
    R.MISSING_CREDENTIAL,
    false,
  )
})

test('HTTP 401 and HTTP 403 are permanent under DISTINCT slugs', () => {
  const unauthorized = expectVerdict(
    classifyTrackerOutcome(new Error('Linear API HTTP 401')),
    V.PERMANENT,
    R.UNAUTHORIZED,
    false,
  )
  const forbidden = expectVerdict(
    classifyTrackerOutcome(new Error('Linear API HTTP 403')),
    V.PERMANENT,
    R.FORBIDDEN,
    false,
  )
  assert.notEqual(unauthorized.reason, forbidden.reason)
})

test('a GraphQL validation error is permanent under validation-error', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('Linear GraphQL error: Cannot query field "value" on Float')),
    V.PERMANENT,
    R.VALIDATION_ERROR,
    false,
  )
  expectVerdict(classifyTrackerOutcome({ status: 400 }), V.PERMANENT, R.VALIDATION_ERROR, false)
})

// BOS-1282 review: the module's own principle is that the text is a human affordance and the
// signal travels on a field. A tagged GraphQL failure is therefore answered from `kind` BEFORE
// any text table, so a server message that happens to contain a transport word cannot flip a
// deterministic rejection into a retry — nor, under `write`, into an `indeterminate` read-back
// for a write the server positively refused.
test('a tagged GraphQL failure is permanent even when its text names a transport word', () => {
  const tagged = new Error('Linear GraphQL error: upstream subscription terminated')
  tagged.kind = 'graphql'
  expectVerdict(classifyTrackerOutcome(tagged), V.PERMANENT, R.VALIDATION_ERROR, false)
  expectVerdict(
    classifyTrackerOutcome(tagged, { operation: 'write' }),
    V.PERMANENT,
    R.VALIDATION_ERROR,
    false,
  )
  // Untagged, the same text is still read as transport — the tag is what adds the evidence,
  // and nothing here silently reclassifies a failure nobody attributed.
  expectVerdict(
    classifyTrackerOutcome(new Error('upstream subscription terminated')),
    V.RETRYABLE,
    R.TRANSPORT_FAILED,
    true,
  )
})

test('every retryable slug differs from every permanent slug', () => {
  const slugs = [
    R.TRANSPORT_FAILED,
    R.REQUEST_TIMEOUT,
    R.SERVER_ERROR,
    R.RATE_LIMITED,
    R.MISSING_CREDENTIAL,
    R.UNAUTHORIZED,
    R.FORBIDDEN,
    R.VALIDATION_ERROR,
  ]
  assert.equal(new Set(slugs).size, slugs.length)
})

// --- indeterminate: a write that may already have applied -------------------------------------

test('a WRITE that timed out is indeterminate with retry DECLINED', () => {
  const v = expectVerdict(
    classifyTrackerOutcome(new Error('The operation timed out'), { operation: 'write' }),
    V.INDETERMINATE,
    R.WRITE_MAY_HAVE_APPLIED,
    false,
  )
  assert.equal(v.retry, false, 'a write that may have applied must never be blind-retried')
  // The SAME text on a read is retryable: the operation is what discriminates.
  assert.equal(classifyTrackerOutcome(new Error('The operation timed out')).verdict, V.RETRYABLE)
})

test('a WRITE whose connection died is indeterminate too, not retryable', () => {
  expectVerdict(
    classifyTrackerOutcome('fetch failed', { operation: 'write' }),
    V.INDETERMINATE,
    R.WRITE_MAY_HAVE_APPLIED,
    false,
  )
})

test('a WRITE rejected by validation is still permanent — it cannot have applied', () => {
  expectVerdict(
    classifyTrackerOutcome(new Error('Linear GraphQL error: bad filter'), { operation: 'write' }),
    V.PERMANENT,
    R.VALIDATION_ERROR,
    false,
  )
})

// --- results, and the false-empty answer ------------------------------------------------------

test('a readable result classifies ok', () => {
  expectVerdict(
    classifyTrackerOutcome({ result: { issues: { nodes: [] } } }),
    V.OK,
    R.SUCCESS,
    false,
  )
  // A bare object with neither error nor status IS the result.
  expectVerdict(
    classifyTrackerOutcome({ issues: { nodes: [{ id: '1' }] } }),
    V.OK,
    R.SUCCESS,
    false,
  )
})

test('a result that is null or a primitive is false-empty, never ok', () => {
  expectVerdict(
    classifyTrackerOutcome({ result: null }),
    V.FALSE_EMPTY,
    R.UNREADABLE_PAYLOAD,
    false,
  )
  expectVerdict(
    classifyTrackerOutcome({ result: 'nope' }),
    V.FALSE_EMPTY,
    R.UNREADABLE_PAYLOAD,
    false,
  )
})

test('readEvidence separates rows, no rows and could-not-evaluate', () => {
  const rows = readEvidence([{ id: 'a' }, { id: 'b' }])
  assert.deepEqual({ ...rows }, { verdict: V.OK, reason: R.ROWS_PRESENT, rows: 2 })

  const empty = readEvidence([])
  assert.deepEqual({ ...empty }, { verdict: V.OK, reason: R.NO_ROWS, rows: 0 })

  for (const malformed of [undefined, null, {}, 'rows', 0]) {
    const verdict = readEvidence(malformed)
    assert.equal(verdict.verdict, V.FALSE_EMPTY)
    assert.equal(verdict.reason, R.UNREADABLE_PAYLOAD)
    // NOT 0 — a count of zero is exactly the collapse this answer exists to prevent.
    assert.equal(verdict.rows, null)
    produced.add(verdict.verdict)
  }
  produced.add(empty.verdict)
})

// --- malformed observations -------------------------------------------------------------------

test('null, undefined, an empty string and a non-object classify without throwing', () => {
  for (const observed of [null, undefined, '', 0, false, Symbol('x')]) {
    const verdict = classifyTrackerOutcome(observed)
    assert.ok(TRACKER_VERDICT_SET.has(verdict.verdict), `${String(observed)} produced a verdict`)
    assert.notEqual(verdict.verdict, V.OK, `${String(observed)} must never read as ok`)
    produced.add(verdict.verdict)
  }
})

test('an unrecognised error text falls to the conservative verdict, never ok', () => {
  const v = expectVerdict(
    classifyTrackerOutcome(new Error('something nobody has seen before')),
    V.PERMANENT,
    R.UNRECOGNIZED,
    false,
  )
  assert.equal(v.retry, false, 'an unknown failure must not spend the retry budget')
})

test('an unrecognised 4xx is not retried and is not mislabelled as a known cause', () => {
  expectVerdict(classifyTrackerOutcome({ status: 404 }), V.PERMANENT, R.UNRECOGNIZED, false)
})

test('a verdict is frozen so a caller cannot mutate the vocabulary through it', () => {
  const v = classifyTrackerOutcome('fetch failed')
  assert.ok(Object.isFrozen(v))
})

// --- withBoundedRetry -------------------------------------------------------------------------

/** A sleep RECORDER: never waits, and keeps every requested delay for assertion. */
function sleepRecorder() {
  const delays = []
  const sleep = async (ms) => {
    delays.push(ms)
  }
  sleep.delays = delays
  return sleep
}

test('withBoundedRetry returns the first success and makes no further attempt', async () => {
  let calls = 0
  const sleep = sleepRecorder()
  const result = await withBoundedRetry(
    async () => {
      calls += 1
      return 'value'
    },
    { sleep },
  )
  assert.equal(result, 'value')
  assert.equal(calls, 1)
  assert.deepEqual(sleep.delays, [], 'a success must not request any sleep')
})

test('withBoundedRetry stops at the attempt cap rather than looping', async () => {
  let calls = 0
  const sleep = sleepRecorder()
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          calls += 1
          throw new Error('fetch failed')
        },
        { maxAttempts: 4, sleep },
      ),
    /fetch failed/,
  )
  assert.equal(calls, 4, 'exactly maxAttempts calls, no more')
  assert.equal(
    sleep.delays.length,
    3,
    'one sleep between each pair of attempts, none after the last',
  )
})

test('withBoundedRetry never requests a sleep longer than the configured cap', async () => {
  const sleep = sleepRecorder()
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          throw new Error('fetch failed')
        },
        { maxAttempts: 8, baseDelayMs: 100, maxDelayMs: 300, sleep },
      ),
    /fetch failed/,
  )
  assert.equal(sleep.delays.length, 7)
  for (const ms of sleep.delays) assert.ok(ms <= 300, `requested ${ms}ms above the 300ms cap`)
  // The clamp, not the exponent, decides the longest wait: 100, 200, then pinned.
  assert.deepEqual(sleep.delays, [100, 200, 300, 300, 300, 300, 300])
})

test('the shipped default caps are small and explicit', async () => {
  const sleep = sleepRecorder()
  let calls = 0
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          calls += 1
          throw new Error('fetch failed')
        },
        { sleep },
      ),
    /fetch failed/,
  )
  assert.equal(calls, TRACKER_RETRY_CAPS.maxAttempts)
  for (const ms of sleep.delays) assert.ok(ms <= TRACKER_RETRY_CAPS.maxDelayMs)
})

test('withBoundedRetry makes NO second attempt for a permanent verdict', async () => {
  const sleep = sleepRecorder()
  for (const message of [
    'LINEAR_API_KEY is not set',
    'Linear API HTTP 401',
    'Linear GraphQL error: bad filter',
  ]) {
    let calls = 0
    await assert.rejects(
      () =>
        withBoundedRetry(
          async () => {
            calls += 1
            throw new Error(message)
          },
          { maxAttempts: 5, sleep },
        ),
      new RegExp(message.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
    )
    assert.equal(calls, 1, `${message} must fail on the first attempt`)
  }
  assert.deepEqual(sleep.delays, [])
})

test('withBoundedRetry makes no second attempt for an indeterminate write', async () => {
  let calls = 0
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          calls += 1
          throw new Error('The operation timed out')
        },
        { maxAttempts: 5, operation: 'write', sleep: sleepRecorder() },
      ),
    /timed out/,
  )
  assert.equal(calls, 1, 'a write that may have applied must be verified, not repeated')
})

test('withBoundedRetry resolves when a transient failure clears', async () => {
  let calls = 0
  const sleep = sleepRecorder()
  const result = await withBoundedRetry(
    async () => {
      calls += 1
      if (calls === 1) throw new Error('fetch failed')
      return 'recovered'
    },
    { sleep },
  )
  assert.equal(result, 'recovered')
  assert.equal(calls, 2)
  assert.deepEqual(sleep.delays, [TRACKER_RETRY_CAPS.baseDelayMs])
})

test('withBoundedRetry re-throws the last error, so fail-closed callers see what they always saw', async () => {
  const boom = new Error('fetch failed')
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          throw boom
        },
        { maxAttempts: 2, sleep: sleepRecorder() },
      ),
    (err) => err === boom,
  )
})

test('withBoundedRetry reports each retry through onRetry', async () => {
  const seen = []
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          throw new Error('fetch failed')
        },
        { maxAttempts: 3, sleep: sleepRecorder(), onRetry: (info) => seen.push(info.attempt) },
      ),
    /fetch failed/,
  )
  assert.deepEqual(seen, [1, 2])
})

test('withBoundedRetry clamps a nonsense attempt count to one real call', async () => {
  let calls = 0
  await assert.rejects(
    () =>
      withBoundedRetry(
        async () => {
          calls += 1
          throw new Error('fetch failed')
        },
        { maxAttempts: 0, sleep: sleepRecorder() },
      ),
    /fetch failed/,
  )
  assert.equal(calls, 1)
})

test('withBoundedRetry rejects a non-function attempt as a wiring error', async () => {
  await assert.rejects(() => withBoundedRetry(null), /must be a function/)
})

// --- the machine-readable line ----------------------------------------------------------------

test('formatOutcomeLine emits a stable, parseable single line', () => {
  const line = formatOutcomeLine(classifyTrackerOutcome('fetch failed'))
  assert.equal(
    line,
    'tracker-outcome verdict=retryable reason=transport-failed retry=yes operation=read',
  )
  assert.equal(line.includes('\n'), false)
  const write = formatOutcomeLine(
    classifyTrackerOutcome('The operation timed out', { operation: 'write' }),
    { operation: 'write' },
  )
  assert.equal(
    write,
    'tracker-outcome verdict=indeterminate reason=write-may-have-applied retry=no operation=write',
  )
})

// --- the contract this whole module hangs on --------------------------------------------------

test('this suite produces every verdict in the frozen set', () => {
  for (const verdict of TRACKER_VERDICT_SET) {
    assert.ok(produced.has(verdict), `no test in this suite produced the verdict ${verdict}`)
  }
})

test('outcome.mjs imports nothing — a vendored copy resolves with no siblings', async () => {
  const { readFileSync } = await import('node:fs')
  const source = readFileSync(new URL('./outcome.mjs', import.meta.url), 'utf8')
  assert.equal(/^import\s/m.test(source), false, 'outcome.mjs must have no imports')
})

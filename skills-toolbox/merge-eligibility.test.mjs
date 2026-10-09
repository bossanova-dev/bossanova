import test from 'node:test'
import assert from 'node:assert/strict'
import {
  SHARED_INELIGIBLE_REASONS,
  checklist,
  ciReasons,
  classifyMergeResult,
  criteriaReasons,
  followUpReasons,
  headReasons,
  judgeCiOnHead,
  mergeArgv,
  mergeStateReasons,
  parseFollowUps,
  parseLedger,
  prShapeReasons,
  sectionToken,
  sections,
  sharedEligibility,
} from './merge-eligibility.mjs'

const HEAD = 'a'.repeat(40),
  OTHER = 'c'.repeat(40)
const body = `## Acceptance criteria
- [x] implementation demonstrated
## Review coverage
full
## Human follow-up
- none
## Open questions
- none
`
const view = (changes = {}) => ({
  state: 'OPEN',
  isDraft: false,
  title: 'Feature',
  body,
  headRefOid: HEAD,
  mergeStateStatus: 'CLEAN',
  ...changes,
})
const facts = (changes = {}) => ({
  criteria: { met: 1, total: 1 },
  ciWaitState: 'settled',
  checkVerdict: { state: 'green', observedSHA: HEAD },
  headSha: HEAD,
  prView: view(),
  heads: [HEAD, HEAD],
  humanFollowUp: { status: 'ok', open: 0 },
  openQuestions: { status: 'ok', open: 0 },
  ...changes,
})

test('criteriaReasons: met, unmet, and malformed fail closed', () => {
  assert.deepEqual(criteriaReasons({ met: 2, total: 2 }), [])
  assert.deepEqual(criteriaReasons({ met: 1, total: 2 }), ['criteria-unmet'])
  for (const bad of [
    null,
    undefined,
    {},
    { met: 1, total: 0 },
    { met: '1', total: 1 },
    { met: 1.5, total: 2 },
    { met: -1, total: 1 },
    { met: 3, total: 2 },
  ])
    assert.deepEqual(criteriaReasons(bad), ['criteria-unknown'], JSON.stringify(bad))
})

test('ciReasons: settled green on the head passes; every other reading fails', () => {
  const green = { state: 'green', observedSHA: HEAD }
  assert.deepEqual(ciReasons({ ciWaitState: 'settled', checkVerdict: green, headSha: HEAD }), [])
  assert.deepEqual(ciReasons({ ciWaitState: 'timeout', checkVerdict: green, headSha: HEAD }), [
    'ci-not-settled',
  ])
  assert.deepEqual(
    ciReasons({
      ciWaitState: 'settled',
      checkVerdict: { ...green, observedSHA: OTHER },
      headSha: HEAD,
    }),
    ['ci-not-green-on-head'],
  )
  assert.deepEqual(
    ciReasons({
      ciWaitState: 'settled',
      checkVerdict: { state: 'failing', observedSHA: HEAD },
      headSha: HEAD,
    }),
    ['ci-not-green-on-head'],
  )
  assert.deepEqual(ciReasons({ ciWaitState: 'settled', checkVerdict: green, headSha: 'abc' }), [
    'ci-not-green-on-head',
  ])
  assert.deepEqual(ciReasons(), ['ci-not-settled', 'ci-not-green-on-head'])
})

test('mergeStateReasons: allow-list, blocking states, and malformed input', () => {
  for (const state of ['CLEAN', 'clean', 'HAS_HOOKS', 'UNSTABLE'])
    assert.deepEqual(mergeStateReasons(view({ mergeStateStatus: state })), [], state)
  for (const state of ['BLOCKED', 'BEHIND', 'DRAFT', 'DIRTY'])
    assert.deepEqual(mergeStateReasons(view({ mergeStateStatus: state })), [
      `merge-state-${state.toLowerCase()}`,
    ])
  for (const state of ['UNKNOWN', '', undefined, {}, 42, 'NOT_A_STATE'])
    assert.deepEqual(
      mergeStateReasons(view({ mergeStateStatus: state })),
      ['merge-state-unknown'],
      String(state),
    )
  assert.deepEqual(mergeStateReasons(undefined), ['merge-state-unknown'])
})

test('prShapeReasons: open non-draft unmarked passes; each marker fires', () => {
  assert.deepEqual(prShapeReasons(view()), [])
  assert.deepEqual(prShapeReasons(view({ state: 'CLOSED' })), ['pr-not-open'])
  assert.deepEqual(prShapeReasons(view({ isDraft: true })), ['pr-draft'])
  assert.deepEqual(prShapeReasons(view({ isDraft: undefined })), ['pr-draft'])
  assert.deepEqual(prShapeReasons(view({ title: 'Do not merge: wip' })), ['do-not-merge-marker'])
  assert.deepEqual(prShapeReasons(view({ title: 'Feature (partial scope)' })), [
    'do-not-merge-marker',
  ])
  assert.deepEqual(prShapeReasons(view({ body: undefined })), ['do-not-merge-marker'])
  assert.deepEqual(prShapeReasons(undefined), ['pr-not-open', 'pr-draft', 'do-not-merge-marker'])
})

test('headReasons: every head 40-hex and equal', () => {
  assert.deepEqual(headReasons([HEAD, HEAD, HEAD, HEAD]), [])
  assert.deepEqual(headReasons([HEAD, HEAD.toUpperCase()]), ['head-mismatch'])
  assert.deepEqual(headReasons([HEAD, OTHER]), ['head-mismatch'])
  assert.deepEqual(headReasons([HEAD, undefined]), ['head-unknown'])
  assert.deepEqual(headReasons([HEAD, 'a'.repeat(39)]), ['head-unknown'])
  assert.deepEqual(headReasons([]), ['head-unknown'])
  assert.deepEqual(headReasons(undefined), ['head-unknown'])
})

test('followUpReasons: open, missing, and malformed counts', () => {
  const ok = { status: 'ok', open: 0 }
  assert.deepEqual(followUpReasons({ humanFollowUp: ok, openQuestions: ok }), [])
  assert.deepEqual(
    followUpReasons({
      humanFollowUp: { status: 'ok', open: 2 },
      openQuestions: { status: 'ok', open: 1 },
    }),
    ['human-follow-up-open', 'open-questions-open'],
  )
  assert.deepEqual(
    followUpReasons({
      humanFollowUp: { status: 'ok', open: '0' },
      openQuestions: { status: 'missing' },
    }),
    ['follow-up-section-missing', 'open-questions-section-missing'],
  )
  assert.deepEqual(followUpReasons(), [
    'follow-up-section-missing',
    'open-questions-section-missing',
  ])
})

test('sharedEligibility: green facts pass and many reasons keep the documented order', () => {
  assert.deepEqual(sharedEligibility(facts()), { eligible: true, reasons: [] })
  const result = sharedEligibility(
    facts({
      criteria: { met: 0, total: 1 },
      ciWaitState: 'pending',
      prView: view({ mergeStateStatus: 'BEHIND', isDraft: true, headRefOid: OTHER }),
      heads: [HEAD, OTHER],
      openQuestions: { status: 'ok', open: 3 },
    }),
  )
  assert.equal(result.eligible, false)
  assert.deepEqual(result.reasons, [
    'criteria-unmet',
    'ci-not-settled',
    'merge-state-behind',
    'pr-draft',
    'head-mismatch',
    'open-questions-open',
  ])
  const empty = sharedEligibility(null)
  assert.equal(empty.eligible, false)
  for (const reason of empty.reasons) assert.ok(SHARED_INELIGIBLE_REASONS.includes(reason), reason)
  // The emitted order is a subsequence of the vocabulary order.
  const positions = empty.reasons.map((reason) => SHARED_INELIGIBLE_REASONS.indexOf(reason))
  assert.deepEqual(
    positions,
    [...positions].sort((a, b) => a - b),
  )
})

test('parseLedger reads criteria and follow-ups, ignoring fenced examples', () => {
  assert.deepEqual(parseLedger(body).criteria, { met: 1, total: 1 })
  const fenced = parseLedger(
    '```\n## Acceptance criteria\n- [ ] example\n```\n## Acceptance criteria\n- [x] a\n- [ ] b\n## Human follow-up\n- [ ] check live\n## Open questions\n- none',
  )
  assert.deepEqual(fenced.criteria, { met: 1, total: 2 })
  assert.equal(fenced.humanFollowUp.open, 1)
  assert.deepEqual(fenced.openQuestions, { status: 'ok', open: 0, done: 0, total: 0 })
})

test('parseLedger: duplicate heading, `- none` criteria and a malformed checkbox are null', () => {
  assert.equal(
    parseLedger('## Acceptance criteria\n- [x] a\n## Acceptance criteria\n- [x] b').criteria,
    null,
  )
  assert.equal(parseLedger('## Acceptance criteria\n- none').criteria, null)
  assert.equal(parseLedger('## Acceptance criteria\n- [x]\n').criteria, null)
  assert.equal(parseLedger('## Acceptance criteria\n- [y] typo\n').criteria, null)
  assert.equal(parseLedger('').criteria, null)
  assert.equal(parseLedger(undefined).humanFollowUp.status, 'missing')
})

test('parseFollowUps and the raw parsers keep their completion-gate behaviour', () => {
  assert.equal(parseFollowUps(body).ok, true)
  assert.equal(sections('## A\nx\n## A\ny').get('a'), null)
  assert.deepEqual(checklist(['- [ ] a', '- [x] b']), { status: 'ok', open: 1, done: 1, total: 2 })
  assert.equal(checklist(['- none']).status, 'malformed')
  assert.equal(checklist(['- none'], true).status, 'ok')
})

test('sectionToken returns the first non-blank line, or empty', () => {
  assert.equal(sectionToken(body, 'review coverage'), 'full')
  assert.equal(
    sectionToken('## Cross-model review\n\n  clean  \nmore', 'cross-model review'),
    'clean',
  )
  assert.equal(sectionToken(body, 'cross-model review'), '')
  assert.equal(sectionToken('## X\na\n## X\nb', 'x'), '')
  assert.equal(sectionToken(undefined, 'x'), '')
})

const run = (name, conclusion, status = 'COMPLETED') => ({
  __typename: 'CheckRun',
  name,
  status,
  conclusion,
})
const statusContext = (context, state) => ({ __typename: 'StatusContext', context, state })

test('judgeCiOnHead ignores failing and pending boss/* contexts in both node forms', () => {
  const result = judgeCiOnHead({
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: [
      run('unit', 'SUCCESS'),
      run('boss/build', 'FAILURE'),
      run('boss/verify', '', 'IN_PROGRESS'),
      statusContext('boss/verify', 'PENDING'),
      statusContext('boss/build', 'FAILURE'),
    ],
  })
  assert.equal(result.verdict.state, 'green')
  assert.equal(result.settled, true)
  assert.equal(result.green, true)
  assert.deepEqual(result.excluded, ['boss/build', 'boss/verify'])
})

test('judgeCiOnHead filters check-run envelopes and paginated pages', () => {
  const checkRuns = [
    { total_count: 2, check_runs: [{ name: 'lint', status: 'completed', conclusion: 'success' }] },
    {
      total_count: 2,
      check_runs: [{ name: 'boss/verify', status: 'completed', conclusion: 'failure' }],
    },
  ]
  const result = judgeCiOnHead({ headSHA: HEAD, observedSHA: HEAD, checkRuns })
  assert.equal(result.green, true)
  assert.deepEqual(result.excluded, ['boss/verify'])
  const single = judgeCiOnHead({
    headSHA: HEAD,
    observedSHA: HEAD,
    checkRuns: { check_runs: [...checkRuns[0].check_runs, ...checkRuns[1].check_runs] },
  })
  assert.equal(single.green, true)
})

test('judgeCiOnHead: a pending non-boss check is unsettled', () => {
  const result = judgeCiOnHead({
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: [run('unit', 'SUCCESS'), run('e2e', '', 'QUEUED')],
  })
  assert.equal(result.settled, false)
  assert.equal(result.green, false)
  assert.deepEqual(result.excluded, [])
})

test('judgeCiOnHead: a green set with zero passed checks is not green', () => {
  const result = judgeCiOnHead({
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: [run('boss/build', 'SUCCESS')],
  })
  assert.equal(result.green, false)
  assert.equal(result.verdict.passed, 0)
})

test('judgeCiOnHead: boss/* names in priorContexts never become an absent gate', () => {
  for (const priorContexts of [
    ['unit', 'boss/verify', 'boss/build'],
    [run('unit', 'SUCCESS'), statusContext('boss/verify', 'SUCCESS')],
  ]) {
    const result = judgeCiOnHead({
      headSHA: HEAD,
      observedSHA: HEAD,
      rollup: [run('unit', 'SUCCESS')],
      priorContexts,
    })
    assert.deepEqual(result.verdict.absent, [], JSON.stringify(priorContexts))
    assert.equal(result.green, true)
    assert.ok(result.excluded.includes('boss/verify'))
  }
  // A non-boss context missing from the head still holds the verdict.
  const absent = judgeCiOnHead({
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: [run('unit', 'SUCCESS')],
    priorContexts: ['unit', 'e2e'],
  })
  assert.deepEqual(absent.verdict.absent, ['e2e'])
  assert.equal(absent.green, false)
})

test('judgeCiOnHead: green requires a well-formed head SHA that the observed SHA matches', () => {
  const rollup = [run('unit', 'SUCCESS')]
  assert.equal(judgeCiOnHead({ headSHA: HEAD, observedSHA: HEAD, rollup }).green, true)
  for (const shas of [
    {},
    { headSHA: HEAD },
    { observedSHA: HEAD },
    { headSHA: 'oops', observedSHA: 'oops' },
    { headSHA: HEAD, observedSHA: 'b'.repeat(40) },
  ])
    assert.equal(judgeCiOnHead({ ...shas, rollup }).green, false, JSON.stringify(shas))
})

test('mergeArgv returns the exact head-pinned argv and throws on misuse', () => {
  assert.deepEqual(mergeArgv({ sessionId: 'sess-1', headSha: HEAD }), [
    'merge',
    'sess-1',
    '--yes',
    '--json',
    '--match-head',
    HEAD,
  ])
  for (const bad of [
    { sessionId: '', headSha: HEAD },
    { sessionId: '   ', headSha: HEAD },
    { sessionId: 42, headSha: HEAD },
    { sessionId: 'sess-1', headSha: 'abc123' },
    { sessionId: 'sess-1', headSha: 42 },
    { sessionId: 'sess-1' },
    undefined,
  ])
    assert.throws(() => mergeArgv(bad), TypeError, JSON.stringify(bad))
})

test('classifyMergeResult maps accepted, HEAD_MISMATCH, and refusals', () => {
  assert.equal(classifyMergeResult({ ok: true, payload: {} }), 'accepted')
  assert.equal(classifyMergeResult({ ok: false, error: 'HEAD_MISMATCH' }), 'head-mismatch')
  assert.equal(
    classifyMergeResult({ ok: false, error: 'NOT_MERGEABLE' }),
    'merge-refused:NOT_MERGEABLE',
  )
  assert.equal(classifyMergeResult({ ok: false, error: 'exit-1' }), 'merge-refused:exit-1')
})

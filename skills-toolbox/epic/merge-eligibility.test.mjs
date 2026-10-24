import test from 'node:test'
import assert from 'node:assert/strict'
import {
  SHARED_INELIGIBLE_REASONS,
  ciReasons,
  classifyMergeResult,
  headReasons,
  isPartial,
  judgeCiOnHead,
  mergeArgv,
  mergeStateReasons,
  prShapeReasons,
  sectionToken,
  sections,
  sharedEligibility,
} from './merge-eligibility.mjs'

const HEAD = 'a'.repeat(40),
  OTHER = 'c'.repeat(40)
const body = `## Review coverage
full
`
const PARTIAL_BODY = `## Summary
Half of it.

## Partial
do not merge — partial: 2/5 acceptance criteria
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
  ciWaitState: 'settled',
  checkVerdict: { state: 'green', observedSHA: HEAD },
  headSha: HEAD,
  prView: view(),
  heads: [HEAD, HEAD],
  ...changes,
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

test('prShapeReasons: open, non-draft and not partial passes; each fires on its own', () => {
  assert.deepEqual(prShapeReasons(view()), [])
  assert.deepEqual(prShapeReasons(view({ state: 'CLOSED' })), ['pr-not-open'])
  assert.deepEqual(prShapeReasons(view({ isDraft: true })), ['pr-draft'])
  assert.deepEqual(prShapeReasons(view({ isDraft: undefined })), ['pr-draft'])
  assert.deepEqual(prShapeReasons(view({ body: PARTIAL_BODY })), ['partial-marker'])
  assert.deepEqual(prShapeReasons(view({ title: '[T-1] Feature (partial 2/5)' })), [
    'partial-marker',
  ])
  assert.deepEqual(prShapeReasons(undefined), ['pr-not-open', 'pr-draft'])
})

test("only boss-build's PARTIAL marker holds; other PR text never does", () => {
  for (const held of [
    { body: PARTIAL_BODY },
    { body: '## partial\n\nDo Not Merge - partial: 1/3 acceptance criteria' },
    { body: '## Partial\ndo not merge – partial: 0/2' },
    { title: 'Feature (partial 3/4)' },
    { title: 'Feature (PARTIAL 3 / 4)' },
  ])
    assert.equal(isPartial(held), true, JSON.stringify(held))
  for (const free of [
    { title: 'Do not merge: wip', body: 'do not merge' },
    { title: 'Feature (partial scope)' },
    { title: 'Feature (partial n/m)' },
    { body: 'do not merge — partial: 2/5 acceptance criteria' },
    { body: '## Notes\ndo not merge — partial: 2/5 acceptance criteria' },
    { body: '## Partial\nShipped in two slices.\ndo not merge — partial: 2/5' },
    { body: '## Partial\ndo not merge — partial: most of it' },
    { body: '## Partial\nNote: do not merge — partial: 2/5 was the old marker' },
    { body: '```\n## Partial\ndo not merge — partial: 2/5\n```' },
    { body: '## Acceptance criteria\n- [ ] unmet\n## Open questions\n- [ ] which way?' },
    {},
  ])
    assert.equal(isPartial(free), false, JSON.stringify(free))
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

test('sharedEligibility: green facts pass and many reasons keep the documented order', () => {
  assert.deepEqual(sharedEligibility(facts()), { eligible: true, reasons: [] })
  const result = sharedEligibility(
    facts({
      ciWaitState: 'pending',
      prView: view({
        mergeStateStatus: 'BEHIND',
        isDraft: true,
        headRefOid: OTHER,
        body: PARTIAL_BODY,
      }),
      heads: [HEAD, OTHER],
    }),
  )
  assert.equal(result.eligible, false)
  assert.deepEqual(result.reasons, [
    'ci-not-settled',
    'merge-state-behind',
    'pr-draft',
    'partial-marker',
    'head-mismatch',
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

test('sharedEligibility: a body with no sections, open boxes or "do not merge" prose passes', () => {
  for (const prBody of [
    '',
    'Fixes the login redirect.',
    '## What\nA change.\n\n## Checklist\n- [ ] tests\n- [ ] docs',
    '## Acceptance criteria\n- [ ] unmet\n## Human follow-up\n- [ ] check prod\n## Open questions\n- [ ] which?',
    'Please do not merge until Friday.',
  ])
    assert.deepEqual(
      sharedEligibility(facts({ prView: view({ title: 'Do not merge yet', body: prBody }) })),
      { eligible: true, reasons: [] },
      prBody,
    )
})

test('sections reads headings outside fences and leaves a repeated one undecidable', () => {
  assert.equal(sections('## A\nx\n## A\ny').get('a'), null)
  assert.equal(sections('```\n## A\n```\n## B\nb').has('a'), false)
  assert.deepEqual(sections('```\n## A\n```\n## B\nb').get('b'), ['b'])
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

test('judgeCiOnHead forwards read failures and structural absence', () => {
  const base = {
    headSHA: HEAD,
    observedSHA: HEAD,
    rollup: [{ name: 'ci', status: 'COMPLETED', conclusion: 'SUCCESS' }],
  }
  const unreadable = judgeCiOnHead({ ...base, readError: true })
  assert.equal(unreadable.verdict.reason, 'unreadable')
  assert.equal(unreadable.green, false)
  const structural = judgeCiOnHead({
    ...base,
    priorContexts: [...base.rollup, { name: 'review', status: 'COMPLETED', conclusion: 'SUCCESS' }],
    structurallyAbsentContexts: ['review'],
  })
  assert.equal(structural.green, true)
})

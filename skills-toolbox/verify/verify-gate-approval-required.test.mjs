import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  HUMAN_REASONS,
  classifyApproval,
  decideVerdict,
  needsHumanDescription,
} from './verify-gate.mjs'
import { APPROVAL_REQUIRED, splitEligibility } from './verify-blockers.mjs'
import { SHARED_INELIGIBLE_REASONS } from '../epic/merge-eligibility.mjs'
import { parkVerdict } from './verify-park-note.mjs'

const facts = (extra = {}) => ({
  changedPaths: [],
  judgeAreas: [],
  prState: 'OPEN',
  isDraft: false,
  headSha: 'a'.repeat(40),
  receipt: 'success',
  ci: { settled: true, green: true },
  extensions: [],
  mergePolicy: { auto: [{ level: 'high' }] },
  ...extra,
})
// A required code-owner approval: GitHub reports BLOCKED and the review decision says why.
const blocked = (extra = {}) =>
  facts({
    eligibilityReasons: ['merge-state-blocked'],
    reviewDecision: 'REVIEW_REQUIRED',
    ...extra,
  })
const finding = {
  severity: 'P1',
  verified: true,
  introduced: true,
  fixed: false,
  file: 'src/a.go',
  line: 3,
  title: 'nil deref',
  evidence: 'x',
}

test('a head blocked only for a review is judged, then parks approval-required', () => {
  for (const reviewDecision of ['REVIEW_REQUIRED', 'CHANGES_REQUESTED']) {
    const r = decideVerdict(blocked({ reviewDecision }))
    assert.deepEqual([r.verdict, r.reasons], ['human', [APPROVAL_REQUIRED]], reviewDecision)
  }
  // Judging runs in full: extensions, the review and blocking findings all come before the park.
  assert.equal(
    decideVerdict(blocked({ extensions: [{ name: 'proof' }] })).verdict,
    'extensions-required',
  )
  assert.equal(
    decideVerdict(blocked({ receipt: 'absent', prClass: 'boss' })).verdict,
    'review-required',
  )
  assert.equal(decideVerdict(blocked({ reviewFindings: [finding] })).verdict, 'blocked')
  // A policy that leaves the merge to a person parks both, so one approval serves the park.
  const both = decideVerdict(
    blocked({ tier: { class: 'human', review: 'receipt', merge: 'human' } }),
  )
  assert.deepEqual(both.reasons, [APPROVAL_REQUIRED, 'human-merge'])
})

test('a block a review cannot explain, or CI not yet green, still waits', () => {
  for (const reviewDecision of ['APPROVED', '', undefined, 'approved?']) {
    const r = decideVerdict(blocked({ reviewDecision }))
    assert.deepEqual([r.verdict, r.reason], ['wait', 'merge-state-blocked'], String(reviewDecision))
  }
  assert.equal(decideVerdict(blocked({ ci: { settled: false } })).reason, 'ci-not-settled')
  // Absent CI cannot rule out a required check that never reported.
  const absent = blocked({ ci: { settled: true, green: false, absent: true } })
  assert.deepEqual(
    [decideVerdict(absent).verdict, decideVerdict(absent).reason],
    ['wait', 'merge-state-blocked'],
  )
  for (const reason of ['merge-state-behind', 'merge-state-unknown'])
    assert.equal(decideVerdict(blocked({ eligibilityReasons: [reason] })).reason, reason)
})

test('only a GitHub approval lifts the park, so the head passes and merge alone waits for GitHub', () => {
  const park = {
    state: 'pending',
    description: needsHumanDescription(APPROVAL_REQUIRED, 'high: x'),
  }
  assert.deepEqual(
    classifyApproval({ prState: 'OPEN', reviewDecision: 'APPROVED', status: park }),
    {
      approval: 'policy-park-approved',
      waive: APPROVAL_REQUIRED,
      via: 'review-approved',
    },
  )
  assert.equal(
    classifyApproval({ prState: 'OPEN', reviewDecision: 'REVIEW_REQUIRED', status: park }).approval,
    'none',
  )
  // A boss approval of the park (its Merge action) leaves the ask standing while GitHub blocks.
  const operator = { decision: 'approved', covers: APPROVAL_REQUIRED }
  assert.deepEqual(
    classifyApproval({
      prState: 'OPEN',
      reviewDecision: 'REVIEW_REQUIRED',
      status: park,
      operator,
    }),
    { approval: 'none', reason: APPROVAL_REQUIRED },
  )
  const waived = decideVerdict(blocked({ waive: `${APPROVAL_REQUIRED},human-merge` }))
  assert.deepEqual([waived.verdict, waived.reasons], ['human', [APPROVAL_REQUIRED]])
  // Once GitHub records the approval, its own state decides: clean passes, still blocked waits.
  assert.equal(decideVerdict(facts({ waive: APPROVAL_REQUIRED })).verdict, 'pass')
  const stillBlocked = decideVerdict(
    blocked({ reviewDecision: 'APPROVED', waive: APPROVAL_REQUIRED }),
  )
  assert.deepEqual([stillBlocked.verdict, stillBlocked.reason], ['wait', 'merge-state-blocked'])
})

test('the park says an approval is required', () => {
  assert.ok(HUMAN_REASONS.includes(APPROVAL_REQUIRED))
  assert.match(
    needsHumanDescription(APPROVAL_REQUIRED, 'high: x'),
    /^needs human: approval-required · /,
  )
  assert.equal(
    parkVerdict([APPROVAL_REQUIRED]).sentence,
    'Verified · waiting for you · verify passed; the repository requires an approving review before this PR can merge',
  )
  // It decides over human-merge, whose Merge action GitHub would refuse.
  assert.equal(
    parkVerdict([APPROVAL_REQUIRED, 'human-merge']).reason,
    parkVerdict([APPROVAL_REQUIRED]).reason,
  )
})

test('a run that never merges waits on no merge-only state, but still on unknown, draft and closed', () => {
  const mergeStates = SHARED_INELIGIBLE_REASONS.filter((r) => r.startsWith('merge-state-'))
  const stillWait = ['merge-state-unknown', 'merge-state-draft']
  for (const reason of mergeStates) {
    const expected = stillWait.includes(reason) ? [reason] : []
    assert.deepEqual(splitEligibility([reason], { mergeless: true }).wait, expected, reason)
  }
  for (const reason of ['pr-not-open', 'pr-draft', 'head-unknown', 'head-mismatch'])
    assert.deepEqual(splitEligibility([reason], { mergeless: true }).wait, [reason], reason)
  const behind = decideVerdict(facts({ trial: true, eligibilityReasons: ['merge-state-behind'] }))
  assert.equal(behind.verdict, 'pass')
  // A trial never merges, so a head GitHub blocks for a review is not parked for that approval.
  assert.equal(decideVerdict(blocked({ trial: true })).verdict, 'pass')
  const conflict = decideVerdict(facts({ trial: true, eligibilityReasons: ['merge-state-dirty'] }))
  assert.deepEqual(
    conflict.blockers.map((b) => b.kind),
    ['conflict'],
  )
  assert.equal(decideVerdict(facts({ trial: true, isDraft: true })).reason, 'pr-draft')
})

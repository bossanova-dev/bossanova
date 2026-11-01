import assert from 'node:assert/strict'
import { test } from 'node:test'
import { RECEIPT_CONTEXT } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS as D } from '../verify/verify-vocabulary.mjs'
import { needsHumanDescription } from '../verify/verify-gate.mjs'
import { renderPrSection } from '../review-verdict.mjs'
import {
  HOLD_MINUTES,
  JUDGE_GRACE_MINUTES,
  CRON_STALE_MINUTES,
  SECOND_MODEL_MIN_REVIEWS,
  SECOND_MODEL_MIN_RAN_SHARE,
  MAX_PRS,
  MAX_HOLD_HEADS,
  mergesWithoutVerdict,
  unjudgedHeads,
  unnecessaryHolds,
  verifyCronHealth,
  secondModelHealth,
  receiptHeads,
  buildReport,
  renderReport,
  outcomeCounts,
} from './retro-outcomes.mjs'

const at = (minutes = 0) => new Date(Date.UTC(2026, 9, 7, 0, minutes)).toISOString()
const status = (description, minutes = 0, context = VERIFY_CONTEXT) => ({
  context,
  state: context === RECEIPT_CONTEXT ? 'SUCCESS' : 'PENDING',
  description,
  createdAt: at(minutes),
})
const receipt = (minutes = 0) => status('', minutes, RECEIPT_CONTEXT)
const commit = (oid, statuses) => ({
  commit: { oid, statusCheckRollup: { contexts: { nodes: statuses } } },
})
const pr = (number = 1, statuses = []) => ({
  number,
  title: 'Change behavior',
  headRefOid: `head${number}`,
  mergedAt: at(40),
  commits: { nodes: [commit(`head${number}`, [receipt(), ...statuses])] },
  reviews: { nodes: [] },
  reviewThreads: { nodes: [] },
})
const window = { start: at(-60), end: at(60) }
const park = needsHumanDescription('partial-build', 'check evidence')

test('thresholds and read bounds are pinned', () => {
  assert.deepEqual(
    [
      HOLD_MINUTES,
      JUDGE_GRACE_MINUTES,
      CRON_STALE_MINUTES,
      SECOND_MODEL_MIN_REVIEWS,
      SECOND_MODEL_MIN_RAN_SHARE,
      MAX_PRS,
      MAX_HOLD_HEADS,
    ],
    [15, 30, 60, 5, 0.5, 200, 50],
  )
})

test('merge-head terminal verdicts exclude passes, parks and send-backs', () => {
  const terminals = [
    D.verified,
    D.verifiedApproved,
    park,
    `${D.defect} tests`,
    `${D.blocked} conflict`,
  ]
  for (const description of terminals)
    assert.equal(mergesWithoutVerdict([pr(1, [status(description)])], window).rows.length, 0)
  const samples = [
    pr(1),
    pr(2, [status(`${D.waiting} checks`)]),
    pr(3, [status(`${D.verifying} runner`)]),
  ]
  samples.push({ ...pr(4), commits: { nodes: [] } }, { ...pr(5), mergedAt: at(90) })
  const rows = mergesWithoutVerdict(samples, window).rows
  assert.deepEqual(
    rows.map(({ number, headState }) => [number, headState]),
    [
      [1, 'absent'],
      [2, 'waiting'],
      [3, 'verifying'],
    ],
  )
  const oldVerdict = pr(6)
  oldVerdict.commits.nodes.unshift(commit('old', [status(D.verified)]))
  assert.equal(mergesWithoutVerdict([oldVerdict], window).rows.length, 1)
})

test('unjudged heads respect receipt lifetime, supersession, closure and terminal parks', () => {
  const live = { ...pr(), mergedAt: null }
  assert.equal(unjudgedHeads([live], { now: at(31) }).rows[0].minutes, 31)
  const replaced = {
    ...pr(),
    commits: { nodes: [commit('old', [receipt()]), commit('new', [receipt(29)])] },
  }
  assert.equal(unjudgedHeads([replaced], { now: at(40) }).rows.length, 0)
  assert.equal(unjudgedHeads([pr(2, [status(park)])], { now: at(40) }).rows.length, 0)
  assert.equal(unjudgedHeads([{ ...live, closedAt: at(29) }], { now: at(40) }).rows.length, 0)
  assert.equal(receiptHeads([live], { start: at(1) }).length, 0)
})

test('unjudged heads report only lifetimes overlapping the window, retaining old live heads', () => {
  const currentWindow = { start: at(60), end: at(120) }
  const options = { now: at(180), window: currentWindow }
  assert.deepEqual(unjudgedHeads([pr()], options).rows, [])
  assert.deepEqual(unjudgedHeads([{ ...pr(), mergedAt: null, closedAt: at(40) }], options).rows, [])
  const live = { ...pr(), mergedAt: null }
  assert.equal(unjudgedHeads([live], options).rows[0].minutes, 120)
  const replaced = {
    ...live,
    commits: { nodes: [commit('old', [receipt()]), commit('new', [receipt(40)])] },
  }
  assert.deepEqual(
    unjudgedHeads([replaced], options).rows.map((head) => head.head),
    ['new'],
  )
  const young = { ...live, commits: { nodes: [commit('young', [receipt(100)])] } }
  assert.deepEqual(unjudgedHeads([young], options).rows, [])
})

test('holds are counted in their approval window even if their PR is updated later', () => {
  const heads = [
    { number: 1, head: 'head1', statuses: [status(D.verifiedApproved, 10), status(park)] },
  ]
  assert.deepEqual(
    unnecessaryHolds(heads, [pr()], { window: { start: at(20), end: at(60) } }).rows,
    [],
  )
  assert.equal(unnecessaryHolds(heads, [pr()], { window }).rows.length, 1)
})

test('holds group earliest parks by code sets and ignore approval activity', () => {
  const history = (minutes) => [
    { number: 1, head: 'head1', statuses: [status(D.verifiedApproved, minutes), status(park)] },
  ]
  const sample = pr()
  sample.reviews.nodes.push({
    state: 'APPROVED',
    body: '',
    submittedAt: at(5),
    author: { __typename: 'User' },
  })
  const result = unnecessaryHolds(history(10), [sample], {})
  assert.equal(result.rows[0].minutes, 10)
  assert.deepEqual(result.rows[0].codes, ['partial-build'])
  assert.equal(result.rows[0].label, 'check evidence')
  assert.deepEqual(result.byCodes, { 'partial-build': 1 })
  assert.equal(unnecessaryHolds(history(20), [sample], {}).rows.length, 0)
  for (const state of ['CHANGES_REQUESTED', 'COMMENTED']) {
    const touched = pr()
    touched.reviews.nodes.push({
      state,
      body: 'please change',
      submittedAt: at(5),
      author: { __typename: 'User' },
    })
    assert.equal(unnecessaryHolds(history(10), [touched], {}).rows.length, 0)
  }
  const touched = pr()
  touched.reviewThreads.nodes.push({
    comments: { nodes: [{ createdAt: at(5), author: { __typename: 'User' } }] },
  })
  assert.equal(unnecessaryHolds(history(10), [touched], {}).rows.length, 0)
  touched.reviewThreads.nodes[0].comments.nodes[0].author.__typename = 'Bot'
  assert.equal(unnecessaryHolds(history(10), [touched], {}).rows.length, 1)
})

test('cron health distinguishes host state and cross-host silence', () => {
  const job = { prompt: '/boss-verify', enabled: true, last_run_at: at(0), next_run_at: at(70) }
  const jobs = [
    { ...job, enabled: false },
    { ...job, last_run_outcome: 'gate_failed' },
    { ...job, last_run_outcome: 'fire_failed' },
    { ...job, last_run_at: null },
    { ...job, next_run_at: at(-1) },
    job,
    { ...job, prompt: '/boss-verify-x' },
  ]
  const result = verifyCronHealth(jobs, { now: at(60), heads: receiptHeads([pr()]) })
  assert.deepEqual(
    result.rows.map((row) => row.health),
    ['disabled', 'failing', 'failing', 'stale', 'stale', 'ok'],
  )
  assert.equal(result.verifySilent, true)
  assert.equal(verifyCronHealth([], { now: at(60), heads: [] }).health, 'none')
  assert.equal(
    verifyCronHealth([], { now: at(60), heads: receiptHeads([pr(1, [status(park)])]) })
      .verifySilent,
    false,
  )
})

const reviewed = (number, ran, reason = 'unavailable', agent = 'other') => ({
  ...pr(number),
  body: renderPrSection({
    status: 'ok',
    verdict: { findings: [], secondModel: { ran, reason, agent } },
  }),
})
test('second-model health counts review outcomes and ranks skip reasons', () => {
  const samples = [
    reviewed(1, true),
    reviewed(2, false),
    reviewed(3, false),
    reviewed(4, false, 'disabled'),
    { ...pr(5), body: renderPrSection({ status: 'error', errors: ['bad output'], verdict: {} }) },
  ]
  const result = secondModelHealth([...samples, pr(6)])
  assert.equal(result.health, 'degraded')
  assert.deepEqual([result.ran, result.skipped, result.error, result.absent], [1, 3, 1, 1])
  assert.deepEqual(result.byAgent, { other: 1 })
  assert.deepEqual(result.topSkipReasons, [
    { reason: 'unavailable', count: 2 },
    { reason: 'disabled', count: 1 },
  ])
  assert.equal(secondModelHealth(samples.slice(0, 4)).health, 'ok')
  assert.equal(
    secondModelHealth([
      reviewed(1, true),
      reviewed(2, true),
      reviewed(3, true),
      reviewed(4, false),
      reviewed(5, false),
    ]).health,
    'ok',
  )
})

test('report renderer includes failures, future sections, row limits and counts-only summary', () => {
  const report = buildReport({
    window,
    truncated: true,
    sections: {
      mergesWithoutVerdict: mergesWithoutVerdict(
        Array.from({ length: 12 }, (_, i) => pr(i)),
        window,
      ),
      unjudgedHeads: { status: 'failed', reason: 'read-failed' },
      unnecessaryHolds: { status: 'skipped', reason: 'no-repo-id' },
      verifyCronHealth: verifyCronHealth([], { now: at(60), heads: [] }),
      secondModelHealth: secondModelHealth([]),
      futureMeasure: { status: 'ok', rows: [{ signal: 'future behavior' }] },
    },
  })
  const rendered = renderReport(report)
  for (const fragment of [
    'Verify outcomes',
    'read-failed',
    'no-repo-id',
    '+2 more',
    'future behavior',
    'none',
  ])
    assert.ok(rendered.includes(fragment))
  assert.equal(report.v, 1)
  const counts = outcomeCounts(report)
  assert.deepEqual(counts.sections.mergesWithoutVerdict, { status: 'ok', count: 12 })
  assert.deepEqual(counts.sections.unjudgedHeads, { status: 'failed' })
  assert.equal(JSON.stringify(counts).includes('rows'), false)
  assert.deepEqual(counts.window, window)
})

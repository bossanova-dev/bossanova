import assert from 'node:assert/strict'
import { test } from 'node:test'
import { RECEIPT_CONTEXT } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS as D } from '../verify/verify-vocabulary.mjs'
import { needsHumanDescription } from '../verify/verify-gate.mjs'
import { escapeRecord, renderEscapeComment, parseEscapeComment } from '../verify/verify-escapes.mjs'
import { renderPrSection } from '../review-verdict.mjs'
import {
  RATE_WINDOW_DAYS,
  VERDICT_CLASSES,
  verdictClass,
  escapeRows,
  laterFixRates,
  mergeVerdict,
  factory,
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
  areaCaps,
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

const escaped = (originalNumber = 1, fixNumber = 20, minutes = 30) => {
  const original = {
    ...pr(originalNumber),
    files: ['skills-toolbox/retro/retro-outcomes.mjs'],
    body: '## Review findings\n- [ ] **P1** defect\n## Unapplied review findings\n- [ ] **P2** deferred\n## Cross-model review\nran (other)\n',
    additions: 12,
    deletions: 3,
    changedFiles: 2,
    verifyState: D.verified,
  }
  const fix = { ...pr(fixNumber), mergedAt: at(minutes), files: original.files }
  const record = escapeRecord({
    repo: 'a/b',
    fix,
    link: { original, basis: 'explicit-reference', evidence: `Fixes #${originalNumber}` },
    tracedAt: at(60),
  })
  const parsed = parseEscapeComment(renderEscapeComment(record))
  assert.ok(parsed)
  return parsed
}

test('verdict classes share the terminal vocabulary and merge-head selection', () => {
  assert.equal(RATE_WINDOW_DAYS, 28)
  const descriptions = [
    D.verified,
    D.verifiedApproved,
    park,
    `${D.defect} tests`,
    `${D.defect} tests${D.defectParkedSuffix}`,
    `${D.blocked} conflict`,
    `${D.waiting} checks`,
    `${D.verifying} runner`,
    undefined,
  ]
  assert.deepEqual(descriptions.map(verdictClass), [
    'passed',
    'approved',
    'parked',
    'sent-back',
    'sent-back',
    'sent-back',
    'none',
    'none',
    'none',
  ])
  const sample = pr(1, [status(D.verified, 2), status(park, 1)])
  sample.commits.nodes.unshift(commit('old', [status(D.verifiedApproved, 5)]))
  assert.equal(factory(sample), true)
  assert.deepEqual(mergeVerdict(sample), {
    number: 1,
    mergedAt: at(40),
    verifyDescription: D.verified,
  })
  assert.equal(mergeVerdict({ ...sample, headRefOid: 'missing' }).verifyDescription, null)
})

test('escape rows retain merge evidence, deduplicate pairs and sort fixes in the window', () => {
  const older = escaped(1, 20, 10)
  const newer = escaped(1, 21, 40)
  const section = escapeRows([older, newer, older, escaped(2, 22, 90)], window)
  assert.deepEqual(
    section.rows.map((row) => row.fix.number),
    [21, 20],
  )
  assert.deepEqual(section.rows[0].areas, newer.areas)
  assert.deepEqual(section.rows[0].signals, newer.signals)
  assert.equal('mergeHeadSha' in section.rows[0].original, false)
  section.skipped = { 'tracker-unavailable': 1 }
  const rendered = renderReport(buildReport({ window, sections: { escapeRows: section } }))
  assert.ok(rendered.includes('tracker-unavailable'))
  for (const fragment of [
    '#1 ← #21',
    newer.basis,
    newer.areas[0].area,
    'blocking: 1',
    'unapplied: 1',
    'P1',
    'ran',
    '+12/-3',
    '2 files',
    D.verified,
  ])
    assert.ok(rendered.includes(fragment), fragment)
})

test('rates count distinct escaped originals against merge classes and expose unmatched originals', () => {
  const merges = Array.from({ length: 4 }, (_, i) => ({
    number: i + 1,
    mergedAt: at(5),
    verifyDescription: D.verified,
  }))
  merges.push({ number: 5, mergedAt: at(5), verifyDescription: park })
  merges.push({ number: 99, mergedAt: at(90), verifyDescription: D.verified })
  const section = laterFixRates(merges, [escaped(1), escaped(1, 21), escaped(99)], window)
  assert.equal(section.by, 'verdict')
  assert.deepEqual(section.window, window)
  assert.deepEqual(
    section.rows.map((row) => row.class),
    VERDICT_CLASSES,
  )
  assert.deepEqual(section.rows[0], { class: 'passed', merges: 4, escapes: 1, rate: 0.25 })
  assert.deepEqual(section.rows[1], { class: 'approved', merges: 0, escapes: 0, rate: null })
  assert.equal(section.unmatched, 1)
  const report = buildReport({ window, sections: { laterFixRates: section } })
  const rendered = renderReport(report)
  for (const fragment of ['25%', 'n/a', 'Unmatched: 1'])
    assert.ok(rendered.includes(fragment), fragment)
  assert.deepEqual(outcomeCounts(report).sections.laterFixRates, { status: 'ok', count: 5 })
})

test('escape and rate read failures remain isolated and preserve their reasons and detail', () => {
  const failed = {
    status: 'failed',
    reason: 'tracer-failed',
    detail: 'trace could not read statuses',
  }
  assert.deepEqual(escapeRows(failed, window), failed)
  assert.deepEqual(laterFixRates([], failed, window), failed)
  const rateFailed = { status: 'failed', reason: 'read-failed' }
  assert.deepEqual(laterFixRates(rateFailed, [], window), rateFailed)
  for (const failure of [failed, rateFailed]) {
    const sections = {
      mergesWithoutVerdict: mergesWithoutVerdict([], window),
      unjudgedHeads: unjudgedHeads([]),
      unnecessaryHolds: unnecessaryHolds([], []),
      verifyCronHealth: verifyCronHealth([]),
      secondModelHealth: secondModelHealth([]),
      escapeRows: escapeRows(failure === failed ? failure : [], window),
      laterFixRates: laterFixRates(
        failure === rateFailed ? failure : [],
        failure === failed ? failure : [],
        window,
      ),
    }
    for (const name of [
      'mergesWithoutVerdict',
      'unjudgedHeads',
      'unnecessaryHolds',
      'verifyCronHealth',
      'secondModelHealth',
    ])
      assert.equal(sections[name].status, 'ok')
    const rendered = renderReport(buildReport({ window, sections }))
    assert.ok(rendered.includes(failure.reason))
    assert.equal(outcomeCounts({ sections }).sections.laterFixRates.count, undefined)
  }
})

const capWindow = { start: '2026-10-25T00:00:00.000Z', end: '2026-11-01T00:00:00.000Z' }
const episode = (key, cappedAt, clear) => ({
  key,
  label: key,
  cappedAt,
  escapes: [
    { original: 3752, fix: 3759, fixMergedAt: cappedAt },
    { original: 3714, fix: 3760, fixMergedAt: '2026-10-01T00:00:00.000Z' },
  ],
  ...(clear ? { clearedAt: clear[0], clearedBy: 'operator', reason: clear[1] } : {}),
})
const capEpisodes = () => [
  episode('cleared-inside', '2026-10-20T00:00:00.000Z', [
    '2026-10-28T00:00:00.000Z',
    'fixed\nthe guard',
  ]),
  episode('standing', '2026-09-01T00:00:00.000Z'),
  episode('cleared-before', '2026-10-01T00:00:00.000Z', ['2026-10-24T23:59:59.000Z', 'old']),
  episode('capped-after', '2026-11-02T00:00:00.000Z'),
  episode('cleared-after', '2026-10-30T00:00:00.000Z', ['2026-11-03T00:00:00.000Z', 'later']),
  episode('also-standing', '2026-10-26T00:00:00.000Z'),
]

test('area caps keep episodes overlapping the window, active or cleared', () => {
  const section = areaCaps(capEpisodes(), capWindow)
  assert.equal(section.status, 'ok')
  const byKey = Object.fromEntries(section.rows.map((row) => [row.key, row]))
  assert.equal(byKey['cleared-before'], undefined)
  assert.equal(byKey['capped-after'], undefined)
  assert.equal(byKey['cleared-inside'].state, 'cleared')
  assert.equal(byKey['cleared-inside'].clearedAt, '2026-10-28T00:00:00.000Z')
  assert.equal(byKey['cleared-inside'].clearedBy, 'operator')
  assert.equal(byKey['cleared-inside'].reason, 'fixed\nthe guard')
  assert.equal(byKey.standing.state, 'active')
  assert.equal(byKey['cleared-after'].state, 'active')
  assert.equal(byKey['cleared-after'].clearedBy, undefined)
  assert.deepEqual(byKey.standing.escapes, [
    { original: 3752, fix: 3759 },
    { original: 3714, fix: 3760 },
  ])
  assert.equal(section.active, 3)
})

test('area caps order active first, newest cap first, and leave input untouched', () => {
  const input = capEpisodes()
  const snapshot = structuredClone(input)
  const rows = areaCaps(input, capWindow).rows
  assert.deepEqual(input, snapshot)
  assert.deepEqual(
    rows.map((row) => row.key),
    ['cleared-after', 'also-standing', 'standing', 'cleared-inside'],
  )
  assert.deepEqual(areaCaps([...input].reverse(), capWindow).rows, rows)
  const failure = { status: 'failed', reason: 'caps-failed' }
  assert.equal(areaCaps(failure, capWindow), failure)
})

test('area caps render one line per episode with state, escapes and clear attribution', () => {
  const report = buildReport({
    window: capWindow,
    sections: { areaCaps: areaCaps(capEpisodes(), capWindow) },
  })
  const lines = renderReport(report).split('\n')
  const heading = lines.indexOf('### Area caps')
  assert.ok(heading > 0)
  const rows = lines.slice(heading + 2).filter((line) => line.startsWith('- '))
  assert.equal(rows.length, 4)
  const cleared = rows.find((line) => line.includes('cleared-inside'))
  for (const fragment of [
    '2026-10-28T00:00:00.000Z',
    'operator',
    'fixed the guard',
    '#3752',
    '#3759',
    '#3714',
    '#3760',
  ])
    assert.ok(cleared.includes(fragment), fragment)
  const standing = rows.find((line) => line.includes('- standing'))
  assert.ok(standing.includes('active'))
  assert.ok(!standing.includes('operator'))
  const failed = renderReport(
    buildReport({
      window: capWindow,
      sections: { areaCaps: { status: 'failed', reason: 'caps-missing' } },
    }),
  )
  assert.ok(failed.includes('caps-missing'))
  const counts = outcomeCounts({
    window: capWindow,
    sections: {
      areaCaps: areaCaps(capEpisodes(), capWindow),
      mergesWithoutVerdict: { status: 'ok', rows: [{}, {}] },
    },
  })
  assert.deepEqual(counts.sections.areaCaps, { status: 'ok', count: 4, active: 3 })
  assert.deepEqual(counts.sections.mergesWithoutVerdict, { status: 'ok', count: 2 })
})

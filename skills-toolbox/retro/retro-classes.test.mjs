import test from 'node:test'
import assert from 'node:assert/strict'
import { classify, report, runCli } from './retro-classes.mjs'
import { clusterNotes, mergeClusters } from './retro-notes.mjs'

const note = (id, statement, run, date) => ({
  id,
  body: `${statement}\nWhere: src/a.js\nRun: test / manual / headless / run:${run}`,
  created_at: date,
})
const selection = (clusters) => ({
  selected: clusters.map((cluster) => ({ cluster, reason: 'selected' })),
  deferred: [],
  dropped: [],
  expired: [],
})
const currency = (clusters) => ({
  live: clusters.map((cluster) => ({ cluster })),
  fixed: [],
  unverifiable: [],
})

test('oldest member keeps class identity when a smaller canonical theme key joins next week', () => {
  const old = clusterNotes([note('old', 'Zebra defect', 'one', '2026-10-01')])
  const next = clusterNotes([
    note('old', 'Zebra defect', 'one', '2026-10-01'),
    note('new', 'Alpha defect', 'two', '2026-10-08'),
  ])
  const weekOne = mergeClusters(old, [old.map((c) => c.key)])
  const weekTwo = mergeClusters(next, [next.map((c) => c.key)])
  assert.notEqual(weekOne[0].key, weekTwo[0].key)
  const first = classify(selection(weekOne), currency(weekOne))[0]
  const second = classify(selection(weekTwo), currency(weekTwo))[0]
  assert.equal(first.classKey, second.classKey)
  assert.deepEqual(
    second.aliases,
    next.map((c) => c.key),
  )
  assert.equal(first.eligible, false)
  assert.equal(second.eligible, true)
  assert.equal(second.runs, 2)
})

test('eligibility counts runs rather than notes and requires live currency', () => {
  const clusters = clusterNotes([
    note('a', 'Defect', 'same', '2026-10-01'),
    note('b', 'Defect', 'same', '2026-10-02'),
  ])
  assert.equal(classify(selection(clusters), currency(clusters))[0].eligible, false)
  const buckets = { live: [], fixed: [{ cluster: clusters[0] }], unverifiable: [] }
  assert.equal(classify(selection([]), buckets, { minRuns: 1 })[0].eligible, false)
  assert.equal(classify(selection(clusters), currency(clusters), { minRuns: 1 })[0].eligible, true)
})

test('nothing-seen-twice reports exact class, run, coverage and eligibility counts', () => {
  const classes = [
    { classKey: 'one', runs: 1, eligible: false, covered: false },
    { classKey: 'two', runs: 2, eligible: false, covered: true },
  ]
  assert.deepEqual(report(classes), {
    outcome: 'nothing-seen-twice',
    counts: { classes: 2, byRuns: { 1: 1, '2+': 1 }, covered: 1, eligible: 0 },
  })
  assert.deepEqual(report([]).counts.byRuns, { 1: 0, '2+': 0 })
})

test('report resolves promotions through aliases and lists the stable class and PR', () => {
  const classes = [
    { classKey: 'old', aliases: ['new', 'old'], title: 'Defect', runs: 2, eligible: true },
  ]
  assert.deepEqual(
    report(classes, [
      { promotions: [{ classKey: 'new', pr: 'https://example.test/pull/1', remedy: 'guard' }] },
    ]),
    {
      outcome: 'promoted',
      promotions: [
        { classKey: 'old', title: 'Defect', pr: 'https://example.test/pull/1', remedy: 'guard' },
      ],
      counts: { classes: 1, byRuns: { 1: 0, '2+': 1 }, covered: 0, eligible: 1 },
    },
  )
})

test('classify retains ladder evidence and counts tracked classes once', () => {
  const clusters = clusterNotes([note('a', 'Defect', 'one', '2026-10-01')])
  clusters[0].ladder = { rung: 'check', target: 'src/a.js', existingCheck: 'lint', instances: 3 }
  const selected = selection([])
  selected.dropped.push({ cluster: clusters[0], reason: 'already-tracked' })
  const [result] = classify(selected, currency(clusters))
  assert.equal(result.covered, true)
  assert.equal(result.rung, 'check')
  assert.equal(result.existingCheck, 'lint')
  assert.equal(result.instances, 3)
})

test('CLI rejects unknown flags and invalid thresholds', () => {
  assert.throws(
    () => runCli(['classify', 'selection.json', 'currency.json', '--unknown']),
    /Unknown option/,
  )
  assert.throws(
    () => runCli(['classify', 'selection.json', 'currency.json', '--min-runs', '0']),
    /positive integer/,
  )
})

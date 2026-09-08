import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { formatGateOutcomeLine } from '../skills-toolbox/gate-outcome.mjs'
import {
  INSUFFICIENT_DATA_THRESHOLD,
  aggregateGateOutcomes,
} from '../skills-toolbox/gate-outcome-report.mjs'
import {
  NO_DATA_HEADING,
  RARITY_CAVEAT,
  buildReport,
  formatFireRate,
  parseArgs,
  renderGateOutcomeReport,
  runCli,
} from './sweep-gate-outcomes-gate.mjs'

const scratch = () => mkdtempSync(join(tmpdir(), 'sweep-gate-outcomes-'))

/** N recorded lines for one gate, `fires` of them firing. */
const runs = (gateId, invocations, fires = 0) =>
  Array.from({ length: invocations }, (_, i) =>
    formatGateOutcomeLine(gateId, i < fires ? 'fire' : 'pass', i < fires ? 'premise-drift' : 'ok'),
  ).join('')

/** A store directory holding one file per named run. */
const storeWith = (files) => {
  const dir = scratch()
  for (const [name, text] of Object.entries(files)) writeFileSync(join(dir, name), text)
  return dir
}

/** The rendered row for one gate, as whitespace-collapsed columns. */
const rowFor = (text, gateId) => {
  const line = text.split('\n').find((candidate) => candidate.startsWith(`${gateId} `))
  return line ? line.trim().split(/\s+/) : null
}

// ---------------------------------------------------------------------------
// The per-gate table: invocations, fires, fire rate.
// ---------------------------------------------------------------------------

test('prints invocation count, fire count and fire rate per gate id', () => {
  const dir = storeWith({
    'run-a.tsv': runs('plan-image-guard', 40, 4),
    'run-b.tsv': runs('plan-contract-guard', 25, 5) + runs('plan-image-guard', 10, 1),
  })
  const text = runCli(['report', '--store', dir])

  assert.deepEqual(rowFor(text, 'plan-image-guard'), [
    'plan-image-guard',
    '50',
    '5',
    '10.0%',
    'fires',
    'regular',
  ])
  assert.deepEqual(rowFor(text, 'plan-contract-guard'), [
    'plan-contract-guard',
    '25',
    '5',
    '20.0%',
    'fires',
    'regular',
  ])
  assert.match(text, /INVOCATIONS/)
  assert.match(text, /FIRE RATE/)
  assert.ok(text.includes(RARITY_CAVEAT))
})

test('the json subcommand emits the aggregate plus what it had to skip', () => {
  const dir = storeWith({ 'run-a.tsv': runs('plan-image-guard', 30, 3) + 'torn' })
  const parsed = JSON.parse(runCli(['json', '--store', dir]))
  assert.equal(parsed.hasData, true)
  assert.equal(parsed.totals.invocations, 30)
  assert.equal(parsed.totals.fires, 3)
  assert.equal(parsed.skippedLines, 1)
  assert.equal(parsed.sources, 1)
  assert.equal(parsed.threshold, INSUFFICIENT_DATA_THRESHOLD)
})

test('reads a single run file when --store names one', () => {
  const dir = storeWith({ 'run-a.tsv': runs('plan-image-guard', 25, 1) })
  const parsed = JSON.parse(runCli(['json', '--store', join(dir, 'run-a.tsv')]))
  assert.equal(parsed.totals.invocations, 25)
  assert.equal(parsed.sources, 1)
})

test('the rarity caveat is real text, so asserting its presence is not vacuous', () => {
  // `includes('')` is true of every string. Without this the caveat could be
  // deleted from the output and every "report carries the caveat" assertion
  // below would stay green.
  assert.ok(RARITY_CAVEAT.length > 20)
  assert.match(RARITY_CAVEAT, /^Rarity is not worthlessness:/)
})

test('an absent rate renders as an absent rate, never as zero percent', () => {
  assert.equal(formatFireRate(null), '—')
  assert.equal(formatFireRate(undefined), '—')
  assert.equal(formatFireRate(0), '0.0%')
  assert.equal(formatFireRate(0.125), '12.5%')
})

test('parses the flags it accepts and ignores the rest', () => {
  assert.deepEqual(parseArgs(['--threshold', '5', '--store', '/tmp/s', '--gates', '/tmp/g.json']), {
    threshold: 5,
    store: '/tmp/s',
    gates: '/tmp/g.json',
  })
  assert.deepEqual(parseArgs(['stray', '--unknown', 'x']), {})
})

test('an unknown subcommand is refused rather than reported on', () => {
  assert.throws(() => runCli(['retire']), /unknown subcommand: retire/)
})

// ---------------------------------------------------------------------------
// Insufficient data is never a retirement candidate.
// ---------------------------------------------------------------------------

test('a gate below the threshold is reported as insufficient data, not as a candidate', () => {
  const below = INSUFFICIENT_DATA_THRESHOLD - 1
  const dir = storeWith({
    'run-a.tsv': runs('thin-gate', below, 0) + runs('busy-gate', 40, 2),
  })
  const text = runCli(['report', '--store', dir])

  assert.deepEqual(rowFor(text, 'thin-gate'), [
    'thin-gate',
    String(below),
    '0',
    '0.0%',
    'insufficient-data',
    'unknown',
  ])
  assert.match(text, /Retirement candidates: none/)
  assert.ok(!text.includes('  thin-gate\n'), 'thin-gate must not be listed as a candidate')

  const parsed = JSON.parse(runCli(['json', '--store', dir]))
  assert.deepEqual(parsed.retirementCandidates, [])
})

test('a zero-invocation gate is not a retirement candidate at --threshold 0', () => {
  // End-to-end over the CLI, which parses --threshold with no lower bound.
  const dir = storeWith({ 'run-a.tsv': runs('busy-gate', 40, 2) })
  const roster = join(dir, 'roster.json')
  writeFileSync(roster, JSON.stringify(['never-run-gate']))
  const text = runCli(['report', '--store', dir, '--gates', roster, '--threshold', '0'])

  assert.deepEqual(rowFor(text, 'never-run-gate'), [
    'never-run-gate',
    '0',
    '0',
    '\u2014',
    'insufficient-data',
    'unknown',
  ])
  assert.ok(
    !text.includes('  never-run-gate\n'),
    'a gate that never ran must never be a retirement candidate, at any threshold',
  )

  const parsed = JSON.parse(runCli(['json', '--store', dir, '--gates', roster, '--threshold', '0']))
  assert.ok(!parsed.retirementCandidates.includes('never-run-gate'))
})

test('a gate with zero recorded invocations is insufficient data, not a candidate', () => {
  const dir = storeWith({ 'run-a.tsv': runs('busy-gate', 40, 2) })
  const roster = join(dir, 'roster.json')
  writeFileSync(roster, JSON.stringify(['never-run-gate']))
  const text = runCli(['report', '--store', dir, '--gates', roster])

  assert.deepEqual(rowFor(text, 'never-run-gate'), [
    'never-run-gate',
    '0',
    '0',
    '—',
    'insufficient-data',
    'unknown',
  ])
  assert.match(text, /Retirement candidates: none/)
  assert.deepEqual(
    JSON.parse(runCli(['json', '--store', dir, '--gates', roster])).retirementCandidates,
    [],
  )
})

test('a gate silent across enough runs IS listed, and only that gate', () => {
  const dir = storeWith({
    'run-a.tsv':
      runs('silent-gate', INSUFFICIENT_DATA_THRESHOLD, 0) +
      runs('thin-gate', 2, 0) +
      runs('busy-gate', 40, 2),
  })
  const text = runCli(['report', '--store', dir])
  assert.match(text, /Retirement candidates \(never fired in >= 20 recorded invocations\):/)
  assert.match(text, /\n {2}silent-gate$/m)
  assert.ok(!/\n {2}thin-gate$/m.test(text), 'thin-gate must not be listed as a candidate')
  assert.ok(!/\n {2}busy-gate$/m.test(text), 'busy-gate must not be listed as a candidate')
})

test('a malformed roster costs the zero-invocation rows, never the counted ones', () => {
  const dir = storeWith({ 'run-a.tsv': runs('busy-gate', 40, 2) })
  const text = runCli(['report', '--store', dir, '--gates', join(dir, 'missing.json')])
  assert.deepEqual(rowFor(text, 'busy-gate'), ['busy-gate', '40', '2', '5.0%', 'fires', 'regular'])
})

// ---------------------------------------------------------------------------
// Degrade, never abort.
// ---------------------------------------------------------------------------

test('malformed lines mixed with valid ones are skipped and the report still prints', () => {
  const dir = storeWith({
    'run-a.tsv': [
      formatGateOutcomeLine('plan-image-guard', 'pass', 'ok'),
      'garbage line with spaces and CAPS\n',
      formatGateOutcomeLine('plan-image-guard', 'fire', 'unreadable-input'),
      '2026-09-08T00:00:00.000Z\tplan-image-guard\tmaybe\tok\n',
      formatGateOutcomeLine('plan-image-guard', 'pass', 'ok'),
      '2026-09-08T00:00:0',
    ].join(''),
  })
  const text = runCli(['report', '--store', dir])

  assert.deepEqual(rowFor(text, 'plan-image-guard'), [
    'plan-image-guard',
    '3',
    '1',
    '33.3%',
    'fires',
    'unknown',
  ])
  assert.match(text, /degraded: 3 malformed line\(s\) skipped/)
})

test('an unreadable run file degrades the report instead of aborting it', () => {
  const dir = storeWith({
    'run-a.tsv': runs('plan-image-guard', 25, 1),
    'run-b.tsv': runs('plan-image-guard', 5, 0),
  })
  const text = runCli(['report', '--store', dir], {
    readFile: (path) => {
      if (path.endsWith('run-b.tsv')) throw new Error('EACCES')
      return runs('plan-image-guard', 25, 1)
    },
  })
  assert.match(text, /1 unreadable file\(s\) skipped/)
  assert.deepEqual(rowFor(text, 'plan-image-guard'), [
    'plan-image-guard',
    '25',
    '1',
    '4.0%',
    'fires',
    'rare',
  ])
})

// ---------------------------------------------------------------------------
// An empty store is NO DATA, not "no gate fires".
// ---------------------------------------------------------------------------

test('an absent store reports no data rather than that no gate fires', () => {
  const text = runCli(['report', '--store', join(scratch(), 'never-created')])
  assert.ok(text.startsWith(NO_DATA_HEADING))
  assert.match(text, /absence of records, not a finding about the gates/)
  assert.match(text, /No gate is a retirement candidate on this report\./)
  assert.ok(!text.includes('Retirement candidates ('), 'no candidate list on an empty store')
  assert.ok(!/\bFIRE RATE\b/.test(text), 'no table on an empty store')
  assert.ok(text.includes(RARITY_CAVEAT))
})

test('an empty store reports no data even when a known-gate roster is supplied', () => {
  const dir = scratch()
  const roster = join(dir, 'roster.json')
  writeFileSync(roster, JSON.stringify(['plan-image-guard', 'plan-contract-guard']))
  const text = runCli(['report', '--store', dir, '--gates', roster])

  assert.ok(text.startsWith(NO_DATA_HEADING))
  assert.match(text, /Gates with no recorded invocation at all:/)
  assert.match(text, /\n {2}plan-image-guard — insufficient-data$/m)
  assert.deepEqual(
    JSON.parse(runCli(['json', '--store', dir, '--gates', roster])).retirementCandidates,
    [],
  )
})

test('a store of only malformed lines reports no data and says what it skipped', () => {
  const dir = storeWith({ 'run-a.tsv': 'garbage\nmore garbage\n' })
  const text = runCli(['report', '--store', dir])
  assert.ok(text.startsWith(NO_DATA_HEADING))
  assert.match(text, /Degraded: 2 malformed line\(s\) skipped\./)
})

test('the no-data rendering never claims a zero fire count', () => {
  const text = renderGateOutcomeReport(aggregateGateOutcomes([]), {
    store: { kind: 'directory', path: '/tmp/boss-gate-outcomes' },
    files: [],
  })
  assert.ok(!/0 fire/.test(text), 'no-data output must not state a fire count')
  assert.match(text, /\/tmp\/boss-gate-outcomes/)
})

// ---------------------------------------------------------------------------
// Store resolution.
// ---------------------------------------------------------------------------

test('with no --store the report reads the recorder default destination', () => {
  const dir = storeWith({ 'run-a.tsv': runs('plan-image-guard', 25, 1) })
  const { report, read } = buildReport({}, { store: { kind: 'directory', path: dir } })
  assert.equal(read.files.length, 1)
  assert.equal(report.totals.invocations, 25)
})

test('an explicit threshold moves the verdict boundary and is reported with it', () => {
  const dir = storeWith({ 'run-a.tsv': runs('thin-gate', 5, 0) })
  const parsed = JSON.parse(runCli(['json', '--store', dir, '--threshold', '5']))
  assert.equal(parsed.threshold, 5)
  assert.deepEqual(parsed.retirementCandidates, ['thin-gate'])
  assert.match(runCli(['report', '--store', dir]), /insufficient-data threshold: 20 invocation/)
})

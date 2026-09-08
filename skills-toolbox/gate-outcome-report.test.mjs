import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { GATE_OUTCOME_FILE_ENV, formatGateOutcomeLine } from './gate-outcome.mjs'
import {
  GATE_RARITY_LABELS,
  GATE_REPORT_VERDICTS,
  INSUFFICIENT_DATA_THRESHOLD,
  aggregateGateOutcomes,
  gateRarity,
  gateVerdict,
  listGateOutcomeFiles,
  parseGateOutcomeLine,
  parseGateOutcomeText,
  readGateOutcomeStore,
  resolveGateOutcomeStore,
} from './gate-outcome-report.mjs'

const scratch = () => mkdtempSync(join(tmpdir(), 'gate-outcome-report-'))

/** Fixture lines built through the recorder, so the reader is tested against real records. */
const recorded = (gateId, outcome, reason = 'ok') =>
  formatGateOutcomeLine(gateId, outcome, reason).trimEnd()

/** N lines for one gate: `fires` of them firing, the rest passing. */
const runs = (gateId, invocations, fires = 0) =>
  Array.from({ length: invocations }, (_, i) =>
    recorded(gateId, i < fires ? 'fire' : 'pass', i < fires ? 'premise-drift' : 'ok'),
  )

const gateNamed = (report, gateId) => report.gates.find((gate) => gate.gateId === gateId)

// ---------------------------------------------------------------------------
// Parsing: the recorder's grammar is the only grammar.
// ---------------------------------------------------------------------------

test('parses a recorded line into its four fields', () => {
  const parsed = parseGateOutcomeLine(recorded('plan-run-guards.premises', 'fire', 'premise-drift'))
  assert.equal(parsed.gateId, 'plan-run-guards.premises')
  assert.equal(parsed.outcome, 'fire')
  assert.equal(parsed.reason, 'premise-drift')
  assert.match(parsed.timestamp, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/)
})

test('refuses lines the recorder could not have written', () => {
  assert.equal(parseGateOutcomeLine('2026-09-08T00:00:00.000Z\tplan-image-guard\tmaybe\tok'), null)
  assert.equal(parseGateOutcomeLine('2026-09-08T00:00:00.000Z\tPlan-Image-Guard\tpass\tok'), null)
  assert.equal(parseGateOutcomeLine('not a record at all'), null)
  assert.equal(parseGateOutcomeLine(''), null)
  assert.equal(parseGateOutcomeLine(undefined), null)
})

test('skips a torn final line and keeps every complete record before it', () => {
  const text = `${runs('plan-image-guard', 3).join('\n')}\n2026-09-08T00:00:0`
  const { records, skipped } = parseGateOutcomeText(text)
  assert.equal(records.length, 3)
  assert.equal(skipped, 1)
})

test('a blank line is the terminator, not a malformed record', () => {
  const { records, skipped } = parseGateOutcomeText(`${recorded('plan-image-guard', 'pass')}\n\n`)
  assert.equal(records.length, 1)
  assert.equal(skipped, 0)
})

// ---------------------------------------------------------------------------
// Aggregation: counts, rate, and the verdict split.
// ---------------------------------------------------------------------------

test('reports invocations, fires and fire rate per gate id', () => {
  const records = [
    ...runs('plan-image-guard', 40, 4),
    ...runs('plan-contract-guard', 25, 5),
    // The verb separator is part of a gate id: two verbs of one CLI are two gates.
    ...runs('plan-run-guards.premises', 30, 0),
  ].flatMap((line) => parseGateOutcomeText(line).records)
  const report = aggregateGateOutcomes(records)

  assert.equal(report.hasData, true)
  assert.equal(report.totals.gates, 3)
  assert.equal(report.totals.records, 95)
  assert.equal(report.totals.fires, 9)

  const image = gateNamed(report, 'plan-image-guard')
  assert.equal(image.invocations, 40)
  assert.equal(image.fires, 4)
  assert.equal(image.passes, 36)
  assert.equal(image.fireRate, 0.1)

  const premises = gateNamed(report, 'plan-run-guards.premises')
  assert.equal(premises.invocations, 30)
  assert.equal(premises.fires, 0)
  assert.equal(premises.fireRate, 0)

  // Deterministic ordering: invocations desc, then gate id.
  assert.deepEqual(
    report.gates.map((gate) => gate.gateId),
    ['plan-image-guard', 'plan-run-guards.premises', 'plan-contract-guard'],
  )
})

test('every verdict and rarity label the report emits is a published member', () => {
  const records = [
    ...runs('busy-gate', 40, 20),
    ...runs('rare-gate', 100, 1),
    ...runs('silent-gate', 40, 0),
    ...runs('thin-gate', 3, 0),
  ].flatMap((line) => parseGateOutcomeText(line).records)
  const report = aggregateGateOutcomes(records)
  for (const gate of report.gates) {
    assert.ok(GATE_REPORT_VERDICTS.includes(gate.verdict), `verdict ${gate.verdict}`)
    assert.ok(GATE_RARITY_LABELS.includes(gate.rarity), `rarity ${gate.rarity}`)
  }
  assert.equal(gateNamed(report, 'busy-gate').rarity, 'regular')
  assert.equal(gateNamed(report, 'rare-gate').rarity, 'rare')
  assert.equal(gateNamed(report, 'silent-gate').rarity, 'never')
  assert.equal(gateNamed(report, 'thin-gate').rarity, 'unknown')
})

test('rarity is a label, never a retirement verdict', () => {
  const records = parseGateOutcomeText(runs('rare-gate', 200, 1).join('\n')).records
  const rare = gateNamed(aggregateGateOutcomes(records), 'rare-gate')
  assert.equal(rare.rarity, 'rare')
  assert.equal(rare.verdict, 'fires')
  assert.equal(rare.retirementCandidate, false)
})

// ---------------------------------------------------------------------------
// The distinction the whole report exists for.
// ---------------------------------------------------------------------------

test('a gate below the threshold is insufficient data, not a retirement candidate', () => {
  const below = INSUFFICIENT_DATA_THRESHOLD - 1
  const records = parseGateOutcomeText(runs('thin-gate', below, 0).join('\n')).records
  const report = aggregateGateOutcomes(records)
  const thin = gateNamed(report, 'thin-gate')

  assert.equal(thin.invocations, below)
  assert.equal(thin.fires, 0)
  assert.equal(thin.verdict, 'insufficient-data')
  assert.equal(thin.retirementCandidate, false)
  assert.deepEqual(report.retirementCandidates, [])
})

test('a gate with zero recorded invocations is insufficient data, not a retirement candidate', () => {
  const report = aggregateGateOutcomes([], { gateIds: ['never-run-gate'] })
  const gate = gateNamed(report, 'never-run-gate')

  assert.equal(gate.invocations, 0)
  assert.equal(gate.fires, 0)
  // 0/0 is not 0%: an absent rate must not render as a perfect pass record.
  assert.equal(gate.fireRate, null)
  assert.equal(gate.verdict, 'insufficient-data')
  assert.equal(gate.retirementCandidate, false)
  assert.deepEqual(report.retirementCandidates, [])
})

test('a zero-invocation gate survives an operator threshold of 0 or below', () => {
  // `--threshold` is an advertised operator knob with no lower bound, and at 0
  // the `invocations >= threshold` comparison alone would make `0 >= 0` true.
  // The zero-invocation guarantee is unconditional in both the module header and
  // docs/skills/gate-firing-rates.md, so it must hold at every threshold.
  for (const threshold of [0, -3]) {
    assert.equal(gateVerdict(0, 0, threshold), 'insufficient-data')
    assert.equal(gateRarity(0, 0, threshold), 'unknown')

    const report = aggregateGateOutcomes([], { threshold, gateIds: ['never-run-gate'] })
    const gate = gateNamed(report, 'never-run-gate')
    assert.equal(gate.verdict, 'insufficient-data')
    assert.equal(gate.rarity, 'unknown')
    assert.equal(gate.retirementCandidate, false)
    assert.deepEqual(report.retirementCandidates, [])
  }

  // The guard is scoped to the zero case only: a gate that actually ran and
  // stayed silent is still evidence at a threshold it clears.
  assert.equal(gateVerdict(1, 0, 0), 'never-fired')
})

test('silence becomes evidence only at the threshold, and the two verdicts stay distinct', () => {
  assert.equal(gateVerdict(INSUFFICIENT_DATA_THRESHOLD - 1, 0), 'insufficient-data')
  assert.equal(gateVerdict(INSUFFICIENT_DATA_THRESHOLD, 0), 'never-fired')
  assert.equal(gateVerdict(INSUFFICIENT_DATA_THRESHOLD, 1), 'fires')
  assert.notEqual(gateVerdict(0, 0), gateVerdict(INSUFFICIENT_DATA_THRESHOLD, 0))
  assert.equal(gateRarity(INSUFFICIENT_DATA_THRESHOLD - 1, 0), 'unknown')
  assert.equal(gateRarity(INSUFFICIENT_DATA_THRESHOLD, 0), 'never')
})

test('a gate at the threshold that never fired is the only retirement candidate', () => {
  const records = [
    ...runs('silent-gate', INSUFFICIENT_DATA_THRESHOLD, 0),
    ...runs('thin-gate', 2, 0),
    ...runs('firing-gate', 50, 1),
  ].flatMap((line) => parseGateOutcomeText(line).records)
  const report = aggregateGateOutcomes(records)
  assert.deepEqual(report.retirementCandidates, ['silent-gate'])
})

test('the threshold is overridable per call without changing the pinned default', () => {
  const records = parseGateOutcomeText(runs('thin-gate', 5, 0).join('\n')).records
  assert.equal(gateNamed(aggregateGateOutcomes(records), 'thin-gate').verdict, 'insufficient-data')
  assert.equal(
    gateNamed(aggregateGateOutcomes(records, { threshold: 5 }), 'thin-gate').verdict,
    'never-fired',
  )
  assert.equal(INSUFFICIENT_DATA_THRESHOLD, 20)
})

// ---------------------------------------------------------------------------
// Degrade, never abort.
// ---------------------------------------------------------------------------

test('malformed lines mixed with valid ones are skipped, not fatal', () => {
  const dir = scratch()
  const file = join(dir, 'run-a.tsv')
  writeFileSync(
    file,
    [
      recorded('plan-image-guard', 'pass'),
      'garbage line with spaces and CAPS',
      recorded('plan-image-guard', 'fire', 'unreadable-input'),
      '2026-09-08T00:00:00.000Z\tplan-image-guard\tmaybe\tok',
      recorded('plan-image-guard', 'pass'),
      '2026-09-08T00:00:0',
    ].join('\n') + '\n',
  )

  const read = readGateOutcomeStore({ store: { kind: 'file', path: file } })
  assert.equal(read.records.length, 3)
  assert.equal(read.skippedLines, 3)

  const report = aggregateGateOutcomes(read.records)
  assert.equal(report.hasData, true)
  const gate = gateNamed(report, 'plan-image-guard')
  assert.equal(gate.invocations, 3)
  assert.equal(gate.fires, 1)
})

test('an unreadable file is counted, and the other runs still report', () => {
  const dir = scratch()
  writeFileSync(join(dir, 'run-a.tsv'), runs('plan-image-guard', 25, 2).join('\n') + '\n')
  writeFileSync(join(dir, 'run-b.tsv'), runs('plan-image-guard', 5, 0).join('\n') + '\n')

  const read = readGateOutcomeStore({
    store: { kind: 'directory', path: dir },
    readFile: (path) => {
      if (path.endsWith('run-b.tsv')) throw new Error('EACCES')
      return readFileSync(path, 'utf8')
    },
  })
  assert.equal(read.unreadableFiles, 1)
  assert.equal(read.records.length, 25)
  assert.equal(gateNamed(aggregateGateOutcomes(read.records), 'plan-image-guard').fires, 2)
})

test('aggregation ignores records whose gate id or outcome is missing', () => {
  const report = aggregateGateOutcomes([
    { gateId: 'plan-image-guard', outcome: 'pass' },
    { gateId: '', outcome: 'fire' },
    { gateId: 'plan-image-guard', outcome: 'exploded' },
    { outcome: 'pass' },
    null,
  ])
  assert.equal(report.totals.records, 1)
  assert.equal(gateNamed(report, 'plan-image-guard').invocations, 1)
})

// ---------------------------------------------------------------------------
// Empty store is NO DATA, not "no gate fires".
// ---------------------------------------------------------------------------

test('an absent store reports no data and names no retirement candidate', () => {
  const read = readGateOutcomeStore({
    store: { kind: 'directory', path: join(scratch(), 'does-not-exist') },
  })
  assert.deepEqual(read.files, [])
  assert.equal(read.records.length, 0)

  const report = aggregateGateOutcomes(read.records)
  assert.equal(report.hasData, false)
  assert.deepEqual(report.gates, [])
  assert.deepEqual(report.retirementCandidates, [])
  assert.equal(report.totals.invocations, 0)
})

test('an empty store with a known-gate roster still names no retirement candidate', () => {
  const report = aggregateGateOutcomes([], { gateIds: ['plan-image-guard', 'plan-contract-guard'] })
  assert.equal(report.hasData, false)
  assert.equal(report.gates.length, 2)
  assert.deepEqual(report.retirementCandidates, [])
  for (const gate of report.gates) assert.equal(gate.verdict, 'insufficient-data')
})

test('a store of only malformed lines reports no data rather than zero fires', () => {
  const dir = scratch()
  writeFileSync(join(dir, 'run-a.tsv'), 'garbage\nmore garbage\n')
  const read = readGateOutcomeStore({ store: { kind: 'directory', path: dir } })
  assert.equal(read.skippedLines, 2)
  const report = aggregateGateOutcomes(read.records)
  assert.equal(report.hasData, false)
  assert.deepEqual(report.retirementCandidates, [])
})

// ---------------------------------------------------------------------------
// Store resolution shares the recorder's destination constants.
// ---------------------------------------------------------------------------

test('an explicit outcome file wins over the per-run directory', () => {
  const store = resolveGateOutcomeStore({ [GATE_OUTCOME_FILE_ENV]: '/tmp/explicit.tsv' })
  assert.deepEqual(store, { kind: 'file', path: '/tmp/explicit.tsv' })
})

test('with no explicit file the store is the whole per-run directory', () => {
  const store = resolveGateOutcomeStore({})
  assert.equal(store.kind, 'directory')
  assert.match(store.path, /boss-gate-outcomes$/)
})

test('only recorded .tsv files are read from a directory store', () => {
  const dir = scratch()
  writeFileSync(join(dir, 'run-b.tsv'), '')
  writeFileSync(join(dir, 'run-a.tsv'), '')
  writeFileSync(join(dir, 'notes.txt'), '')
  assert.deepEqual(
    listGateOutcomeFiles({ kind: 'directory', path: dir }).map((path) => path.split('/').pop()),
    ['run-a.tsv', 'run-b.tsv'],
  )
})

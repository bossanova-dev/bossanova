import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { OUTCOMES, coverage, reconcile, record, seedLedger } from './bs-review-ledger.mjs'

const scriptPath = fileURLToPath(new URL('./bs-review-ledger.mjs', import.meta.url))

function runCli(args = []) {
  const res = spawnSync(process.execPath, [scriptPath, ...args], { encoding: 'utf8' })
  return { stdout: res.stdout.trim(), stderr: res.stderr.trim(), status: res.status }
}

function fixturePopulations() {
  return {
    lenses: [
      { lens: 'go', skill: 'golang-pro' },
      { lens: 'api', skill: 'api-review' },
    ],
    rounds: [{ name: 'boss-review-ce' }],
    defaultRounds: [{ capability: 'second-voice' }],
  }
}

function lensEnvelope(extension, items = []) {
  return JSON.stringify({ ok: true, extension, role: 'lens', items })
}

test('seed creates one not-reached row per lens, round extension, and default round', () => {
  const ledger = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
  assert.deepEqual(
    ledger.rows.map((row) => [row.name, row.phase, row.outcome]),
    [
      ['lens:api', 'Phase 1', 'not-reached'],
      ['lens:go', 'Phase 1', 'not-reached'],
      ['default:second-voice', 'Phase D', 'not-reached'],
      ['round:boss-review-ce', 'Phase R', 'not-reached'],
    ],
  )
  for (const row of ledger.rows) {
    assert.deepEqual(Object.keys(row), [
      'name',
      'phase',
      'tier',
      'mode',
      'outcome',
      'cause',
      'completedAtMs',
      'durationMs',
    ])
  }
})

test('a lens with no Tier-1 extension still gets a row', () => {
  const ledger = seedLedger({
    runId: 'run-1',
    populations: {
      lenses: [{ lens: 'db', skill: 'database-review' }],
      rounds: [],
      defaultRounds: [],
    },
    now: 100,
  })
  assert.equal(ledger.rows.length, 1)
  assert.equal(ledger.rows[0].name, 'lens:db')
})

test('not-reached stays distinct from skipped, timed-out, and completed', () => {
  const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
  const skipped = record(seeded, 'default:second-voice', {
    tier: 'default',
    outcome: OUTCOMES.skipped,
    cause: 'probe not_installed',
  })
  const timed = record(skipped, 'round:boss-review-ce', {
    tier: 'tier1',
    outcome: OUTCOMES.timedOut,
    cause: 'timeout',
  })
  const counts = coverage(timed)
  assert.deepEqual(counts, {
    discovered: 4,
    completed: 0,
    skipped: 1,
    timedOut: 1,
    notReached: 2,
  })
})

test('reconcile preserves not-reached when nothing upgraded the row', () => {
  const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
  const reconciled = reconcile(seeded, { populations: fixturePopulations() })
  assert.equal(reconciled.rows.find((row) => row.name === 'lens:go').outcome, 'not-reached')
})

test('reconcile re-seeds truncated ledgers instead of reporting full coverage', () => {
  const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
  const truncated = { ...seeded, rows: seeded.rows.filter((row) => row.name !== 'lens:api') }
  const reconciled = reconcile(truncated, { populations: fixturePopulations() })
  assert.equal(reconciled.rows.find((row) => row.name === 'lens:api').outcome, 'not-reached')
  assert.equal(coverage(reconciled).discovered, 4)
})

test('completed is reproducible from findings files alone', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-lens-0-golang-pro.json'), `${lensEnvelope('golang-pro')}\n`)
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const stripped = {
      ...seeded,
      rows: seeded.rows.map((row) => ({ ...row, outcome: 'not-reached' })),
    }
    const reconciled = reconcile(stripped, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'lens:go')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.tier, 'tier1')
    assert.equal(row.mode, 'dispatched')
    assert.equal(typeof row.completedAtMs, 'number')
    assert.equal(typeof row.durationMs, 'number')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('completed round extension files reconcile with Tier 1 provenance', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-round-boss-review-ce.json'), '[]\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'round:boss-review-ce')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.tier, 'tier1')
    assert.equal(row.mode, 'dispatched')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('indexed lens findings resolve through the original matched-lens order', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-lens-1-api-review.json'), `${lensEnvelope('api-review')}\n`)
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    assert.equal(reconciled.rows.find((r) => r.name === 'lens:api').outcome, 'completed')
    assert.equal(reconciled.rows.find((r) => r.name === 'lens:go').outcome, 'not-reached')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('duplicate matched lens ids keep one ledger row per entry', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  const populations = {
    lenses: [
      { lens: 'shared', skill: 'first-reviewer' },
      { lens: 'shared', skill: 'second-reviewer' },
    ],
    rounds: [],
    defaultRounds: [],
  }
  try {
    writeFileSync(
      join(dir, 'findings-lens-1-second-reviewer.json'),
      `${lensEnvelope('second-reviewer')}\n`,
    )
    const seeded = seedLedger({ runId: 'run-1', populations, now: 100 })
    assert.deepEqual(
      seeded.rows.map((row) => row.name),
      ['lens:0:shared', 'lens:1:shared'],
    )
    const reconciled = reconcile(seeded, { findingsDir: dir, populations })
    assert.equal(reconciled.rows.find((r) => r.name === 'lens:0:shared').outcome, 'not-reached')
    assert.equal(reconciled.rows.find((r) => r.name === 'lens:1:shared').outcome, 'completed')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('a successful sibling lens extension completes a skipped lens row', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-lens-0-golang-pro.json'), `${lensEnvelope('golang-pro')}\n`)
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const skipped = record(seeded, 'lens:go', {
      phase: 'Phase 1',
      tier: 'tier1',
      outcome: OUTCOMES.skipped,
      cause: 'extension unavailable',
    })
    const reconciled = reconcile(skipped, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'lens:go')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.cause, null)
    assert.equal(row.tier, 'tier1')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('bare Tier-1 lens arrays do not count as completed findings', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-lens-0-golang-pro.json'), '[]\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    assert.equal(reconciled.rows.find((r) => r.name === 'lens:go').outcome, 'not-reached')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('invalid findings sources do not count as completed', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-round-boss-review-ce.json'), '[]\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, {
      findingsDir: dir,
      populations: fixturePopulations(),
      invalid: [{ source: { filename: 'findings-round-boss-review-ce.json' } }],
    })
    assert.equal(
      reconciled.rows.find((row) => row.name === 'round:boss-review-ce').outcome,
      'not-reached',
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('reconcile preserves explicitly recorded terminal outcomes', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-round-boss-review-ce.json'), '[]\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const skipped = record(seeded, 'round:boss-review-ce', {
      phase: 'Phase R',
      outcome: OUTCOMES.skipped,
      cause: 'timeout after envelope',
    })
    const reconciled = reconcile(skipped, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'round:boss-review-ce')
    assert.equal(row.outcome, 'skipped')
    assert.equal(row.cause, 'timeout after envelope')
    assert.equal(row.completedAtMs, null)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('tier marker maps selected fallback rows to inlined or dispatched mode', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const findings = join(dir, 'findings-round-inline.json')
    writeFileSync(findings, '[]\n')
    writeFileSync(`${findings}.tier`, 'inlined\n')
    const seeded = seedLedger({
      runId: 'run-1',
      populations: { lenses: [], rounds: [{ name: 'boss-review-ce' }], defaultRounds: [] },
      now: 100,
    })
    const reconciled = reconcile(seeded, {
      findingsDir: dir,
      populations: { rounds: [{ name: 'boss-review-ce' }] },
    })
    const row = reconciled.rows.find((r) => r.name === 'round:inline')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.mode, 'inlined')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('fallback review files reconcile into selected fallback rows', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const findings = join(dir, 'findings-round-builtin.json')
    writeFileSync(findings, '[]\n')
    writeFileSync(`${findings}.tier`, 'dispatched\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'round:builtin')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.tier, 'tier2')
    assert.equal(row.mode, 'dispatched')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('default round findings complete with default provenance', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-round-default-second-voice.json'), '[]\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'default:second-voice')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.tier, 'default')
    assert.equal(row.mode, 'dispatched')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('successful fallback output replaces a provisional fallback skip', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const findings = join(dir, 'findings-round-inline.json')
    writeFileSync(findings, '[]\n')
    writeFileSync(`${findings}.tier`, 'inlined\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const skipped = record(seeded, 'round:inline', {
      phase: 'Phase R',
      tier: 'tier3',
      mode: 'inlined',
      outcome: OUTCOMES.skipped,
      cause: 'selected fallback pending',
    })
    const reconciled = reconcile(skipped, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'round:inline')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.cause, null)
    assert.equal(row.tier, 'tier3')
    assert.equal(row.mode, 'inlined')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('successful lens fallback output replaces a provisional lens skip', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const findings = join(dir, 'findings-lens-0-golang-pro.json')
    writeFileSync(findings, '[]\n')
    writeFileSync(`${findings}.tier`, 'dispatched\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const skipped = record(seeded, 'lens:go', {
      phase: 'Phase 1',
      outcome: OUTCOMES.skipped,
      cause: 'extension unavailable',
    })
    const reconciled = reconcile(skipped, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'lens:go')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.cause, null)
    assert.equal(row.tier, 'tier2')
    assert.equal(row.mode, 'dispatched')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('successful sibling lens extension completes a timed-out lens row', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(join(dir, 'findings-lens-0-golang-pro.json'), `${lensEnvelope('golang-pro')}\n`)
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const timedOut = record(seeded, 'lens:go', {
      phase: 'Phase 1',
      tier: 'tier1',
      outcome: OUTCOMES.timedOut,
      cause: 'extension timeout',
    })
    const reconciled = reconcile(timedOut, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'lens:go')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.cause, null)
    assert.equal(row.tier, 'tier1')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('successful inline lens fallback records tier3 provenance', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const findings = join(dir, 'findings-lens-1-api-review.json')
    writeFileSync(findings, '[]\n')
    writeFileSync(`${findings}.tier`, 'inlined\n')
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'lens:api')
    assert.equal(row.outcome, 'completed')
    assert.equal(row.tier, 'tier3')
    assert.equal(row.mode, 'inlined')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('record keeps provenance modes out of the tier field', () => {
  const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
  const recorded = record(seeded, 'round:inline', {
    phase: 'Phase R',
    tier: 'inlined',
    outcome: OUTCOMES.skipped,
    cause: 'selected fallback pending',
  })
  const row = recorded.rows.find((r) => r.name === 'round:inline')
  assert.equal(row.tier, null)
  assert.equal(row.mode, 'inlined')
})

test('hypothetical fallback rows are not seeded before selection', () => {
  const ledger = seedLedger({
    runId: 'run-1',
    populations: { lenses: [], rounds: [{ name: 'boss-review-ce' }], defaultRounds: [] },
    now: 100,
  })
  assert.deepEqual(
    ledger.rows.map((row) => row.name),
    ['round:boss-review-ce'],
  )
})

test('wrapped findings must pass envelope validation before completing a row', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-boss-review-ce.json'),
      JSON.stringify({ ok: true, items: [] }),
    )
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    assert.equal(
      reconciled.rows.find((row) => row.name === 'round:boss-review-ce').outcome,
      'not-reached',
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('wrapped findings items must pass role validation before completing a row', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-boss-review-ce.json'),
      JSON.stringify({
        ok: true,
        extension: 'boss-review-ce',
        role: 'round',
        items: [{ severity: 'Warning', file: 'x.go' }],
      }),
    )
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    const row = reconciled.rows.find((r) => r.name === 'round:boss-review-ce')
    assert.equal(row.outcome, 'not-reached')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('wrapped findings with invalid items do not complete a row', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-boss-review-ce.json'),
      JSON.stringify({ ok: true, extension: 'boss-review-ce', role: 'round', items: [{}] }),
    )
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    assert.equal(
      reconciled.rows.find((row) => row.name === 'round:boss-review-ce').outcome,
      'not-reached',
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('handled-failure envelopes do not count as completed findings', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    writeFileSync(
      join(dir, 'findings-round-boss-review-ce.json'),
      JSON.stringify({ ok: false, error: 'load failed', items: [] }),
    )
    const seeded = seedLedger({ runId: 'run-1', populations: fixturePopulations(), now: 100 })
    const reconciled = reconcile(seeded, { findingsDir: dir, populations: fixturePopulations() })
    assert.equal(
      reconciled.rows.find((row) => row.name === 'round:boss-review-ce').outcome,
      'not-reached',
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('ledger writes replace atomically without leaving scratch files', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const ledgerPath = join(dir, 'ledger-run-1.json')
    const res = runCli([
      'seed',
      '--run-id',
      'run-1',
      '--populations',
      JSON.stringify(fixturePopulations()),
      '--out',
      ledgerPath,
      '--now',
      '100',
    ])
    assert.equal(res.status, 0, res.stderr)
    assert.equal(existsSync(ledgerPath), true)
    assert.deepEqual(
      readdirSync(dir).filter((name) => name.includes('.tmp')),
      [],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('cli seed, reconcile, and coverage round-trip', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-review-ledger-'))
  try {
    const ledgerPath = join(dir, 'ledger-run-1.json')
    const pops = join(dir, 'populations.json')
    writeFileSync(pops, JSON.stringify(fixturePopulations()))
    let res = runCli([
      'seed',
      '--run-id',
      'run-1',
      '--populations',
      pops,
      '--out',
      ledgerPath,
      '--now',
      '100',
    ])
    assert.equal(res.status, 0, res.stderr)
    writeFileSync(join(dir, 'findings-round-default-second-voice.json'), '[]\n')
    res = runCli([
      'reconcile',
      '--in',
      ledgerPath,
      '--out',
      ledgerPath,
      '--findings-dir',
      dir,
      '--populations',
      pops,
    ])
    assert.equal(res.status, 0, res.stderr)
    res = runCli(['coverage', '--in', ledgerPath])
    assert.equal(res.status, 0, res.stderr)
    assert.deepEqual(JSON.parse(res.stdout), {
      discovered: 4,
      completed: 1,
      skipped: 0,
      timedOut: 0,
      notReached: 3,
    })
    assert.equal(JSON.parse(readFileSync(ledgerPath, 'utf8')).rows.length, 4)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('cli rejects malformed input instead of fabricating a pass', () => {
  const res = runCli(['unknown'])
  assert.notEqual(res.status, 0)
  assert.match(res.stderr, /usage: bs-review-ledger/)
})

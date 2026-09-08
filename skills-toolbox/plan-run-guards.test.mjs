import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  PREMISE_LIMIT,
  planIdempotencePrecheck,
  premiseDrift,
  validateDraftMetadata,
} from './plan-run-guards.mjs'
import { DEFAULT_CONFIG, requiredPlanSections } from './skill-config.mjs'

// Pin this suite's gate-outcome destination. Why, and the test enforcing it: gate-outcome.test.mjs.
process.env.BOSS_GATE_OUTCOME_FILE = path.join(
  mkdtempSync(path.join(tmpdir(), 'gate-outcome-suite-')),
  'outcomes.tsv',
)
const GUARD = fileURLToPath(new URL('./plan-run-guards.mjs', import.meta.url))
const TEST_CONFIG = {
  ...DEFAULT_CONFIG,
  trackerConfig: {
    linear: {
      states: { planned: 'Todo' },
    },
  },
}

const descriptionSummary = (planningLines = ['- Contract: v1']) =>
  `${requiredPlanSections(DEFAULT_CONFIG)
    .map((heading) => `${heading}\n\nBounded metadata summary prose for ${heading}.`)
    .join('\n\n')
    .replace(
      '## Planning\n\nBounded metadata summary prose for ## Planning.',
      `## Planning\n\n${planningLines.join('\n')}`,
    )}\n`

const metadata = (overrides = {}) => ({
  planPath: '.linear-plans/BOS-1-test.md',
  labels: ['improvement'],
  agentFriendly: true,
  estimate: 3,
  priority: 3,
  openQuestions: [],
  descriptionSummary: descriptionSummary(),
  ...overrides,
})

const issue = (overrides = {}) => ({
  id: 'BOS-1',
  status: 'Todo',
  description: descriptionSummary(),
  attachments: [{ id: 'att-1', title: 'Implementation plan (BOS-1)' }],
  ...overrides,
})

test('validateDraftMetadata accepts a well-formed bounded metadata object', () => {
  const result = validateDraftMetadata(metadata())
  assert.equal(result.ok, true)
  assert.deepEqual(result.missing, [])
  assert.deepEqual(result.invalid, [])
  assert.deepEqual(result.violations, [])
})

test('validateDraftMetadata rejects missing descriptionSummary', () => {
  const value = metadata()
  delete value.descriptionSummary
  const result = validateDraftMetadata(value)
  assert.equal(result.ok, false)
  assert.deepEqual(result.missing, ['descriptionSummary'])
})

test('validateDraftMetadata rejects strict type and estimate violations', () => {
  assert.deepEqual(validateDraftMetadata(metadata({ agentFriendly: 'false' })).invalid, [
    'agentFriendly',
  ])
  assert.deepEqual(validateDraftMetadata(metadata({ estimate: 8 })).invalid, ['estimate'])

  const noAtomic = validateDraftMetadata(metadata({ estimate: 5 }))
  assert.equal(noAtomic.ok, false)
  assert.ok(noAtomic.invalid.includes('estimate'))
  assert.ok(noAtomic.violations.some((violation) => violation.code === 'missing-atomic-5'))

  const withAtomic = validateDraftMetadata(
    metadata({
      estimate: 5,
      descriptionSummary: descriptionSummary([
        '- Contract: v1',
        '- Atomic-5: one indivisible cutover',
      ]),
    }),
  )
  assert.equal(withAtomic.ok, true)
})

test('validateDraftMetadata rejects unknown keys and leaked plan bodies in descriptionSummary', () => {
  const unknown = validateDraftMetadata(metadata({ transcript: 'raw run log' }))
  assert.equal(unknown.ok, false)
  assert.ok(unknown.violations.some((violation) => violation.code === 'unknown-key'))

  const leaked = validateDraftMetadata(
    metadata({
      descriptionSummary: descriptionSummary().replace(
        '## Planning',
        '## Extra Plan Body\n\nx\n\n## Planning',
      ),
    }),
  )
  assert.equal(leaked.ok, false)
  assert.ok(
    leaked.violations.some((violation) => violation.code === 'description-summary-unknown-section'),
  )
})

test('planIdempotencePrecheck noops only when all three conjuncts hold', () => {
  assert.deepEqual(planIdempotencePrecheck({ issue: issue(), config: TEST_CONFIG }), {
    action: 'noop',
    reasons: [],
  })
  assert.deepEqual(
    planIdempotencePrecheck({ issue: issue({ status: 'Unplanned' }), config: TEST_CONFIG }),
    {
      action: 'plan',
      reasons: ['state-not-planned'],
    },
  )
  assert.deepEqual(
    planIdempotencePrecheck({ issue: issue({ description: 'too small' }), config: TEST_CONFIG }),
    {
      action: 'plan',
      reasons: ['description-invalid'],
    },
  )
  assert.deepEqual(
    planIdempotencePrecheck({ issue: issue({ attachments: [] }), config: TEST_CONFIG }),
    {
      action: 'plan',
      reasons: ['plan-attachment-missing'],
    },
  )
})

test('planIdempotencePrecheck accepts common tracker state shapes', () => {
  for (const currentIssue of [
    issue({ status: undefined, stateName: 'Todo' }),
    issue({ status: undefined, state: 'Todo' }),
    issue({ status: undefined, state: { name: 'Todo' } }),
  ]) {
    assert.deepEqual(planIdempotencePrecheck({ issue: currentIssue, config: TEST_CONFIG }), {
      action: 'noop',
      reasons: [],
    })
  }
})

test('planIdempotencePrecheck reports every failed conjunct separately', () => {
  assert.deepEqual(
    planIdempotencePrecheck({
      issue: issue({ status: 'Unplanned', description: 'too small', attachments: null }),
      config: TEST_CONFIG,
    }),
    {
      action: 'plan',
      reasons: ['state-not-planned', 'description-invalid', 'plan-attachment-missing'],
    },
  )
})

test('premiseDrift reports drifted, unresolved, and empty inputs', () => {
  assert.deepEqual(premiseDrift([], {}), { ok: true, drifted: [], unresolved: [] })
  assert.deepEqual(premiseDrift(undefined, {}), { ok: true, drifted: [], unresolved: [] })
  assert.deepEqual(
    premiseDrift(
      [
        { id: 'BOS-1', state: 'Todo' },
        { id: 'BOS-2', state: 'In Progress' },
      ],
      { 'BOS-1': 'In Review' },
    ),
    {
      ok: false,
      drifted: [{ id: 'BOS-1', plannedState: 'Todo', currentState: 'In Review' }],
      unresolved: ['BOS-2'],
    },
  )
})

test('premises CLI enforces PREMISE_LIMIT', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-'))
  const premisesPath = path.join(dir, 'premises.json')
  const livePath = path.join(dir, 'live.json')
  writeFileSync(
    premisesPath,
    JSON.stringify(
      Array.from({ length: PREMISE_LIMIT + 1 }, (_, index) => ({
        id: `BOS-${index}`,
        state: 'Todo',
      })),
    ),
  )
  writeFileSync(
    livePath,
    JSON.stringify(
      Object.fromEntries(
        Array.from({ length: PREMISE_LIMIT + 1 }, (_, index) => [`BOS-${index}`, 'Todo']),
      ),
    ),
  )
  const result = spawnSync(process.execPath, [GUARD, 'premises', premisesPath, livePath], {
    encoding: 'utf8',
  })
  assert.equal(result.status, 1)
  assert.match(result.stderr, /premise-limit/)
})

test('premises CLI reports drift as a warning without aborting', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-'))
  const premisesPath = path.join(dir, 'premises.json')
  const livePath = path.join(dir, 'live.json')
  writeFileSync(premisesPath, JSON.stringify([{ id: 'BOS-1', state: 'Todo' }]))
  writeFileSync(livePath, JSON.stringify({ 'BOS-1': 'In Review' }))

  const result = spawnSync(process.execPath, [GUARD, 'premises', premisesPath, livePath], {
    encoding: 'utf8',
  })

  assert.equal(result.status, 0)
  assert.match(result.stderr, /premise-drift/)
})

test('premises CLI aborts when a premise cannot be re-read', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-'))
  const premisesPath = path.join(dir, 'premises.json')
  const livePath = path.join(dir, 'live.json')
  writeFileSync(premisesPath, JSON.stringify([{ id: 'BOS-1', state: 'Todo' }]))
  writeFileSync(livePath, JSON.stringify({}))

  const result = spawnSync(process.execPath, [GUARD, 'premises', premisesPath, livePath], {
    encoding: 'utf8',
  })

  assert.equal(result.status, 1)
  assert.match(result.stderr, /premise-unresolved/)
})

// ---------------------------------------------------------------------------
// BOS-1209: one gate-outcome line per invocation, and the gate id carries the
// VERB — BOS-1211 needs per-verb firing rates, not one summed rate for a CLI
// whose verbs run at different points and refuse for different reasons.
// Recording is telemetry, so each case also asserts the exit code and stderr.
// ---------------------------------------------------------------------------

function runGuardRecording(args, outcomes) {
  return spawnSync(process.execPath, [GUARD, ...args], {
    encoding: 'utf8',
    env: { ...process.env, BOSS_GATE_OUTCOME_FILE: outcomes },
  })
}

const recordedOutcomes = (outcomes) =>
  readFileSync(outcomes, 'utf8')
    .split('\n')
    .filter((line) => line !== '')
    .map((line) => line.split('\t').slice(1))

test('the premises verb records one line per invocation under its own verb id', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-outcomes-'))
  const outcomes = path.join(dir, 'outcomes.tsv')
  const premisesPath = path.join(dir, 'premises.json')
  const livePath = path.join(dir, 'live.json')
  writeFileSync(premisesPath, JSON.stringify([{ id: 'BOS-1', state: 'Todo' }]))
  writeFileSync(livePath, JSON.stringify({ 'BOS-1': 'Todo' }))

  const pass = runGuardRecording(['premises', premisesPath, livePath], outcomes)
  assert.equal(pass.status, 0, pass.stderr)
  assert.deepEqual(recordedOutcomes(outcomes), [['plan-run-guards.premises', 'pass', 'ok']])

  writeFileSync(livePath, JSON.stringify({}))
  const fire = runGuardRecording(['premises', premisesPath, livePath], outcomes)
  assert.equal(fire.status, 1)
  assert.match(fire.stderr, /premise-unresolved/, 'the stderr code must be unchanged')
  assert.deepEqual(recordedOutcomes(outcomes), [
    ['plan-run-guards.premises', 'pass', 'ok'],
    ['plan-run-guards.premises', 'fire', 'premise-unresolved'],
  ])
})

test('the metadata verb records its own verb id for both a pass and a fire', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-outcomes-'))
  const outcomes = path.join(dir, 'outcomes.tsv')
  const good = path.join(dir, 'good.json')
  const bad = path.join(dir, 'bad.json')
  writeFileSync(good, JSON.stringify(metadata()))
  writeFileSync(bad, JSON.stringify({ ...metadata(), estimate: 4 }))

  const pass = runGuardRecording(['metadata', good], outcomes)
  assert.equal(pass.status, 0, pass.stderr)
  assert.deepEqual(recordedOutcomes(outcomes), [['plan-run-guards.metadata', 'pass', 'ok']])

  const fire = runGuardRecording(['metadata', bad], outcomes)
  assert.equal(fire.status, 1)
  assert.match(fire.stderr, /invalid estimate/)
  assert.deepEqual(recordedOutcomes(outcomes)[1], [
    'plan-run-guards.metadata',
    'fire',
    'invalid-metadata',
  ])
})

test('an unreadable input and an unknown verb record distinct gate ids', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-outcomes-'))
  const outcomes = path.join(dir, 'outcomes.tsv')

  const unreadable = runGuardRecording(['metadata', path.join(dir, 'absent.json')], outcomes)
  assert.equal(unreadable.status, 1)
  assert.match(unreadable.stderr, /unreadable-input/)

  const usage = runGuardRecording(['not-a-verb'], outcomes)
  assert.equal(usage.status, 2)
  assert.match(usage.stderr, /^usage: plan-run-guards\.mjs/m)

  assert.deepEqual(recordedOutcomes(outcomes), [
    ['plan-run-guards.metadata', 'fire', 'unreadable-input'],
    ['plan-run-guards.usage', 'fire', 'unknown-verb'],
  ])
})

// The verb a throwing branch records under is derived from the branch that was entered, not from a
// second list of verb names a future verb could be left out of. A verb added to the dispatch chain
// but missing from such a list would record its refusals as `plan-run-guards.usage` — a wrong
// firing rate for the exact mechanism this record exists to measure, with every test still green.
test('every verb whose body throws records under its own gate id, never the usage id', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-outcomes-'))
  const absent = path.join(dir, 'absent.json')
  const readable = path.join(dir, 'readable.json')
  writeFileSync(readable, JSON.stringify({}))

  for (const [verb, args] of [
    ['metadata', ['metadata', absent]],
    ['idempotence', ['idempotence', absent]],
    ['premises', ['premises', absent, readable]],
  ]) {
    const outcomes = path.join(dir, `${verb}.tsv`)
    const res = runGuardRecording(args, outcomes)
    assert.equal(res.status, 1, `${verb}: ${res.stderr}`)
    assert.match(res.stderr, /unreadable-input/)
    assert.deepEqual(recordedOutcomes(outcomes), [
      [`plan-run-guards.${verb}`, 'fire', 'unreadable-input'],
    ])
  }
})

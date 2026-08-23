import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, writeFileSync } from 'node:fs'
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

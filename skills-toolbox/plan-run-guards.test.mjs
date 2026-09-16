import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
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

// `## Key changes` names repo-relative paths rather than the shared filler prose: the plan-contract
// gate resolves the subject's own change areas from the composed description before the attachment
// is finalized, and prose that names no path resolves to zero areas.
const descriptionSummary = (planningLines = ['- Contract: v1']) =>
  `${requiredPlanSections(DEFAULT_CONFIG)
    .map((heading) => `${heading}\n\nBounded metadata summary prose for ${heading}.`)
    .join('\n\n')
    .replace(
      '## Planning\n\nBounded metadata summary prose for ## Planning.',
      `## Planning\n\n${planningLines.join('\n')}`,
    )
    .replace(
      '## Key changes\n\nBounded metadata summary prose for ## Key changes.',
      '## Key changes\n\n- `skills-toolbox/plan-run-guards.mjs`: the bounded change.',
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

test('validateDraftMetadata hands moduleRoots to the contract check', () => {
  // The same list the dependency scan gets. Without it the contract check ran with an empty one
  // and rejected a `## Key changes` naming a marked bare module name that the scan resolves fine.
  const bareModule = metadata({
    descriptionSummary: descriptionSummary().replace(
      '- `skills-toolbox/plan-run-guards.mjs`: the bounded change.',
      '- `bossalib`: the shared library.',
    ),
  })
  const codes = (result) => result.violations.map((violation) => violation.code)
  assert.ok(
    codes(validateDraftMetadata(bareModule)).includes(
      'description-summary-subject-areas-unresolved',
    ),
  )
  assert.deepEqual(validateDraftMetadata(bareModule, { moduleRoots: ['bossalib'] }).violations, [])
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

// ---------------------------------------------------------------------------
// BOS-1254: `descriptionSummary` is a discriminated union — today's inline string, or a
// by-reference `{ path }` naming THIS run's declared `description` scratch artifact. The
// contract required the field inline while forbidding the drafter to return plan content, and
// the dispatch return channel is not byte-preserving, so the bytes a `--require-verbatim` gate
// later compares had no legal channel. These cases pin the widening as FAIL-CLOSED: the check
// still runs, and it runs over the resolved BYTES rather than over the path.
// ---------------------------------------------------------------------------

const DESCRIPTION_REF = '.linear-plans/run-abc123/BOS-1.description.md'
const PLAN_REF = '.linear-plans/run-abc123/BOS-1-test.md'

test('validateDraftMetadata accepts a by-reference descriptionSummary when a resolver is supplied', () => {
  const bytes = descriptionSummary()
  const seen = []
  const result = validateDraftMetadata(
    metadata({ descriptionSummary: { path: DESCRIPTION_REF } }),
    {
      resolveDescription: (file) => {
        seen.push(file)
        return bytes
      },
    },
  )

  assert.equal(result.ok, true, JSON.stringify(result.violations))
  assert.deepEqual(result.invalid, [])
  assert.deepEqual(result.violations, [])
  assert.deepEqual(seen, [DESCRIPTION_REF], 'the guard must resolve the reference it was given')
})

test('validateDraftMetadata refuses a by-reference descriptionSummary with no resolver', () => {
  // The non-vacuity pin: widening the accepted SHAPE must not become a blanket pass. A caller
  // that accepts the reference without hydrating it is refused, never silently skipped.
  const result = validateDraftMetadata(metadata({ descriptionSummary: { path: DESCRIPTION_REF } }))

  assert.equal(result.ok, false)
  assert.ok(
    result.violations.some((violation) => violation.code === 'description-summary-unresolved'),
    'an unhydrated reference must fire description-summary-unresolved',
  )
})

test('validateDraftMetadata refuses a reference outside the description scratch family', () => {
  for (const path of [PLAN_REF, '.linear-plans/BOS-1.description.md', 'BOS-1.description.md']) {
    const result = validateDraftMetadata(metadata({ descriptionSummary: { path } }), {
      resolveDescription: () => descriptionSummary(),
    })
    assert.equal(result.ok, false, path)
    assert.ok(result.invalid.includes('descriptionSummary'), path)
    assert.ok(
      result.violations.some((violation) => violation.code === 'description-summary-bad-reference'),
      `${path} must fire description-summary-bad-reference`,
    )
  }
})

test('validateDraftMetadata refuses a malformed reference object and non-union values', () => {
  const malformed = validateDraftMetadata(
    metadata({ descriptionSummary: { path: DESCRIPTION_REF, inline: 'also this' } }),
    { resolveDescription: () => descriptionSummary() },
  )
  assert.equal(malformed.ok, false)
  assert.ok(
    malformed.violations.some(
      (violation) => violation.code === 'description-summary-bad-reference',
    ),
  )

  for (const value of [42, ['a'], null, '', '   ']) {
    const result = validateDraftMetadata(metadata({ descriptionSummary: value }), {
      resolveDescription: () => descriptionSummary(),
    })
    assert.equal(result.ok, false, JSON.stringify(value))
    assert.deepEqual(result.invalid, ['descriptionSummary'], JSON.stringify(value))
  }
})

test('validateDraftMetadata runs the description contract over the RESOLVED bytes', () => {
  // Proves the resolver reads bytes rather than pattern-matching a path: the reference is the
  // same accepted one as the passing case above, and only the file's content differs.
  const truncated = descriptionSummary().replace(
    /## Required proof\n\nBounded metadata summary prose for ## Required proof\.\n\n/,
    '',
  )
  assert.ok(!truncated.includes('## Required proof'), 'the fixture must drop a required section')

  const result = validateDraftMetadata(
    metadata({ descriptionSummary: { path: DESCRIPTION_REF } }),
    {
      resolveDescription: () => truncated,
    },
  )

  assert.equal(result.ok, false)
  assert.ok(
    result.violations.some(
      (violation) => violation.code === 'description-summary-missing-sections',
    ),
    `expected description-summary-missing-sections, got ${JSON.stringify(result.violations.map((v) => v.code))}`,
  )
})

test('validateDraftMetadata reports an unreadable reference rather than throwing', () => {
  const result = validateDraftMetadata(
    metadata({ descriptionSummary: { path: DESCRIPTION_REF } }),
    {
      resolveDescription: () => {
        throw new Error('ENOENT: no such file')
      },
    },
  )

  assert.equal(result.ok, false)
  assert.ok(
    result.violations.some((violation) => violation.code === 'description-summary-unreadable'),
  )
})

test('the Atomic-5 justification is read from the resolved bytes of a by-reference summary', () => {
  // The estimate-5 sub-check reads the same union arm the contract check does; a reference whose
  // bytes carry the justification must not be refused for a justification it does carry.
  const withAtomic = validateDraftMetadata(
    metadata({ estimate: 5, descriptionSummary: { path: DESCRIPTION_REF } }),
    {
      resolveDescription: () =>
        descriptionSummary(['- Contract: v1', '- Atomic-5: one indivisible cutover']),
    },
  )
  assert.equal(withAtomic.ok, true, JSON.stringify(withAtomic.violations))

  const without = validateDraftMetadata(
    metadata({ estimate: 5, descriptionSummary: { path: DESCRIPTION_REF } }),
    { resolveDescription: () => descriptionSummary() },
  )
  assert.equal(without.ok, false)
  assert.ok(without.violations.some((violation) => violation.code === 'missing-atomic-5'))
})

test('an estimate-5 arm that carried no bytes is not also accused of missing its Atomic-5', () => {
  // Regression: reading the union once, above the estimate check, made `descriptionText` null on
  // every non-`text` arm — so an unhydrated or unreadable reference fabricated `missing-atomic-5`
  // on top of its real cause, sending the reader to edit a `## Planning` section in a file this
  // run never opened. Only arms that actually carried bytes may be judged for the justification.
  const assertNoFabricatedAtomic5 = (result, expectedCode) => {
    const codes = result.violations.map((violation) => violation.code)
    assert.equal(result.ok, false, JSON.stringify(codes))
    assert.ok(
      codes.includes(expectedCode),
      `expected ${expectedCode}, got ${JSON.stringify(codes)}`,
    )
    assert.ok(
      !codes.includes('missing-atomic-5'),
      `a non-text arm must not fabricate missing-atomic-5, got ${JSON.stringify(codes)}`,
    )
    assert.ok(
      !result.invalid.includes('estimate'),
      `a non-text arm must not mark estimate invalid, got ${JSON.stringify(result.invalid)}`,
    )
  }

  // No resolver: the shape is well-formed, the cause is that nobody hydrated it.
  assertNoFabricatedAtomic5(
    validateDraftMetadata(metadata({ estimate: 5, descriptionSummary: { path: DESCRIPTION_REF } })),
    'description-summary-unresolved',
  )

  // Unreadable: hydration was attempted and failed; that is the cause, and the only one.
  assertNoFabricatedAtomic5(
    validateDraftMetadata(
      metadata({ estimate: 5, descriptionSummary: { path: DESCRIPTION_REF } }),
      {
        resolveDescription: () => {
          throw new Error('ENOENT: no such file')
        },
      },
    ),
    'description-summary-unreadable',
  )

  // A reference outside the description family never got as far as bytes either.
  assertNoFabricatedAtomic5(
    validateDraftMetadata(metadata({ estimate: 5, descriptionSummary: { path: PLAN_REF } }), {
      resolveDescription: () => descriptionSummary(),
    }),
    'description-summary-bad-reference',
  )

  // Unchanged for every shape that was legal before the union existed: an inline string still
  // carries its own bytes, and an absent key still leaves estimate 5 with nothing to justify it.
  const inline = validateDraftMetadata(
    metadata({
      estimate: 5,
      descriptionSummary: descriptionSummary(['- Contract: v1', '- Atomic-5: one cutover']),
    }),
  )
  assert.equal(inline.ok, true, JSON.stringify(inline.violations))

  const inlineWithout = validateDraftMetadata(metadata({ estimate: 5 }))
  assert.ok(inlineWithout.violations.some((violation) => violation.code === 'missing-atomic-5'))
  assert.ok(inlineWithout.invalid.includes('estimate'))

  const absent = metadata({ estimate: 5 })
  delete absent.descriptionSummary
  const absentResult = validateDraftMetadata(absent)
  assert.ok(
    absentResult.violations.some((violation) => violation.code === 'missing-atomic-5'),
    'a missing descriptionSummary still cannot justify an estimate 5',
  )
  assert.ok(absentResult.invalid.includes('estimate'))

  // `invalid` is deliberately still judged — an empty string behaved that way before the union.
  const empty = validateDraftMetadata(metadata({ estimate: 5, descriptionSummary: '' }))
  assert.ok(empty.violations.some((violation) => violation.code === 'missing-atomic-5'))
  assert.deepEqual(empty.invalid, ['estimate', 'descriptionSummary'])
})

// The CLI verb is the boundary the orchestrator actually invokes, and it is the only place the
// reference is hydrated — an exported-function test alone cannot prove hydration happens there.
function writeByReferenceFixture({ descriptionBytes }) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-ref-'))
  const runDir = path.join(dir, '.linear-plans', 'run-abc123')
  mkdirSync(runDir, { recursive: true })
  writeFileSync(path.join(runDir, 'BOS-1.description.md'), descriptionBytes)
  const metadataPath = path.join(dir, 'metadata.json')
  writeFileSync(
    metadataPath,
    JSON.stringify(metadata({ descriptionSummary: { path: DESCRIPTION_REF } })),
  )
  return { dir, metadataPath }
}

test('the metadata CLI hydrates a by-reference descriptionSummary from disk', () => {
  const good = writeByReferenceFixture({ descriptionBytes: descriptionSummary() })
  const pass = spawnSync(process.execPath, [GUARD, 'metadata', good.metadataPath], {
    cwd: good.dir,
    encoding: 'utf8',
  })
  assert.equal(pass.status, 0, pass.stderr)

  const bad = writeByReferenceFixture({
    descriptionBytes: descriptionSummary().replace('## Required proof', '## Not A Real Section'),
  })
  const fire = spawnSync(process.execPath, [GUARD, 'metadata', bad.metadataPath], {
    cwd: bad.dir,
    encoding: 'utf8',
  })
  assert.equal(fire.status, 1)
  assert.match(fire.stderr, /description-summary-/)

  const missing = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-ref-'))
  const missingMetadata = path.join(missing, 'metadata.json')
  writeFileSync(
    missingMetadata,
    JSON.stringify(metadata({ descriptionSummary: { path: DESCRIPTION_REF } })),
  )
  const unreadable = spawnSync(process.execPath, [GUARD, 'metadata', missingMetadata], {
    cwd: missing,
    encoding: 'utf8',
  })
  assert.equal(unreadable.status, 1)
  assert.match(unreadable.stderr, /description-summary-unreadable/)
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

test('premiseDrift reports drifted, unresolved, and verification coverage', () => {
  assert.deepEqual(premiseDrift([], {}), {
    ok: true,
    drifted: [],
    unresolved: [],
    verified: 0,
    declared: 0,
  })
  assert.deepEqual(premiseDrift(undefined, {}), {
    ok: true,
    drifted: [],
    unresolved: [],
    verified: 0,
    declared: 0,
  })
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
      verified: 1,
      declared: 2,
    },
  )
})

test('premises CLI reports zero verification coverage for an empty declared set', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-'))
  const premisesPath = path.join(dir, 'premises.json')
  const livePath = path.join(dir, 'live.json')
  writeFileSync(premisesPath, '[]')
  writeFileSync(livePath, '{}')
  const result = spawnSync(process.execPath, [GUARD, 'premises', premisesPath, livePath], {
    encoding: 'utf8',
  })
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stderr, /premises: verified 0 of 0/)
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

// ---------------------------------------------------------------------------
// Shape guard (BOS-1244 row 7)
// ---------------------------------------------------------------------------

test('the idempotence verb refuses a {issue:{…}} wrapper instead of printing action:"plan"', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-wrapper-'))
  const bare = path.join(dir, 'bare.json')
  const wrapped = path.join(dir, 'wrapped.json')
  const unplanned = path.join(dir, 'unplanned.json')
  writeFileSync(bare, JSON.stringify(issue()))
  writeFileSync(wrapped, JSON.stringify({ issue: issue() }))
  writeFileSync(unplanned, JSON.stringify(issue({ status: 'Unplanned', attachments: [] })))

  // The wrapper used to leave every field undefined, so all three reasons fired and the verb printed
  // a verdict byte-identical to the genuinely-unplanned one below — the guard reading as working
  // while having evaluated nothing.
  const wrappedRun = runGuardRecording(['idempotence', wrapped], path.join(dir, 'wrapped.tsv'))
  assert.notEqual(wrappedRun.status, 0, 'a wrapper must exit non-zero')
  assert.equal(wrappedRun.stdout, '', 'and must print no verdict line at all')
  assert.match(wrappedRun.stderr, /unreadable-input/, 'routed through the named verb reason')
  assert.match(
    wrappedRun.stderr,
    /idempotence <issue\.json>/,
    'the message names the expected shape',
  )
  assert.deepEqual(recordedOutcomes(path.join(dir, 'wrapped.tsv')), [
    ['plan-run-guards.idempotence', 'fire', 'unreadable-input'],
  ])
  // BOS-1244 review round 1 (boss-review-ce). The `unreadable-input` handler supplies the module
  // prefix itself, so the thrown message must not carry a second one: this printed
  // `unreadable-input: plan-run-guards: plan-run-guards: …`, a stutter no sibling diagnostic here has.
  assert.doesNotMatch(
    wrappedRun.stderr,
    /plan-run-guards: plan-run-guards:/,
    'the module prefix must appear exactly once',
  )

  // The verdict the wrapper used to impersonate is still produced for a real unplanned ticket.
  const unplannedRun = runGuardRecording(
    ['idempotence', unplanned],
    path.join(dir, 'unplanned.tsv'),
  )
  assert.equal(unplannedRun.status, 0, unplannedRun.stderr)
  assert.equal(JSON.parse(unplannedRun.stdout).action, 'plan')

  // And the bare object — the documented shape — is unaffected.
  const bareRun = runGuardRecording(['idempotence', bare], path.join(dir, 'bare.tsv'))
  assert.equal(bareRun.status, 0, bareRun.stderr)
  assert.ok(['noop', 'plan'].includes(JSON.parse(bareRun.stdout).action))
})

test('planIdempotencePrecheck is unchanged for an issue that legitimately carries an `issue` field', () => {
  // The guard is scoped to an object whose ONLY own key is `issue`; a real payload with its own
  // fields plus one called `issue` is not the recorded mistake and must still be evaluated.
  const result = planIdempotencePrecheck({
    issue: issue({ issue: 'a field of its own' }),
    config: TEST_CONFIG,
  })
  assert.deepEqual(result, { action: 'noop', reasons: [] })
})

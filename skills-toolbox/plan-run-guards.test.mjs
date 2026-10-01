import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import {
  chmodSync,
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  PREMISE_LIMIT,
  adoptReturnedMetadata,
  planIdempotencePrecheck,
  premiseDrift,
  reconcilePremiseAnnotations,
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

// BOS-1329 R8: an absolute or `./`-prefixed spelling of the repo-relative description artifact
// names the same file, so refusing it discarded an already-drafted plan over spelling alone.
test('validateDraftMetadata accepts an absolute or ./-prefixed description reference under the tree', () => {
  const cwd = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-cwd-'))
  for (const spelling of [path.join(cwd, DESCRIPTION_REF), `./${DESCRIPTION_REF}`]) {
    const seen = []
    const result = validateDraftMetadata(metadata({ descriptionSummary: { path: spelling } }), {
      cwd,
      resolveDescription: (file) => {
        seen.push(file)
        return descriptionSummary()
      },
    })
    assert.equal(result.ok, true, `${spelling}: ${JSON.stringify(result.violations)}`)
    assert.deepEqual(seen, [spelling], 'the resolver still reads the spelling it was handed')
  }
})

test('validateDraftMetadata refuses an absolute or ./ reference that resolves outside the tree', () => {
  const cwd = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-cwd-'))
  for (const spelling of [
    path.join(path.dirname(cwd), DESCRIPTION_REF),
    `./../${DESCRIPTION_REF}`,
    path.join(cwd, '.linear-plans', 'BOS-1.description.md'),
  ]) {
    const result = validateDraftMetadata(metadata({ descriptionSummary: { path: spelling } }), {
      cwd,
      resolveDescription: () => descriptionSummary(),
    })
    assert.equal(result.ok, false, spelling)
    assert.ok(
      result.violations.some((violation) => violation.code === 'description-summary-bad-reference'),
      `${spelling} must fire description-summary-bad-reference`,
    )
  }
})

// BOS-1329 R9: returned labels are the content taxonomy only, resolved through the config.
test('validateDraftMetadata rejects a label outside the content taxonomy with unknown-label', () => {
  const result = validateDraftMetadata(
    metadata({ labels: ['improvement', 'agent-friendly', 'refactor'] }),
  )
  assert.equal(result.ok, false)
  assert.ok(result.invalid.includes('labels'))
  const violation = result.violations.find((entry) => entry.code === 'unknown-label')
  assert.ok(violation, JSON.stringify(result.violations))
  assert.deepEqual(violation.labels, ['agent-friendly', 'refactor'])
  assert.match(violation.message, /"agent-friendly", "refactor"/)
  assert.deepEqual(violation.allowed, ['bug', 'feature', 'improvement', 'docs'])
})

test('validateDraftMetadata accepts taxonomy literals and a config-mapped display name', () => {
  for (const labels of [['bug'], ['improvement'], ['bug', 'feature', 'improvement', 'docs'], []]) {
    const result = validateDraftMetadata(metadata({ labels }))
    assert.equal(result.ok, true, `${labels}: ${JSON.stringify(result.violations)}`)
  }
  const mapped = {
    ...DEFAULT_CONFIG,
    trackerConfig: { linear: { labels: { bug: 'Bug', docs: 'Documentation' } } },
  }
  assert.equal(
    validateDraftMetadata(metadata({ labels: ['Bug', 'Documentation'] }), { config: mapped }).ok,
    true,
  )
  // Once a role is mapped, its bare literal is no longer a label the tracker has.
  const literal = validateDraftMetadata(metadata({ labels: ['bug'] }), { config: mapped })
  assert.equal(literal.ok, false)
  assert.ok(literal.violations.some((entry) => entry.code === 'unknown-label'))
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

// BOS-1289 — the precheck interrogated the payload it was handed but never asked whether that
// payload was the ticket the run selected, so a mispicked id planned a different ticket with every
// guard clean.
test('planIdempotencePrecheck is byte-identical when selectedID is absent', () => {
  // The whole optionality claim in one assertion: omitted, null, and blank must all reproduce
  // today's verdict exactly, or the conjunct is a breaking change to every existing caller.
  const baseline = planIdempotencePrecheck({ issue: issue(), config: TEST_CONFIG })
  assert.deepEqual(baseline, { action: 'noop', reasons: [] })
  for (const selectedID of [undefined, null, '', '   ']) {
    assert.deepEqual(
      planIdempotencePrecheck({ issue: issue(), selectedID, config: TEST_CONFIG }),
      baseline,
      `selectedID ${JSON.stringify(selectedID)} must not change the verdict`,
    )
  }
})

test('planIdempotencePrecheck matches selectedID against id and identifier', () => {
  for (const selectedID of ['BOS-1', 'bos-1', ' BOS-1 ']) {
    assert.deepEqual(
      planIdempotencePrecheck({ issue: issue(), selectedID, config: TEST_CONFIG }),
      { action: 'noop', reasons: [] },
      `${JSON.stringify(selectedID)} names the fetched issue`,
    )
  }
  // The human identifier is as legitimate a selector as the UUID, so either field may satisfy it.
  assert.deepEqual(
    planIdempotencePrecheck({
      issue: issue({ id: undefined, identifier: 'BOS-1' }),
      selectedID: 'BOS-1',
      config: TEST_CONFIG,
    }),
    { action: 'noop', reasons: [] },
  )
  // ...and the UUID satisfies it when that is the field the payload carries.
  const uuid = 'c0ffee00-dead-4bee-8000-000000000001'
  assert.deepEqual(
    planIdempotencePrecheck({
      issue: issue({
        id: uuid,
        attachments: [{ id: 'att-1', title: `Implementation plan (${uuid})` }],
      }),
      selectedID: uuid,
      config: TEST_CONFIG,
    }),
    { action: 'noop', reasons: [] },
  )
})

test('planIdempotencePrecheck can never noop on a fetched-issue id mismatch', () => {
  // Every other conjunct is satisfied — this is exactly the run that shipped the wrong ticket
  // silently, and the identity conjunct is the only thing standing between it and `noop`.
  assert.deepEqual(
    planIdempotencePrecheck({ issue: issue(), selectedID: 'BOS-999', config: TEST_CONFIG }),
    { action: 'plan', reasons: ['fetched-issue-id-mismatch'] },
  )
  // It only ever ADDS a reason, so it composes with the existing three rather than masking them.
  assert.deepEqual(
    planIdempotencePrecheck({
      issue: issue({ status: 'Unplanned', description: 'too small', attachments: null }),
      selectedID: 'BOS-999',
      config: TEST_CONFIG,
    }),
    {
      action: 'plan',
      reasons: [
        'fetched-issue-id-mismatch',
        'state-not-planned',
        'description-invalid',
        'plan-attachment-missing',
      ],
    },
  )
  // A payload carrying no usable id is not a match either: an unidentifiable fetch is precisely
  // what this conjunct must refuse to call clean.
  assert.ok(
    planIdempotencePrecheck({
      issue: issue({ id: undefined, identifier: undefined }),
      selectedID: 'BOS-1',
      config: TEST_CONFIG,
    }).reasons.includes('fetched-issue-id-mismatch'),
  )
})

test('the idempotence CLI verb threads --selected-id through to the precheck', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-selected-'))
  const issuePath = path.join(dir, 'issue.json')
  writeFileSync(issuePath, JSON.stringify(issue()))

  const run = (...extra) =>
    spawnSync(process.execPath, [GUARD, 'idempotence', issuePath, ...extra], {
      cwd: dir,
      encoding: 'utf8',
    })

  // The CLI resolves its own config from the repo, so the other three conjuncts may legitimately
  // fire against this synthetic fixture. Only the identity conjunct is this test's subject.
  const reasons = (result) => {
    assert.equal(result.status, 0, result.stderr)
    return JSON.parse(result.stdout).reasons
  }
  const omitted = reasons(run())
  assert.equal(omitted.includes('fetched-issue-id-mismatch'), false)
  assert.deepEqual(
    reasons(run('--selected-id', 'BOS-1')),
    omitted,
    'a matching selector must leave the pre-flag verdict untouched',
  )
  const mismatched = reasons(run('--selected-id', 'BOS-999'))
  assert.ok(mismatched.includes('fetched-issue-id-mismatch'))
  assert.equal(JSON.parse(run('--selected-id', 'BOS-999').stdout).action, 'plan')
  rmSync(dir, { recursive: true, force: true })
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

const annotationPremises = [
  { id: 'GIG-46', state: 'Todo' },
  { id: 'GIG-47', state: 'Todo' },
]
const reconcile = (text, states = { 'GIG-46': 'In Progress', 'GIG-47': 'Todo' }) =>
  reconcilePremiseAnnotations(text, {
    premises: annotationPremises,
    drifted: premiseDrift(annotationPremises, states).drifted,
  })
const annotationBody = (prose) =>
  `## Risks / unknowns\n\n${prose}\n\n## Planning\n\n- Contract: v1\n- Scope: bounded\n\n## Original notes\n\nGIG-46 was Todo.\n`

test('reconcilePremiseAnnotations flags drifted prose only, reporting sections', () => {
  const before = annotationBody('- GIG-46 and GIG-47 were Todo.\n- GIG-47 stays Todo.')
  const after = reconcile(before)
  assert.ok(
    after.includes('- GIG-46 and GIG-47 were Todo. (premise drift: GIG-46 is now In Progress)'),
  )
  assert.ok(after.includes('- GIG-47 stays Todo.\n'))
  assert.ok(
    after.includes(
      '- Scope: bounded\n- Premise drift: GIG-46 was Todo, is now In Progress; flagged inline in Risks / unknowns\n\n## Original notes',
    ),
  )
  assert.ok(!after.includes('premise drift: GIG-47'))
})

test('reconcilePremiseAnnotations reports checkbox sections without rewriting their bullets', () => {
  const bullets =
    '- [ ] GIG-46 was Todo — check: `verify GIG-46`\n- [ ] GIG-47 was Todo — check: `verify GIG-47`'
  const before = annotationBody('').replace(
    '## Planning',
    `## Premises\n\n${bullets}\n\n## Acceptance criteria\n\n${bullets}\n\n## Planning`,
  )
  const after = reconcile(before)
  assert.equal(after.split(bullets).length, 3)
  assert.ok(after.includes('; also stated in Premises, Acceptance criteria\n'))
  assert.ok(!after.includes('; flagged inline in'))
})

test('reconcilePremiseAnnotations leaves Original notes through EOF and fenced code byte-identical', () => {
  const code =
    '```md\n## Premises\nGIG-46 (premise drift: GIG-46 is now Old)\n- Premise drift: GIG-46 was Todo, is now Old\n```\n~~~\nGIG-46\n~~~'
  const before =
    annotationBody(`GIG-46 prose.\n${code}`) +
    '## Evidence\nGIG-46 (premise drift: GIG-46 is now Old)\n'
  const after = reconcile(before)
  assert.ok(after.includes(code))
  const originalNotesStart = before.indexOf('## Original notes')
  assert.ok(originalNotesStart >= 0)
  const protectedTail = before.slice(originalNotesStart)
  assert.ok(after.endsWith(protectedTail))
  assert.ok(after.includes('GIG-46 prose. (premise drift: GIG-46 is now In Progress)'))
})

test('reconcilePremiseAnnotations inserts missing Planning before an EOF Original notes heading', () => {
  for (const heading of ['## Original notes', '## Original notes ###']) {
    for (const ending of ['', '\n']) {
      const protectedTail = heading + ending
      const before = `## Risks / unknowns\n\nGIG-46 was Todo.\n\n${protectedTail}`
      const after = reconcile(before)
      assert.ok(after.endsWith(protectedTail))
      assert.ok(after.indexOf('## Planning') < after.indexOf(heading))
      assert.ok(after.includes('- Premise drift: GIG-46 was Todo, is now In Progress'))
      assert.equal(reconcile(after), after)
    }
  }
})

test('reconcilePremiseAnnotations ignores fenced Original notes headings when inserting Planning', () => {
  const code = '```md\n## Original notes\nGIG-46 example.\n```'
  for (const tail of ['', '\n\n## Original notes', '\n\n## Original notes ###\n']) {
    const before = `## Risks / unknowns\n\n${code}\n\nGIG-46 was Todo.${tail}`
    const after = reconcile(before)
    assert.ok(after.includes(code))
    assert.ok(after.indexOf('## Planning') > after.indexOf(code) + code.length)
    assert.ok(after.includes('GIG-46 was Todo. (premise drift: GIG-46 is now In Progress)'))
    if (tail) assert.ok(after.endsWith(tail.trimStart()))
    assert.equal(reconcile(after), after)
  }
})

test('reconcilePremiseAnnotations matches whole tokens, skipping headings and table rows', () => {
  const after = reconcile(
    annotationBody('GIG-46.\nGIG-461 and XGIG-46 and GIG-46-extra.\n### GIG-46\n| GIG-46 | Todo |'),
  )
  assert.ok(after.includes('GIG-46. (premise drift: GIG-46 is now In Progress)'))
  assert.ok(
    after.includes('GIG-461 and XGIG-46 and GIG-46-extra.\n### GIG-46\n| GIG-46 | Todo |\n'),
  )
})

test('reconcilePremiseAnnotations skips Markdown tables with optional outer pipes', () => {
  const table = 'Ticket | State\n--- | ---\nGIG-46 | Todo'
  const after = reconcile(annotationBody(`${table}\n\nGIG-46 prose.`))
  assert.ok(after.includes(table + '\n\n'))
  assert.ok(after.includes('GIG-46 prose. (premise drift: GIG-46 is now In Progress)'))
})

test('reconcilePremiseAnnotations is idempotent and replaces a second-pass state', () => {
  const once = reconcile(annotationBody('GIG-46 was Todo.'))
  assert.equal(reconcile(once), once)
  const twice = reconcile(once, { 'GIG-46': 'In Review', 'GIG-47': 'Todo' })
  assert.ok(twice.includes('(premise drift: GIG-46 is now In Review)'))
  assert.ok(twice.includes('- Premise drift: GIG-46 was Todo, is now In Review;'))
  assert.ok(!twice.includes('In Progress'))
  assert.equal(reconcile(twice, { 'GIG-46': 'In Review', 'GIG-47': 'Todo' }), twice)
})

test('reconcilePremiseAnnotations removes resolved annotations and preserves undeclared ids', () => {
  const before = annotationBody('GIG-46 was Todo.\nGIG-48 (premise drift: GIG-48 is now Done)')
  const once = reconcile(before)
  assert.equal(reconcile(once, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
  assert.ok(reconcile(once).includes('(premise drift: GIG-48 is now Done)'))
})

test('reconcilePremiseAnnotations preserves no-drift bytes, CRLF and missing terminal newline', () => {
  for (const before of [
    annotationBody('GIG-46.'),
    annotationBody('GIG-46.').replaceAll('\n', '\r\n').trimEnd(),
  ]) {
    assert.equal(reconcile(before, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
    const once = reconcile(before)
    assert.equal(reconcile(once), once)
    assert.equal(reconcile(once, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
  }
})

test('reconcilePremiseAnnotations marks multiple drifted ids without treating markers as mentions', () => {
  const before = annotationBody('GIG-46 and GIG-47 were Todo.')
  const states = { 'GIG-46': 'In Review', 'GIG-47': 'Done' }
  const after = reconcile(before, states)
  assert.ok(
    after.includes('(premise drift: GIG-46 is now In Review) (premise drift: GIG-47 is now Done)'),
  )
  assert.equal(reconcile(after, states), after)
  assert.equal(reconcile(after, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
})

test('reconcilePremiseAnnotations reconciles parenthesized states without marking ids inside markers', () => {
  const before = annotationBody('GIG-46 was Todo.')
  const states = { 'GIG-46': 'Review (waiting on GIG-47)', 'GIG-47': 'Done' }
  const after = reconcile(before, states)
  assert.ok(after.includes('(premise drift: GIG-46 is now Review \\(waiting on GIG-47\\))'))
  assert.ok(!after.includes('(premise drift: GIG-47'))
  assert.equal(reconcile(after, states), after)
  assert.equal(reconcile(after, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
})

test('reconcilePremiseAnnotations treats unmatched state parentheses as opaque text', () => {
  const before = annotationBody('GIG-46 and GIG-47 were Todo.')
  for (const state of ['Review (waiting', 'Review ) waiting', 'Review \\(waiting)']) {
    const states = { 'GIG-46': state, 'GIG-47': 'Done' }
    const after = reconcile(before, states)
    const escapedState = state.replace(/[\\()]/g, '\\$&')
    assert.ok(after.includes(`(premise drift: GIG-46 is now ${escapedState})`))
    assert.equal(escapedState.replace(/\\([\\()])/g, '$1'), state)
    assert.equal(reconcile(after, states), after)
    assert.equal(reconcile(after, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
    const changed = reconcile(after, { 'GIG-46': 'In Review', 'GIG-47': 'Done' })
    assert.ok(!changed.includes(`(premise drift: GIG-46 is now ${escapedState})`))
    assert.equal(reconcile(changed, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
    const control = annotationBody('GIG-46 was Todo. (premise drift: GIG-48 is now Done)')
    const marked = reconcile(control, { 'GIG-46': state, 'GIG-47': 'Todo' })
    assert.equal(reconcile(marked, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), control)
  }
})

test('reconcilePremiseAnnotations removes balanced relocated markers without losing following prose', () => {
  for (const suffix of [
    ' followed by required condition (check)',
    '. Required condition (check).',
    ', required condition; check!',
  ]) {
    for (const state of ['Old', 'Review (waiting on GIG-47)']) {
      const prose = `GIG-46 prose${suffix}`
      const before = annotationBody(prose)
      const relocated = annotationBody(
        `GIG-46 prose (premise drift: GIG-46 is now ${state})${suffix}`,
      )
      const after = reconcile(relocated)
      assert.equal(after, reconcile(before))
      assert.ok(after.includes(prose))
      assert.equal(reconcile(after), after)
      assert.equal(reconcile(after, { 'GIG-46': 'Todo', 'GIG-47': 'Todo' }), before)
    }
  }
})

test('premises --annotate updates two files, records drift and leaves a repeat byte-identical', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'premise-annotate-'))
  try {
    const p = path.join(dir, 'premises.json'),
      live = path.join(dir, 'live.json')
    const files = [path.join(dir, 'description.md'), path.join(dir, 'plan.md')]
    const modes = [0o640, 0o600]
    writeFileSync(p, JSON.stringify(annotationPremises))
    writeFileSync(live, JSON.stringify({ 'GIG-46': 'In Review', 'GIG-47': 'Todo' }))
    for (const file of files) writeFileSync(file, annotationBody('GIG-46 was Todo.'))
    files.forEach((file, index) => chmodSync(file, modes[index]))
    const args = ['premises', p, live, ...files.flatMap((file) => ['--annotate', file])]
    const outcomes = path.join(dir, 'outcomes.tsv')
    const first = runGuardRecording(args, outcomes)
    assert.equal(first.status, 0, first.stderr)
    assert.match(
      first.stderr,
      /premise-annotated: plan-run-guards: GIG-46 flagged in Risks \/ unknowns/,
    )
    const after = files.map((file) => readFileSync(file, 'utf8'))
    assert.ok(after.every((text) => text.includes('(premise drift: GIG-46 is now In Review)')))
    assert.deepEqual(
      files.map((file) => statSync(file).mode & 0o7777),
      modes,
    )
    const second = runGuardRecording(args, outcomes)
    assert.equal(second.status, 0, second.stderr)
    assert.deepEqual(
      files.map((file) => readFileSync(file, 'utf8')),
      after,
    )
    writeFileSync(live, JSON.stringify({ 'GIG-46': 'Todo', 'GIG-47': 'Todo' }))
    assert.equal(runGuardRecording(args, outcomes).status, 0)
    assert.ok(
      files.every((file) => readFileSync(file, 'utf8') === annotationBody('GIG-46 was Todo.')),
    )
    assert.equal(runGuardRecording(args, outcomes).status, 0)
    assert.deepEqual(
      recordedOutcomes(outcomes).map((row) => row[2]),
      ['premise-drift', 'premise-drift', 'ok', 'ok'],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test(
  'premises --annotate refuses an unwritable target before changing either file',
  {
    skip:
      process.getuid?.() === 0
        ? 'root can write chmod 0444 files; this fixture requires an unprivileged user'
        : false,
  },
  () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'premise-unwritable-'))
    const first = path.join(dir, 'first.md'),
      second = path.join(dir, 'second.md')
    try {
      const p = path.join(dir, 'premises.json'),
        live = path.join(dir, 'live.json')
      const before = annotationBody('GIG-46 was Todo.')
      writeFileSync(p, JSON.stringify(annotationPremises))
      writeFileSync(live, JSON.stringify({ 'GIG-46': 'In Review', 'GIG-47': 'Todo' }))
      writeFileSync(first, before)
      writeFileSync(second, before)
      chmodSync(second, 0o444)
      const result = spawnSync(
        process.execPath,
        [GUARD, 'premises', p, live, '--annotate', first, '--annotate', second],
        { encoding: 'utf8' },
      )
      assert.equal(result.status, 1, result.stderr)
      assert.match(result.stderr, /unreadable-input:/)
      assert.equal(readFileSync(first, 'utf8'), before)
      assert.equal(readFileSync(second, 'utf8'), before)
    } finally {
      chmodSync(second, 0o644)
      rmSync(dir, { recursive: true, force: true })
    }
  },
)

for (const phase of ['stage', 'commit']) {
  test(`premises --annotate restores all bytes and modes when the second ${phase} fails`, () => {
    const dir = mkdtempSync(path.join(tmpdir(), 'premise-transaction-'))
    try {
      const p = path.join(dir, 'premises.json'),
        live = path.join(dir, 'live.json'),
        outcomes = path.join(dir, 'outcomes.tsv'),
        preload = path.join(dir, 'fault.mjs')
      const files = [path.join(dir, 'first.md'), path.join(dir, 'second.md')]
      const before = annotationBody('GIG-46 was Todo.')
      const modes = [0o640, 0o600]
      writeFileSync(p, JSON.stringify(annotationPremises))
      writeFileSync(live, JSON.stringify({ 'GIG-46': 'In Review', 'GIG-47': 'Todo' }))
      files.forEach((file, index) => {
        writeFileSync(file, before)
        chmodSync(file, modes[index])
      })
      // Patch the builtin before guard imports: fail one exact staging/commit operation,
      // leaving rollback operations unaffected and avoiding permission/timing dependence.
      writeFileSync(
        preload,
        `
import fs from 'node:fs'
import { syncBuiltinESMExports } from 'node:module'
const phase = ${JSON.stringify(phase)}
const method = phase === 'stage' ? 'writeFileSync' : 'renameSync'
const original = fs[method]
let calls = 0
fs[method] = function (file, ...args) {
  if (String(file).endsWith('/next') && String(file).includes('/.premise-annotations-')) {
    calls += 1
    if (calls === 2) {
      const first = fs.readFileSync(${JSON.stringify(files[0])}, 'utf8')
      const before = ${JSON.stringify(before)}
      if (phase === 'stage' && first !== before) throw new Error('first target changed before staging completed')
      if (phase === 'commit' && first === before) throw new Error('first commit was not exercised')
      throw new Error('injected second ' + phase + ' failure')
    }
  }
  return original.call(this, file, ...args)
}
syncBuiltinESMExports()
`,
      )
      const result = spawnSync(
        process.execPath,
        [
          '--import',
          preload,
          GUARD,
          'premises',
          p,
          live,
          ...files.flatMap((file) => ['--annotate', file]),
        ],
        { encoding: 'utf8', env: { ...process.env, BOSS_GATE_OUTCOME_FILE: outcomes } },
      )
      assert.equal(result.status, 1, result.stderr)
      assert.match(result.stderr, new RegExp(`unreadable-input:.*injected second ${phase} failure`))
      assert.doesNotMatch(result.stderr, /premise-annotated:/)
      assert.deepEqual(
        files.map((file) => readFileSync(file)),
        files.map(() => Buffer.from(before)),
      )
      assert.deepEqual(
        files.map((file) => statSync(file).mode & 0o7777),
        modes,
      )
      assert.ok(!readdirSync(dir).some((name) => name.startsWith('.premise-annotations-')))
      assert.deepEqual(recordedOutcomes(outcomes), [
        ['plan-run-guards.premises', 'fire', 'unreadable-input'],
      ])
    } finally {
      rmSync(dir, { recursive: true, force: true })
    }
  })
}

test('premises --annotate stages nothing for unchanged targets', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'premise-noop-'))
  try {
    const p = path.join(dir, 'premises.json'),
      live = path.join(dir, 'live.json'),
      file = path.join(dir, 'description.md'),
      preload = path.join(dir, 'fault.mjs')
    const before = annotationBody('GIG-46 was Todo.')
    writeFileSync(p, JSON.stringify(annotationPremises))
    writeFileSync(live, JSON.stringify({ 'GIG-46': 'Todo', 'GIG-47': 'Todo' }))
    writeFileSync(file, before)
    const originalStat = statSync(file)
    writeFileSync(
      preload,
      `
import fs from 'node:fs'
import { syncBuiltinESMExports } from 'node:module'
fs.mkdtempSync = () => { throw new Error('unchanged targets must not stage') }
syncBuiltinESMExports()
`,
    )
    const result = spawnSync(
      process.execPath,
      ['--import', preload, GUARD, 'premises', p, live, '--annotate', file],
      { encoding: 'utf8' },
    )
    assert.equal(result.status, 0, result.stderr)
    assert.equal(readFileSync(file, 'utf8'), before)
    assert.equal(statSync(file).ino, originalStat.ino)
    assert.equal(statSync(file).mtimeMs, originalStat.mtimeMs)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('premises --annotate writes nothing on premise-limit, premise-unresolved or unreadable-input', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'premise-abort-'))
  try {
    const p = path.join(dir, 'premises.json'),
      live = path.join(dir, 'live.json')
    const file = path.join(dir, 'description.md'),
      absent = path.join(dir, 'absent.md')
    const before = annotationBody('GIG-46 was Todo.')
    for (const [reason, premises, states, extra] of [
      [
        'premise-limit',
        Array.from({ length: PREMISE_LIMIT + 1 }, (_, i) => ({ id: `GIG-${i}`, state: 'Todo' })),
        {},
        [],
      ],
      ['premise-unresolved', annotationPremises, { 'GIG-46': 'In Review' }, []],
      ['unreadable-input', annotationPremises, 'bad json', []],
      [
        'unreadable-input',
        annotationPremises,
        { 'GIG-46': 'In Review', 'GIG-47': 'Todo' },
        ['--annotate', absent],
      ],
    ]) {
      writeFileSync(file, before)
      writeFileSync(p, JSON.stringify(premises))
      writeFileSync(live, typeof states === 'string' ? states : JSON.stringify(states))
      const result = spawnSync(
        process.execPath,
        [GUARD, 'premises', p, live, '--annotate', file, ...extra],
        { encoding: 'utf8' },
      )
      assert.equal(result.status, 1, result.stderr)
      assert.ok(result.stderr.includes(reason))
      assert.equal(readFileSync(file, 'utf8'), before)
    }
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
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

// ---------------------------------------------------------------------------
// BOS-1278 / R3 — the orchestrator validates the object it RECEIVED, never a
// same-named file the worker could have written at that path.
// ---------------------------------------------------------------------------

test('adoptReturnedMetadata refuses when the on-disk file is not the returned object', () => {
  // The defect: `draft-metadata` is a DECLARED scratch family, and the drafting worker is told to
  // write its local files under declared basenames — so the worker can write the very path the
  // orchestrator is specified to write from the returned object before validating.
  const returned = metadata()
  const workerWrote = metadata({ estimate: 1, agentFriendly: false })
  const diverged = adoptReturnedMetadata({ returned, onDisk: workerWrote })
  assert.equal(diverged.ok, false)
  assert.equal(diverged.state, 'diverged')
  assert.deepEqual(
    diverged.violations.map((v) => v.code),
    ['metadata-not-from-message'],
  )

  // Able to fire both other ways, so the refusal is the DIVERGENCE's doing: an absent file is
  // adopted, and an equal one is adopted even with its keys in a different write order (a resumed
  // pass re-running this step is not a contract violation).
  assert.deepEqual(
    [adoptReturnedMetadata({ returned }).state, adoptReturnedMetadata({ returned }).ok],
    ['absent', true],
  )
  const reordered = Object.fromEntries(Object.entries(returned).reverse())
  assert.equal(adoptReturnedMetadata({ returned, onDisk: reordered }).state, 'identical')
})

test('adoptReturnedMetadata refuses a dispatch that returned no message at all', () => {
  // Artifact readiness is not message readiness. A caller with nothing in hand must not be able to
  // reach the guard at all, because the file it would validate is the only thing left.
  for (const nothing of [undefined, null, 'not an object']) {
    const result = adoptReturnedMetadata({ returned: nothing, onDisk: metadata() })
    assert.equal(result.ok, false)
    assert.deepEqual(
      result.violations.map((v) => v.code),
      ['metadata-not-returned'],
    )
  }
})

test('adopt-metadata CLI writes the returned object, and leaves a diverged file untouched', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-adopt-'))
  const metadataPath = path.join(dir, 'BOS-1.draft-metadata.json')
  const returned = metadata()

  // Nothing on disk: the returned object is what lands, and what gets validated.
  const fresh = spawnSync(
    process.execPath,
    [GUARD, 'adopt-metadata', metadataPath, JSON.stringify(returned)],
    { encoding: 'utf8' },
  )
  assert.equal(fresh.status, 0, fresh.stderr)
  assert.deepEqual(JSON.parse(readFileSync(metadataPath, 'utf8')), returned)

  // A worker-authored file that differs: refuse, and do NOT overwrite. The file is the evidence
  // that the dispatch broke its contract; an orchestrator that silently repaired it would keep
  // dispatching workers that do this.
  const workerWrote = metadata({ estimate: 1 })
  writeFileSync(metadataPath, JSON.stringify(workerWrote))
  const refused = spawnSync(
    process.execPath,
    [GUARD, 'adopt-metadata', metadataPath, JSON.stringify(returned)],
    { encoding: 'utf8' },
  )
  assert.equal(refused.status, 1)
  assert.match(refused.stderr, /metadata-not-from-message/)
  assert.deepEqual(
    JSON.parse(readFileSync(metadataPath, 'utf8')),
    workerWrote,
    'the diverged file must survive the refusal',
  )

  // The adopted object is still held to the ordinary metadata contract, so adoption is not a way
  // around validation: an invalid returned object fails after it is written.
  const invalidPath = path.join(dir, 'BOS-2.draft-metadata.json')
  const invalid = spawnSync(
    process.execPath,
    [GUARD, 'adopt-metadata', invalidPath, JSON.stringify(metadata({ estimate: 8 }))],
    { encoding: 'utf8' },
  )
  assert.equal(invalid.status, 1)
  assert.match(invalid.stderr, /estimate/)
})

// ---------------------------------------------------------------------------
// BOS-1335: the epic-reverify verb — the run-boundary CLI over epicReverifyVerdict. Every path it
// reads is derived from the bundle's own run directory; every description it judges is a stored
// read-back file, never a field of the bundle.
// ---------------------------------------------------------------------------

const RV_STATES = {
  unplanned: 'Unplanned',
  planned: 'Todo',
  inProgress: 'In Progress',
  inReview: 'In Review',
}
const RV_LABELS = {
  agentPlan: 'agent-plan',
  agentFriendly: 'agent-friendly',
  needsHuman: 'needs-human',
  agentQuestion: 'agent-question',
  epic: 'epic',
}
const RV_NOTES = 'Reporter context.\n\n- one observation\n'
const rvChildBody = (key) =>
  [
    `## Summary\n\nChild ${key}.`,
    '## Approach\n\n- do the thing',
    '## Key changes\n\n- `skills-toolbox/x.mjs`',
    '## Testing\n\n- unit coverage',
    '## Risks / unknowns\n\n- none',
    '## Acceptance criteria\n\n- [ ] it works',
    '## Required proof\n\n- [ ] (backend-only) no screenshot applicable',
    '## Planning\n\n- Contract: v1',
    `<!-- boss-plan-epic-child:${key} -->\n\n## Original notes\n\n${RV_NOTES}`,
  ].join('\n\n')
const RV_OVERVIEW = [
  '## Summary\n\nDecompose the epic.',
  '## Child tickets\n\n- BOS-11\n- BOS-12',
  '## Planning\n\n- Contract: v1',
  `## Original notes\n\n${RV_NOTES}`,
].join('\n\n')

/**
 * A passing epic on disk: a temp repo with a config, and a run directory holding the bundle plus
 * every derived file. `edit(ctx)` mutates before writing; returns the paths the test pokes at.
 */
function writeEpicFixture(edit = () => {}) {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-epic-'))
  const runDir = path.join(dir, '.linear-plans', 'run-r1')
  mkdirSync(runDir, { recursive: true })
  const ctx = {
    labels: { ...RV_LABELS },
    parentState: 'Todo',
    childIds: ['BOS-11', 'BOS-12'],
    childBodyStored: { 'BOS-11': rvChildBody('c1'), 'BOS-12': rvChildBody('c2') },
    extraBundle: {},
    bundleChildDescription: null,
    skip: new Set(),
  }
  edit(ctx)
  writeFileSync(
    path.join(dir, '.boss-skills.json'),
    JSON.stringify({
      trackerConfig: {
        linear: { mcpServer: 'demo', team: 'Demo', states: RV_STATES, labels: ctx.labels },
      },
    }),
  )
  const spec = {
    schemaVersion: 1,
    parentId: 'BOS-10',
    parent: { title: 'Epic', goal: 'g', keyChanges: ['x'], priority: 2 },
    children: [
      {
        key: 'c1',
        title: 't1',
        goal: 'g',
        keyChanges: ['x'],
        blockedByKeys: [],
        estimate: 2,
        priority: 2,
        agentFriendly: true,
        agentQuestion: false,
      },
      {
        key: 'c2',
        title: 't2',
        goal: 'g',
        keyChanges: ['x'],
        blockedByKeys: ['c1'],
        estimate: 2,
        priority: 2,
        agentFriendly: true,
        agentQuestion: false,
      },
    ],
  }
  const child = (n, key) => ({
    id: `uuid-${n}`,
    identifier: `BOS-${n}`,
    title: `t ${key}`,
    state: { name: 'Todo' },
    labels: [{ name: 'agent-friendly' }],
    attachments: [{ id: `p${n}`, title: `Implementation plan (BOS-${n})` }],
    links: [],
    ...(ctx.bundleChildDescription === null ? {} : { description: ctx.bundleChildDescription }),
  })
  const bundle = {
    parentId: 'BOS-10',
    childIds: ctx.childIds,
    parent: {
      id: 'uuid-10',
      identifier: 'BOS-10',
      state: { name: ctx.parentState },
      labels: [{ name: 'epic' }],
      attachments: [
        { id: 'spec', title: 'Epic spec (BOS-10)' },
        { id: 'plan', title: 'Implementation plan (BOS-10)' },
      ],
      links: [],
    },
    children: [child(11, 'c1'), child(12, 'c2')],
    ...ctx.extraBundle,
  }
  const files = {
    'BOS-10.epic-reverify.json': JSON.stringify(bundle),
    'BOS-10.epic-spec.json': JSON.stringify(spec),
    'BOS-10.epic-overview.md': RV_OVERVIEW,
    'BOS-10.image-guard-stored.md': RV_OVERVIEW,
    'BOS-10.child-BOS-11.image-guard-new.md': rvChildBody('c1'),
    'BOS-10.child-BOS-12.image-guard-new.md': rvChildBody('c2'),
    'BOS-10.child-BOS-11.image-guard-stored.md': ctx.childBodyStored['BOS-11'],
    'BOS-10.child-BOS-12.image-guard-stored.md': ctx.childBodyStored['BOS-12'],
  }
  for (const [name, text] of Object.entries(files)) {
    if (!ctx.skip.has(name)) writeFileSync(path.join(runDir, name), text)
  }
  return { dir, bundle: '.linear-plans/run-r1/BOS-10.epic-reverify.json', runDir }
}

function runEpicReverifyCli(fixture, extraEnv = {}) {
  const res = spawnSync(process.execPath, [GUARD, 'epic-reverify', fixture.bundle], {
    cwd: fixture.dir,
    encoding: 'utf8',
    env: { ...process.env, ...extraEnv },
  })
  const lines = res.stdout.split('\n').filter((line) => line !== '')
  return { ...res, lines, verdict: lines.length === 1 ? JSON.parse(lines[0]) : null }
}
const blockerCodes = (verdict) => verdict.blockers.map((b) => b.code)

test('epic-reverify: a conforming epic on disk exits 0 with one JSON verdict line on stdout', () => {
  const res = runEpicReverifyCli(writeEpicFixture())
  assert.equal(res.status, 0, res.stderr)
  assert.equal(res.lines.length, 1)
  assert.equal(res.verdict.ok, true)
  assert.equal(res.verdict.class, 'pass')
})

test('epic-reverify: a missing child stored read-back is a named stored-missing blocker', () => {
  const res = runEpicReverifyCli(
    writeEpicFixture((ctx) => ctx.skip.add('BOS-10.child-BOS-12.image-guard-stored.md')),
  )
  assert.notEqual(res.status, 0)
  const missing = res.verdict.blockers.filter((b) => b.code === 'stored-missing')
  assert.equal(missing.length, 1)
  assert.match(missing[0].message, /BOS-12/)
})

test('epic-reverify: a malformed bundle exits 3 as needs-human with unreadable-input', () => {
  const fixture = writeEpicFixture()
  writeFileSync(path.join(fixture.runDir, 'BOS-10.epic-reverify.json'), '{not json')
  const res = runEpicReverifyCli(fixture)
  assert.equal(res.status, 3)
  assert.equal(res.verdict.class, 'needs-human')
  assert.deepEqual(blockerCodes(res.verdict), ['unreadable-input'])
})

test('epic-reverify: UUID children resolve their files under the identifier the sentinel uses', () => {
  // The fixture's children carry `id: uuid-N` beside `identifier: BOS-N`, and every file is named
  // with the identifier — the passing run above already proves resolution; this pins it directly.
  const res = runEpicReverifyCli(writeEpicFixture())
  assert.equal(res.status, 0, res.stderr)
  assert.ok(!blockerCodes(res.verdict).includes('stored-missing'))
})

test('epic-reverify: a bundle description is ignored in favour of the stored file', () => {
  const res = runEpicReverifyCli(
    writeEpicFixture((ctx) => {
      ctx.bundleChildDescription = '## Summary\n\n…(truncated, use get_issue for full description)'
    }),
  )
  assert.equal(res.status, 0, res.stderr)
})

test('epic-reverify: the class — and the exit code — follow the parent state', () => {
  const drift = (ctx) =>
    (ctx.childBodyStored['BOS-11'] = rvChildBody('c1').replace(RV_NOTES, 'Changed.\n'))
  const planned = runEpicReverifyCli(writeEpicFixture(drift))
  assert.equal(planned.status, 3, planned.stderr)
  assert.equal(planned.verdict.class, 'needs-human')
  assert.ok(blockerCodes(planned.verdict).includes('child-body-drift'))

  const unplanned = runEpicReverifyCli(
    writeEpicFixture((ctx) => {
      drift(ctx)
      ctx.parentState = 'Unplanned'
    }),
  )
  assert.equal(unplanned.status, 1, unplanned.stderr)
  assert.equal(unplanned.verdict.class, 'resumable')
})

test('epic-reverify: a paths field in the bundle cannot redirect a comparison', () => {
  const fixture = writeEpicFixture((ctx) => {
    ctx.extraBundle = {
      paths: { intendedOverview: '/nonexistent', storedOverview: '/nonexistent' },
    }
  })
  const res = runEpicReverifyCli(fixture)
  assert.equal(res.status, 0, res.stderr)
})

test('epic-reverify: a config with no agentPlan or agentQuestion mapping runs and forbids nothing extra', () => {
  const res = runEpicReverifyCli(
    writeEpicFixture((ctx) => {
      ctx.labels = { agentFriendly: 'agent-friendly', needsHuman: 'needs-human', epic: 'epic' }
    }),
  )
  assert.equal(res.status, 0, res.stderr)
})

test('epic-reverify: a queue label on the parent is a parent-forbidden-label blocker', () => {
  const fixture = writeEpicFixture()
  const bundlePath = path.join(fixture.runDir, 'BOS-10.epic-reverify.json')
  const bundle = JSON.parse(readFileSync(bundlePath, 'utf8'))
  bundle.parent.labels.push({ name: 'agent-plan' })
  writeFileSync(bundlePath, JSON.stringify(bundle))
  const res = runEpicReverifyCli(fixture)
  assert.equal(res.status, 3)
  assert.ok(blockerCodes(res.verdict).includes('parent-forbidden-label'))
})

test('epic-reverify: the usage line names the verb, and the gate records under its own id', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'plan-run-guards-outcomes-'))
  const outcomes = path.join(dir, 'outcomes.tsv')
  const usage = runGuardRecording(['epic-reverify'], outcomes)
  assert.equal(usage.status, 2)
  assert.match(usage.stderr, /epic-reverify <bundle\.json>/)

  const pass = runEpicReverifyCli(writeEpicFixture(), { BOSS_GATE_OUTCOME_FILE: outcomes })
  assert.equal(pass.status, 0, pass.stderr)
  const fire = runEpicReverifyCli(
    writeEpicFixture((ctx) => (ctx.parentState = 'Unplanned')),
    { BOSS_GATE_OUTCOME_FILE: outcomes },
  )
  assert.equal(fire.status, 1)
  assert.deepEqual(recordedOutcomes(outcomes), [
    ['plan-run-guards.usage', 'fire', 'unknown-verb'],
    ['plan-run-guards.epic-reverify', 'pass', 'ok'],
    ['plan-run-guards.epic-reverify', 'fire', 'parent-unplanned'],
  ])
})

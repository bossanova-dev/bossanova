import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mergeRules, policyCodes } from './verify-merge-policy.mjs'
import { levelConfidence, levelFacts, legacyCodes } from './verify-level.mjs'
import { pathParkFacts } from './verify-path-parks.mjs'
import { reviewPolicyFromBlock } from './verify-review-policy.mjs'
import { AREAS } from './verify-areas.mjs'
import {
  DEFAULT_CONFIG,
  VERIFY_HUMAN_REVIEW_CATEGORIES,
  validateConfig,
  verifyPolicyFromConfigText,
} from '../skill-config.mjs'
import { schema } from '../skill-config-validate.generated.mjs'

const SIZE = { ok: true, thresholds: { p90: 100, p99: 200 } }

/** The codes a head over `paths` parks with under a review block and merge rules. */
function codesFor(
  paths,
  { review: block = null, auto, extensionReasons = [], outcomes = [], ...gate } = {},
) {
  const review = reviewPolicyFromBlock(block)
  const facts = {
    ...pathParkFacts(paths, { review }),
    changedPaths: paths,
    changedLines: 12,
    sizeBaseline: SIZE,
    judgeAreas: [],
    ci: { green: true },
    ...gate,
  }
  const result = levelConfidence(facts, { extensionReasons, outcomes })
  const merge = auto === undefined ? null : { auto }
  return policyCodes(result, levelFacts(facts, { extensionReasons, outcomes }), review, merge)
}

test('mergeRules: an absent block is null, a block without rules is empty', () => {
  assert.equal(mergeRules(undefined), null)
  assert.equal(mergeRules(null), null)
  assert.deepEqual(mergeRules({}), [])
  assert.deepEqual(mergeRules({ auto: [] }), [])
  assert.deepEqual(mergeRules({ auto: [{ level: 'high' }, {}, { area: 'ui', bogus: 'x' }] }), [
    { level: 'high' },
  ])
})

test('a high head merges only under a level-only high rule', () => {
  assert.deepEqual(codesFor(['src/a.go']), ['human-merge'])
  assert.deepEqual(codesFor(['src/a.go'], { auto: [] }), ['human-merge'])
  assert.deepEqual(codesFor(['src/a.go'], { auto: [{ level: 'moderate' }] }), ['human-merge'])
  assert.deepEqual(codesFor(['src/a.go'], { auto: [{ area: 'infra' }] }), ['human-merge'])
  assert.deepEqual(codesFor(['src/a.go'], { auto: [{ level: 'high' }] }), [])
})

test('with no rules a moderate head parks exactly as legacy codes do', () => {
  for (const paths of [['main.tf'], ['db/001.sql', 'main.tf'], ['api/x.proto']]) {
    const review = reviewPolicyFromBlock(null)
    const facts = {
      ...pathParkFacts(paths, { review }),
      changedPaths: paths,
      changedLines: 12,
      sizeBaseline: SIZE,
    }
    const result = levelConfidence(facts)
    assert.deepEqual(codesFor(paths), legacyCodes(result, review), paths.join())
    assert.deepEqual(codesFor(paths, { auto: [] }), legacyCodes(result, review), paths.join())
  }
})

test('floor asks park even under a level-only rule for their level', () => {
  const moderate = [{ level: 'moderate' }, { level: 'low' }]
  const cases = [
    ['alwaysReview', ['billing/a.go'], { review: { alwaysReview: { Billing: ['billing/**'] } } }],
    ['alwaysHumanPaths', ['billing/a.go'], { review: { alwaysHumanPaths: ['billing/**'] } }],
    ['extension', ['src/a.go'], { extensionReasons: ['human-review:security'] }],
    ['verify-policy', ['src/a.go'], { policySurfaceHit: true }],
    ['unproven behavior', ['src/a.go'], { extensionReasons: ['human-review:unproven'] }],
  ]
  for (const [label, paths, options] of cases) {
    assert.notDeepEqual(codesFor(paths, { ...options, auto: moderate }), [], label)
  }
  assert.deepEqual(
    codesFor(['billing/a.go'], {
      review: { alwaysReview: { Billing: ['billing/**'] } },
      auto: moderate,
    }),
    ['always-human-path'],
  )
  assert.deepEqual(
    codesFor(['src/a.go'], { extensionReasons: ['human-review:security'], auto: moderate }),
    ['human-review:security'],
  )
})

test('a capped area is a floor ask', () => {
  const cap = {
    key: 'other:src',
    label: 'src',
    cappedAt: '2026-10-01T00:00:00.000Z',
    escapes: [{ original: 1, fix: 2, fixMergedAt: '2026-10-01T00:00:00.000Z' }],
  }
  const capped = { areaCaps: { caps: [cap], health: [] } }
  assert.deepEqual(codesFor(['src/a.go'], capped), ['human-review:asks'])
  assert.deepEqual(codesFor(['src/a.go'], { ...capped, auto: [{ level: 'moderate' }] }), [
    'human-review:asks',
  ])
})

test('an uncovered ask keeps its own code and a covered ask beside it adds none', () => {
  const paths = ['db/001.sql', 'main.tf']
  assert.deepEqual(codesFor(paths), ['human-review:database', 'human-review:infra'])
  assert.deepEqual(codesFor(paths, { auto: [{ area: 'migrations' }] }), ['human-review:infra'])
  assert.deepEqual(codesFor(paths, { auto: [{ area: 'migrations' }, { area: 'infra' }] }), [])
  assert.deepEqual(codesFor(paths, { auto: [{ area: 'infra', level: 'low' }] }), [
    'human-review:database',
    'human-review:infra',
  ])
})

test('a legacy humanReview category is matched by its area id', () => {
  const review = { humanReview: { security: ['auth/**'] } }
  assert.deepEqual(codesFor(['auth/a.go'], { review }), ['human-review:security'])
  assert.deepEqual(codesFor(['auth/a.go'], { review, auto: [{ area: 'security' }] }), [])
  assert.deepEqual(codesFor(['auth/a.go'], { review, auto: [{ area: 'database' }] }), [
    'human-review:security',
  ])
})

test('a judge-raised ask is not covered by a level-only rule', () => {
  const judgeAreas = [
    {
      area: 'judge:access',
      label: 'Access',
      paths: ['src/a.go'],
      defaultAsk: 'always',
      source: 'judge',
      reason: 'removed an authorization check',
    },
  ]
  assert.deepEqual(codesFor(['src/a.go'], { judgeAreas, auto: [{ level: 'moderate' }] }), [
    'human-review:judge',
  ])
})

test('a size cause is covered only by a level-only rule for the head level', () => {
  const big = { changedLines: 150 }
  assert.deepEqual(codesFor(['src/a.go'], big), ['human-review:size'])
  assert.deepEqual(codesFor(['src/a.go'], { ...big, auto: [{ level: 'high' }] }), [
    'human-review:size',
  ])
  assert.deepEqual(codesFor(['src/a.go'], { ...big, auto: [{ area: 'infra' }] }), [
    'human-review:size',
  ])
  assert.deepEqual(codesFor(['src/a.go'], { ...big, auto: [{ level: 'moderate' }] }), [])
  assert.deepEqual(
    codesFor(['main.tf'], { ...big, auto: [{ area: 'infra' }] }),
    ['human-review:size'],
    'a covered ask leaves the size cause',
  )
})

test('a change rule matches only the area change fact it names', () => {
  const apiArea = (change) => ({
    areas: [
      {
        area: 'api-contract',
        label: 'API contract',
        paths: ['api/x.proto'],
        defaultAsk: 'always',
        source: 'builtin',
        ...(change ? { change } : {}),
      },
    ],
  })
  const auto = [{ area: 'api-contract', change: 'additive' }]
  assert.deepEqual(codesFor(['api/x.proto'], { ...apiArea('additive'), auto }), [])
  assert.deepEqual(codesFor(['api/x.proto'], { ...apiArea('breaking'), auto }), [
    'human-review:api-contract',
  ])
  assert.deepEqual(
    codesFor(['api/x.proto'], { ...apiArea(undefined), auto }),
    ['human-review:api-contract'],
    'a missing change fact never matches',
  )
})

test('a proof rule covers a UI ask only with that proof', () => {
  const review = { humanReview: { ui: ['web/**'] } }
  const auto = [{ area: 'ui', proof: 'tests' }]
  const proven = { uiTests: { covered: true } }
  assert.deepEqual(codesFor(['web/App.tsx'], { review, auto, ...proven }), [])
  assert.deepEqual(codesFor(['web/App.tsx'], { review, auto }), ['human-review:ui'])
  assert.deepEqual(
    codesFor(['web/App.tsx'], { review, auto: [{ area: 'ui', proof: 'recording' }], ...proven }),
    ['human-review:ui'],
  )
})

test('a proof rule covers a config UI ask only when the proof speaks to each of its paths', () => {
  const review = { humanReview: { ui: ['web/**', 'tui/**'] } }
  const proven = { uiTests: { covered: true } }
  const recorded = { outcomes: [{ outcome: 'ok', verdict: 'pass', recorded: true }] }
  const tests = [{ area: 'ui', proof: 'tests' }]
  const recording = [{ area: 'ui', proof: 'recording' }]
  const tested = ['web/App.tsx', 'web/App.test.tsx']
  assert.deepEqual(codesFor(tested, { review, auto: tests, ...proven }), [])
  assert.deepEqual(codesFor(['tui/x.go', ...tested], { review, auto: tests, ...proven }), [
    'human-review:ui',
  ])
  assert.deepEqual(codesFor(['web/App.tsx'], { review, auto: recording, ...recorded }), [])
  assert.deepEqual(
    codesFor(['tui/x.go', 'web/App.tsx'], { review, auto: recording, ...recorded }),
    ['human-review:ui'],
  )
})

test('cannot-assess and blocked heads keep their legacy codes', () => {
  assert.deepEqual(codesFor(['src/a.go'], { sizeBaseline: { ok: false, error: 'x' } }), [
    'cannot-assess',
  ])
  assert.deepEqual(
    codesFor(['src/a.go'], {
      blockers: [{ kind: 'ci', summary: 'red' }],
      auto: [{ level: 'high' }],
    }),
    [],
  )
})

/** The message a base-tip read and a full config load reject `merge` with, or null. */
function mergeFault(merge) {
  const text = JSON.stringify({ verifyDefaults: { merge } })
  const messages = []
  for (const load of [
    () => verifyPolicyFromConfigText(text, 'base'),
    () => validateConfig({ ...DEFAULT_CONFIG, verifyDefaults: { merge } }, 'test'),
  ]) {
    try {
      load()
      messages.push(null)
    } catch (error) {
      messages.push(error.message.replace(/^skill-config: invalid config from \w+: /, ''))
    }
  }
  assert.equal(messages[0], messages[1], JSON.stringify(merge))
  return messages[0]
}

test('schema: verifyDefaults.merge accepts the documented rules', () => {
  assert.equal(
    mergeFault({
      auto: [
        { level: 'high' },
        { area: 'ui', proof: 'tests' },
        { area: 'api-contract', change: 'additive' },
        { area: 'database', level: 'moderate' },
      ],
    }),
    null,
  )
  assert.equal(mergeFault({ policy: 'docs/merge-policy.md' }), null)
  assert.equal(mergeFault({}), null)
  assert.equal(mergeFault({ auto: [] }), null)
})

test('schema: verifyDefaults.merge rejects what it does not define with a named message', () => {
  const expected = 'expected level, area, proof, change'
  for (const [merge, message] of [
    ['x', 'verifyDefaults.merge must be an object when present'],
    [{ auto: 'x' }, 'verifyDefaults.merge.auto must be an array of rules'],
    [
      { auto: [{}] },
      `verifyDefaults.merge.auto.0 must name at least one of level, area, proof, change`,
    ],
    [
      { auto: [{ level: 'high', path: 'x' }] },
      `verifyDefaults.merge.auto.0.path is unknown (${expected})`,
    ],
    [{ auto: [{ proof: 'tests' }] }, 'verifyDefaults.merge.auto.0.proof needs area "ui"'],
    [
      { auto: [{ area: 'infra', proof: 'tests' }] },
      'verifyDefaults.merge.auto.0.proof needs area "ui"',
    ],
    [{ auto: [{ change: 'additive' }] }, 'verifyDefaults.merge.auto.0.change needs an area'],
    [
      { auto: [{ area: 'api-contract', change: 'breaking' }] },
      'verifyDefaults.merge.auto.0.change must be "additive"',
    ],
    [
      { auto: [{ level: 'blocked' }] },
      'verifyDefaults.merge.auto.0.level must be "high", "moderate" or "low"',
    ],
    [{ auto: [{ area: 'housekeeping' }] }, /^verifyDefaults\.merge\.auto\.0\.area must be one of /],
    [
      { auto: Array.from({ length: 33 }, () => ({ level: 'high' })) },
      'verifyDefaults.merge.auto holds at most 32 rules',
    ],
  ]) {
    const fault = mergeFault(merge)
    if (message instanceof RegExp) assert.match(fault ?? '', message)
    else assert.equal(fault, message)
  }
})

test('schema: a rule area is a built-in area that can ask, or a humanReview category', () => {
  const derived = new Set([
    ...AREAS.filter((area) => area.defaultAsk !== 'never').map((area) => area.area),
    ...VERIFY_HUMAN_REVIEW_CATEGORIES,
  ])
  const rule = schema.properties.verifyDefaults.properties.merge.properties.auto.items
  assert.deepEqual(new Set(rule.properties.area.enum), derived)
})

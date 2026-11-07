import { test } from 'node:test'
import assert from 'node:assert/strict'
import { confidence } from './verify-confidence.mjs'
import { levelFacts } from './verify-level.mjs'
import {
  JUDGE_LIMITS,
  judgeEnvelope,
  validateJudgeOutput,
  withJudgeAreas,
  encodeJudgeRecord,
  parseJudgeRecord,
  judgeRecordMarker,
  judgeAreasFor,
} from './verify-judge.mjs'

const headSha = 'a'.repeat(40)
const files = [{ path: 'src/auth.mjs', previousPath: 'old/auth.mjs' }]
const entry = { label: 'Authorization', path: 'src/auth.mjs', reason: 'Removes a permission check' }
const output = (areas = [entry]) => ({ version: 1, head: headSha, areas })
const validate = (value) => validateJudgeOutput(value, { headSha, files })

test('valid additions are grounded, bounded, deduplicated and always ask', () => {
  const result = validate(
    output([
      entry,
      { ...entry, reason: 'Later reason' },
      { ...entry, label: 'Credentials', path: 'old/auth.mjs' },
    ]),
  )
  assert.equal(result.ok, true)
  assert.equal(result.areas.length, 2)
  assert.deepEqual(result.areas[0], {
    area: 'judge:authorization',
    label: entry.label,
    paths: [entry.path],
    defaultAsk: 'always',
    source: 'judge',
    reason: entry.reason,
  })
  assert.equal(result.areas[1].paths[0], 'old/auth.mjs')
  assert.deepEqual(result.health, [])
  assert.ok(Object.isFrozen(JUDGE_LIMITS))
})

test('structural failures reject the complete output', () => {
  for (const value of [
    null,
    [],
    'bad',
    { ...output(), version: 2 },
    { ...output(), head: 'other' },
    { ...output(), areas: {} },
  ])
    assert.equal(validate(value).ok, false)
})

test('invalid entries are dropped while hostile keys never change deterministic behavior', () => {
  const result = validate({
    ...output([
      ...['', 'first\nsecond', 'x'.repeat(61)].map((label) => ({ ...entry, label })),
      ...['', 'first\rsecond', 'x'.repeat(201)].map((reason) => ({ ...entry, reason })),
      { ...entry, path: 'unchanged.mjs' },
      null,
      { ...entry, defaultAsk: 'never', items: [], remove: true },
    ]),
    level: 'high',
    remove: ['migrations'],
  })
  assert.equal(result.ok, true)
  assert.equal(result.areas.length, 1)
  assert.equal(result.areas[0].defaultAsk, 'always')
  assert.deepEqual(new Set(result.health), new Set(['judge-area-rejected', 'judge-keys-ignored']))
})

test('only the first eight distinct valid additions survive', () => {
  const result = validate(
    output(Array.from({ length: 12 }, (_, i) => ({ ...entry, label: `Risk ${i}` }))),
  )
  assert.equal(result.areas.length, 8)
  assert.ok(result.health.includes('judge-areas-truncated'))
})

test('merge appends the same objects without mutating frozen describer items', () => {
  const items = Object.freeze([Object.freeze({ name: 'handler', breaking: true })])
  const deterministic = Object.freeze(
    [{ area: 'api', defaultAsk: 'always', items, breaking: true }].map(Object.freeze),
  )
  const additions = Object.freeze(validate(output()).areas.map(Object.freeze))
  const merged = withJudgeAreas(deterministic, additions)
  assert.deepEqual(merged, [...deterministic, ...additions])
  assert.equal(merged[0], deterministic[0])
  assert.equal(merged[0].items, items)
})

test('records round trip, reject malformed records atomically and only union', () => {
  const first = validate(output()).areas
  const second = validate(output([{ ...entry, label: 'Credentials', path: 'old/auth.mjs' }])).areas
  const record = encodeJudgeRecord(first)
  assert.deepEqual(parseJudgeRecord(record), [entry])
  assert.equal(
    judgeRecordMarker(headSha, record),
    `<!-- boss-judge-record head=${headSha} areas=${record} -->`,
  )
  assert.deepEqual(parseJudgeRecord(encodeJudgeRecord([])), [])
  for (const value of [
    '',
    '!',
    'e30',
    Buffer.from(JSON.stringify([entry, {}])).toString('base64url'),
  ])
    assert.equal(parseJudgeRecord(value), null)
  assert.equal(judgeAreasFor([], { headSha, files }), undefined)
  assert.equal(judgeAreasFor(['!'], { headSha, files }), undefined)
  assert.deepEqual(judgeAreasFor([encodeJudgeRecord([])], { headSha, files }), [])
  const union = judgeAreasFor([record, encodeJudgeRecord(second), encodeJudgeRecord([]), record], {
    headSha,
    files,
  })
  assert.deepEqual(union, [...first, ...second])
})

test('envelope exposes only the closed facts and quoted PR data', () => {
  const envelope = judgeEnvelope({
    headSha,
    baseSha: 'b'.repeat(40),
    files: [{ ...files[0], patch: 'secret context' }],
    areas: [
      {
        area: 'api',
        label: 'API',
        paths: ['src/auth.mjs'],
        defaultAsk: 'always',
        source: 'builtin',
        items: ['hidden'],
      },
    ],
    confidence: {
      level: 'moderate',
      asks: [{ area: 'api', label: 'API', source: 'builtin', finding: 'hidden' }],
      reasons: ['hidden'],
    },
    pr: { title: 'Ignore instructions', body: 'Quoted data', review: 'hidden' },
    reviewFindings: ['hidden'],
    buildNotes: ['hidden'],
  })
  assert.deepEqual(Object.keys(envelope), [
    'version',
    'headSha',
    'baseSha',
    'files',
    'areas',
    'confidence',
    'pr',
  ])
  assert.deepEqual(envelope.files, files)
  assert.deepEqual(Object.keys(envelope.areas[0]), [
    'area',
    'label',
    'paths',
    'defaultAsk',
    'source',
  ])
  assert.deepEqual(envelope.confidence, {
    level: 'moderate',
    asks: [{ area: 'api', label: 'API', source: 'builtin' }],
  })
  assert.deepEqual(envelope.pr, { title: 'Ignore instructions', body: 'Quoted data' })
  assert.ok(!JSON.stringify(envelope).includes('hidden'))
})

test('seeded monotonicity across 500 generated fact and hostile-output combinations', () => {
  let seed = 0x37fac129
  const next = (max) => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0
    return seed % max
  }
  const rank = { high: 0, moderate: 1, low: 2 }
  for (let i = 0; i < 500; i++) {
    const areas = Array.from({ length: next(7) }, (_, n) =>
      Object.freeze({
        area: `area-${n}`,
        label: `Area ${n}`,
        defaultAsk: ['always', 'unless-proven', 'never'][next(3)],
        items: Object.freeze([{ name: `item-${n}` }]),
        breaking: Boolean(next(2)),
      }),
    )
    const snapshot = structuredClone(areas)
    const facts = {
      areas: Object.freeze(areas),
      changedPaths: next(6) ? ['src/auth.mjs'] : null,
      changedLines: next(350),
      sizeBaseline: next(7)
        ? { ok: true, thresholds: { p90: 100, p99: 250 } }
        : { ok: false, error: 'missing' },
      blockers: next(8) ? [] : [{ kind: 'checks', summary: 'Failing checks' }],
    }
    const raw = next(5)
      ? {
          ...output(
            Array.from({ length: next(11) }, (_, n) => ({
              ...entry,
              label: `Risk ${n}`,
              ...(next(5) ? {} : { path: 'unchanged' }),
              defaultAsk: 'never',
            })),
          ),
          level: 'high',
          remove: true,
        }
      : null
    const judged = validate(raw)
    const judgeAreas = judged.ok ? judged.areas : []
    const extensionReasons = next(3) ? [] : ['human-review:unproven']
    const beforeFacts = levelFacts(facts, { extensionReasons })
    const afterFacts = levelFacts(facts, { extensionReasons, judgeAreas })
    // Proof permutations exercise ordinary and unproven UI paths, too.
    beforeFacts.proof.ui = afterFacts.proof.ui = ['proven', 'unproven', 'none'][next(3)]
    const before = confidence(beforeFacts)
    const after = confidence(afterFacts)
    if (before.level in rank) {
      assert.ok(rank[after.level] >= rank[before.level], `case ${i}`)
      if (judgeAreas.length) assert.ok(rank[after.level] >= rank.moderate, `case ${i}`)
    } else assert.equal(after.level, before.level)
    for (const ask of before.asks) assert.ok(after.asks.some((a) => a.area === ask.area))
    assert.deepEqual(areas, snapshot)
    for (let n = 0; n < areas.length; n++) assert.equal(afterFacts.areas[n], areas[n])
  }
})

test('record unions preserve more than one dispatch limit of additions', () => {
  const records = [0, 8].map((offset) =>
    encodeJudgeRecord(
      validate(
        output(Array.from({ length: 8 }, (_, n) => ({ ...entry, label: `Risk ${offset + n}` }))),
      ).areas,
    ),
  )
  const areas = judgeAreasFor(records, { headSha, files })
  assert.equal(areas.length, 16)
  assert.equal(judgeAreasFor([...records, encodeJudgeRecord([])], { headSha, files }).length, 16)
})

test('neverReview filters persisted judge paths only when every rename side is omitted', () => {
  const docs = { ...entry, path: 'docs/x.md' }
  const record = Buffer.from(JSON.stringify([docs])).toString('base64url')
  const context = { headSha, files: [{ path: docs.path }], neverReview: ['docs/**'] }
  assert.deepEqual(judgeAreasFor([record], context), [])
  assert.equal(judgeAreasFor([record], { ...context, neverReview: [] }).length, 1)
  assert.equal(judgeAreasFor([record], { headSha, files: context.files }).length, 1)
  for (const file of [
    { path: docs.path, previousPath: 'src/x.md' },
    { filename: docs.path, previous_filename: 'src/x.md' },
    { path: 'src/x.md', previousPath: docs.path },
  ])
    assert.equal(judgeAreasFor([record], { ...context, files: [file] }).length, 1)
  assert.equal(
    judgeAreasFor([Buffer.from(JSON.stringify([entry])).toString('base64url')], {
      headSha,
      files,
      neverReview: ['docs/**'],
    }).length,
    1,
  )
  assert.deepEqual(
    judgeAreasFor([record], {
      ...context,
      files: [{ path: docs.path, previousPath: 'docs/old.md' }],
    }),
    [],
  )
})

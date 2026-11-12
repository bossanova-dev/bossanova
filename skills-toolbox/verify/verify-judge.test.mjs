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
  judgeReview,
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
        items: ['describer fact'],
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
    'items',
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

test('noSignOff filters persisted judge paths only when every rename side is omitted', () => {
  const docs = { ...entry, path: 'docs/x.md' }
  const record = Buffer.from(JSON.stringify([docs])).toString('base64url')
  const context = { headSha, files: [{ path: docs.path }], noSignOff: ['docs/**'] }
  assert.deepEqual(judgeAreasFor([record], context), [])
  assert.equal(judgeAreasFor([record], { ...context, noSignOff: [] }).length, 1)
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
      noSignOff: ['docs/**'],
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

test('judgeReview keeps findings on noSignOff paths as notes, never as asking areas', () => {
  const docs = { ...entry, label: 'Docs drift', path: 'docs/x.md', reason: 'Stale example' }
  const code = { ...entry, label: 'Access', path: 'src/a.go', reason: 'Access checks removed' }
  const record = Buffer.from(JSON.stringify([docs, code])).toString('base64url')
  const context = {
    headSha,
    files: [{ path: docs.path }, { path: code.path }],
    noSignOff: ['docs/**'],
  }
  const review = judgeReview([record], context)
  assert.deepEqual(
    review.notes.map((a) => [a.label, a.paths, a.reason]),
    [['Docs drift', ['docs/x.md'], 'Stale example']],
  )
  assert.deepEqual(
    review.areas.map((a) => [a.label, a.paths, a.reason]),
    [['Access', ['src/a.go'], 'Access checks removed']],
  )
  assert.deepEqual(judgeAreasFor([record], context), review.areas)
  assert.equal(judgeReview([], context), undefined)
  assert.deepEqual(judgeReview([record], { ...context, noSignOff: [] }).notes, [])
  assert.equal(judgeReview([record], { ...context, noSignOff: [] }).areas.length, 2)
  // A rename with one side outside the globs stays an ask, as for judgeAreasFor.
  const renamed = judgeReview([Buffer.from(JSON.stringify([docs])).toString('base64url')], {
    ...context,
    files: [{ path: docs.path, previousPath: 'src/x.md' }],
  })
  assert.equal(renamed.areas.length, 1)
  assert.deepEqual(renamed.notes, [])
  const within = judgeReview([Buffer.from(JSON.stringify([docs])).toString('base64url')], {
    ...context,
    files: [{ path: docs.path, previousPath: 'docs/old.md' }],
  })
  assert.deepEqual(within.areas, [])
  assert.equal(within.notes.length, 1)
})

test('policy decisions and describer evidence cross the closed judge boundary', async () => {
  const { gitBlobSha } = await import('./verify-merge-prose.mjs')
  const { parseJudgeDecisions } = await import('./verify-judge.mjs')
  const text = '- Authorization changes require human review.'
  const mergePolicy = { state: 'ok', path: 'policy.md', blob: gitBlobSha(Buffer.from(text)), text }
  const area = {
    area: 'api-contract',
    label: 'API',
    paths: ['src/auth.mjs'],
    defaultAsk: 'always',
    items: ['5 additive changes, 0 breaking'],
    change: 'additive',
    ignored: true,
  }
  const input = {
    headSha,
    baseSha: 'b'.repeat(40),
    files,
    areas: [area],
    confidence: { level: 'moderate', asks: [] },
    pr: {},
    mergePolicy,
  }
  const envelope = judgeEnvelope(input)
  assert.deepEqual(envelope.mergePolicy, { path: 'policy.md', blob: mergePolicy.blob, text })
  assert.deepEqual(envelope.areas[0].items, area.items)
  assert.equal(envelope.areas[0].change, 'additive')
  assert.equal(envelope.areas[0].ignored, undefined)
  assert.equal('mergePolicy' in judgeEnvelope({ ...input, mergePolicy: { state: 'none' } }), false)
  const decision = {
    ask: 'Authorization',
    decision: 'human',
    rule: 'Authorization changes require human review.',
    reason: 'Changes the authorization boundary',
    path: 'src/auth.mjs',
  }
  const result = validateJudgeOutput(
    { ...output(), policyBlob: mergePolicy.blob, decisions: [decision] },
    { headSha, files, mergePolicy },
  )
  assert.equal(result.ok, true)
  assert.deepEqual(result.health, [])
  assert.equal(result.decisions[0].ask, 'judge:authorization')
  const record = encodeJudgeRecord(result.areas, {
    policy: mergePolicy,
    decisions: result.decisions,
  })
  assert.deepEqual(parseJudgeRecord(record), [entry])
  assert.deepEqual(parseJudgeDecisions(record), {
    policyBlob: mergePolicy.blob,
    decisions: result.decisions,
  })
  assert.equal(parseJudgeDecisions(encodeJudgeRecord(result.areas)), null)
  assert.equal(
    encodeJudgeRecord(result.areas, { policy: { state: 'none' }, decisions: [] }),
    encodeJudgeRecord(result.areas),
  )
  const malformed = Buffer.from(
    JSON.stringify({
      areas: [entry],
      policyBlob: mergePolicy.blob,
      decisions: [{ ...decision, verified: 'yes' }],
    }),
  ).toString('base64url')
  assert.equal(parseJudgeRecord(malformed), null)
  assert.equal(parseJudgeDecisions(malformed), null)
})

test('judge policy decisions require the exact policy blob that was assessed', async () => {
  const { gitBlobSha } = await import('./verify-merge-prose.mjs')
  const text = '- Authorization changes require human review.'
  const mergePolicy = { state: 'ok', path: 'policy.md', blob: gitBlobSha(Buffer.from(text)), text }
  for (const policyBlob of [undefined, 'c'.repeat(40)]) {
    const result = validateJudgeOutput(
      { ...output(), policyBlob, decisions: [] },
      { headSha, files, mergePolicy },
    )
    assert.equal(result.ok, false)
  }
  assert.equal(
    validateJudgeOutput(
      { ...output(), policyBlob: mergePolicy.blob },
      { headSha, files, mergePolicy },
    ).ok,
    true,
  )
})

import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { MAX_ITEMS, MAX_TEXT_CHARS } from './verify-proof.mjs'
import { Readable } from 'node:stream'
import { validateResult } from '../skill-extensions.mjs'
import { NO_SIGN_OFF_AREA } from './verify-areas.mjs'
import {
  foldChange,
  boundDescription,
  describerMain,
  refineDescription,
  parseDescribeMarker,
  runDescribers,
  describeAreas,
  MAX_DESCRIBE_LINKS,
  MAX_DESCRIBE_OUTPUT_BYTES,
} from './verify-describe.mjs'
import { MAX_BLOCKS, MAX_OPERATION_CHARS, locatedRisks } from './verify-describe-blocks.mjs'

const valid = () => ({
  ok: true,
  extension: 'describe-test',
  role: 'proof-describe',
  items: ['A change'],
  breaking: false,
  undisclosed: [],
})
const area = (defaultAsk = 'never') => ({
  area: 'housekeeping',
  label: 'Housekeeping',
  paths: ['README.md'],
  defaultAsk,
})

test('description bounds and strict keys share the proof limits', () => {
  assert.deepEqual(refineDescription(valid()), [])
  for (const patch of [
    { ask: false },
    { defaultAsk: 'never' },
    { level: 'high' },
    { ok: false },
    { role: 'verify' },
    { items: 'text' },
    { items: [1] },
    { items: [''] },
    { items: ['line\nbreak'] },
    { items: [' '.repeat(2)] },
    { items: ['x'.repeat(MAX_TEXT_CHARS + 1)] },
    { items: Array(MAX_ITEMS + 1).fill('x') },
    { undisclosed: Array(MAX_ITEMS + 1).fill('x') },
    { breaking: 1 },
    { links: Array(MAX_DESCRIBE_LINKS + 1).fill({ text: 'x', url: 'https://example.test' }) },
    ...[
      'http://example.test',
      'javascript:alert(1)',
      'https://user@example.test',
      'https://example.test/a b',
    ].map((url) => ({ links: [{ text: 'x', url }] })),
  ])
    assert.ok(refineDescription({ ...valid(), ...patch }).length, JSON.stringify(patch))
  assert.deepEqual(
    refineDescription({
      ...valid(),
      items: Array(MAX_ITEMS).fill('x'.repeat(MAX_TEXT_CHARS)),
      links: Array(MAX_DESCRIBE_LINKS).fill({ text: 'x', url: 'https://example.test' }),
    }),
    [],
  )
})

test('marker accepts extension-relative scripts and comma separated area ids', () => {
  assert.deepEqual(
    parseDescribeMarker({ describe: './scripts/describe.mjs', areas: 'api-contract, config:db' }),
    { describe: 'scripts/describe.mjs', describeAreas: ['api-contract', 'config:db'] },
  )
  for (const describe of [
    '/tmp/script.mjs',
    '../script.mjs',
    'a/../script.mjs',
    'C:\\script.mjs',
    '',
  ])
    assert.equal(parseDescribeMarker({ describe, areas: 'ui' }), null)
  for (const areas of ['', 'UI', 'api contract', 'ui,'])
    assert.equal(parseDescribeMarker({ describe: 'describe.mjs', areas }), null)
})

function fixture(t, script) {
  const root = tempDir('verify-describe-')
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const dir = path.join(root, 'extension')
  fs.mkdirSync(dir)
  fs.writeFileSync(path.join(dir, 'describe.mjs'), script)
  return {
    root,
    dir,
    extension: {
      name: 'describe-test',
      dir,
      describe: 'describe.mjs',
      describeAreas: ['housekeeping'],
      order: 1,
    },
  }
}

test('runner resolves script under extension, runs matching areas, and sends only facts', (t) => {
  const { root, extension } = fixture(
    t,
    `let input='';for await (const chunk of process.stdin) input+=chunk;const envelope=JSON.parse(input);process.stdout.write(JSON.stringify({ok:true,extension:envelope.extension,role:envelope.role,items:[JSON.stringify(envelope),process.cwd()],breaking:false,undisclosed:[]}))`,
  )
  const outcomes = runDescribers({
    extensions: [extension],
    areas: [area(), { ...area(), area: 'ui' }],
    baseSha: 'b'.repeat(40),
    headSha: 'a'.repeat(40),
    pr: { number: 4, title: 'title', body: 'body' },
    cwd: root,
  })
  assert.equal(outcomes.length, 1)
  assert.equal(outcomes[0].ok, true)
  const input = JSON.parse(outcomes[0].description.items[0])
  assert.deepEqual(input.area, {
    area: 'housekeeping',
    label: 'Housekeeping',
    paths: ['README.md'],
  })
  assert.deepEqual(input.pr, { number: 4, title: 'title', body: 'body' })
  assert.equal(input.baseSha, 'b'.repeat(40))
  assert.equal(input.headSha, 'a'.repeat(40))
  assert.equal(input.ask, undefined)
  assert.equal(input.level, undefined)
  assert.equal(input.area.defaultAsk, undefined)
  assert.equal(outcomes[0].description.items[1], fs.realpathSync(root))
})

for (const [reason, script] of [
  ['exit:1', 'process.exit(1)'],
  ['signal', "process.kill(process.pid,'SIGTERM')"],
  ['timeout', 'setInterval(()=>{},1000)'],
  ['oversize', `process.stdout.write('x'.repeat(${MAX_DESCRIBE_OUTPUT_BYTES + 1}))`],
  ['not-json', "process.stdout.write('oops')"],
  ['invalid:', `process.stdout.write(JSON.stringify({ ...${JSON.stringify(valid())},ask:false }))`],
  ['script-missing', null],
])
  test(`runner reports ${reason} without throwing`, (t) => {
    const { root, dir, extension } = fixture(t, script ?? '')
    if (script === null) fs.unlinkSync(path.join(dir, 'describe.mjs'))
    const [outcome] = runDescribers({
      extensions: [extension],
      areas: [area()],
      baseSha: 'b'.repeat(40),
      headSha: 'a'.repeat(40),
      pr: {},
      cwd: root,
      ...(reason === 'timeout' ? { timeoutMs: 200 } : {}),
    })
    assert.equal(outcome.ok, false)
    assert.ok(outcome.reason.startsWith(reason), outcome.reason)
  })

test('ask monotonicity holds for every path policy and describer outcome', () => {
  for (const defaultAsk of ['never', 'unless-proven', 'always'])
    for (const result of [
      { ok: true, description: valid() },
      { ok: true, description: { ...valid(), breaking: true } },
      { ok: false, reason: 'timeout' },
    ]) {
      const original = Object.freeze({ ...area(defaultAsk), paths: Object.freeze(['README.md']) })
      const merged = describeAreas(Object.freeze([original]), [
        { extension: 'describe-test', area: 'housekeeping', order: 1, ...result },
      ])
      assert.equal(merged.areas[0].defaultAsk, result.description?.breaking ? 'always' : defaultAsk)
      assert.deepEqual(merged.collectors, [
        { name: 'describe:describe-test:housekeeping', ok: result.ok },
      ])
      assert.equal(merged.health.length, result.ok ? 0 : 1)
      if (!result.ok) assert.equal(merged.health[0].code, 'describer-failed')
    }
})

test('a breaking describer keeps its facts on the no-sign-off area but never makes it ask', () => {
  const outcome = (areaId) => [
    {
      extension: 'describe-test',
      area: areaId,
      order: 1,
      ok: true,
      description: { ...valid(), items: ['Drops a column'], breaking: true },
    },
  ]
  const noSignOff = { ...area('never'), area: NO_SIGN_OFF_AREA, label: 'No sign-off' }
  const [described] = describeAreas([noSignOff], outcome(NO_SIGN_OFF_AREA)).areas
  assert.equal(described.defaultAsk, 'never')
  assert.equal(described.breaking, true)
  assert.deepEqual(described.items, ['Drops a column'])
  assert.deepEqual(described.describers, [
    { extension: 'describe-test', outcome: 'breaking', items: ['Drops a column'] },
  ])
  assert.equal(
    describeAreas([area('never')], outcome('housekeeping')).areas[0].defaultAsk,
    'always',
  )
})

test('merge is deterministic, deduplicates in descriptor order and bounds lists', () => {
  const first = {
    extension: 'first',
    area: 'housekeeping',
    order: 1,
    ok: true,
    description: {
      ...valid(),
      items: Array.from({ length: MAX_ITEMS }, (_, i) => `item ${i}`),
      links: [{ text: 'doc', url: 'https://example.test' }],
      undisclosed: ['missing'],
    },
  }
  const second = {
    extension: 'second',
    area: 'housekeeping',
    order: 2,
    ok: true,
    description: {
      ...valid(),
      items: ['item 0', 'overflow'],
      links: [{ text: 'doc', url: 'https://example.test' }],
      undisclosed: ['missing'],
    },
  }
  const merged = describeAreas([area()], [second, first])
  assert.deepEqual(merged, describeAreas([area()], [first, second]))
  assert.deepEqual(merged.areas[0].items, first.description.items)
  assert.equal(merged.areas[0].links.length, 1)
  assert.deepEqual(merged.undisclosed, { housekeeping: ['missing'] })
  assert.equal(merged.health[0].code, 'describer-items-truncated')
})

test('merged undisclosed lists retain the full unique union across bounded describers', () => {
  const first = Array.from({ length: MAX_ITEMS }, (_, i) => `first ${i}`)
  const second = Array.from({ length: MAX_ITEMS }, (_, i) => `second ${i}`)
  const outcomes = [first, second].map((undisclosed, order) => ({
    extension: `facts-${order}`,
    area: 'housekeeping',
    order,
    ok: true,
    description: { ...valid(), undisclosed },
  }))
  const merged = describeAreas([area()], outcomes)
  assert.deepEqual(merged.undisclosed.housekeeping, [...first, ...second])
  assert.deepEqual(merged.areas[0].undisclosed, [...first, ...second])
  assert.deepEqual(merged.health, [])
})

test('boundDescription makes any raw facts a valid bounded result', () => {
  const result = boundDescription('boss-verify-x', {
    items: [
      'a\nb',
      'x'.repeat(MAX_TEXT_CHARS + 50),
      '  ',
      'a\nb',
      ...Array.from({ length: 30 }, (_, i) => `i${i}`),
    ],
    breaking: 'yes',
  })
  assert.deepEqual(validateResult(result, 'proof-describe').errors, [])
  assert.equal(result.items.length, MAX_ITEMS)
  assert.equal(result.items[0], 'a b')
  assert.equal(result.breaking, false)
  assert.deepEqual(result.undisclosed, [])
})

test('describerMain reads the envelope from stdin and rejects flags', async () => {
  let written = ''
  const stdout = { write: (text) => (written += text) }
  const envelope = { extension: 'boss-verify-x', area: { paths: ['a'] } }
  await describerMain((input, { cwd }) => ({ items: [input.area.paths[0], cwd], breaking: true }), {
    argv: [],
    stdin: Readable.from([JSON.stringify(envelope)]),
    stdout,
    cwd: '/repo',
  })
  const result = JSON.parse(written)
  assert.deepEqual(result.items, ['a', '/repo'])
  assert.equal(result.breaking, true)
  await assert.rejects(
    describerMain(() => ({}), { argv: ['--fast'], stdin: Readable.from([]), stdout }),
  )
})

test('schema change facts fold failures and breaking descriptions monotonically', () => {
  const input = { ...area('never'), change: 'additive', changeReasons: ['schema additive'] }
  const failing = { area: input.area, extension: 'failed-tool', ok: false, reason: 'exit:1' }
  const broken = describeAreas([input], [failing]).areas[0]
  assert.equal(broken.change, 'breaking')
  assert.equal(broken.defaultAsk, 'never')
  assert.ok(
    broken.changeReasons.some((text) => text.includes('failed-tool') && text.includes('failed')),
  )
  const outcome = {
    area: input.area,
    extension: 'describe-test',
    ok: true,
    description: { ...valid(), breaking: true },
  }
  assert.equal(describeAreas([input], [outcome]).areas[0].change, 'breaking')
  assert.equal(describeAreas([{ ...input, change: 'breaking' }], []).areas[0].change, 'breaking')
  assert.equal('change' in describeAreas([area()], [outcome]).areas[0], false)
})

test('foldChange can only strengthen a computed change', () => {
  for (const change of ['additive', 'breaking'])
    for (const breaking of [false, true])
      for (const failed of [false, true])
        assert.equal(
          foldChange(change, { breaking, failed }),
          change === 'breaking' || breaking || failed ? 'breaking' : 'additive',
        )
})

test('holding describers retain their own items and failure reasons in descriptor order', () => {
  const outputs = [
    { extension: 'additive', area: 'housekeeping', ok: true, order: 1, description: valid() },
    {
      extension: 'breaking',
      area: 'housekeeping',
      ok: true,
      order: 2,
      description: { ...valid(), breaking: true, items: ['Removed field', 'Removed field'] },
    },
    { extension: 'failed', area: 'housekeeping', ok: false, order: 3, reason: 'timeout' },
  ]
  const result = describeAreas([area()], outputs)
  assert.deepEqual(result.areas[0].describers, [
    { extension: 'breaking', outcome: 'breaking', items: ['Removed field'] },
    { extension: 'failed', outcome: 'failed', reason: 'timeout' },
  ])
  assert.deepEqual(result.areas[0].items, ['A change', 'Removed field'])
  assert.equal(result.areas[0].defaultAsk, 'always')
  assert.equal(result.areas[0].breaking, true)
  assert.deepEqual(describeAreas([area()], []).areas[0].describers, [])
})

/** One block of every kind, a signal and a located risk, as a describer would send them. */
const typed = () => ({
  blocks: [
    {
      kind: 'schema',
      types: [
        {
          keyword: 'type',
          name: 'Mutation',
          change: 'changed',
          class: 'breaking',
          mobile: 'Shipped builds still send `archive`',
          fields: [
            {
              name: 'markOverdue',
              args: [{ name: 'asOf', type: 'Date!', default: 'null' }],
              type: 'Int!',
              directives: ['@auth'],
              change: 'added',
            },
            { name: 'archive', type: 'Boolean', change: 'removed' },
            { name: 'total', type: 'Int!', before: 'Int', change: 'changed' },
          ],
        },
        { keyword: 'enum', name: 'Status', change: 'added', fields: [{ name: 'OVERDUE' }] },
      ],
    },
    {
      kind: 'client-call',
      name: 'Mutation.markOverdue',
      path: 'web/useMarkOverdue.ts',
      operation: 'mutation M($asOf: Date!) {\n  markOverdue(asOf: $asOf)\n}',
      before: ['useInvoices()'],
      after: ['useInvoices()', 'useMarkOverdue()'],
      note: 'A new document',
    },
    {
      kind: 'route',
      method: 'POST',
      path: '/webhooks/run',
      auth: 'none',
      change: 'added',
      note: 'The scheduler calls it',
    },
    {
      kind: 'table',
      name: 'invoices',
      relation: 'table',
      materialization: 'table',
      change: 'changed',
      columns: [
        {
          name: 'email',
          type: 'text',
          constraints: 'not null',
          change: 'added',
          pii: true,
          note: 'From the customer',
        },
      ],
    },
    { kind: 'code', path: 'api/routes.go', start: 5, lines: ['func Routes() {', '', '}'] },
    { kind: 'steps', steps: ['Run `./migrate`'] },
  ],
  signals: [{ key: 'db', label: 'Database', state: 'additive', summary: 'Adds a column' }],
  risks: [
    {
      level: 'high',
      title: 'Anyone can run billing',
      impact: 'An unauthenticated POST moves invoices',
      guard: 'None',
      at: { path: 'api/routes.go', line: 7 },
    },
  ],
})

test('every block kind, signal and risk validates and rides its area through the merge', () => {
  const facts = typed()
  assert.deepEqual(refineDescription({ ...valid(), ...facts }), [])
  const [merged] = describeAreas(
    [area()],
    [{ extension: 'x', area: 'housekeeping', ok: true, description: { ...valid(), ...facts } }],
  ).areas
  assert.deepEqual(
    merged.blocks.map((b) => b.kind),
    ['schema', 'client-call', 'route', 'table', 'code', 'steps'],
  )
  assert.deepEqual(merged.signals, facts.signals)
  assert.deepEqual(merged.risks, facts.risks)
})

test('an untyped description leaves its area exactly as before', () => {
  const [merged] = describeAreas(
    [area()],
    [{ extension: 'x', area: 'housekeeping', ok: true, description: valid() }],
  ).areas
  for (const key of ['blocks', 'signals', 'risks']) assert.ok(!Object.hasOwn(merged, key), key)
})

test('an unknown block kind or key rejects the description with an error naming its path', () => {
  const facts = typed()
  const at = (/** @type {Record<string, unknown>} */ patch) =>
    refineDescription({ ...valid(), ...facts, ...patch })[0]
  const swap = (/** @type {number} */ i, /** @type {Record<string, unknown>} */ change) => ({
    blocks: facts.blocks.map((b, j) => (i === j ? { ...b, ...change } : b)),
  })
  assert.equal(
    at({ blocks: [{ kind: 'chart', series: [] }] }),
    'blocks.0.kind: unknown block kind "chart"',
  )
  assert.equal(
    at({ blocks: [{ kind: 'constructor' }] }),
    'blocks.0.kind: unknown block kind "constructor"',
  )
  assert.equal(at(swap(2, { colour: 'red' })), 'blocks.2.colour: unknown key')
  assert.equal(at({ graph: [] }), 'unknown key: graph')
  assert.equal(
    at(swap(0, { types: [{ ...facts.blocks[0].types[1], fields: [{ name: 'A', x: 1 }] }] })),
    'blocks.0.types.0.fields.0.x: unknown key',
  )
  assert.equal(
    at(swap(3, { columns: [{ name: 'a', type: 'text', pii: 'yes' }] })),
    'blocks.3.columns.0.pii: expected boolean',
  )
  assert.equal(
    at(swap(2, { method: 'FETCH' })),
    'blocks.2.method: expected one of GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS, ANY',
  )
  assert.equal(
    at(swap(4, { lines: ['a\nb'] })),
    'blocks.4.lines.0: expected one source line of at most 300 characters',
  )
  assert.equal(
    at({ blocks: [{ kind: 'client-call', name: 'Query.x' }] }),
    'blocks.0: a client call needs an operation or a call site',
  )
  assert.equal(
    at({ signals: [{ ...facts.signals[0], state: 'great' }] }),
    'signals.0.state: expected one of none, safe, additive, proven, review, manual, unproven, risky, breaking',
  )
  assert.equal(
    at({ risks: [{ ...facts.risks[0], at: { path: 'a.go' } }] }),
    'risks.0.at.line: required',
  )
})

test('typed facts are bounded in count and size', () => {
  const facts = typed()
  const code = facts.blocks[4]
  for (const [patch, error] of [
    [
      { blocks: Array(MAX_BLOCKS + 1).fill(code) },
      `blocks: expected at most ${MAX_BLOCKS} entries`,
    ],
    [
      { blocks: [{ ...facts.blocks[1], operation: 'x'.repeat(MAX_OPERATION_CHARS + 1) }] },
      `blocks.0.operation: expected operation text of at most ${MAX_OPERATION_CHARS} characters`,
    ],
    [{ blocks: [{ ...code, lines: [] }] }, 'blocks.0.lines: expected 1 to 40 entries'],
    [{ signals: Array(9).fill(facts.signals[0]) }, 'signals: expected at most 8 entries'],
    [
      { signals: [{ ...facts.signals[0], summary: 'two\nlines' }] },
      `signals.0.summary: expected single-line text of at most ${MAX_TEXT_CHARS} characters`,
    ],
  ])
    assert.equal(refineDescription({ ...valid(), ...patch })[0], error)
})

test('the runner fails a describer that sends an unknown block kind, naming the error', (t) => {
  const { root, extension } = fixture(
    t,
    `process.stdout.write(JSON.stringify({ ...${JSON.stringify(valid())}, blocks: [{ kind: 'chart' }] }))`,
  )
  const [outcome] = runDescribers({
    extensions: [extension],
    areas: [area()],
    baseSha: 'b'.repeat(40),
    headSha: 'a'.repeat(40),
    pr: {},
    cwd: root,
  })
  assert.equal(outcome.reason, 'invalid:blocks.0.kind: unknown block kind "chart"')
})

test('merging keeps the first signal per key and reports a cut list', () => {
  const facts = typed()
  const outcome = (/** @type {string} */ extension, /** @type {number} */ order, description) => ({
    extension,
    area: 'housekeeping',
    order,
    ok: true,
    description: { ...valid(), ...description },
  })
  const merged = describeAreas(
    [area()],
    [
      outcome('second', 2, {
        signals: [{ ...facts.signals[0], summary: 'later' }],
        blocks: Array(MAX_BLOCKS).fill(facts.blocks[4]),
      }),
      outcome('first', 1, { signals: facts.signals, blocks: [facts.blocks[5]] }),
    ],
  )
  assert.deepEqual(merged.areas[0].signals, facts.signals)
  assert.equal(merged.areas[0].blocks.length, MAX_BLOCKS)
  const signal = (key, summary) => ({ key, label: key, state: 'safe', summary })
  const ordered = describeAreas(
    [area()],
    [
      outcome('first', 1, { signals: [signal('a', 'a1'), signal('b', 'b1')] }),
      outcome('second', 2, { signals: [signal('a', 'a2'), signal('c', 'c2')] }),
    ],
  )
  assert.deepEqual(
    ordered.areas[0].signals.map((s) => s.summary),
    ['a1', 'b1', 'c2'],
    'signals keep their describers order',
  )
  assert.equal(merged.areas[0].blocks[0].kind, 'steps')
  assert.equal(merged.health[0].code, 'describer-items-truncated')
})

test('a risk shows only where the diff holds its location, and every drop says why', () => {
  const risk = (/** @type {string} */ path, /** @type {number} */ line) => ({
    ...typed().risks[0],
    at: { path, line },
  })
  const hasLine = (/** @type {string} */ path, /** @type {number} */ line) =>
    path === 'api/routes.go' ? line <= 8 : null
  const { kept, dropped } = locatedRisks(
    [risk('api/routes.go', 7), risk('docs/elsewhere.md', 1), risk('api/routes.go', 40)],
    { changedPaths: ['api/routes.go'], hasLine },
  )
  assert.deepEqual(kept, [risk('api/routes.go', 7)])
  assert.deepEqual(dropped, [
    { at: 'risks.1', reason: 'at.path: docs/elsewhere.md is not changed by this PR' },
    { at: 'risks.2', reason: 'at.line: api/routes.go has no line 40 at head' },
  ])
  assert.deepEqual(
    locatedRisks([risk('api/routes.go', 7)], { changedPaths: null }).dropped.map((d) => d.reason),
    ['at.path: the changed files are unknown, so it cannot be checked'],
  )
})

test('boundDescription passes typed facts through for the core to validate', () => {
  const facts = typed()
  const result = boundDescription('boss-verify-x', { items: ['a'], ...facts })
  assert.deepEqual(validateResult(result, 'proof-describe').errors, [])
  assert.deepEqual(result.blocks, facts.blocks)
  assert.ok(!Object.hasOwn(boundDescription('x', { items: [] }), 'blocks'))
})

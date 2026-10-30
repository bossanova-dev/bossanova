import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { tempDir } from '../../scripts/test-infra/test-tmpdir.mjs'
import { MAX_ITEMS, MAX_TEXT_CHARS } from './verify-proof.mjs'
import {
  refineDescription,
  parseDescribeMarker,
  runDescribers,
  describeAreas,
  MAX_DESCRIBE_LINKS,
  MAX_DESCRIBE_OUTPUT_BYTES,
} from './verify-describe.mjs'

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

import assert from 'node:assert/strict'
import test from 'node:test'
import { analyzeSource, compareBaseline } from './check-fixture-rm-retries.mjs'

for (const [name, source, count] of [
  ['single line', 'rmSync(dir, { recursive: true, force: true })', 1],
  [
    'multiline qualified call',
    'fs.rm(join(a, b), {\n recursive: true,\n force: true\n}, callback)',
    1,
  ],
  ['promises call with retries', 'fsp.rm(dir, { recursive: true, maxRetries: 5 })', 0],
  ['promises qualified call', 'promises.rm(dir, { recursive: true })', 1],
  ['fs promises call', 'fs.promises.rm(dir, { recursive: true })', 1],
  ['quoted property', 'rmSync(dir, { "recursive": true })', 1],
  ['quoted retries', 'rmSync(dir, { "recursive": true, "maxRetries": 5 })', 0],
  ['rmdirSync', 'rmdirSync(dir, { recursive: true })', 1],
  ['nonrecursive', 'rmSync(dir, { force: true })', 0],
  ['identifier options', 'rmSync(dir, options)', 1],
  ['spread options', 'rmSync(dir, { ...options, maxRetries: 5 })', 1],
  [
    'nested retry property is not retry policy',
    'rmSync(dir, { recursive: true, other: { maxRetries: 5 } })',
    1,
  ],
  ['comments and strings', '// rmSync(dir, options)\nconst s = "rmSync(dir, options)"', 0],
  [
    'nested arguments and misleading string',
    'fs.rm(join(a, fn(b)), { recursive: true, note: "maxRetries" })',
    1,
  ],
]) {
  test(`classifier: ${name}`, () => assert.equal(analyzeSource(source).length, count))
}

test('ratchet accepts exact counts', () => {
  assert.deepEqual(compareBaseline({ 'a.test.mjs': 2 }, { 'a.test.mjs': 2 }), [])
})
for (const [name, counts, baseline, kind] of [
  ['new file', { 'a.test.mjs': 1 }, {}, 'new-instance'],
  ['new instance in baselined file', { 'a.test.mjs': 3 }, { 'a.test.mjs': 2 }, 'new-instance'],
  ['reduced count', { 'a.test.mjs': 1 }, { 'a.test.mjs': 2 }, 'stale-entry'],
  ['deleted file', {}, { 'a.test.mjs': 2 }, 'stale-entry'],
  ['unsorted baseline', {}, { 'z.test.mjs': 1, 'a.test.mjs': 1 }, 'invalid-baseline'],
  ['invalid count', {}, { 'a.test.mjs': -1 }, 'invalid-baseline'],
]) {
  test(`ratchet rejects ${name}`, () => {
    assert.ok(compareBaseline(counts, baseline).some((failure) => failure.kind === kind))
  })
}

test('a new recursive rm without retries fails the ratchet', () => {
  const source = 'fs.rmSync(dir, { recursive: true, force: true })\n'
  const counts = { 'scripts/seeded.test.mjs': analyzeSource(source).length }
  const failures = compareBaseline(counts, {})
  assert.deepEqual(
    failures.map((failure) => [failure.kind, failure.file]),
    [['new-instance', 'scripts/seeded.test.mjs']],
  )
})

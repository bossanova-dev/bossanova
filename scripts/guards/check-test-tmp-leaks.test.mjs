import assert from 'node:assert/strict'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import test from 'node:test'
import { classifyLeftovers, prefixKey, run } from './check-test-tmp-leaks.mjs'
import { tempDir } from '../test-infra/test-tmpdir.mjs'

function exercise(t, source, baseline = []) {
  const fixture = tempDir('leak-gate-test-', t)
  writeFileSync(join(fixture, 'baseline.json'), JSON.stringify(baseline))
  const output = []
  const rootFile = join(fixture, 'root')
  const code = run(
    [
      process.execPath,
      '-e',
      `
    const fs = require('node:fs');
    fs.writeFileSync(${JSON.stringify(rootFile)}, process.env.TMPDIR);
    if (process.env.TMP !== process.env.TMPDIR || process.env.TEMP !== process.env.TMPDIR) process.exit(99);
    ${source}
  `,
    ],
    { repoRoot: fixture, baselinePath: 'baseline.json', write: (line) => output.push(line) },
  )
  const root = readFileSync(rootFile, 'utf8')
  assert.equal(existsSync(root), false, 'gate removes its root')
  return { code, output: output.join('\n') }
}

test('a leaking command fails and the root is removed', (t) => {
  const result = exercise(t, "fs.mkdirSync(process.env.TMPDIR + '/foo-AbC123')")
  assert.equal(result.code, 1)
  assert.match(result.output, /foo-: 1 leftover/)
  assert.match(result.output, /test-tmpdir.mjs/)
})

test('a clean command passes and the root is removed', (t) => {
  const result = exercise(t, '')
  assert.equal(result.code, 0)
  assert.match(result.output, /passed.*0 leftovers/)
})

test('a failing command preserves its exit code despite a leak', (t) => {
  const result = exercise(t, "fs.mkdirSync(process.env.TMPDIR + '/foo-AbC123'); process.exit(7)")
  assert.equal(result.code, 7)
})

test('a stale baseline fails and the root is removed', (t) => {
  const result = exercise(t, '', ['foo- — an external owner'])
  assert.equal(result.code, 1)
  assert.match(result.output, /foo- no longer occurs/)
})

test('an explained live baseline entry passes', (t) => {
  const result = exercise(t, "fs.mkdirSync(process.env.TMPDIR + '/foo-AbC123')", [
    'foo- — an external owner',
  ])
  assert.equal(result.code, 0)
})

test('prefix keys group Node and Go names and retain fixed names', () => {
  assert.equal(prefixKey('foo-AbC123'), 'foo-')
  assert.equal(prefixKey('bossdtest-1234567890'), 'bossdtest-')
  assert.equal(prefixKey('bossdtest1234567890'), 'bossdtest')
  assert.equal(prefixKey('boss-gate-outcomes'), 'boss-gate-outcomes')
  assert.deepEqual(
    [...classifyLeftovers(['foo-AbC123', 'foo-ZyX987', 'state'])],
    [
      ['foo-', 2],
      ['state', 1],
    ],
  )
})

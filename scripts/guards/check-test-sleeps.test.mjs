import assert from 'node:assert/strict'
import test from 'node:test'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'

/** @param {import('node:test').TestContext} t @param {Record<string, string>} files */
function fixture(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'go-test-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  for (const [file, contents] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), contents)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
import { scanSleeps, compareBaseline } from './check-test-sleeps.mjs'

for (const [name, source, count] of [
  [
    'a seeded time.Sleep in a test file fails',
    'package x\nimport "time"\nfunc f(){ time.Sleep(time.Millisecond) }',
    1,
  ],
  [
    'an aliased time import is caught',
    'package x\nimport t2 "time"\nfunc f(){ t2.Sleep(t2.Millisecond) }',
    1,
  ],
  [
    'sleep inside an aliased synctest bubble passes',
    'package x\nimport ("time"; st "testing/synctest"; "testing")\nfunc f(t *testing.T){ st.Test(t, func(t *testing.T){ time.Sleep(time.Millisecond) }) }',
    0,
  ],
]) {
  test(name, (t) => {
    const root = fixture(t, { 'x_test.go': source })
    const counts = scanSleeps(root).counts
    assert.equal(counts['x_test.go'] ?? 0, count)
    assert.equal(compareBaseline(counts, {}).length, count)
  })
}

test('non-test Go files are ignored', (t) => {
  const root = fixture(t, { 'x.go': 'package x\nimport "time"\nfunc f(){time.Sleep(1)}' })
  assert.deepEqual(scanSleeps(root).counts, {})
})

test('scanner parse errors fail the gate', (t) => {
  const root = fixture(t, { 'x_test.go': 'package x\nfunc broken(' })
  assert.throws(() => scanSleeps(root), /scan|parse/i)
})

test('sleep count ratchet rejects growth and stale entries', () => {
  assert.equal(compareBaseline({ 'x_test.go': 2 }, { 'x_test.go': 1 })[0].kind, 'new-instance')
  assert.equal(compareBaseline({ 'x_test.go': 1 }, { 'x_test.go': 2 })[0].kind, 'stale-entry')
  assert.equal(compareBaseline({}, { 'x_test.go': 1 })[0].kind, 'stale-entry')
  assert.deepEqual(compareBaseline({ 'x_test.go': 1 }, { 'x_test.go': 1 }), [])
  assert.equal(compareBaseline({}, { 'x_test.go': 0 })[0].kind, 'invalid-baseline')
  assert.equal(compareBaseline({}, null)[0].kind, 'invalid-baseline')
})

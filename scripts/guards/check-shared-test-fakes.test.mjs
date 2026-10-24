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
import { scanFakes, compareBaseline, suggestedPackage } from './check-shared-test-fakes.mjs'

test('a seeded second declaration of a shared fake fails', (t) => {
  const root = fixture(t, {
    'one/go.mod': 'module one\n',
    'two/go.mod': 'module two\n',
    'one/a_test.go': 'package one\ntype fakeClock struct{}\n',
    'two/b_test.go': 'package two\ntype fakeClock struct{}\n',
  })
  const result = scanFakes(root)
  assert.equal(result.counts.fakeClock, 2)
  const failures = compareBaseline(result.counts, {})
  assert.equal(failures[0].kind, 'new-instance')
  assert.deepEqual(
    result.declarations.map((item) => item.file),
    ['one/a_test.go', 'two/b_test.go'],
  )
  assert.equal(
    suggestedPackage(
      root,
      'fakeClock',
      result.declarations.map((item) => item.dir),
    ),
    'lib/bossalib/clocktest',
  )
})

test('baselined name gaining another package fails', () => {
  assert.equal(compareBaseline({ fakeClock: 3 }, { fakeClock: 2 })[0].kind, 'new-instance')
})

test('stale fake count fails, including removal at one', () => {
  assert.equal(compareBaseline({ fakeClock: 2 }, { fakeClock: 3 })[0].kind, 'stale-entry')
  assert.equal(compareBaseline({}, { fakeClock: 2 })[0].kind, 'stale-entry')
  assert.equal(compareBaseline({}, { fakeClock: 1 })[0].kind, 'invalid-baseline')
})

test('internal and external tests in the same directory count once', (t) => {
  const root = fixture(t, {
    'go.mod': 'module example\n',
    'x/a_test.go': 'package x\ntype fakeClock struct{}\n',
    'x/b_test.go': 'package x_test\ntype fakeClock struct{}\n',
  })
  assert.deepEqual(scanFakes(root).counts, {})
  assert.equal(suggestedPackage(root, 'fakeClock', ['x/a', 'x/b']), 'x/xtest')
})

test('fake ratchet accepts exact counts and rejects malformed baselines', () => {
  assert.deepEqual(compareBaseline({ fakeClock: 2 }, { fakeClock: 2 }), [])
  assert.equal(compareBaseline({}, null)[0].kind, 'invalid-baseline')
  assert.equal(compareBaseline({}, { stubZ: 2, fakeA: 2 })[0].kind, 'invalid-baseline')
})

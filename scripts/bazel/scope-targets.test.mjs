import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import { bazelScopeTargets } from './scope-targets.mjs'

test('package scopes map to Bazel patterns or widen the entire module', (t) => {
  const repoRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'bazel-scope-'))
  t.after(() => fs.rmSync(repoRoot, { recursive: true, force: true, maxRetries: 3 }))
  for (const dir of ['m', 'm/a', 'm/a/b', 'm/ignored']) {
    fs.mkdirSync(path.join(repoRoot, dir), { recursive: true })
    fs.writeFileSync(path.join(repoRoot, dir, dir === 'm/a/b' ? 'BUILD' : 'BUILD.bazel'), '')
  }
  fs.writeFileSync(path.join(repoRoot, '.bazelignore'), '# comment\nm/ignored\n')
  const scope = (goTestPackages) => bazelScopeTargets({ moduleRoot: 'm', goTestPackages, repoRoot })
  for (const [input, patterns] of [
    ['.', ['//m:all']],
    ['./a', ['//m/a:all']],
    ['./a/b', ['//m/a/b:all']],
    ['./a/...', ['//m/a/...']],
    ['./a ./a/b', ['//m/a:all', '//m/a/b:all']],
  ]) {
    assert.deepEqual(scope(input).patterns, patterns, input)
    assert.equal(scope(input).narrowed, true, input)
  }
  for (const input of [
    undefined,
    '',
    ' ',
    './...',
    'github.com/x/y',
    '../a',
    '/a',
    '-race',
    './a/../b',
    './a/*',
    './a?',
    './missing',
    './ignored',
    './a ./missing',
    './a ./...',
  ]) {
    assert.deepEqual(scope(input).patterns, ['//m/...'], String(input))
    assert.equal(scope(input).narrowed, false, String(input))
  }
})

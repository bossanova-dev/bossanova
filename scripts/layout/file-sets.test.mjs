import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { resolveFileSets } from './file-sets.mjs'

test('test runner executes tests with default and caller-selected reporters', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'script-file-sets-reporters-'))
  try {
    fs.mkdirSync(path.join(root, 'scripts'))
    fs.writeFileSync(
      path.join(root, 'scripts/selected.test.mjs'),
      "import test from 'node:test'; test('selected fixture executes', () => {})\n",
    )
    for (const options of [
      '',
      '--test-reporter=tap',
      '--test-reporter tap',
      '--test-reporter=spec',
    ]) {
      const env = { ...process.env, NODE_OPTIONS: options }
      delete env.NODE_TEST_CONTEXT
      const result = spawnSync(
        process.execPath,
        [fileURLToPath(new URL('./file-sets.mjs', import.meta.url)), 'test', '--root', root],
        { encoding: 'utf8', env },
      )
      assert.ifError(result.error)
      assert.equal(result.status, 0, result.stdout + result.stderr)
      assert.match(result.stdout, /selected fixture executes/)
      assert.match(result.stdout, /(?:# tests 1|tests 1)/)
      if (options.includes('spec')) assert.doesNotMatch(result.stdout, /TAP version/)
      else assert.match(result.stdout, /TAP version 13/)
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('recursive file sets include new areas in test, format and syntax and exclude fixtures', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'script-file-sets-'))
  const files = [
    'scripts/new/deep/example.test.mjs',
    'skills-toolbox/new/deep/helper.mjs',
    'scripts/fixtures/broken.test.mjs',
    'scripts/assets/broken.mjs',
    'skills-toolbox/testdata/broken.test.mjs',
    'scripts/skill-parity/baseline/broken.mjs',
    '.claude/skills/example/gate/gate.mjs',
    '.claude/skills/verify/bin/control-boss',
    '.claude/skills/verify/lib/control.test.mjs',
  ]
  try {
    for (const file of files) {
      fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
      fs.writeFileSync(path.join(root, file), '')
    }
    const sets = resolveFileSets(root)
    assert.deepEqual(sets.test, [
      '.claude/skills/verify/lib/control.test.mjs',
      'scripts/new/deep/example.test.mjs',
    ])
    for (const name of ['format', 'syntax']) {
      assert.deepEqual(sets[name], [files[6], files[7], files[8], files[0], files[1]].sort())
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

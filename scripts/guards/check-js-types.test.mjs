import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { formatBaseline } from './baseline.mjs'
import { checkJsTypes } from './check-js-types.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const CLI = path.join(repoRoot, 'scripts', 'guards', 'check-js-types.mjs')
const BASELINE = 'scripts/guards/check-js-types-baseline.json'

// The fixture has no node_modules, so its config names no `types`.
const TSCONFIG = {
  compilerOptions: {
    allowJs: true,
    checkJs: true,
    noEmit: true,
    strict: false,
    target: 'ES2023',
    lib: ['ES2023'],
    module: 'NodeNext',
    moduleResolution: 'NodeNext',
    skipLibCheck: true,
  },
}

const SEEDED_ERROR = "/** @type {number} */\nexport const n = 'x'\n"
const CLEAN = '/** @type {number} */\nexport const n = 1\n'

// A throwaway git repo. Tracked files are staged, never committed, so `git ls-files` sees them
// and no host signing config is involved; `untracked` files are written but never added.
function fixtureRepo({ files = {}, untracked = {}, baseline = [], tsconfig = TSCONFIG } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-js-types-'))
  const tracked = {
    'scripts/tsconfig.json': JSON.stringify(tsconfig, null, 2) + '\n',
    [BASELINE]: formatBaseline(baseline),
    ...files,
  }
  const write = (entries) => {
    for (const [file, text] of Object.entries(entries)) {
      fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
      fs.writeFileSync(path.join(root, file), text)
    }
  }
  write(tracked)
  const env = { ...process.env, GIT_CONFIG_GLOBAL: '/dev/null', GIT_CONFIG_NOSYSTEM: '1' }
  execFileSync('git', ['init', '-q'], { cwd: root, env })
  execFileSync('git', ['add', '-A'], { cwd: root, env })
  write(untracked)
  return root
}

function cleanup(root) {
  fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
}

function withFixture(options, fn) {
  const root = fixtureRepo(options)
  try {
    return fn(root)
  } finally {
    cleanup(root)
  }
}

const runCli = (root, ...args) =>
  spawnSync(process.execPath, [CLI, '--root', root, ...args], { encoding: 'utf8' })

test('checkJsTypes fails a seeded type error in a file outside the baseline', () => {
  withFixture({ files: { 'scripts/a.mjs': SEEDED_ERROR, 'scripts/ok.mjs': CLEAN } }, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.equal(result.failures.length, 1)
    assert.match(result.failures[0], /^scripts\/a\.mjs: fails tsc --checkJs/)
    assert.deepEqual(result.newViolations, ['scripts/a.mjs'])
    assert.equal(result.checked, 2)
    assert.match(result.details, /scripts\/a\.mjs.*TS2322/)

    const cli = runCli(root)
    assert.equal(cli.status, 1)
    assert.match(cli.stderr, /scripts\/a\.mjs\(2,14\): error TS2322/)
    assert.match(cli.stderr, /scripts\/a\.mjs: fails tsc --checkJs/)
  })
})

test('checkJsTypes fails a baselined file that has become clean', () => {
  withFixture({ files: { 'scripts/b.mjs': CLEAN }, baseline: ['scripts/b.mjs'] }, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [`${BASELINE}: scripts/b.mjs no longer occurs — remove it`])
    assert.deepEqual(result.staleEntries, ['scripts/b.mjs'])
    assert.equal(runCli(root).status, 1)
  })
})

test('checkJsTypes passes a baselined file that still has type errors', () => {
  const options = {
    files: { 'skills-toolbox/a.mjs': SEEDED_ERROR, 'scripts/ok.mjs': CLEAN },
    baseline: ['skills-toolbox/a.mjs'],
  }
  withFixture(options, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [])
    assert.deepEqual(result.offenders, ['skills-toolbox/a.mjs'])
    const cli = runCli(root)
    assert.equal(cli.status, 0, cli.stderr)
    assert.match(cli.stdout, /2 files checked, 1 baselined, 0 ignored out-of-scope diagnostics/)
  })
})

test('checkJsTypes ignores test files and untracked files', () => {
  const options = {
    files: { 'scripts/a.test.mjs': SEEDED_ERROR, 'scripts/ok.mjs': CLEAN },
    untracked: { 'scripts/local.mjs': SEEDED_ERROR },
  }
  withFixture(options, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [])
    assert.equal(result.checked, 1)
    assert.equal(result.ignored, 0)
  })
})

test('checkJsTypes ignores and counts errors in an imported out-of-scope file', () => {
  const options = {
    files: {
      'scripts/a.mjs': "import { n } from '../vendor/x.mjs'\nexport const m = n\n",
      'vendor/x.mjs': SEEDED_ERROR,
    },
  }
  withFixture(options, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [])
    assert.equal(result.ignored, 1)
    assert.deepEqual(result.offenders, [])
  })
})

test('checkJsTypes fails a checked file that opts out with @ts-nocheck', () => {
  const nocheck = `// header\n// @ts-nocheck\n${SEEDED_ERROR}`
  withFixture({ files: { 'scripts/a.mjs': nocheck } }, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [
      'scripts/a.mjs: remove @ts-nocheck; a file leaves the check only through the baseline',
    ])
    assert.deepEqual(result.newViolations, ['scripts/a.mjs'])
  })
  // Baselined, it still fails, and does not also read as a stale entry.
  withFixture({ files: { 'scripts/a.mjs': nocheck }, baseline: ['scripts/a.mjs'] }, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [
      'scripts/a.mjs: remove @ts-nocheck; a file leaves the check only through the baseline',
    ])
    assert.deepEqual(result.staleEntries, [])
  })
  // TypeScript honours the colon form too, so it must not slip past the gate.
  const colon = `// @ts-nocheck: generated\n${SEEDED_ERROR}`
  withFixture({ files: { 'scripts/a.mjs': colon } }, (root) => {
    assert.deepEqual(checkJsTypes({ repoRoot: root }).newViolations, ['scripts/a.mjs'])
  })
})

test('checkJsTypes skips a tracked file deleted from the working tree', () => {
  withFixture({ files: { 'scripts/a.mjs': CLEAN, 'scripts/gone.mjs': CLEAN } }, (root) => {
    fs.rmSync(path.join(root, 'scripts/gone.mjs'))
    const result = checkJsTypes({ repoRoot: root })
    assert.deepEqual(result.failures, [])
    assert.equal(result.checked, 1)
  })
})

test('checkJsTypes fails a config-level diagnostic rather than passing silently', () => {
  const tsconfig = {
    compilerOptions: { ...TSCONFIG.compilerOptions, types: ['does-not-exist'] },
  }
  withFixture({ files: { 'scripts/ok.mjs': CLEAN }, tsconfig }, (root) => {
    const result = checkJsTypes({ repoRoot: root })
    assert.equal(result.failures.length, 1)
    assert.match(result.failures[0], /^config: .*does-not-exist.*\(TS2688\)$/)
    const cli = runCli(root)
    assert.equal(cli.status, 1)
    assert.match(cli.stderr, /TS2688/)
  })
})

test('checkJsTypes --update writes the sorted offenders in the shared baseline format', () => {
  const options = {
    files: {
      'skills-toolbox/z.mjs': SEEDED_ERROR,
      'scripts/b.mjs': SEEDED_ERROR,
      'scripts/clean.mjs': CLEAN,
    },
    baseline: ['scripts/clean.mjs'],
  }
  withFixture(options, (root) => {
    const cli = runCli(root, '--update')
    assert.equal(cli.status, 0, cli.stderr)
    assert.equal(
      fs.readFileSync(path.join(root, BASELINE), 'utf8'),
      formatBaseline(['scripts/b.mjs', 'skills-toolbox/z.mjs']),
    )
    assert.deepEqual(checkJsTypes({ repoRoot: root }).failures, [])
  })
})

import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const roots = ['services/web', 'services/marketing', 'lib/web-helpers']
const source = 'services/web/src/example.ts'
const suppression = '// biome-ignore lint/complexity/noExcessiveCognitiveComplexity: existing\n'

function fixture(t, { text = '', subjects = {}, threshold = 15, level = 'error' } = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'ts-complexity-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const files = {
    [source]: text,
    'scripts/guards/budgets.json': JSON.stringify({
      'ts-cognitive-complexity': { default: 15 },
      'ts-cognitive-complexity-suppressions': { subjects },
    }),
  }
  for (const dir of roots) {
    files[`${dir}/biome.json`] = JSON.stringify({
      linter: {
        rules: {
          complexity: {
            noExcessiveCognitiveComplexity: {
              level,
              options: { maxAllowedComplexity: threshold },
            },
          },
        },
      },
    })
  }
  for (const [file, contents] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), contents)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '--', ...Object.keys(files)], { cwd: root })
  const canonical = path.resolve(import.meta.dirname, '../..')
  fs.copyFileSync(
    path.join(canonical, 'scripts/guards/check-ts-complexity.mjs'),
    path.join(root, 'scripts/guards/check-ts-complexity.mjs'),
  )
  for (const file of [
    'scripts/guards/registry.mjs',
    'scripts/guards/budgets.mjs',
    'scripts/guards/registry',
    'skills-toolbox/main-module.mjs',
  ]) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.symlinkSync(path.join(canonical, file), path.join(root, file))
  }
  return root
}

function run(t, root) {
  const result = spawnSync(process.execPath, ['scripts/guards/check-ts-complexity.mjs'], {
    cwd: root,
    encoding: 'utf8',
  })
  assert.equal(result.error, undefined)
  t.diagnostic(
    `node scripts/guards/check-ts-complexity.mjs → exit ${result.status}: ${(result.stderr || result.stdout).trim()}`,
  )
  return result
}

test('ts-complexity rejects a seeded new suppression', (t) => {
  const result = run(t, fixture(t, { text: suppression }))
  assert.equal(result.status, 1)
  assert.match(result.stderr, /example.ts: 1 suppressions exceeds.*0/)
})

for (const directive of ['biome-ignore', 'biome-ignore-all', 'biome-ignore-start']) {
  test(`ts-complexity counts ${directive} comments`, (t) => {
    const result = run(
      t,
      fixture(t, {
        text: `/* ${directive} lint/complexity/noExcessiveCognitiveComplexity: reason */\n`,
      }),
    )
    assert.equal(result.status, 1)
    assert.match(result.stderr, /1 suppressions exceeds/)
  })
}

test('ts-complexity rejects a drifted threshold and disabled rule', (t) => {
  for (const options of [{ threshold: 16 }, { level: 'warn' }]) {
    const result = run(t, fixture(t, options))
    assert.equal(result.status, 1)
    assert.match(result.stderr, /biome.json:.*error.*15/)
  }
})

test('ts-complexity rejects a removed budgeted suppression', (t) => {
  const result = run(t, fixture(t, { subjects: { [source]: 1 } }))
  assert.equal(result.status, 1)
  assert.match(result.stderr, /example.ts:.*remove or lower/)
})

test('ts-complexity rejects growth within a budgeted file', (t) => {
  const result = run(t, fixture(t, { text: suppression.repeat(2), subjects: { [source]: 1 } }))
  assert.equal(result.status, 1)
  assert.match(result.stderr, /2 suppressions exceeds its budget of 1/)
})

test('ts-complexity ignores other rules and ignore-end', (t) => {
  const text =
    '// biome-ignore lint/style/noVar: reason\n' +
    '// biome-ignore-end lint/complexity/noExcessiveCognitiveComplexity: reason\n'
  assert.equal(run(t, fixture(t, { text })).status, 0)
})

test('ts-complexity rejects a vanished budgeted source', (t) => {
  const root = fixture(t, { text: suppression, subjects: { [source]: 1 } })
  fs.unlinkSync(path.join(root, source))
  const result = run(t, root)
  assert.equal(result.status, 1)
  assert.match(result.stderr, /example.ts:.*vanished/)
})

test('ts-complexity accepts exact budgets and ignores untracked suppressions', (t) => {
  const root = fixture(t, { text: suppression, subjects: { [source]: 1 } })
  fs.writeFileSync(path.join(root, 'services/web/src/untracked.ts'), suppression)
  assert.equal(run(t, root).status, 0)
})

test('ts-complexity passes at head', (t) => {
  assert.equal(run(t, path.resolve(import.meta.dirname, '../..')).status, 0)
})

test('ts-complexity rejects tracked suppressions outside src in every package', (t) => {
  const root = fixture(t)
  const files = roots.map((dir) => `${dir}/tests/complexity.spec.ts`)
  for (const file of files) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), suppression)
  }
  execFileSync('git', ['add', '--', ...files], { cwd: root })
  const result = run(t, root)
  assert.equal(result.status, 1, result.stdout)
  for (const file of files) assert.ok(result.stderr.includes(`${file}: 1 suppressions exceeds`))
})

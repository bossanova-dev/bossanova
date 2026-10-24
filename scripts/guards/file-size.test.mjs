import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { checkFileSizes } from './file-size.mjs'

function fixture(t, files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'warehouse-guard-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, text] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '--', ...Object.keys(files)], { cwd: root })
  return root
}

function runScriptGuard(t, repoRoot) {
  // Copy the real entry point so its existing import-relative root resolves to the fixture.
  // Dependencies stay canonical; no production root override is introduced for the tests.
  const sourceRoot = path.resolve(import.meta.dirname, '../..')
  fs.copyFileSync(
    path.join(sourceRoot, 'scripts/guards/file-size.mjs'),
    path.join(repoRoot, 'scripts/guards/file-size.mjs'),
  )
  for (const file of [
    'scripts/guards/registry.mjs',
    'scripts/guards/budgets.mjs',
    'scripts/guards/registry',
    'scripts/layout/file-sets.mjs',
    'skills-toolbox/main-module.mjs',
  ]) {
    fs.mkdirSync(path.dirname(path.join(repoRoot, file)), { recursive: true })
    fs.symlinkSync(path.join(sourceRoot, file), path.join(repoRoot, file))
  }
  const result = spawnSync(process.execPath, ['scripts/guards/file-size.mjs'], {
    cwd: repoRoot,
    encoding: 'utf8',
  })
  assert.equal(result.error, undefined)
  t.diagnostic(
    `node scripts/guards/file-size.mjs → exit ${result.status}: ${(result.stderr || result.stdout).trim()}`,
  )
  return result
}

function check(t, source, budget) {
  const root = fixture(t, {
    'services/warehouse/models/growth/x.sql': source,
    'scripts/guards/budgets.json': JSON.stringify({ 'warehouse-file-size': budget }),
  })
  return checkFileSizes({ repoRoot: root })
}
test('file-size fails a seeded over-budget SQL file', (t) => {
  assert.match(
    check(t, 'select 1;\n'.repeat(401), { default: 400 }).failures.join('\n'),
    /x.sql: 401 is over/,
  )
})
test('file-size fails a pinned subject that no longer needs its own budget', (t) => {
  const result = check(t, 'select 1;\n'.repeat(400), {
    default: 400,
    subjects: { 'services/warehouse/models/growth/x.sql': 539 },
  })
  assert.match(result.failures.join('\n'), /no longer needs its own budget/)
})
test('file-size counts physical newline characters and accepts an active pin', (t) => {
  assert.deepEqual(
    check(t, 'x\n'.repeat(401) + 'unterminated', {
      default: 400,
      subjects: { 'services/warehouse/models/growth/x.sql': 401 },
    }).failures,
    [],
  )
})

test('file-size fails a seeded over-budget Python test file', (t) => {
  const repoRoot = fixture(t, {
    'services/warehouse/tests/unit/test_large.py': 'pass\n'.repeat(1501),
    'scripts/guards/budgets.json': JSON.stringify({
      'warehouse-test-file-size': { default: 1500 },
    }),
  })
  assert.match(
    checkFileSizes({ repoRoot }).failures.join('\n'),
    /warehouse-test-file-size:.*test_large.py: 1501 is over/,
  )
})

for (const [name, file, count, subjects, message] of [
  [
    'grown subject',
    'skills-toolbox/large.mjs',
    1002,
    { 'skills-toolbox/large.mjs': 1001 },
    /1002 is over its budget/,
  ],
  [
    'retired subject',
    'scripts/large.mjs',
    1000,
    { 'scripts/large.mjs': 1001 },
    /no longer needs its own budget/,
  ],
  [
    'moved subject',
    'scripts/nested/large.mjs',
    1001,
    { 'scripts/large.mjs': 1001 },
    /has a budget but no measurement/,
  ],
]) {
  test(`script-file-size rejects a ${name}`, (t) => {
    const repoRoot = fixture(t, {
      [file]: 'x\n'.repeat(count),
      'scripts/guards/budgets.json': JSON.stringify({
        'script-file-size': { default: 1000, subjects },
      }),
    })
    const result = runScriptGuard(t, repoRoot)
    assert.equal(result.status, 1, result.stderr)
    assert.match(result.stderr, message)
  })
}
test('script-file-size accepts nested files and ignores tests, generated files and fixtures', (t) => {
  const repoRoot = fixture(t, {
    'scripts/nested/source.mjs': 'x\n'.repeat(1000),
    'scripts/source.test.mjs': 'x\n'.repeat(1001),
    'skills-toolbox/source.generated.mjs': 'x\n'.repeat(1001),
    'scripts/fixtures/source.mjs': 'x\n'.repeat(1001),
    'scripts/guards/budgets.json': JSON.stringify({ 'script-file-size': { default: 1000 } }),
  })
  const result = runScriptGuard(t, repoRoot)
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /File sizes OK/)
})

test('script-file-size rejects a new file', (t) => {
  const repoRoot = fixture(t, {
    'scripts/new.mjs': 'x\n'.repeat(1001),
    'scripts/guards/budgets.json': JSON.stringify({ 'script-file-size': { default: 1000 } }),
  })
  const result = runScriptGuard(t, repoRoot)
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, /1001 is over the default/)
})

test('agents-index-size fails seeded byte growth and accepts multibyte content at budget', (t) => {
  const repoRoot = fixture(t, {
    'AGENTS.md': 'é'.repeat(500),
    'scripts/guards/budgets.json': JSON.stringify({
      'agents-index-size': { subjects: { 'AGENTS.md': 1000 } },
    }),
  })
  assert.deepEqual(checkFileSizes({ repoRoot }).failures, [])
  fs.appendFileSync(path.join(repoRoot, 'AGENTS.md'), 'x')
  assert.match(
    checkFileSizes({ repoRoot }).failures.join('\n'),
    /agents-index-size: AGENTS.md: 1001 is over its budget of 1000/,
  )
})

function sourceSizeFixture(t, files, budgets) {
  return fixture(t, {
    ...files,
    'scripts/guards/budgets.json': JSON.stringify(budgets),
  })
}

function rejectsNewSource(t, file, guardId, cap) {
  const root = sourceSizeFixture(
    t,
    { [file]: 'x\n'.repeat(cap + 1) },
    {
      [guardId]: { default: cap },
    },
  )
  const result = runScriptGuard(t, root)
  assert.equal(result.status, 1, result.stderr)
  assert.match(result.stderr, new RegExp(`${guardId}:.*${cap + 1} is over the default`))
}

test('go-file-size rejects a new over-cap source file', (t) => {
  rejectsNewSource(t, 'plugins/example/source.go', 'go-file-size', 1000)
})
test('tsx-file-size rejects a new over-cap component', (t) => {
  rejectsNewSource(t, 'services/web/src/Large.tsx', 'tsx-file-size', 400)
})
test('test-file-size rejects a new over-cap Go test', (t) => {
  rejectsNewSource(t, 'lib/example/source_test.go', 'test-file-size', 1500)
})
test('test-file-size rejects a new over-cap TSX test', (t) => {
  rejectsNewSource(t, 'services/web/src/large.test.tsx', 'test-file-size', 1500)
})

for (const [guardId, file, cap] of [
  ['go-file-size', 'lib/example/source.go', 1000],
  ['tsx-file-size', 'services/web/src/Large.tsx', 400],
  ['test-file-size', 'lib/example/source_test.go', 1500],
]) {
  for (const [scenario, size, subject, message] of [
    ['grown', cap + 2, file, /is over its budget/],
    ['retired', cap, file, /no longer needs its own budget/],
    ['vanished', cap + 1, `${file}.deleted`, /has a budget but no measurement/],
  ]) {
    test(`${guardId} rejects a ${scenario} budget subject`, (t) => {
      const root = sourceSizeFixture(
        t,
        { [file]: 'x\n'.repeat(size) },
        {
          [guardId]: { default: cap, subjects: { [subject]: cap + 1 } },
        },
      )
      const result = runScriptGuard(t, root)
      assert.equal(result.status, 1, result.stderr)
      assert.match(result.stderr, message)
    })
  }
}

test('source sizes accept caps, shrinking budgets and excluded generated code and fixtures', (t) => {
  const files = {
    'source.go': 'x\n'.repeat(1000),
    'Large.tsx': 'x\n'.repeat(400),
    'large_test.go': 'x\n'.repeat(1500),
    'large.test.ts': 'x\n'.repeat(1500),
    'large.test.tsx': 'x\n'.repeat(1500),
    'large.spec.ts': 'x\n'.repeat(1500),
    'large.spec.tsx': 'x\n'.repeat(1500),
    'shrinking.go': 'x\n'.repeat(1001),
    'Shrinking.tsx': 'x\n'.repeat(401),
    'shrinking_test.go': 'x\n'.repeat(1501),
    'gen/large.go': 'x\n'.repeat(2000),
    'gen/Large.tsx': 'x\n'.repeat(2000),
    'testdata/large.go': 'x\n'.repeat(2000),
    'testdata/large_test.go': 'x\n'.repeat(2000),
    'nested/gen/large.go': 'x\n'.repeat(2000),
    'nested/testdata/large.go': 'x\n'.repeat(2000),
    'nested/testdata/large_test.go': 'x\n'.repeat(2000),
    'large.pb.go': 'x\n'.repeat(2000),
    'sqlc.go': '// Code generated by sqlc. DO NOT EDIT.\n' + 'x\n'.repeat(2000),
    'view_templ.go':
      '//go:build ignore\n\n// Code generated by templ - DO NOT EDIT.\n' + 'x\n'.repeat(2000),
    'scripts/large.test.mjs': 'x\n'.repeat(2000),
  }
  const root = sourceSizeFixture(t, files, {
    'go-file-size': { default: 1000, subjects: { 'shrinking.go': 1002 } },
    'test-file-size': { default: 1500, subjects: { 'shrinking_test.go': 1502 } },
    'tsx-file-size': { default: 400, subjects: { 'Shrinking.tsx': 402 } },
  })
  const result = runScriptGuard(t, root)
  assert.equal(result.status, 0, result.stderr)
})

test('file-size passes at repository head', (t) => {
  const result = spawnSync(process.execPath, ['scripts/guards/file-size.mjs'], {
    cwd: path.resolve(import.meta.dirname, '../..'),
    encoding: 'utf8',
  })
  assert.equal(result.status, 0, result.stderr)
  t.diagnostic(result.stdout.trim())
})

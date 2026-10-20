import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync } from 'node:child_process'
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

import assert from 'node:assert/strict'
import { spawnSync, execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { checkScriptLayout } from './script-layout.mjs'

const command = path.resolve('scripts/guards/script-layout.mjs')
function fixture(t, files = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'script-layout-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const contents = {
    'scripts/layout/moves.json': '[]\n',
    'scripts/layout/allowlist.json': '[]\n',
    'scripts/guards/script-layout-baseline.json': '[]\n',
    ...files,
  }
  for (const [file, source] of Object.entries(contents)) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '--', '.'], { cwd: root })
  return root
}
function run(root) {
  return spawnSync(process.execPath, [command, '--root', root], { encoding: 'utf8' })
}
test('script-layout rejects a seeded loose script with the registered command', (t) => {
  const root = fixture(t, { 'scripts/seeded-loose.mjs': 'export const value = 1\n' })
  const red = run(root)
  assert.equal(red.status, 1)
  assert.match(red.stderr, /scripts\/seeded-loose.mjs:.*area folder/)
  fs.writeFileSync(
    path.join(root, 'scripts/guards/script-layout-baseline.json'),
    '["scripts/seeded-loose.mjs"]\n',
  )
  assert.equal(run(root).status, 0)
})
test('script-layout baseline must shrink when a loose file moves', (t) => {
  const root = fixture(t, {
    'scripts/guards/script-layout-baseline.json': '["scripts/gone.mjs"]\n',
  })
  assert.match(checkScriptLayout({ repoRoot: root }).failures.join('\n'), /no longer occurs/)
})
test('script-layout rejects moved-away citations with file and line, ignoring the ledger and longer names', (t) => {
  const old = 'scripts/retired-helper.mjs'
  const root = fixture(t, {
    'scripts/layout/moves.json': JSON.stringify([[old, 'scripts/dev/retired-helper.mjs']]),
    'docs/layout-example.md': `current\nnode ${old}\n`,
    'scripts/dev/reference.mjs': `// ${old}.backup\n`,
  })
  const red = run(root)
  assert.equal(red.status, 1)
  assert.match(red.stderr, /docs\/layout-example.md:2:.*moved to scripts\/dev\/retired-helper.mjs/)
  assert.equal(checkScriptLayout({ repoRoot: root }).failures.length, 1)
  fs.writeFileSync(path.join(root, 'docs/layout-example.md'), 'current\n')
  assert.equal(run(root).status, 0)
})
test('script-layout rejects a tracked test omitted by the test selector', (t) => {
  const root = fixture(t, { 'skills-toolbox/new-area/seed.test.mjs': '' })
  const result = checkScriptLayout({ repoRoot: root, fileSets: { test: [] } })
  assert.match(
    result.failures.join('\n'),
    /skills-toolbox\/new-area\/seed.test.mjs:.*test file-set/,
  )
  assert.equal(run(root).status, 0)
})
test('script-layout ignores excluded fixture tests and scans nested real tests', (t) => {
  const root = fixture(t, {
    'scripts/fixtures/seed.test.mjs': '',
    'scripts/testdata/seed.test.mjs': '',
    'scripts/assets/seed.test.mjs': '',
    'scripts/skill-parity/baseline/seed.test.mjs': '',
    'scripts/new-area/deep/real.test.mjs': '',
  })
  assert.deepEqual(checkScriptLayout({ repoRoot: root }).failures, [])
})
test('script-layout fails malformed move ledger rather than silently skipping stale references', (t) => {
  const root = fixture(t, { 'scripts/layout/moves.json': '[{"from":"x"}]' })
  assert.match(checkScriptLayout({ repoRoot: root }).failures.join('\n'), /moves.json.*pairs/)
})

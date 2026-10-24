import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { checkBossConnectOnlyInAdapters } from './boss-connect-only-in-adapters.mjs'
import { splitBaselineEntry } from './baseline.mjs'

const PROTO = 'connectrpc.com/connect'
const source = (pkg) => `package x\nimport _ "${pkg}"\n`
function write(root, file, text) {
  fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
  fs.writeFileSync(path.join(root, file), text)
  execFileSync('git', ['add', file], { cwd: root })
}
const check = (repoRoot, write = false) =>
  checkBossConnectOnlyInAdapters({ repoRoot, baselinePath: 'baseline.json', write })
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'boss-connect-only-in-adapters-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  execFileSync('git', ['init', '-q'], { cwd: root })
  write(root, '.golangci.yml', fs.readFileSync('.golangci.yml', 'utf8'))
  write(root, 'baseline.json', '[]\n')
  assert.deepEqual(check(root, true).failures, [])
  return root
}
test('boss boundary detects new violations and stale baseline exclusions', (t) => {
  const root = fixture(t)
  write(root, 'services/boss/internal/feature/x.go', source(PROTO))
  assert.equal(check(root).offenders.length, 1)
  assert.equal(check(root).failures.length, 1)
  assert.deepEqual(check(root, true).failures, [])
  const config = fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8')
  write(
    root,
    '.golangci.yml',
    config.replace('!**/services/boss/internal/feature/x.go', '!**/wrong.go'),
  )
  assert.match(check(root).failures.join('\n'), /stale/)
  assert.deepEqual(check(root, true).failures, [])
  write(root, 'services/boss/internal/feature/x.go', 'package x\n')
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
  assert.deepEqual(check(root, true).failures, [])
})

test('boss boundary exempts adapters and tests but covers cmd and future packages', (t) => {
  const root = fixture(t)
  for (const file of [
    'services/boss/internal/adapter/a/x.go',
    'services/boss/internal/feature/x_test.go',
  ])
    write(root, file, source(PROTO))
  assert.deepEqual(check(root).failures, [])
  for (const file of ['services/boss/cmd/x.go', 'services/boss/future/x.go'])
    write(root, file, source(PROTO))
  assert.equal(check(root).failures.length, 2)
})

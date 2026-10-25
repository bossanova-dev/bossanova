import { runGolangciSync } from '../test-infra/golangci-admission.mjs'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync, execFileSync } from 'node:child_process'
import test from 'node:test'
import { checkStoreOnlySql } from './store-only-sql.mjs'

function fixture(t, files = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'store-only-sql-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, source] of Object.entries({
    '.golangci.yml': fs.readFileSync('.golangci.yml', 'utf8'),
    'baseline.json': '[]\n',
    ...files,
  })) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  checkStoreOnlySql({ repoRoot: root, baselinePath: 'baseline.json', write: true })
  return root
}
const check = (repoRoot, write = false) =>
  checkStoreOnlySql({ repoRoot, baselinePath: 'baseline.json', write })
const file = 'services/bosso/internal/server/x.go'
test('store-only-sql rejects seeded SQL imports outside the store including dbx', (t) => {
  const root = fixture(t)
  for (const location of ['server', 'dbx']) {
    const target = `services/bosso/internal/${location}/x.go`
    fs.mkdirSync(path.dirname(path.join(root, target)), { recursive: true })
    fs.writeFileSync(path.join(root, target), 'package x\nimport "database/sql"\n')
    execFileSync('git', ['add', target], { cwd: root })
  }
  assert.equal(check(root).failures.length, 2)
  assert.match(check(root).failures.join('\n'), /domain.ErrNotFound/)
})
test('store-only-sql exempts only store, generated, test helper and test files', (t) => {
  const files = Object.fromEntries(
    ['db/x.go', 'db/sqlcgen/x.go', 'dbtest/x.go', 'testharness/x.go', 'server/x_test.go'].map(
      (suffix) => [`services/bosso/internal/${suffix}`, 'package x\nimport "database/sql"'],
    ),
  )
  files['services/bossd/testharness/x.go'] = 'package x\nimport "database/sql"'
  assert.deepEqual(check(fixture(t, files)).offenders, [])
})
test('store-only-sql reads aliased single and grouped imports without matching comments or strings', (t) => {
  const root = fixture(t)
  fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
  fs.writeFileSync(
    path.join(root, file),
    'package x\n// import "database/sql"\nvar s = `\nimport "database/sql"\n`\nimport (\n driver "github.com/jackc/pgx/v5/stdlib"\n)',
  )
  execFileSync('git', ['add', file], { cwd: root })
  assert.equal(check(root).failures.length, 1)
  fs.writeFileSync(path.join(root, file), 'package x\nimport alias \"database/sql/driver\"\n')
  assert.equal(check(root).failures.length, 1)
  assert.deepEqual(check(root, true).failures, [])
  const snapshot = fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8')
  assert.deepEqual(check(root, true).failures, [])
  assert.equal(fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8'), snapshot)
  fs.writeFileSync(
    path.join(root, file),
    'package x\n// import "database/sql"\nvar s = `\nimport "database/sql"\n`',
  )
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
})
test('store-only-sql detects generated block drift and missing deny rules', (t) => {
  const root = fixture(t)
  const config = path.join(root, '.golangci.yml')
  fs.writeFileSync(
    config,
    fs
      .readFileSync(config, 'utf8')
      .replace(
        /(# BEGIN generated: store-only-sql[^\n]*\n)/,
        '$1            - "!**/services/bosso/internal/server/x.go"\n',
      ),
  )
  assert.match(check(root).failures.join('\n'), /generated.*stale/)
  check(root, true)
  fs.writeFileSync(
    config,
    fs.readFileSync(config, 'utf8').replace('pkg: database/sql', 'pkg: not/database/sql'),
  )
  assert.match(check(root).failures.join('\n'), /missing deny.*database\/sql/)
})
test('store-only-sql real tree passes', () => assert.deepEqual(checkStoreOnlySql().failures, []))
test('store-only-sql real depguard flags seeded server and dbx imports', (t) => {
  const available = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') {
    if (process.env.GUARD_PROOF_REQUIRE_TOOLS === '1')
      assert.fail('golangci-lint required by GUARD_PROOF_REQUIRE_TOOLS=1')
    t.skip('golangci-lint not on PATH')
    return
  }
  assert.equal(available.status, 0, available.stderr)
  const root = fixture(t, { 'go.mod': 'module example.com/guardproof\n\ngo 1.26.0\n' })
  for (const location of ['server', 'dbx', 'db', 'dbtest']) {
    const dir = path.join(root, `services/bosso/internal/${location}`)
    fs.mkdirSync(dir, { recursive: true })
    fs.writeFileSync(path.join(dir, 'x.go'), 'package x\nimport "database/sql"\nvar DB *sql.DB\n')
  }
  const result = runGolangciSync(['run', '--enable-only', 'depguard', './...'], {
    cwd: root,
    encoding: 'utf8',
    timeout: 120000,
    maxBuffer: 4 * 1024 * 1024,
  })
  assert.equal(result.error, undefined)
  assert.equal(result.status, 1, result.stdout + result.stderr)
  assert.match(result.stdout, /server\/x.go.*store-only-sql/)
  assert.match(result.stdout, /dbx\/x.go.*store-only-sql/)
  assert.doesNotMatch(result.stdout, /internal\/(?:db|dbtest)\/x.go.*store-only-sql/)
})

test('store-only-sql permits bossd migration infrastructure but rejects sibling adapters', (t) => {
  const root = fixture(t)
  for (const adapter of ['migrate', 'server']) {
    const target = `services/bossd/internal/adapter/${adapter}/migrate.go`
    fs.mkdirSync(path.dirname(path.join(root, target)), { recursive: true })
    fs.writeFileSync(path.join(root, target), 'package x\nimport "database/sql"\n')
    execFileSync('git', ['add', target], { cwd: root })
  }
  assert.deepEqual(check(root).offenders, ['services/bossd/internal/adapter/server/migrate.go'])
})

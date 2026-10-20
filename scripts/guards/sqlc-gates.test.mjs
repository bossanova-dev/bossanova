// guard-id: sqlc-fresh
// guard-id: sqlc-vet
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import YAML from 'yaml'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const sqlc = path.join(root, 'bin/sqlc')
const required = process.env.GUARD_PROOF_REQUIRE_TOOLS === '1'
function tool(t) {
  if (fs.existsSync(sqlc)) return true
  if (required) assert.fail('bin/sqlc required; run make sqlc-bin')
  t.skip('bin/sqlc missing; run make sqlc-bin')
  return false
}
function fixture(t, service) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'sqlc-proof-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 }))
  for (const item of [
    'sqlc.yaml',
    'sqlc.vet.yaml',
    service === 'bossd' ? 'migrations' : 'migrations_postgres',
    'internal/db/queries',
    'internal/db/sqlcgen',
  ]) {
    const target = path.join(dir, item)
    fs.mkdirSync(path.dirname(target), { recursive: true })
    fs.cpSync(path.join(root, 'services', service, item), target, { recursive: true })
  }
  return dir
}
function vet(service, config) {
  return spawnSync('go', ['run', `./services/${service}/internal/dbtest/migratedb`, '--', config], {
    cwd: root,
    encoding: 'utf8',
    timeout: 240000,
  })
}

test('sqlc configs preserve hermetic generation and vet parity', () => {
  for (const service of ['bossd', 'bosso']) {
    const generate = YAML.parse(
      fs.readFileSync(path.join(root, 'services', service, 'sqlc.yaml'), 'utf8'),
    ).sql[0]
    const vetConfig = YAML.parse(
      fs.readFileSync(path.join(root, 'services', service, 'sqlc.vet.yaml'), 'utf8'),
    ).sql[0]
    for (const key of ['engine', 'schema', 'queries'])
      assert.deepEqual(generate[key], vetConfig[key])
    assert.equal(generate.database, undefined)
    assert.deepEqual(vetConfig.rules, ['sqlc/db-prepare'])
  }
})

test('sqlc-fresh rejects a hand edit', (t) => {
  if (!tool(t)) return
  for (const service of ['bossd', 'bosso']) {
    const dir = fixture(t, service)
    const args = ['diff', '-f', path.join(dir, 'sqlc.yaml')]
    const before = spawnSync(sqlc, args, { encoding: 'utf8' })
    assert.equal(before.status, 0, before.stderr)
    fs.appendFileSync(path.join(dir, 'internal/db/sqlcgen/db.go'), '\n// seeded drift\n')
    const after = spawnSync(sqlc, args, { encoding: 'utf8' })
    assert.equal(after.status, 1, after.stderr)
  }
})

test('sqlc-vet rejects a broken SQLite query', (t) => {
  if (!tool(t)) return
  const dir = fixture(t, 'bossd')
  const config = path.join(dir, 'sqlc.vet.yaml')
  const before = vet('bossd', config)
  assert.equal(before.status, 0, before.stderr)
  fs.writeFileSync(
    path.join(dir, 'internal/db/queries/broken.sql'),
    '-- name: ListBroken :many\nSELECT missing_column FROM repos ORDER BY CASE WHEN ? THEN id ELSE id END;\n',
  )
  const after = vet('bossd', config)
  assert.notEqual(after.status, 0)
  assert.equal(after.error, undefined)
  assert.match(after.stderr, /missing_column|no such column/i)
})

test('sqlc-vet rejects a broken Postgres query', (t) => {
  if (!tool(t)) return
  if (!process.env.BOSSO_TEST_DATABASE_URL) {
    if (process.env.BOSSO_REQUIRE_POSTGRES_TESTS === '1') assert.fail('Postgres URL required')
    t.skip('BOSSO_TEST_DATABASE_URL missing')
    return
  }
  const dir = fixture(t, 'bosso')
  const config = path.join(dir, 'sqlc.vet.yaml')
  const before = vet('bosso', config)
  assert.equal(before.status, 0, before.stderr)
  fs.writeFileSync(
    path.join(dir, 'internal/db/queries/broken.sql'),
    '-- name: GetBroken :one\nSELECT misspelled_column FROM organizations;\n',
  )
  const after = vet('bosso', config)
  assert.notEqual(after.status, 0)
  assert.equal(after.error, undefined)
  assert.match(after.stderr, /misspelled_column|does not exist/i)
})

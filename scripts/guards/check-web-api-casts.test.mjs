import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const guard = path.join(root, 'scripts/guards/check-web-api-casts.mjs')
const file = 'services/web/src/seed.test.ts'

function fixture(t, source, baseline = []) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'web-api-casts-'))
  t.after(() => fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  fs.mkdirSync(path.join(dir, 'services/web/src'), { recursive: true })
  fs.mkdirSync(path.join(dir, 'scripts/guards'), { recursive: true })
  fs.writeFileSync(path.join(dir, file), source)
  fs.writeFileSync(
    path.join(dir, 'scripts/guards/web-api-casts-baseline.json'),
    JSON.stringify(baseline),
  )
  execFileSync('git', ['init', '-q', dir])
  execFileSync('git', ['-C', dir, 'add', '.'])
  return spawnSync(process.execPath, [guard, '--root', dir], { encoding: 'utf8' })
}

test('seeded as Api fails naming the test file', (t) => {
  const result = fixture(t, 'const api = {} as Api')
  assert.equal(result.status, 1)
  assert.match(result.stderr, /services\/web\/src\/seed\.test\.ts/)
  assert.match(result.stderr, /fakeApi/)
})

for (const cast of [
  'as unknown as Api',
  'as\nunknown\nas\nApi',
  'as Partial<Api>',
  'as CronJobsApi',
]) {
  test(`rejects ${JSON.stringify(cast)}`, (t) => {
    assert.equal(fixture(t, `const api = {} ${cast}`).status, 1)
  })
}

test('stale baseline entry fails', (t) => {
  const result = fixture(t, 'const api: Api = client', [file])
  assert.equal(result.status, 1)
  assert.match(result.stderr, /no longer occurs/)
})

test('a reasoned existing baseline entry passes', (t) => {
  assert.equal(fixture(t, 'const api = {} as Api', [`${file} — existing migration`]).status, 0)
})

test('type annotations and similar identifiers pass', (t) => {
  assert.equal(
    fixture(t, 'const api: Api = client; const key = value as ApiKey; asApi()').status,
    0,
  )
})

test('real tree passes', () => {
  const result = spawnSync(process.execPath, [guard], { cwd: root, encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
})

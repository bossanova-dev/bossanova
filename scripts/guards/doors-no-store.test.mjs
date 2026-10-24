import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { checkDoorsNoStore } from './doors-no-store.mjs'

const source = (pkg) => `package x\nimport _ "${pkg}"\n`
function write(root, file, text) {
  fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
  fs.writeFileSync(path.join(root, file), text)
  execFileSync('git', ['add', file], { cwd: root })
}
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'doors-no-store-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  execFileSync('git', ['init', '-q'], { cwd: root })
  write(root, '.golangci.yml', fs.readFileSync('.golangci.yml', 'utf8'))
  write(root, 'baseline.json', '[]\n')
  assert.deepEqual(check(root, true).failures, [])
  return root
}
const check = (repoRoot, write = false) =>
  checkDoorsNoStore({ repoRoot, baselinePath: 'baseline.json', write })
test('doors-no-store rejects seeded current and future doors and store subpackages', (t) => {
  const root = fixture(t)
  for (const [file, pkg] of [
    ['services/bosso/api/webhook/x.go', 'github.com/recurser/bosso/internal/db'],
    ['services/bossd/api/daemon/x.go', 'github.com/recurser/bossd/internal/db'],
    ['services/bosso/api/auth/x.go', 'github.com/recurser/bosso/internal/db/sqlcgen'],
    ['services/bosso/api/routing/x.go', 'github.com/recurser/bosso/internal/adapter/githubapp'],
    ...['daemon', 'hooks', 'proxy', 'plugin', 'upstream'].flatMap((door) => [
      [`services/bossd/api/${door}/store.go`, 'github.com/recurser/bossd/internal/db'],
      [`services/bossd/api/${door}/adapter.go`, 'github.com/recurser/bossd/internal/adapter/git'],
    ]),
  ])
    write(root, file, source(pkg))
  assert.equal(check(root).offenders.length, 14)
  assert.equal(check(root).failures.length, 14)
  assert.match(check(root).failures.join('\n'), /declare a consumer interface/)
})
test('doors-no-store allows dbtest, tests and feature packages', (t) => {
  const root = fixture(t)
  write(
    root,
    'services/bosso/api/webhook/x.go',
    source('github.com/recurser/bosso/internal/dbtest'),
  )
  write(
    root,
    'services/bossd/api/daemon/x_test.go',
    source('github.com/recurser/bossd/internal/db'),
  )
  write(
    root,
    'services/bosso/internal/sessionread/x.go',
    source('github.com/recurser/bosso/internal/db'),
  )
  assert.deepEqual(check(root).failures, [])
})
test('doors-no-store baseline shrinks and generated exclusions stay fresh and idempotent', (t) => {
  const root = fixture(t)
  const file = 'services/bosso/api/orchestrator/x.go'
  write(root, file, source('github.com/recurser/bosso/internal/db'))
  assert.deepEqual(check(root, true).failures, [])
  const config = fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8')
  assert.deepEqual(check(root, true).failures, [])
  assert.equal(fs.readFileSync(path.join(root, '.golangci.yml'), 'utf8'), config)
  write(root, '.golangci.yml', config.replace('"!**/' + file + '"', '"!**/wrong.go"'))
  assert.match(check(root).failures.join('\n'), /generated.*stale/)
  check(root, true)
  write(root, file, 'package x\n')
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
})
test('doors-no-store rejects missing deny entries and rule scopes', (t) => {
  const root = fixture(t)
  const file = '.golangci.yml'
  write(
    root,
    file,
    fs
      .readFileSync(path.join(root, file), 'utf8')
      .replaceAll('pkg: github.com/recurser/bossd/internal/db$', 'pkg: wrong/db$')
      .replace('**/services/bosso/api/**', '**/wrong/api/**'),
  )
  assert.match(check(root).failures.join('\n'), /missing deny/)
  assert.match(check(root).failures.join('\n'), /files must match/)
})
test('doors-no-store real tree passes', () => assert.deepEqual(checkDoorsNoStore().failures, []))
test('doors-no-store real depguard flags seeded current and future doors', (t) => {
  const available = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') {
    if (process.env.GUARD_PROOF_REQUIRE_TOOLS === '1')
      assert.fail('golangci-lint required by GUARD_PROOF_REQUIRE_TOOLS=1')
    return t.skip('golangci-lint not on PATH')
  }
  assert.equal(available.status, 0, available.stderr)
  for (const service of ['bosso', 'bossd']) {
    const root = fixture(t)
    write(
      root,
      `services/${service}/go.mod`,
      `module github.com/recurser/${service}\n\ngo 1.26.0\n`,
    )
    for (const pkg of ['db', 'dbtest', 'db/sqlcgen', 'adapter/githubapp'])
      write(root, `services/${service}/internal/${pkg}/x.go`, 'package x\n')
    for (const door of [
      'api/webhook',
      'api/future',
      'api/daemon',
      'api/hooks',
      'api/proxy',
      'api/plugin',
      'api/upstream',
    ])
      write(
        root,
        `services/${service}/${door}/x.go`,
        source(`github.com/recurser/${service}/internal/db`),
      )
    write(
      root,
      `services/${service}/api/auth/x.go`,
      source(`github.com/recurser/${service}/internal/db/sqlcgen`),
    )
    write(
      root,
      `services/${service}/api/routing/x.go`,
      source(`github.com/recurser/${service}/internal/dbtest`),
    )
    write(
      root,
      `services/${service}/internal/stream/x_test.go`,
      source(`github.com/recurser/${service}/internal/db`),
    )
    write(
      root,
      `services/${service}/api/routing/adapter.go`,
      source(`github.com/recurser/${service}/internal/adapter/githubapp`),
    )
    const result = spawnSync(
      'golangci-lint',
      [
        'run',
        '--allow-parallel-runners',
        '--config',
        path.join(root, '.golangci.yml'),
        '--enable-only',
        'depguard',
        './...',
      ],
      {
        cwd: path.join(root, `services/${service}`),
        env: { ...process.env, GOWORK: 'off' },
        encoding: 'utf8',
        timeout: 120000,
        maxBuffer: 4 * 1024 * 1024,
      },
    )
    assert.equal(result.error, undefined)
    assert.equal(result.status, 1, result.stdout + result.stderr)
    assert.match(result.stdout, /webhook\/x.go.*doors-no-store/)
    assert.match(result.stdout, /future\/x.go.*doors-no-store/)
    for (const door of ['daemon', 'hooks', 'proxy', 'plugin', 'upstream'])
      assert.match(result.stdout, new RegExp(`${door}/x.go.*doors-no-store`))
    assert.match(result.stdout, /auth\/x.go.*doors-no-store/)
    assert.match(result.stdout, /routing\/adapter.go.*doors-no-store/)
    assert.doesNotMatch(result.stdout, /(?:routing\/x.go|stream\/x_test.go).*doors-no-store/)
    for (const door of ['webhook', 'future', 'daemon', 'hooks', 'proxy', 'plugin', 'upstream'])
      write(root, `services/${service}/api/${door}/x.go`, 'package x\n')
    write(root, `services/${service}/api/auth/x.go`, 'package x\n')
    write(root, `services/${service}/api/routing/adapter.go`, 'package x\n')
    assert.deepEqual(check(root).failures, [])
  }
})

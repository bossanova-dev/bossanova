import { runGolangciSync } from '../test-infra/golangci-admission.mjs'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import test from 'node:test'
import { checkDomainImports } from './domain-imports.mjs'

function fixture(t, files = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'domain-imports-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, source] of Object.entries({
    '.golangci.yml': fs.readFileSync('.golangci.yml', 'utf8'),
    ...files,
  })) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
const check = (repoRoot) => checkDomainImports({ repoRoot })
test('domain-imports rejects seeded service imports in domain and nested domain packages', (t) => {
  const cases = [
    ['bossd', '', 'github.com/recurser/bossd/internal/rotation'],
    ['bosso', 'policy/', 'github.com/recurser/bosso/internal/db'],
    ['bosso', 'cloudsession/', 'github.com/recurser/bosso/internal/db'],
    ['bosso', 'sessionreach/', 'github.com/recurser/bosso/internal/domainish'],
    ['bossd', '', 'github.com/recurser/bossd/cmd'],
  ]
  for (const [service, nested, pkg] of cases) {
    const file = `services/${service}/internal/domain/${nested}x.go`
    const root = fixture(t, { [file]: `package domain\nimport alias "${pkg}"\n` })
    const result = check(root)
    assert.deepEqual(result.offenders, [file])
    assert.match(result.failures.join('\n'), /move the dependency to the caller/)
  }
})
test('domain-imports allows external imports and excludes tests and neighboring packages', (t) => {
  const root = fixture(t, {
    'services/bossd/internal/domain/x.go':
      'package domain\nimport ("time"; "github.com/recurser/bossalib/models")\n',
    'services/bosso/internal/domain/cloudsession/x.go':
      'package cloudsession\n// import "github.com/recurser/bosso/internal/db"\nvar source = `import "github.com/recurser/bosso/internal/db"`\n',
    'services/bossd/internal/domain/x_test.go':
      'package domain\nimport "github.com/recurser/bossd/internal/db"\n',
    'services/bosso/internal/domainish/x.go':
      'package domainish\nimport "github.com/recurser/bosso/internal/db"\n',
  })
  assert.deepEqual(check(root), { failures: [], offenders: [] })
})
test('domain-imports permits domain dependencies but rejects similar prefixes', (t) => {
  const file = 'services/bossd/internal/domain/decision/x.go'
  for (const pkg of [
    'github.com/recurser/bossd/internal/domain',
    'github.com/recurser/bossd/internal/domain/entity',
    'github.com/recurser/bosso/internal/domain/entity',
  ]) {
    const root = fixture(t, { [file]: `package decision\nimport "${pkg}"\n` })
    assert.deepEqual(check(root), { failures: [], offenders: [] })
  }
  for (const pkg of [
    'github.com/recurser/bossd/internal/domainish',
    'github.com/recurser/bosso/internal/domainish/entity',
  ]) {
    const root = fixture(t, { [file]: `package decision\nimport "${pkg}"\n` })
    assert.deepEqual(check(root).offenders, [file])
  }
})
test('domain-imports detects missing rule, changed scopes and missing deny prefixes', (t) => {
  const root = fixture(t)
  const configPath = path.join(root, '.golangci.yml')
  const source = fs.readFileSync(configPath, 'utf8')
  for (const [before, after, expected] of [
    [
      'domain-imports-nothing-internal:',
      'renamed-domain-rule:',
      /missing domain-imports-nothing-internal depguard rule/,
    ],
    [
      '**/services/bossd/internal/domain/**',
      '**/services/bossd/internal/domain/*.go',
      /files must match/,
    ],
    ['list-mode: lax', 'list-mode: original', /list-mode must be lax/],
    ['github.com/recurser/bossd/internal/domain$', 'example.com/domain$', /allow must match/],
    [
      'pkg: github.com/recurser/bossd/',
      'pkg: example.com/bossd/',
      /missing deny github.com\/recurser\/bossd\//,
    ],
  ]) {
    const start = source.indexOf('      domain-imports-nothing-internal:')
    const prefix = source.slice(0, start)
    fs.writeFileSync(configPath, prefix + source.slice(start).replace(before, after))
    assert.match(check(root).failures.join('\n'), expected)
  }
})
test('domain-imports real tree passes', () => assert.deepEqual(checkDomainImports().failures, []))
test('domain-imports real depguard flags seeded domain imports', (t) => {
  const available = spawnSync('golangci-lint', ['version'], { encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') {
    if (process.env.GUARD_PROOF_REQUIRE_TOOLS === '1')
      assert.fail('golangci-lint required by GUARD_PROOF_REQUIRE_TOOLS=1')
    t.skip('golangci-lint not on PATH')
    return
  }
  assert.equal(available.status, 0, available.stderr)
  const root = fixture(t, {
    'go.mod': 'module github.com/recurser/bossd\n\ngo 1.26.0\n',
    'internal/rotation/x.go': 'package rotation\n',
    'internal/domain/entity/x.go': 'package entity\n',
    'internal/domainish/x.go': 'package domainish\n',
    'services/bossd/internal/domain/allowed/x.go':
      'package allowed\nimport _ "github.com/recurser/bossd/internal/domain/entity"\n',
    'services/bossd/internal/domain/similar/x.go':
      'package similar\nimport _ "github.com/recurser/bossd/internal/domainish"\n',
    'services/bossd/internal/domain/x.go':
      'package domain\nimport _ "github.com/recurser/bossd/internal/rotation"\n',
    'services/bossd/internal/domain/nested/x.go':
      'package nested\nimport _ "github.com/recurser/bossd/internal/rotation"\n',
    'services/bossd/internal/domain/x_test.go':
      'package domain\nimport _ "github.com/recurser/bossd/internal/rotation"\n',
  })
  const run = () =>
    runGolangciSync(['run', '--enable-only', 'depguard', './...'], {
      cwd: root,
      encoding: 'utf8',
      timeout: 120000,
      maxBuffer: 4 * 1024 * 1024,
    })
  const result = run()
  assert.equal(result.error, undefined)
  assert.equal(result.status, 1, result.stdout + result.stderr)
  assert.match(result.stdout, /domain\/x.go.*domain-imports-nothing-internal/)
  assert.match(result.stdout, /domain\/nested\/x.go.*domain-imports-nothing-internal/)
  assert.match(result.stdout, /domain\/similar\/x.go.*domain-imports-nothing-internal/)
  assert.doesNotMatch(result.stdout, /domain\/allowed\/x.go.*domain-imports-nothing-internal/)
  assert.doesNotMatch(result.stdout, /x_test.go.*domain-imports-nothing-internal/)
  for (const file of ['x.go', 'nested/x.go', 'similar/x.go'])
    fs.writeFileSync(path.join(root, 'services/bossd/internal/domain', file), 'package domain\n')
  const green = run()
  assert.equal(green.error, undefined)
  assert.equal(green.status, 0, green.stdout + green.stderr)
})

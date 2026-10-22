import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { execFileSync, spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'
import { checkStoreImports } from './store-imports.mjs'
import { imports } from './depguard-rule.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const edge = (service, pkg) => `services/${service}/internal/db -> ${pkg}`
function fixture(t, files = {}, baseline = []) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'store-imports-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  for (const [file, source] of Object.entries({
    'baseline.json': JSON.stringify(baseline.sort()) + '\n',
    ...files,
  })) {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), source)
  }
  execFileSync('git', ['init', '-q'], { cwd: root })
  execFileSync('git', ['add', '.'], { cwd: root })
  return root
}
const check = (repoRoot) => checkStoreImports({ repoRoot, baselinePath: 'baseline.json' })

test('store-imports rejects seeded upward imports in either store', (t) => {
  const root = fixture(t, {
    'services/bossd/internal/db/x.go':
      'package db\nimport "github.com/recurser/bossd/internal/rotation"\n',
    'services/bosso/internal/db/nested/x.go':
      'package nested\nimport pb "github.com/recurser/bossalib/gen/bossanova/v1"\n',
  })
  const result = check(root)
  assert.deepEqual(result.offenders, [
    edge('bossd', 'github.com/recurser/bossd/internal/rotation'),
    edge('bosso', 'github.com/recurser/bossalib/gen/bossanova/v1'),
  ])
  assert.equal(result.failures.length, 2)
  assert.match(result.failures.join('\n'), /internal\/domain/)
})
test('store-imports allows domain, store subpackages, shared libraries and test imports', (t) => {
  const root = fixture(t, {
    'services/bosso/internal/db/x.go': `package db
import (
  "database/sql"
  "github.com/recurser/bosso/internal/domain"
  "github.com/recurser/bosso/internal/domain/cloudsession"
  "github.com/recurser/bosso/internal/db/sqlcgen"
  "github.com/recurser/bossalib/models"
)
`,
    'services/bosso/internal/db/x_test.go':
      'package db\nimport "github.com/recurser/bosso/internal/server"\n',
    'services/bossd/internal/db/nested/x_test.go':
      'package nested\nimport "github.com/recurser/bossalib/gen/bossanova/v1"\n',
  })
  assert.deepEqual(check(root), { failures: [], offenders: [] })
})
test('store-imports accepts existing edges once and rejects stale baseline entries', (t) => {
  const pkg = 'github.com/recurser/bossd/internal/rotation'
  const root = fixture(
    t,
    {
      'services/bossd/internal/db/x.go': `package db\nimport "${pkg}"\n`,
      'services/bossd/internal/db/nested/x.go': `package nested\nimport "${pkg}"\n`,
    },
    [edge('bossd', pkg)],
  )
  assert.deepEqual(check(root), { failures: [], offenders: [edge('bossd', pkg)] })
  fs.writeFileSync(path.join(root, 'services/bossd/internal/db/x.go'), 'package db\n')
  fs.writeFileSync(path.join(root, 'services/bossd/internal/db/nested/x.go'), 'package nested\n')
  assert.match(check(root).failures.join('\n'), /no longer occurs/)
})
test('store-imports rejects similarly named domain and store packages', (t) => {
  const root = fixture(t, {
    'services/bossd/internal/db/x.go': `package db
import (
  "github.com/recurser/bossd/internal/domainextra"
  "github.com/recurser/bossd/internal/dbtest"
)
`,
  })
  assert.equal(check(root).failures.length, 2)
})
test('store-imports real tree passes', () => {
  assert.deepEqual(checkStoreImports().failures, [])
})
test('store-imports parser agrees with go list on real store packages', (t) => {
  const available = spawnSync('go', ['version'], { encoding: 'utf8' })
  if (available.error?.code === 'ENOENT') {
    if (process.env.GUARD_PROOF_REQUIRE_TOOLS === '1')
      assert.fail('go required by GUARD_PROOF_REQUIRE_TOOLS=1')
    t.skip('go not on PATH')
    return
  }
  assert.equal(available.status, 0, available.stderr)
  const offenders = new Set()
  for (const service of ['bossd', 'bosso']) {
    const result = spawnSync('go', ['list', '-json', './internal/db/...'], {
      cwd: path.join(ROOT, 'services', service),
      encoding: 'utf8',
      timeout: 120000,
      maxBuffer: 4 * 1024 * 1024,
    })
    assert.equal(result.error, undefined)
    assert.equal(result.status, 0, result.stderr)
    // go list emits a stream of package objects rather than a JSON array.
    const packages = JSON.parse(`[${result.stdout.trim().replace(/}\s*{/g, '},{')}]`)
    assert.ok(
      packages.some((pkg) => pkg.ImportPath === `github.com/recurser/${service}/internal/db`),
    )
    for (const pkg of packages) {
      const parsed = new Set(
        pkg.GoFiles.flatMap((file) => imports(fs.readFileSync(path.join(pkg.Dir, file), 'utf8'))),
      )
      assert.deepEqual([...parsed].sort(), [...(pkg.Imports ?? [])].sort(), pkg.ImportPath)
      for (const dependency of pkg.Imports ?? []) {
        const own = `github.com/recurser/${service}/internal/`
        if (
          dependency.startsWith('github.com/recurser/bossalib/gen/') ||
          (dependency.startsWith(`github.com/recurser/${service}/`) &&
            dependency !== `${own}domain` &&
            !dependency.startsWith(`${own}domain/`) &&
            !dependency.startsWith(`${own}db/`))
        )
          offenders.add(edge(service, dependency))
      }
    }
  }
  assert.deepEqual(checkStoreImports().offenders, [...offenders].sort())
})

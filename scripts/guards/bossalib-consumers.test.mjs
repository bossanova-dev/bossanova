import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import test from 'node:test'
import { consumersByPackage } from './bossalib-consumers.mjs'

const shared = 'github.com/recurser/bossalib'
const modules = [
  { Path: shared, Dir: '/work/lib/bossalib' },
  { Path: 'example/bossd', Dir: '/work/services/bossd' },
  { Path: 'example/runner-a', Dir: '/work/plugins/runner-a' },
  { Path: 'example/runner-b', Dir: '/work/plugins/runner-b' },
]
const pkg = (name, Imports = [], extra = {}) => ({ ImportPath: name, Imports, ...extra })
const analyze = (listing, baseline = []) => consumersByPackage(listing, modules, baseline)

test('single consumer names its destination module', () => {
  const result = analyze([pkg(`${shared}/one`), pkg('example/bossd', [`${shared}/one`])])
  assert.deepEqual(result.packages[0].consumers, ['services/bossd'])
  assert.match(result.violations[0], /move it into services\/bossd\/internal\/one/)
})
test('unused package must be deleted', () => {
  assert.match(analyze([pkg(`${shared}/unused`)]).violations[0], /no module; delete it/)
})
test('transitive consumption follows shared imports and terminates cycles', () => {
  const result = analyze([
    pkg(`${shared}/one`, [`${shared}/two`]),
    pkg(`${shared}/two`, [`${shared}/one`]),
    pkg('example/bossd', [`${shared}/one`]),
    pkg('example/runner-a', [`${shared}/two`]),
  ])
  assert.equal(result.violations.length, 0)
  assert.ok(result.packages.every((entry) => entry.consumers.length === 2))
})
test('test support uses test imports only when there are no production importers', () => {
  const listing = [
    pkg(`${shared}/support`),
    pkg('example/bossd', [], { TestImports: [`${shared}/support`] }),
    pkg('example/runner-a', [], { XTestImports: [`${shared}/support`] }),
  ]
  const result = analyze(listing)
  assert.equal(result.packages[0].testSupport, true)
  assert.equal(result.violations.length, 0)
  listing[1].Imports.push(`${shared}/support`)
  assert.equal(analyze(listing).packages[0].testSupport, false)
  assert.equal(analyze(listing).violations.length, 1)
})
test('gen packages are excluded but their imports propagate consumers', () => {
  const result = analyze([
    pkg(`${shared}/gen/api`, [`${shared}/one`]),
    pkg(`${shared}/one`),
    pkg('example/bossd', [`${shared}/gen/api`]),
    pkg('example/runner-a', [`${shared}/gen/api`]),
  ])
  assert.deepEqual(
    result.packages.map((entry) => entry.package),
    ['lib/bossalib/one'],
  )
  assert.equal(result.violations.length, 0)
})
test('reasoned baseline exempts a current violation', () => {
  assert.deepEqual(analyze([pkg(`${shared}/one`)], ['lib/bossalib/one — parity']).violations, [])
})
test('baseline fails when moved or newly shared', () => {
  assert.match(analyze([], ['lib/bossalib/gone — parity']).violations[0], /no longer occurs/)
  assert.match(
    analyze(
      [
        pkg(`${shared}/one`),
        pkg('example/runner-a', [`${shared}/one`]),
        pkg('example/runner-b', [`${shared}/one`]),
      ],
      ['lib/bossalib/one — parity'],
    ).violations[0],
    /no longer occurs/,
  )
})
test('each plugin counts as a separate module', () => {
  const result = analyze([
    pkg(`${shared}/one`),
    pkg('example/runner-a', [`${shared}/one`]),
    pkg('example/runner-b', [`${shared}/one`]),
  ])
  assert.deepEqual(result.packages[0].consumers, ['plugins/runner-a', 'plugins/runner-b'])
  assert.equal(result.violations.length, 0)
})
test('bossalib-consumers real go list rejects one consumer and accepts two', () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'bossalib-consumers-'))
  const write = (file, text) => {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
  }
  try {
    write('go.work', 'go 1.26\nuse (\n./lib/bossalib\n./services/bossd\n./plugins/runner-a\n)\n')
    write('lib/bossalib/go.mod', `module ${shared}\ngo 1.26\n`)
    write('lib/bossalib/one/one.go', 'package one\n')
    write('services/bossd/go.mod', 'module example/bossd\ngo 1.26\n')
    write('plugins/runner-a/go.mod', 'module example/runner-a\ngo 1.26\n')
    write('services/bossd/main.go', `package bossd\nimport _ "${shared}/one"\n`)
    write('plugins/runner-a/main.go', 'package runner\n')
    write('scripts/guards/bossalib-consumers-baseline.json', '[]\n')
    const run = () =>
      spawnSync(
        process.execPath,
        [fileURLToPath(new URL('./bossalib-consumers.mjs', import.meta.url)), '--root', root],
        {
          encoding: 'utf8',
          env: { ...process.env, GOWORK: path.join(root, 'go.work'), GOTOOLCHAIN: 'local' },
        },
      )
    let result = run()
    assert.equal(result.status, 1, result.stdout + result.stderr)
    assert.match(result.stdout + result.stderr, /services\/bossd/)
    write('plugins/runner-a/main.go', `package runner\nimport _ "${shared}/one"\n`)
    result = run()
    assert.equal(result.status, 0, result.stdout + result.stderr)
    fs.rmSync(path.join(root, 'lib/bossalib/one'), {
      recursive: true,
      force: true,
      maxRetries: 5,
      retryDelay: 100,
    })
    result = run()
    assert.equal(result.status, 2, result.stdout + result.stderr)
  } finally {
    fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

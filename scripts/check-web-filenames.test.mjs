import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { checkWebFilenames } from './check-web-filenames.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const biome = path.join(repoRoot, 'services/web/node_modules/.bin/biome')
const baselinePath = 'scripts/web-filenames-baseline.json'

function fixture(t) {
  assert.ok(fs.existsSync(biome), 'Biome is missing — run pnpm install')
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'web-filenames-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const web = path.join(root, 'services/web')
  fs.mkdirSync(web, { recursive: true })
  fs.mkdirSync(path.join(root, 'scripts'))
  fs.copyFileSync(path.join(repoRoot, 'services/web/biome.json'), path.join(web, 'biome.json'))
  fs.copyFileSync(path.join(repoRoot, baselinePath), path.join(root, baselinePath))
  const entries = JSON.parse(fs.readFileSync(path.join(root, baselinePath), 'utf8'))
  const write = (name) => {
    fs.mkdirSync(path.dirname(path.join(web, name)), { recursive: true })
    fs.writeFileSync(path.join(web, name), '')
  }
  for (const entry of entries) write(entry)
  execFileSync('git', ['init', '-q', root])
  const check = () => {
    execFileSync('git', ['-C', root, 'add', 'services/web'])
    return checkWebFilenames({ root, biome })
  }
  return { root, web, write, check, entries }
}

test('checkWebFilenames fails a seeded kebab-case .ts file', (t) => {
  const f = fixture(t)
  f.write('src/foo-bar.ts')
  assert.match(f.check().failures.join('\n'), /foo-bar\.ts/)
})

test('checkWebFilenames fails a seeded camelCase .tsx file', (t) => {
  const f = fixture(t)
  f.write('src/fooBar.tsx')
  assert.match(f.check().failures.join('\n'), /fooBar\.tsx/)
})

test('checkWebFilenames accepts camelCase TS and PascalCase TSX and framework exceptions', (t) => {
  const f = fixture(t)
  for (const name of [
    'src/fooBar.ts',
    'src/FooBar.tsx',
    'src/main.tsx',
    'src/vite-env.d.ts',
    'src/Layout.authRedirect.test.tsx',
  ])
    f.write(name)
  assert.deepEqual(f.check().failures, [])
})

test('checkWebFilenames rejects a removed baseline file as stale', (t) => {
  const f = fixture(t)
  fs.unlinkSync(path.join(f.web, f.entries[0]))
  assert.match(f.check().failures.join('\n'), /no longer occurs — remove it/)
})

test('checkWebFilenames rejects an override literal missing from the baseline', (t) => {
  const f = fixture(t)
  const file = path.join(f.web, 'biome.json')
  const config = JSON.parse(fs.readFileSync(file, 'utf8'))
  config.overrides.at(-1).includes.push('src/another-file.ts')
  fs.writeFileSync(file, JSON.stringify(config))
  assert.match(f.check().failures.join('\n'), /another-file\.ts.*baseline/)
})

test('checkWebFilenames rejects a grandfathered file Biome no longer flags', (t) => {
  const f = fixture(t)
  const before = f.entries[0]
  const after = 'src/analytics/billingAnalytics.ts'
  fs.renameSync(path.join(f.web, before), path.join(f.web, after))
  const entries = f.entries.map((entry) => (entry === before ? after : entry)).sort()
  fs.writeFileSync(path.join(f.root, baselinePath), JSON.stringify(entries))
  const file = path.join(f.web, 'biome.json')
  const config = JSON.parse(fs.readFileSync(file, 'utf8'))
  config.overrides.at(-1).includes = config.overrides
    .at(-1)
    .includes.map((entry) => (entry === before ? after : entry))
  fs.writeFileSync(file, JSON.stringify(config))
  assert.match(f.check().failures.join('\n'), /billingAnalytics\.ts no longer occurs — remove it/)
})

test('checkWebFilenames fails when Biome is missing', (t) => {
  const f = fixture(t)
  assert.match(
    checkWebFilenames({ root: f.root, biome: path.join(f.root, 'missing') }).failures.join('\n'),
    /pnpm install/,
  )
})

test('checkWebFilenames rejects an untracked grandfathered file as stale', (t) => {
  const f = fixture(t)
  f.check()
  execFileSync('git', ['-C', f.root, 'rm', '--cached', '--', `services/web/${f.entries[0]}`])
  assert.match(
    checkWebFilenames({ root: f.root, biome }).failures.join('\n'),
    /no longer occurs — remove it/,
  )
})

test('checkWebFilenames rejects a missing exceptions override', (t) => {
  const f = fixture(t)
  const file = path.join(f.web, 'biome.json')
  const config = JSON.parse(fs.readFileSync(file, 'utf8'))
  config.overrides.pop()
  fs.writeFileSync(file, JSON.stringify(config))
  assert.match(
    f.check().failures.join('\n'),
    /no exceptions override contains every baseline entry/,
  )
})

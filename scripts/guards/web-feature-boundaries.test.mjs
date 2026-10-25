import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

const script = path.resolve('scripts/guards/web-feature-boundaries.mjs')
const biome = path.resolve('services/web/node_modules/.bin/biome')
const playwright = {
  includes: ['**', '!e2e/**', '!playwright.config.ts'],
  linter: {
    rules: {
      style: {
        noRestrictedImports: {
          level: 'error',
          options: { paths: { '@playwright/test': 'e2e only' } },
        },
      },
    },
  },
}
function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'web-boundaries-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const write = (name, body) => {
    const dest = path.join(root, name)
    fs.mkdirSync(path.dirname(dest), { recursive: true })
    fs.writeFileSync(dest, body)
  }
  write('services/web/biome.json', JSON.stringify({ overrides: [playwright] }))
  write('scripts/guards/web-layout-baseline.json', '[]\n')
  write('services/web/src/features/chat/index.ts', 'export const X = 1\n')
  write('services/web/src/features/sessions/pages/probe.ts', "import '~/features/chat'\n")
  write('services/web/src/shared/ui/probe.ts', 'export const x = 1\n')
  spawnSync('git', ['init', '-q'], { cwd: root })
  const run = (verb, mode = '--check', binary = biome) =>
    spawnSync(process.execPath, [script, verb, mode, '--root', root, '--biome', binary], {
      encoding: 'utf8',
    })
  return { root, write, run }
}
function passed(result) {
  assert.equal(result.status, 0, result.stdout + result.stderr)
}
function failed(result) {
  assert.equal(result.status, 1, result.stdout + result.stderr)
}

test('overrides rejects a hand edit and preserves Playwright restrictions', (t) => {
  const f = fixture(t)
  const fileRule = {
    includes: ['src/app/main.tsx', 'src/shared/lib/sentry-init.ts'],
    linter: { rules: { style: { useFilenamingConvention: 'off' } } },
  }
  f.write('services/web/biome.json', JSON.stringify({ overrides: [playwright, fileRule] }))
  passed(f.run('overrides', '--write'))
  const configPath = path.join(f.root, 'services/web/biome.json')
  const before = fs.readFileSync(configPath, 'utf8')
  passed(
    spawnSync(biome, ['format', 'biome.json'], {
      cwd: path.join(f.root, 'services/web'),
      encoding: 'utf8',
    }),
  )
  passed(f.run('overrides', '--write'))
  assert.equal(fs.readFileSync(configPath, 'utf8'), before)
  const config = JSON.parse(before)
  assert.deepEqual(config.overrides[0], playwright)
  assert.deepEqual(config.overrides[1], fileRule)
  config.overrides.at(-1).linter.rules.performance.noBarrelFile = 'error'
  fs.writeFileSync(configPath, JSON.stringify(config))
  failed(f.run('overrides'))
  passed(f.run('overrides', '--write'))
  f.write('services/web/src/features/sessions/pages/probe.ts', "import '@playwright/test'\n")
  failed(f.run('boundaries'))
})

function rejectsBoundary(t, file, source) {
  const f = fixture(t)
  f.write('services/web/src/components/Old.ts', 'export const old = 1\n')
  passed(f.run('overrides', '--write'))
  passed(f.run('boundaries'))
  f.write(`services/web/src/${file}`, source)
  const result = f.run('boundaries')
  failed(result)
  assert.match(result.stderr, /noRestrictedImports/)
}

test('boundaries rejects a seeded deep feature import', (t) => {
  rejectsBoundary(
    t,
    'features/sessions/pages/probe.ts',
    "import '~/features/chat/components/ChatListPanel'\n",
  )
})

test('boundaries rejects a seeded relative feature escape', (t) => {
  rejectsBoundary(t, 'features/sessions/pages/probe.ts', "import '../../chat/index'\n")
})

test('boundaries rejects a seeded shared feature import', (t) => {
  rejectsBoundary(t, 'shared/ui/probe.ts', "import '~/features/sessions'\n")
})

test('layout rejects a new legacy file and update only shrinks', (t) => {
  const f = fixture(t)
  passed(f.run('layout'))
  f.write('services/web/src/components/Old.tsx', 'export const x = 1\n')
  spawnSync('git', ['add', '.'], { cwd: f.root })
  failed(f.run('layout'))
  failed(f.run('layout', '--update'))
  f.write(
    'scripts/guards/web-layout-baseline.json',
    JSON.stringify(['services/web/src/components/Old.tsx']),
  )
  passed(f.run('layout'))
  fs.unlinkSync(path.join(f.root, 'services/web/src/components/Old.tsx'))
  spawnSync('git', ['add', '-u'], { cwd: f.root })
  failed(f.run('layout'))
  passed(f.run('layout', '--update'))
  passed(f.run('layout'))
})

test('boundaries fails closed when Biome cannot provide diagnostics', (t) => {
  const f = fixture(t)
  passed(f.run('overrides', '--write'))
  failed(f.run('boundaries', '--check', path.join(f.root, 'missing-biome')))
  const fake = path.join(f.root, 'biome')
  fs.writeFileSync(fake, '#!/bin/sh\nprintf "{}"\n', { mode: 0o755 })
  failed(f.run('boundaries', '--check', fake))
})

test('overrides leaves the config unchanged when its formatter fails', (t) => {
  const f = fixture(t)
  passed(f.run('overrides', '--write'))
  const config = path.join(f.root, 'services/web/biome.json')
  const original = fs.readFileSync(config, 'utf8')
  failed(f.run('overrides', '--write', path.join(f.root, 'missing-biome')))
  assert.equal(fs.readFileSync(config, 'utf8'), original)
  const fake = path.join(f.root, 'biome')
  fs.writeFileSync(fake, '#!/bin/sh\nprintf "{}"\n', { mode: 0o755 })
  failed(f.run('overrides', '--write', fake))
  assert.equal(fs.readFileSync(config, 'utf8'), original)
})

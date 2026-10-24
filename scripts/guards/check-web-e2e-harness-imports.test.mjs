import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { checkWebE2eHarnessImports } from './check-web-e2e-harness-imports.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const biome = path.join(repoRoot, 'services/web/node_modules/.bin/biome')
const baselinePath = 'scripts/guards/web-e2e-harness-imports-baseline.json'

function fixture(t) {
  assert.ok(fs.existsSync(biome), 'Biome is missing — run pnpm install')
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'web-e2e-harness-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5 }))
  const web = path.join(root, 'services/web')
  fs.mkdirSync(web, { recursive: true })
  fs.mkdirSync(path.join(root, 'scripts/guards'), { recursive: true })
  fs.copyFileSync(path.join(repoRoot, 'services/web/biome.json'), path.join(web, 'biome.json'))
  fs.writeFileSync(path.join(root, baselinePath), '[]\n')
  const write = (name, content = "import '@playwright/test'\n") => {
    fs.mkdirSync(path.dirname(path.join(web, name)), { recursive: true })
    fs.writeFileSync(path.join(web, name), content)
  }
  write('src/valid.ts', 'export {}\n')
  const config = (edit) => {
    const file = path.join(web, 'biome.json')
    const value = JSON.parse(fs.readFileSync(file, 'utf8'))
    edit(value)
    fs.writeFileSync(file, JSON.stringify(value))
  }
  return { root, web, write, config, check: () => checkWebE2eHarnessImports({ root, biome }) }
}

test('checkWebE2eHarnessImports fails a seeded Playwright import outside e2e', (t) => {
  const f = fixture(t)
  f.write('src/seeded.ts')
  assert.match(f.check().failures.join('\n'), /src\/seeded\.ts/)
})

test('checkWebE2eHarnessImports allows Playwright in e2e and its config', (t) => {
  const f = fixture(t)
  f.write('e2e/seeded.ts')
  f.write('e2e/real/seeded.ts')
  f.write('playwright.config.ts')
  assert.deepEqual(f.check().failures, [])
})

test('checkWebE2eHarnessImports rejects extra override exemptions', (t) => {
  const f = fixture(t)
  f.config((config) =>
    config.overrides
      .find((o) => o.linter?.rules?.style?.noRestrictedImports)
      .includes.push('!src/**'),
  )
  assert.match(f.check().failures.join('\n'), /includes.*baseline/)
})

test('checkWebE2eHarnessImports rejects a disabled restriction', (t) => {
  const f = fixture(t)
  f.config((config) => {
    config.overrides.find(
      (o) => o.linter?.rules?.style?.noRestrictedImports,
    ).linter.rules.style.noRestrictedImports.level = 'off'
  })
  assert.match(f.check().failures.join('\n'), /error.*@playwright\/test/)
})

test('checkWebE2eHarnessImports verifies baseline entries still import Playwright', (t) => {
  const f = fixture(t)
  f.write('src/legacy.ts')
  fs.writeFileSync(path.join(f.root, baselinePath), '["src/legacy.ts"]\n')
  f.config((config) =>
    config.overrides
      .find((o) => o.linter?.rules?.style?.noRestrictedImports)
      .includes.push('!src/legacy.ts'),
  )
  assert.deepEqual(f.check().failures, [])
  f.write('src/legacy.ts', 'export {}\n')
  assert.match(f.check().failures.join('\n'), /legacy\.ts no longer occurs/)
})

test('checkWebE2eHarnessImports fails closed without Biome', (t) => {
  const f = fixture(t)
  assert.match(
    checkWebE2eHarnessImports({ root: f.root, biome: path.join(f.root, 'missing') }).failures.join(
      '\n',
    ),
    /Biome could not run/,
  )
})

test('checkWebE2eHarnessImports fails closed on malformed Biome output', (t) => {
  const f = fixture(t)
  const fake = path.join(f.root, 'biome')
  fs.writeFileSync(fake, '#!/bin/sh\nprintf "{}"\n', { mode: 0o755 })
  assert.match(
    checkWebE2eHarnessImports({ root: f.root, biome: fake }).failures.join('\n'),
    /readable report/,
  )
})

test('checkWebE2eHarnessImports rejects a boundary override that drops Playwright restrictions', (t) => {
  const f = fixture(t)
  f.config((config) => {
    const boundary = config.overrides.find(
      (o) => o.linter?.rules?.style?.noRestrictedImports && !o.includes?.includes('**'),
    )
    delete boundary.linter.rules.style.noRestrictedImports.options.paths['@playwright/test']
  })
  assert.match(f.check().failures.join('\n'), /error.*@playwright\/test/)
})

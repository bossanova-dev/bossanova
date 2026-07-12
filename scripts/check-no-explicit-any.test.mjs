#!/usr/bin/env node

import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { scanForExplicitAny } from './check-no-explicit-any.mjs'

function fixtureRoot(files) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'check-no-explicit-any-'))
  for (const [name, contents] of Object.entries(files)) {
    const full = path.join(root, name)
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, contents)
  }
  return root
}

test('clean fixture reports no offenders', () => {
  const root = fixtureRoot({
    'a.ts': 'export const x: number = 1\n',
    'b/c.tsx': 'const foo = bar as Widget\n',
  })

  assert.deepEqual(scanForExplicitAny([root]), [])
})

test('literal `as any` cast is detected', () => {
  const root = fixtureRoot({ 'cast.ts': 'const y = x as any\n' })

  const offenders = scanForExplicitAny([root])
  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].rule, 'as any cast')
  assert.equal(offenders[0].line, 1)
  assert.ok(offenders[0].file.endsWith('cast.ts'))
})

test('biome-ignore line naming noExplicitAny is detected', () => {
  const root = fixtureRoot({
    'sup.tsx': [
      '// biome-ignore lint/suspicious/noExplicitAny: test helpers use loose typing',
      'const z = 1',
      '',
    ].join('\n'),
  })

  const offenders = scanForExplicitAny([root])
  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].rule, 'noExplicitAny suppression')
  assert.equal(offenders[0].line, 1)
})

test('file-level biome-ignore-all naming noExplicitAny is detected', () => {
  const root = fixtureRoot({
    'all.ts': [
      '// biome-ignore-all lint/suspicious/noExplicitAny: whole-file suppression',
      'export const q = 2',
      '',
    ].join('\n'),
  })

  const offenders = scanForExplicitAny([root])
  assert.equal(offenders.length, 1)
  assert.equal(offenders[0].rule, 'noExplicitAny suppression')
})

test('benign lines are not flagged', () => {
  const root = fixtureRoot({
    'benign.ts': [
      'const a = value as unknown as Foo',
      'const anyValue = 3',
      'const el = canvas',
      'type T = { asanyfoo: number }',
      'const list = z.anyOf([])',
      '// biome-ignore lint/style/useConst: intentional let',
      '',
    ].join('\n'),
  })

  assert.deepEqual(scanForExplicitAny([root]), [])
})

test('missing roots are skipped without error', () => {
  const missing = path.join(os.tmpdir(), 'check-no-explicit-any-does-not-exist-xyz')

  assert.deepEqual(scanForExplicitAny([missing]), [])
})

test('node_modules / gen / dist directories are pruned', () => {
  const root = fixtureRoot({
    'node_modules/dep.ts': 'const a = x as any\n',
    'gen/proto.ts': 'const b = y as any\n',
    'dist/bundle.js': 'const c = z as any\n',
    'src/real.ts': 'export const ok = 1\n',
  })

  assert.deepEqual(scanForExplicitAny([root]), [])
})

test('the real repository tree is clean (gate is green after BOS-335 Tasks 1-2)', () => {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
  const roots = [
    path.join(repoRoot, 'services/web/src'),
    path.join(repoRoot, 'services/marketing/src'),
  ]

  assert.deepEqual(scanForExplicitAny(roots), [])
})

test('both TS CI workflows run the gate after their Lint step', () => {
  const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
  for (const wf of ['.github/workflows/test-web.yml', '.github/workflows/test-marketing.yml']) {
    const contents = fs.readFileSync(path.join(repoRoot, wf), 'utf8')
    assert.ok(
      contents.includes('node scripts/check-no-explicit-any.mjs'),
      `${wf} should invoke the gate`,
    )
  }
})

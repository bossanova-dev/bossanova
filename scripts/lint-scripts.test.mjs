import { test } from 'node:test'
import assert from 'node:assert/strict'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { computeScriptsLintKey, isCached, resolveScriptFiles } from './lint-scripts.mjs'

const files = () => [
  { path: 'scripts/a.mjs', content: Buffer.from('export const a = 1\n') },
  { path: 'scripts/b.mjs', content: Buffer.from('export const b = 2\n') },
]

test('computeScriptsLintKey is deterministic and order-independent', () => {
  const nodeVersion = 'v24.0.0'
  const k1 = computeScriptsLintKey({ files: files(), nodeVersion })
  const k2 = computeScriptsLintKey({ files: [...files()].reverse(), nodeVersion })
  assert.equal(k1, k2)
  assert.match(k1, /^[a-f0-9]{64}$/)
})

test('key changes when any file content changes', () => {
  const nodeVersion = 'v24.0.0'
  const base = computeScriptsLintKey({ files: files(), nodeVersion })
  const edited = files()
  edited[0].content = Buffer.from('export const a = 999\n')
  assert.notEqual(base, computeScriptsLintKey({ files: edited, nodeVersion }))
})

test('key changes when the file set changes', () => {
  const nodeVersion = 'v24.0.0'
  const base = computeScriptsLintKey({ files: files(), nodeVersion })
  const added = [...files(), { path: 'scripts/c.mjs', content: Buffer.from('//\n') }]
  assert.notEqual(base, computeScriptsLintKey({ files: added, nodeVersion }))
})

test('key changes with the Node version (node --check tracks the runtime)', () => {
  const k1 = computeScriptsLintKey({ files: files(), nodeVersion: 'v22.0.0' })
  const k2 = computeScriptsLintKey({ files: files(), nodeVersion: 'v24.0.0' })
  assert.notEqual(k1, k2)
})

test('isCached fails open: disabled / forced / missing stamp all re-check', () => {
  const stampPath = path.join(os.tmpdir(), 'definitely-missing-stamp-xyz')
  assert.equal(isCached({ stampsEnabled: false, force: false, stampPath }), false)
  assert.equal(isCached({ stampsEnabled: true, force: true, stampPath }), false)
  assert.equal(isCached({ stampsEnabled: true, force: false, stampPath }), false)
})

test('isCached true only when enabled, unforced, and the stamp exists', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'scripts-lint-stamp-'))
  const stampPath = path.join(dir, 'scripts-lint-abc')
  fs.writeFileSync(stampPath, 'x\n')
  try {
    assert.equal(isCached({ stampsEnabled: true, force: false, stampPath }), true)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

test('resolveScriptFiles covers this test file and excludes non-.mjs', () => {
  const repoRoot = execFileSync('git', ['rev-parse', '--show-toplevel'], {
    encoding: 'utf8',
  }).trim()
  const found = resolveScriptFiles(repoRoot)
  assert.ok(found.includes('scripts/lint-scripts.mjs'))
  assert.ok(found.includes('scripts/lint-scripts.test.mjs'))
  // Sorted, unique, and only ever .cjs/.mjs.
  assert.deepEqual(found, [...found].sort())
  for (const rel of found) assert.match(rel, /\.(c|m)js$/)
})

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { CSS_FILE, UPDATE_COMMAND, buildTokens } from './build.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..')
const CLI = path.join(repoRoot, 'scripts', 'skills', 'proof-tokens', 'build.mjs')
const cli = (/** @type {string[]} */ ...args) =>
  spawnSync(process.execPath, [CLI, ...args], { encoding: 'utf8' })
const css = fs.readFileSync(path.join(repoRoot, CSS_FILE), 'utf8')

test('the committed proof page theme matches lib/ui-tokens', () => {
  const result = cli('--check')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /is up to date/)
})

test('a token edit in lib/ui-tokens makes the committed theme stale', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tokens-'))
  try {
    const seeded = path.join(dir, 'index.css')
    fs.writeFileSync(seeded, css.replace('--success: #5edc9b;', '--success: #00ff00;'))
    const result = cli('--check', '--css', seeded)
    assert.equal(result.status, 1, result.stdout)
    assert.ok(result.stderr.includes(UPDATE_COMMAND), result.stderr)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('dark is the default, light follows the system unless a theme is forced', async () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'proof-tokens-'))
  try {
    const file = path.join(dir, 'tokens.mjs')
    fs.writeFileSync(file, buildTokens(css))
    const { PROOF_TOKENS } = await import(file)
    const rules = Object.fromEntries(
      [...PROOF_TOKENS.matchAll(/(^|\n)([^{\n]+)\{([^}]*)\}/g)].map((m) => [m[2], m[3]]),
    )
    assert.match(rules[':root'], /--success:#5edc9b;.*color-scheme:dark$/)
    assert.match(
      PROOF_TOKENS,
      /@media \(prefers-color-scheme:light\)\{:root:not\(\[data-theme\]\)\{[^}]*--success:#167d4f;/,
    )
    assert.match(rules[':root[data-theme="light"]'], /--success:#167d4f;.*color-scheme:light$/)
    assert.equal(rules[':root[data-theme="dark"]'], 'color-scheme:dark')
    assert.doesNotMatch(PROOF_TOKENS, /\/\*/)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

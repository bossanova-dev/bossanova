import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { PROOF_STYLE } from '../../../skills-toolbox/verify/verify-proof-style.mjs'
import { UPDATE_COMMAND, run, shadowStyle } from './build.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..')
const CLI = path.join(repoRoot, 'scripts', 'skills', 'review-page-style', 'build.mjs')

test('the committed web copy matches the hosted page stylesheet', () => {
  const result = spawnSync(process.execPath, [CLI, '--check'], { encoding: 'utf8' })
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /is up to date/)
})

test('a stylesheet edit makes the committed copy stale', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'review-page-style-'))
  try {
    const out = path.join(dir, 'style.ts')
    assert.equal(run({ outPath: out }).ok, true)
    const stale = run({ check: true, outPath: out, style: `${PROOF_STYLE}\n.x{color:red}` })
    assert.equal(stale.ok, false)
    assert.ok(stale.message.includes(UPDATE_COMMAND), stale.message)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('document selectors become the shadow host, and width queries query the host', () => {
  const sheet = shadowStyle(PROOF_STYLE)
  for (const host of [
    ':host{',
    ':host([data-theme="light"]){',
    ':host([data-theme="dark"]){',
    ':host(:not([data-theme])){',
  ])
    assert.ok(sheet.includes(host), host)
  assert.doesNotMatch(sheet, /:root|\nhtml\{|\nbody|@media \(max-width/)
  assert.match(sheet, /@container \(max-width:560px\)/)
  assert.match(sheet, /\n:host\{display:block;container-type:inline-size\}$/)
  const rules = (css) => css.split('}').length
  assert.equal(rules(sheet), rules(PROOF_STYLE) + 1, 'every page rule is kept, plus the host rule')
})

test('a document selector or viewport query the host cannot stand in for fails the build', () => {
  for (const extra of [
    '\nhtml.dark .x{color:red}',
    '\n.a{}\nbody>main{color:red}',
    '@media (min-width:900px){.x{color:red}}',
  ])
    assert.throws(() => shadowStyle(PROOF_STYLE + extra), /a shadow root cannot match/, extra)
})

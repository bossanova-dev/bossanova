import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import { registryCoverage } from './registry-coverage.mjs'

const CLI = fileURLToPath(new URL('./registry-coverage.mjs', import.meta.url))
const records = [
  { id: 'first-guard', origin: { classKeys: ['known-class', 'old-alias'], solutions: [] } },
  { id: 'existing-guard' },
]

test('registryCoverage matches canonical keys, aliases and registered existing checks', () => {
  assert.deepEqual(
    registryCoverage(
      [
        { classKey: 'known-class', aliases: [] },
        { classKey: 'new-class', aliases: ['old-alias'] },
        { classKey: 'check-class', existingCheck: 'existing-guard' },
        { classKey: 'unknown-class', aliases: [], existingCheck: 'not-registered' },
        { classKey: 'prose-only', existingCheck: 'node existing-guard' },
      ],
      records,
    ),
    [
      { classKey: 'known-class', coverage: 'covered:first-guard' },
      { classKey: 'new-class', coverage: 'covered:first-guard' },
      { classKey: 'check-class', coverage: 'covered:existing-guard' },
      { classKey: 'unknown-class', coverage: 'uncovered' },
      { classKey: 'prose-only', coverage: 'uncovered' },
    ],
  )
})

test('registryCoverage resolves ambiguous matches deterministically by guard id', () => {
  const duplicate = { id: 'a-guard', origin: { classKeys: ['known-class'] } }
  assert.deepEqual(registryCoverage([{ classKey: 'known-class' }], [...records, duplicate]), [
    { classKey: 'known-class', coverage: 'covered:a-guard' },
  ])
})

test('coverage CLI reads classes JSON and the repository registry', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'registry-coverage-'))
  try {
    const classes = path.join(dir, 'classes.json')
    fs.writeFileSync(
      classes,
      JSON.stringify([
        { classKey: 'registered-check', existingCheck: 'guard-registry' },
        { classKey: 'unknown-class' },
      ]),
    )
    const result = spawnSync(process.execPath, [CLI, '--classes', classes], { encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
    assert.deepEqual(JSON.parse(result.stdout), [
      { classKey: 'registered-check', coverage: 'covered:guard-registry' },
      { classKey: 'unknown-class', coverage: 'uncovered' },
    ])
    for (const args of [[], ['--classes', classes, '--unknown'], ['--classes', classes, 'stray']]) {
      assert.notEqual(spawnSync(process.execPath, [CLI, ...args]).status, 0)
    }
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5 })
  }
})

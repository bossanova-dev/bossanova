import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import * as skillConfig from '../../skills-toolbox/skill-config.mjs'
import { OUTPUT_FILE, SCHEMA_FILE, UPDATE_COMMAND, run } from './build.mjs'
import * as schemaModule from './schema.mjs'

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const CLI = path.join(repoRoot, 'scripts', 'skill-config-schema', 'build.mjs')

const cli = (...args) => spawnSync(process.execPath, [CLI, ...args], { encoding: 'utf8' })

function scratch() {
  return fs.mkdtempSync(path.join(os.tmpdir(), 'skill-config-schema-'))
}

test('build --check passes on the committed validator', () => {
  const result = cli('--check')
  assert.equal(result.status, 0, result.stderr)
  assert.match(result.stdout, /is up to date/)
})

test('build --check fails after a seeded schema edit', () => {
  const dir = scratch()
  try {
    const seeded = path.join(dir, 'schema.mjs')
    fs.writeFileSync(
      seeded,
      `${fs.readFileSync(path.join(repoRoot, SCHEMA_FILE), 'utf8')}\nschema.properties.seeded = { type: 'string' }\n`,
    )
    const result = cli('--check', '--schema', seeded)
    assert.equal(result.status, 1, result.stdout)
    assert.match(result.stderr, /is stale against/)
    assert.ok(result.stderr.includes(UPDATE_COMMAND), result.stderr)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('build --check fails when the generated file is missing', async () => {
  const dir = scratch()
  try {
    const result = await run({ check: true, outPath: path.join(dir, 'absent.mjs') })
    assert.equal(result.ok, false)
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('build writes the committed bytes, so regenerating is idempotent', async () => {
  const dir = scratch()
  try {
    const out = path.join(dir, 'out.mjs')
    assert.equal((await run({ outPath: out })).ok, true)
    assert.equal(
      fs.readFileSync(out, 'utf8'),
      fs.readFileSync(path.join(repoRoot, OUTPUT_FILE), 'utf8'),
    )
  } finally {
    fs.rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('the schema and skill-config.mjs agree on the vocabularies both declare', () => {
  assert.equal(schemaModule.VERIFY_PATH_LABEL_MAX, skillConfig.VERIFY_PATH_LABEL_MAX)
  assert.deepEqual(schemaModule.ADAPTER_KINDS, [...skillConfig.ADAPTER_KINDS])
  assert.deepEqual(schemaModule.PLAN_SECTION_REQUIRED_KINDS, [
    ...skillConfig.PLAN_SECTION_REQUIRED_KINDS,
  ])
  assert.deepEqual(schemaModule.VERIFY_HUMAN_REVIEW_CATEGORIES, [
    ...skillConfig.VERIFY_HUMAN_REVIEW_CATEGORIES,
  ])
})

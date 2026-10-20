// guard-id: skill-config-validator-standalone

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { copyFileSync, existsSync, mkdtempSync, readFileSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { DEFAULT_CONFIG, validateConfig } from './skill-config.mjs'

const GENERATED = join(
  dirname(fileURLToPath(import.meta.url)),
  'skill-config-validate.generated.mjs',
)
// Any static or dynamic import, re-export, or require call. Ordinary words such as `required`
// do not match.
const MODULE_LOAD =
  /\bimport\s*[\s({'"*]|\bimport\s*\(|\bexport\s*[^;]*?\bfrom\s*['"]|\brequire\s*\(/

function capture(run) {
  const originalWarn = console.warn
  const warnings = []
  console.warn = (message) => warnings.push(String(message))
  try {
    return { result: run(), warnings }
  } catch (error) {
    return { error, warnings }
  } finally {
    console.warn = originalWarn
  }
}

test('the generated validator imports and runs alone in an empty directory', async () => {
  const dir = realpathSync(mkdtempSync(join(tmpdir(), 'skill-config-validate-')))
  try {
    for (let up = dir; ; up = dirname(up)) {
      assert.equal(existsSync(join(up, 'node_modules')), false, `${up} has a node_modules`)
      assert.equal(existsSync(join(up, 'package.json')), false, `${up} has a package.json`)
      if (dirname(up) === up) break
    }
    const copy = join(dir, 'skill-config-validate.generated.mjs')
    copyFileSync(GENERATED, copy)
    const source = readFileSync(copy, 'utf8')
    assert.doesNotMatch(source, MODULE_LOAD)

    const { validate, schema } = await import(pathToFileURL(copy).href)
    assert.equal(typeof validate, 'function')
    assert.equal(schema.type, 'object')

    assert.equal(validate(structuredClone(DEFAULT_CONFIG)), true, JSON.stringify(validate.errors))

    const bad = structuredClone(DEFAULT_CONFIG)
    bad.env.headlessWhenNoTty = 'false'
    assert.equal(validate(bad), false)
    assert.ok(
      validate.errors.some((error) => error.instancePath === '/env/headlessWhenNoTty'),
      JSON.stringify(validate.errors),
    )
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
})

test('through validateConfig, unknown keys warn where the block is open-for-warning and throw where it is closed', () => {
  const unknownTop = { ...structuredClone(DEFAULT_CONFIG), mystery: true }
  const top = capture(() => validateConfig(unknownTop, 'fixture.json'))
  assert.equal(top.error, undefined)
  assert.deepEqual(top.warnings, [
    'skill-config: fixture.json: mystery is not a known key; ignored',
  ])
  assert.equal(unknownTop.mystery, true, 'an unknown key is retained')

  const unknownReview = structuredClone(DEFAULT_CONFIG)
  unknownReview.review.secondModle = false
  const review = capture(() => validateConfig(unknownReview, 'fixture.json'))
  assert.equal(review.error, undefined)
  assert.deepEqual(review.warnings, [
    'skill-config: fixture.json: review.secondModle is not a known key; ignored',
  ])

  const unknownDeps = structuredClone(DEFAULT_CONFIG)
  unknownDeps.planDependencies.repoWideToken = ['x']
  const deps = capture(() => validateConfig(unknownDeps, 'fixture.json'))
  assert.match(
    deps.error?.message ?? '',
    /^skill-config: invalid config from fixture\.json: planDependencies\.repoWideToken is not a known key$/,
  )

  const unknownRetro = { ...structuredClone(DEFAULT_CONFIG), retro: { maxIssue: 3 } }
  assert.match(
    capture(() => validateConfig(unknownRetro, 'fixture.json')).error?.message ?? '',
    /retro\.maxIssue is unknown/,
  )
})

test('retired keys warn once as retired, never also as unknown', () => {
  const config = structuredClone(DEFAULT_CONFIG)
  config.lensMap = {}
  config.reviewDefaults.rounds = 3
  const { error, warnings } = capture(() => validateConfig(config, 'fixture.json'))
  assert.equal(error, undefined)
  assert.deepEqual(warnings, [
    'skill-config: fixture.json: lensMap is retired and ignored',
    'skill-config: fixture.json: reviewDefaults.rounds is retired and ignored',
  ])
})

test('a deferred load records a stage-block fault and still fails a global block', () => {
  const staged = structuredClone(DEFAULT_CONFIG)
  staged.planDependencies.moduleRoots = 'lib'
  const deferred = capture(() => validateConfig(staged, 'fixture.json', { deferStageBlocks: true }))
  assert.equal(deferred.error, undefined)
  assert.deepEqual(deferred.warnings, [
    'skill-config: deferred planDependencies: skill-config: invalid config from fixture.json: planDependencies.moduleRoots must be an array; run boss skills install',
  ])

  const global = structuredClone(DEFAULT_CONFIG)
  global.adapters.tracker = ''
  assert.match(
    capture(() => validateConfig(global, 'fixture.json', { deferStageBlocks: true })).error
      ?.message ?? '',
    /adapters\.tracker must be a non-empty string/,
  )
})

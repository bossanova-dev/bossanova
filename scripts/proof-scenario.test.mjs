import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  validateScenario,
  loadScenario,
  deriveScenes,
  STEP_OPS,
  SCENARIO_MAX_SCENES,
} from './proof-scenario.mjs'
import { MATCH_MODES } from './proof-evidence-matcher.mjs'

const FIXTURE_DIR = fileURLToPath(new URL('./testdata/scenario-fixtures/', import.meta.url))
const readFixture = (name) => JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, name), 'utf8'))
const SCHEMA_PATH = new URL('../proof/scenarios/schema.json', import.meta.url)
const readSchema = () => JSON.parse(fs.readFileSync(SCHEMA_PATH, 'utf8'))

// Every invalid-*.json rejection class, its pinned reason keyword, and whether a
// scenes[ path is applicable in the first error.
const INVALID_CASES = {
  'invalid-unknown-op.json': { reason: 'unknown step op', pathScoped: true },
  'invalid-multi-op.json': { reason: 'exactly one', pathScoped: true },
  'invalid-bad-mode.json': { reason: 'unknown match mode', pathScoped: true },
  'invalid-no-title.json': { reason: 'title', pathScoped: true },
  'invalid-empty-scenes.json': { reason: 'at least one', pathScoped: false },
  'invalid-five-scenes.json': { reason: 'at most 4', pathScoped: false },
  'invalid-bad-timeout.json': { reason: 'timeoutMs', pathScoped: true },
  'invalid-empty-expect-text.json': { reason: 'empty text', pathScoped: true },
  'invalid-bad-regex.json': { reason: 'invalid regex', pathScoped: true },
  'invalid-unknown-top.json': { reason: 'unknown top-level', pathScoped: false },
  'invalid-bad-env.json': { reason: 'env values must be strings', pathScoped: false },
  'invalid-daemon-no-action.json': { reason: 'daemon.action', pathScoped: true },
}

test('validateScenario accepts every valid-*.json fixture', () => {
  const valids = fs.readdirSync(FIXTURE_DIR).filter((f) => f.startsWith('valid-'))
  assert.ok(valids.length >= 2, 'expected valid-full + valid-minimal fixtures')
  for (const name of valids) {
    const r = validateScenario(readFixture(name))
    assert.equal(r.ok, true, `${name} should validate; errors: ${JSON.stringify(r.errors)}`)
  }
})

test('validateScenario rejects every invalid-*.json with a pointerful reason', () => {
  const invalids = fs.readdirSync(FIXTURE_DIR).filter((f) => f.startsWith('invalid-'))
  // every invalid fixture is represented in the table
  for (const name of invalids) {
    assert.ok(INVALID_CASES[name], `no pinned expectation for fixture ${name}`)
  }
  for (const [name, { reason, pathScoped }] of Object.entries(INVALID_CASES)) {
    const r = validateScenario(readFixture(name))
    assert.equal(r.ok, false, `${name} should be rejected`)
    const first = r.errors[0]
    assert.match(
      first,
      new RegExp(reason.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')),
      `${name}: ${first}`,
    )
    if (pathScoped)
      assert.ok(first.includes('scenes['), `${name} first error lacks scenes[ path: ${first}`)
  }
})

test('multi-op step names both ops in the error', () => {
  const r = validateScenario(readFixture('invalid-multi-op.json'))
  assert.equal(r.ok, false)
  assert.match(r.errors[0], /found key, type/)
})

test('validator collects ALL errors in one pass (multi-op fixture has two)', () => {
  const r = validateScenario(readFixture('invalid-multi-op.json'))
  assert.equal(r.errors.length, 2)
  assert.match(r.errors[1], /waitFor, waitMs/)
})

test('waitFor accepts a bare string and an expectation object', () => {
  const base = (waitFor) => ({
    version: 1,
    title: 't',
    scenes: [{ title: 's', steps: [{ waitFor }] }],
  })
  assert.equal(validateScenario(base('Ready')).ok, true)
  assert.equal(validateScenario(base({ text: 'Ready', match: 'literal' })).ok, true)
  assert.equal(validateScenario(base({ text: 'x', match: 'bogus' })).ok, false)
})

test('caption is allowed on every op', () => {
  for (const step of [
    { key: 'down', caption: 'c' },
    { type: '45', caption: 'c' },
    { waitFor: 'Ready', caption: 'c' },
    { waitMs: 100, caption: 'c' },
    { daemon: { action: 'push' }, caption: 'c' },
    { expect: 'x', caption: 'c' },
  ]) {
    const r = validateScenario({ version: 1, title: 't', scenes: [{ title: 's', steps: [step] }] })
    assert.equal(
      r.ok,
      true,
      `caption rejected on ${Object.keys(step)[0]} step: ${JSON.stringify(r.errors)}`,
    )
  }
})

test('fixture preset defaults to "demo" when absent', () => {
  const r = validateScenario(readFixture('valid-minimal.json'))
  assert.equal(r.ok, true)
  assert.equal(r.scenario.fixture.preset, 'demo')
})

test('version must be exactly 1', () => {
  const r = validateScenario({
    version: 2,
    title: 't',
    scenes: [{ title: 's', steps: [{ expect: 'x' }] }],
  })
  assert.equal(r.ok, false)
  assert.match(r.errors.join('\n'), /version: must be 1/)
})

// --- Task 5: loader + derivation ---

test('deriveScenes yields normalizeScenes-shaped scenes with displayText evidence', () => {
  const { scenario } = validateScenario(readFixture('valid-full.json'))
  const scenes = deriveScenes(scenario)
  assert.equal(scenes.length, 1)
  assert.deepEqual(Object.keys(scenes[0]).sort(), ['expectedEvidence', 'id', 'stepsHints', 'title'])
  assert.equal(scenes[0].id, 'scene-01')
  assert.equal(scenes[0].title, 'Open session settings')
  assert.deepEqual(scenes[0].stepsHints, [])
  assert.deepEqual(scenes[0].expectedEvidence, [
    'Archive delay',
    'Archive delay',
    '45 minutes',
    'delay shows minutes',
    'delay rendered',
  ])
})

test('deriveScenes synthesizes scene-NN ids when absent (matching normalizeScenes padding)', () => {
  const { scenario } = validateScenario({
    version: 1,
    title: 't',
    scenes: [
      { title: 'a', steps: [{ expect: 'x' }] },
      { title: 'b', steps: [{ key: 'down' }] },
    ],
  })
  const scenes = deriveScenes(scenario)
  assert.deepEqual(
    scenes.map((s) => s.id),
    ['scene-01', 'scene-02'],
  )
  // a scene with no expects yields an empty evidence list
  assert.deepEqual(scenes[1].expectedEvidence, [])
})

test('loadScenario throws with the file path on a missing file', () => {
  assert.throws(() => loadScenario('/no/such/scenario.json'), /ENOENT|no such file/i)
})

test('loadScenario throws with the file path on invalid JSON', () => {
  const p = path.join(os.tmpdir(), `bos218-badjson-${process.pid}.json`)
  fs.writeFileSync(p, '{ not json ')
  try {
    assert.throws(
      () => loadScenario(p),
      (err) => err.message.includes(p) && /invalid JSON/.test(err.message),
    )
  } finally {
    fs.rmSync(p, { force: true })
  }
})

test('loadScenario throws with ALL collected errors joined on a validation failure', () => {
  const p = path.join(os.tmpdir(), `bos218-invalid-${process.pid}.json`)
  fs.writeFileSync(p, fs.readFileSync(path.join(FIXTURE_DIR, 'invalid-multi-op.json'), 'utf8'))
  try {
    assert.throws(
      () => loadScenario(p),
      (err) => /exactly one/.test(err.message) && /waitFor, waitMs/.test(err.message),
    )
  } finally {
    fs.rmSync(p, { force: true })
  }
})

test('loadScenario returns scenario + derived scenes for a valid file', () => {
  const { scenario, scenes } = loadScenario(path.join(FIXTURE_DIR, 'valid-full.json'))
  assert.equal(scenario.version, 1)
  assert.equal(scenes[0].title, 'Open session settings')
})

// --- Task 6: schema-agreement suite (D2 drift pin) ---

test('schema agreement: step union lists exactly the validator STEP_OPS', () => {
  const schema = readSchema()
  const variants = schema.$defs.step.oneOf.map(
    (v) => Object.keys(v.properties).filter((k) => !['caption', 'timeoutMs'].includes(k))[0],
  )
  assert.deepEqual(variants.sort(), [...STEP_OPS].sort())
})

test('schema agreement: match-mode enum equals MATCH_MODES', () => {
  const schema = readSchema()
  assert.deepEqual(schema.$defs.expectation.oneOf[1].properties.match.enum, MATCH_MODES)
})

test('schema agreement: scene bounds and version const match the validator', () => {
  const schema = readSchema()
  assert.equal(schema.properties.scenes.maxItems, SCENARIO_MAX_SCENES)
  assert.equal(schema.properties.version.const, 1)
})

// ── BOS-359: the committed rich 4-scene account-flow scenario ─────────────────
// The first committed scenario to exercise the schema's maxItems: 4 ceiling with
// account flows over the raised 6-min/17-min ladder. loadScenario throws on any
// shape error, so this both validates it against the schema and pins its scene
// count at the ceiling — a future edit that drops it below 4 (or breaks its
// shape) fails CI without a live key.
test('BOS-359 committed 4-scene account-flow scenario validates and has exactly 4 scenes', () => {
  const scenarioPath = fileURLToPath(
    new URL('../proof/scenarios/bos359-account-flows-4scene.scenario.json', import.meta.url),
  )
  const { scenario, scenes } = loadScenario(scenarioPath)
  assert.equal(scenario.scenes.length, 4, 'must exercise the maxItems: 4 ceiling')
  assert.equal(scenario.scenes.length, SCENARIO_MAX_SCENES)
  assert.equal(scenes.length, 4, 'derived scenes match the authored count')
})

// guard-id: skill-config-validator-standalone

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { copyFileSync, existsSync, mkdtempSync, readFileSync, realpathSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { DEFAULT_CONFIG, validateConfig } from './skill-config.mjs'
import { SECTION_ID } from './verify/verify-proof-sections.mjs'

const GENERATED = join(
  dirname(fileURLToPath(import.meta.url)),
  'skill-config-validate.generated.mjs',
)

test('publish adapters accept access-put without bucket and retain daemon requirements @sessions.AC256204', () => {
  const check = (proof) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), publishConfig: { proof } },
        'fixture.json',
      ),
    )
  const baseUrl = 'https://proof.example'
  for (const proof of [
    { kind: 'access-put', baseUrl },
    { kind: 'access-put', baseUrl, keychainService: 'my-proof-host', extra: true },
    { kind: 'daemon', baseUrl, bucket: 'proof' },
    { baseUrl, bucket: 'proof' },
  ]) {
    const result = check(proof)
    assert.equal(result.error, undefined)
    assert.deepEqual(result.warnings, [])
  }
  for (const proof of [
    { kind: 'access-put' },
    { kind: 'access-put', baseUrl, keychainService: '' },
    { kind: 'daemon', baseUrl },
    { baseUrl },
    { kind: 'unknown', baseUrl, bucket: 'proof' },
  ])
    assert.match(check(proof).error?.message ?? '', /publishConfig/)
})

test('publishConfig.proof accepts false, refuses other non-objects and keeps object messages @sessions.AC261106', () => {
  const check = (proof, adapter = 'proof') =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), publishConfig: { [adapter]: proof } },
        'fixture.json',
      ),
    )
  const accepted = check(false)
  assert.equal(accepted.error, undefined)
  assert.deepEqual(accepted.warnings, [])
  const message = (proof, adapter) =>
    String(check(proof, adapter).error?.message).replace(
      /^skill-config: invalid config from fixture\.json: /,
      '',
    )
  for (const proof of [true, null, 'off', 1])
    assert.equal(message(proof), 'publishConfig.proof must be an object or false', String(proof))
  const baseUrl = 'https://proof.example'
  assert.equal(
    message({ kind: 'access-put' }),
    'publishConfig.proof.baseUrl must be a non-empty string',
  )
  assert.equal(
    message({ kind: 'daemon', baseUrl }),
    'publishConfig.proof.bucket must be a non-empty string',
  )
  assert.equal(
    message({ kind: 'unknown', baseUrl, bucket: 'proof' }),
    'publishConfig.proof.kind must be "daemon" or "access-put"',
  )
  // Only the proof block may be false.
  assert.equal(message(false, 'store'), 'publishConfig.store must be an object')
})

test('publishConfig.proof.hostedFallback takes a boolean @sessions.AC278701', () => {
  const baseUrl = 'https://proof.example'
  const check = (hostedFallback) =>
    capture(() =>
      validateConfig(
        {
          ...structuredClone(DEFAULT_CONFIG),
          publishConfig: { proof: { kind: 'access-put', baseUrl, hostedFallback } },
        },
        'fixture.json',
      ),
    )
  for (const value of [false, true]) {
    const accepted = check(value)
    assert.equal(accepted.error, undefined, String(value))
    assert.deepEqual(accepted.warnings, [])
  }
  for (const value of ['false', 0, null, {}])
    assert.equal(
      String(check(value).error?.message).replace(
        /^skill-config: invalid config from fixture\.json: /,
        '',
      ),
      'publishConfig.proof.hostedFallback must be a boolean',
      JSON.stringify(value),
    )
})

test('a proof block holding only hostedFallback needs no publisher fields @sessions.AC278701', () => {
  const message = (proof) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), publishConfig: { proof } },
        'fixture.json',
      ),
    ).error?.message?.replace(/^skill-config: invalid config from fixture\.json: /, '')
  for (const hostedFallback of [false, true])
    assert.equal(message({ hostedFallback }), undefined, String(hostedFallback))
  const baseUrl = 'https://proof.example'
  assert.equal(message({}), 'publishConfig.proof.baseUrl must be a non-empty string')
  assert.equal(
    message({ hostedFallback: false, bucket: 'proof' }),
    'publishConfig.proof.baseUrl must be a non-empty string',
  )
  assert.equal(
    message({ hostedFallback: false, baseUrl }),
    'publishConfig.proof.bucket must be a non-empty string',
  )
  assert.equal(message({ hostedFallback: false, kind: 'access-put', baseUrl }), undefined)
})
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

test('proofDefaults warns on unknown keys and rejects malformed values', () => {
  const valid = {
    pages: 'all',
    builtins: { 'boss-verify-protos': false },
    sections: { infra: true },
    dbt: { piiTerms: ['email'] },
    media: {
      sources: [{ suite: 'ui', workflow: 'ui.yml', job: 'proof', artifact: 'media-{head}' }],
    },
    fallback: 'none',
  }
  const check = (proofDefaults, options) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), proofDefaults },
        'fixture.json',
        options,
      ),
    )
  assert.deepEqual(check(valid).warnings, [])
  assert.equal(check(valid).error, undefined)
  const unknown = check({
    mystery: true,
    builtins: { 'boss-plan-notes': false },
    dbt: { extra: true },
    sections: { Bad_Id: true },
    media: { extra: true },
  })
  assert.equal(unknown.error, undefined)
  for (const key of [
    'mystery',
    'builtins.boss-plan-notes',
    'dbt.extra',
    'sections.Bad_Id',
    'media.extra',
  ]) {
    assert.ok(
      unknown.warnings.some((warning) =>
        warning.includes(`proofDefaults.${key} is not a known key; ignored`),
      ),
    )
  }
  for (const value of [
    null,
    [],
    false,
    { pages: 'some' },
    { fallback: true },
    { builtins: { 'boss-verify-protos': 'no' } },
    { builtins: [] },
    { sections: { infra: 1 } },
    { dbt: { piiTerms: [' '] } },
    { dbt: { piiTerms: 'email' } },
    { media: { sources: [false] } },
  ]) {
    assert.match(check(value).error?.message ?? '', /proofDefaults/)
  }
})

test('proofDefaults patterned values report the failing key in errors and deferred warnings', () => {
  for (const [block, key] of [
    ['builtins', 'boss-verify-protos'],
    ['sections', 'infra'],
  ]) {
    const config = {
      ...structuredClone(DEFAULT_CONFIG),
      proofDefaults: { [block]: { [key]: 'no' } },
    }
    const message = `proofDefaults.${block}.${key} must be true or false`
    const strict = capture(() => validateConfig(config, 'fixture.json'))
    assert.equal(
      strict.error?.message,
      `skill-config: invalid config from fixture.json: ${message}`,
    )
    const deferred = capture(() =>
      validateConfig(config, 'fixture.json', { deferStageBlocks: true }),
    )
    assert.equal(deferred.error, undefined)
    assert.deepEqual(deferred.warnings, [
      `skill-config: deferred proofDefaults: skill-config: invalid config from fixture.json: ${message}; run boss skills install`,
    ])
  }
})

test('proofDefaults faults defer as a stage block', () => {
  const result = capture(() =>
    validateConfig(
      { ...structuredClone(DEFAULT_CONFIG), proofDefaults: { pages: 'some' } },
      'fixture.json',
      { deferStageBlocks: true },
    ),
  )
  assert.equal(result.error, undefined)
  assert.ok(result.warnings.some((warning) => warning.includes('deferred proofDefaults:')))
})

test('proofDefaults.sections accepts SECTION_ID-shaped ids and warns on others', () => {
  for (const id of ['aa', 'a0', 'a-', 'a'.repeat(24), 'a', 'A0', 'Bad_Id', 'a'.repeat(25)]) {
    const result = capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), proofDefaults: { sections: { [id]: true } } },
        'fixture.json',
      ),
    )
    assert.equal(result.error, undefined)
    assert.equal(result.warnings.length, SECTION_ID.test(id) ? 0 : 1, id)
  }
})

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

for (const verifyFastPath of [true, false]) {
  test(`buildDefaults.verifyFastPath accepts boolean ${verifyFastPath}`, () => {
    const config = { ...structuredClone(DEFAULT_CONFIG), buildDefaults: { verifyFastPath } }
    const result = capture(() => validateConfig(config, 'fixture.json'))
    assert.equal(result.error, undefined)
    assert.deepEqual(result.warnings, [])
  })
}
test('buildDefaults rejects non-boolean values and warns on unknown keys', () => {
  assert.throws(
    () =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), buildDefaults: { verifyFastPath: 'true' } },
        'fixture.json',
      ),
    /buildDefaults.verifyFastPath must be a boolean when present/,
  )
  const result = capture(() =>
    validateConfig(
      { ...structuredClone(DEFAULT_CONFIG), buildDefaults: { foo: true } },
      'fixture.json',
    ),
  )
  assert.equal(result.error, undefined)
  assert.deepEqual(result.warnings, [
    'skill-config: fixture.json: buildDefaults.foo is not a known key; ignored',
  ])
})

test('the acceptance block is a known key whose derived checks must name a file placeholder', () => {
  const config = structuredClone(DEFAULT_CONFIG)
  config.acceptance = {
    derivedChecks: [{ glob: '**/*.test.mjs', command: 'node --test {file}' }],
    fullGateCommands: ['make lint', 'make test'],
  }
  assert.deepEqual(capture(() => validateConfig(config, 'fixture.json')).warnings, [])
  config.acceptance.derivedChecks[0].command = 'node --test'
  assert.match(
    capture(() => validateConfig(config, 'fixture.json')).error?.message ?? '',
    /acceptance\.derivedChecks/,
  )
})

test('fullGateCommands accept directory-scoped entries and reject malformed ones', () => {
  const check = (fullGateCommands) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), acceptance: { fullGateCommands } },
        'fixture.json',
      ),
    )
  assert.equal(check(['make test', { dir: 'apps/*', command: 'make test' }]).error, undefined)
  for (const bad of [
    [{ dir: 'apps/web' }],
    [{ dir: '', command: 'make test' }],
    [{ command: 'x', cwd: 'y', dir: 'z' }],
  ])
    assert.match(check(bad).error?.message ?? '', /acceptance\.fullGateCommands/)
})

test('verifyDefaults.ci accepts whenAbsent and absentGraceMinutes, warns on unknown keys and rejects bad values', () => {
  const check = (ci) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), verifyDefaults: { ci } },
        'fixture.json',
      ),
    )
  const good = check({ whenAbsent: 'wait', absentGraceMinutes: 0 })
  assert.equal(good.error, undefined)
  assert.deepEqual(good.warnings, [])
  assert.equal(check({ later: true }).error, undefined)
  assert.match(check({ later: true }).warnings.join('\n'), /verifyDefaults\.ci\.later/)
  for (const bad of [
    { whenAbsent: 'merge' },
    { absentGraceMinutes: -1 },
    { absentGraceMinutes: 1.5 },
  ])
    assert.match(check(bad).error?.message ?? '', /verifyDefaults\.ci\./)
})

test('proof media sources warn on unknown keys and reject malformed fields and overflow', () => {
  const source = { suite: 'ui', workflow: 'ui.yaml', job: 'proof', artifact: 'media-{head}' }
  const check = (sources) =>
    capture(() =>
      validateConfig(
        { ...structuredClone(DEFAULT_CONFIG), proofDefaults: { media: { sources } } },
        'fixture.json',
      ),
    )
  assert.deepEqual(check([source]).warnings, [])
  assert.equal(check([source]).error, undefined)
  const unknown = check([{ ...source, typo: true }])
  assert.equal(unknown.error, undefined)
  assert.ok(unknown.warnings.some((w) => w.includes('typo')))
  for (const field of ['suite', 'workflow', 'job', 'artifact']) {
    assert.ok(check([{ ...source, [field]: 2 }]).error, field)
    const missing = { ...source }
    delete missing[field]
    assert.ok(check([missing]).error, field)
  }
  for (const bad of [
    { suite: '../ui' },
    { suite: 'u'.repeat(25) },
    { workflow: 'ui.txt' },
    { workflow: '../ui.yml' },
    { job: ' ' },
    { artifact: 'proof' },
  ])
    assert.ok(check([{ ...source, ...bad }]).error, JSON.stringify(bad))
  assert.ok(check(Array.from({ length: 9 }, () => source)).error)
  assert.equal(check(Array.from({ length: 8 }, () => source)).error, undefined)
})

import { tempDir } from '../scripts/test-tmpdir.mjs'
import assert from 'node:assert/strict'
import {
  existsSync,
  readdirSync,
  readFileSync,
  realpathSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { isMainModule } from './main-module.mjs'

function fixture() {
  const dir = realpathSync(tempDir('main-module-'))
  const script = path.join(dir, 'script.mjs')
  const link = path.join(dir, 'script-link.mjs')
  writeFileSync(script, '')
  symlinkSync(script, link)
  return { script, link, moduleUrl: pathToFileURL(script).href }
}

test('isMainModule accepts direct, symlinked, and relative entry paths', () => {
  const { script, link, moduleUrl } = fixture()
  assert.equal(isMainModule(moduleUrl, { argvPath: script }), true)
  assert.equal(isMainModule(moduleUrl, { argvPath: link }), true)
  assert.equal(isMainModule(moduleUrl, { argvPath: path.relative(process.cwd(), script) }), true)
})

test('isMainModule rejects absent and non-string argv paths without resolving them', () => {
  const { moduleUrl } = fixture()
  let calls = 0
  const failIfCalled = () => {
    calls += 1
    throw new Error('realpathSync should not be called')
  }
  assert.equal(isMainModule(moduleUrl, { argvPath: undefined, realpathSync: failIfCalled }), false)
  assert.equal(isMainModule(moduleUrl, { argvPath: 42, realpathSync: failIfCalled }), false)
  assert.equal(calls, 0)
})

test('isMainModule falls back to resolved paths if realpath fails', () => {
  const { script, moduleUrl } = fixture()
  assert.equal(
    isMainModule(moduleUrl, {
      argvPath: script,
      realpathSync: () => {
        throw new Error('injected realpath failure')
      },
    }),
    true,
  )
})

test('isMainModule stays quiet for a different file and warns for a same-basename near miss', () => {
  const { script, moduleUrl } = fixture()
  const other = path.join(path.dirname(script), 'other.mjs')
  const sameBasename = path.join(realpathSync(tempDir('main-module-other-')), 'script.mjs')
  writeFileSync(other, '')
  writeFileSync(sameBasename, '')
  const warnings = []

  assert.equal(
    isMainModule(moduleUrl, { argvPath: other, warn: (message) => warnings.push(message) }),
    false,
  )
  assert.deepEqual(warnings, [])
  assert.equal(
    isMainModule(moduleUrl, { argvPath: sameBasename, warn: (message) => warnings.push(message) }),
    false,
  )
  assert.equal(warnings.length, 1)
  assert.match(warnings[0], /main module/i)
})

// guard-id: main-module-sweep

const FRAGILE_FORMS = [
  'import.meta.url === `file://${process.argv[1]}`',
  'path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)',
  'fileURLToPath(import.meta.url) === path.resolve(process.argv[1])',
  'process.argv[1] === fileURLToPath(import.meta.url)',
  'import.meta.url === pathToFileURL(process.argv[1]).href',
]

function sourceFiles(root) {
  return readdirSync(root, { withFileTypes: true }).flatMap((entry) => {
    const file = path.join(root, entry.name)
    if (entry.isDirectory()) return sourceFiles(file)
    return entry.name.endsWith('.mjs') && !entry.name.endsWith('.test.mjs') ? [file] : []
  })
}

test('main-module sweep rejects every fragile guard form and resolves every helper import', () => {
  for (const form of FRAGILE_FORMS) {
    assert.match(
      form,
      /process\.argv\[1\].*import\.meta\.url|import\.meta\.url.*process\.argv\[1\]/,
    )
  }

  const repoRoot = fileURLToPath(new URL('..', import.meta.url))
  const files = [
    ...sourceFiles(path.join(repoRoot, 'scripts')),
    ...sourceFiles(path.join(repoRoot, 'skills-toolbox')),
  ].filter((file) => path.basename(file) !== 'main-module.mjs')
  const sources = files.map((file) => ({ file, source: readFileSync(file, 'utf8') }))
  const calls = sources.filter(({ source }) => source.includes('isMainModule(import.meta.url)'))

  assert.ok(files.length >= 50, `visited only ${files.length} source files`)
  assert.ok(calls.length >= 50, `matched only ${calls.length} main-module guards`)
  assert.equal(
    sources.filter(({ source }) => /(?:function|const)\s+isInvokedDirectly\b/.test(source)).length,
    0,
    'the predicate must have one shared definition',
  )

  assert.deepEqual(sources.flatMap(fragileGuardFindings), [])
})

const REMEDY =
  'guard the entry point with `isMainModule(import.meta.url)` imported from skills-toolbox/main-module.mjs; hand-rolled argv[1] comparisons fail through a symlinked entry path'

// The sweep's per-file predicate: every way `file` hand-rolls or mis-imports its entry guard.
function fragileGuardFindings({ file, source }) {
  const findings = []
  for (const form of FRAGILE_FORMS) {
    if (source.includes(form)) findings.push(`${file} contains \`${form}\` — ${REMEDY}`)
  }
  if (
    source.includes('process.argv[1]') &&
    source.includes('import.meta.url') &&
    !/from ['"].*main-module\.mjs['"]/.test(source)
  ) {
    findings.push(`${file} compares process.argv[1] with import.meta.url — ${REMEDY}`)
  }
  for (const match of source.matchAll(/from ['"]([^'"]*main-module\.mjs)['"]/g)) {
    if (!existsSync(path.resolve(path.dirname(file), match[1]))) {
      findings.push(`${file} imports ${match[1]}, which does not resolve`)
    }
  }
  return findings
}

test('the main-module sweep reports a seeded hand-rolled argv guard', () => {
  const dir = realpathSync(tempDir('main-module-sweep-'))
  const file = path.join(dir, 'entry.mjs')
  const source = `if (process.argv[1] === fileURLToPath(import.meta.url)) main()\n`
  writeFileSync(file, source)
  assert.deepEqual(fragileGuardFindings({ file, source }), [
    `${file} contains \`process.argv[1] === fileURLToPath(import.meta.url)\` — ${REMEDY}`,
    `${file} compares process.argv[1] with import.meta.url — ${REMEDY}`,
  ])
  const unresolved = `import { isMainModule } from './missing/main-module.mjs'\n`
  assert.deepEqual(fragileGuardFindings({ file, source: unresolved }), [
    `${file} imports ./missing/main-module.mjs, which does not resolve`,
  ])
})

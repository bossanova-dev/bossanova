import assert from 'node:assert/strict'
import {
  existsSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  realpathSync,
  symlinkSync,
  writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath, pathToFileURL } from 'node:url'

import { isMainModule } from './main-module.mjs'

function fixture() {
  const dir = mkdtempSync(path.join(realpathSync(tmpdir()), 'main-module-'))
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
  const sameBasename = path.join(path.dirname(path.dirname(script)), 'script.mjs')
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

  for (const { file, source } of sources) {
    for (const form of FRAGILE_FORMS) {
      assert.ok(!source.includes(form), `${file} still contains ${form}`)
    }
    if (source.includes('process.argv[1]') && source.includes('import.meta.url')) {
      assert.match(source, /from ['"].*main-module\.mjs['"]/)
    }
    for (const match of source.matchAll(/from ['"]([^'"]*main-module\.mjs)['"]/g)) {
      assert.ok(existsSync(path.resolve(path.dirname(file), match[1])), `${file} import resolves`)
    }
  }
})

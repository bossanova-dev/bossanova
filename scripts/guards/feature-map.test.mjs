import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'
import { BEGIN_MARKER, END_MARKER, BASELINE, replaceIndexRegion } from './feature-map.mjs'
import { formatBaseline } from './baseline.mjs'

const cli = fileURLToPath(new URL('./feature-map.mjs', import.meta.url))
const readme = `---
uiRoots: []
userFacingRoots: [services/boss/api/**]
ignore: ['**/*_test.go']
---

# Features

${BEGIN_MARKER}

${END_MARKER}

Verify conventions live here.
`

function fixture(t) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'feature-map-'))
  t.after(() => fs.rmSync(root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const git = (...args) => {
    const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stdout + result.stderr)
  }
  git('init', '-q')
  git('config', 'commit.gpgsign', 'false')
  const put = (file, text = '') => {
    fs.mkdirSync(path.dirname(path.join(root, file)), { recursive: true })
    fs.writeFileSync(path.join(root, file), text)
    git('add', '--', file)
  }
  const feature = (file = 'sample.md', fields = '', id = 'sample') =>
    put(
      `docs/features/${file}`,
      `---\nid: ${id}\ntitle: Sample\nowns: [services/boss/api/cli/owned/**]\nsurfaces: [cli]\n${fields}---\n\n- AC1: Can run a command.\n`,
    )
  put('docs/features/README.md', readme)
  put(BASELINE, '[]\n')
  feature()
  const run = (verb, mode = '--check') =>
    spawnSync(process.execPath, [cli, verb, mode, '--root', root], { encoding: 'utf8' })
  const pass = (result) => assert.equal(result.status, 0, result.stdout + result.stderr)
  return { root, put, feature, run, pass }
}

test('feature ownership rejects an unowned file', (t) => {
  const f = fixture(t)
  f.pass(f.run('ownership'))
  const file = 'services/boss/api/cli/zz/zz.go'
  f.put(file, 'package zz\n')
  const result = f.run('ownership')
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes(file))
  assert.ok(result.stderr.includes('docs/features/'))
  assert.ok(result.stderr.includes('owns:'))
})

test('feature ownership rejects a file shared by distinct features outside user-facing roots', (t) => {
  const f = fixture(t)
  const file = 'services/bossd/internal/shared/worker.go'
  f.put(file, 'package shared\n')
  f.feature('sample.md', 'owns: [services/bossd/internal/shared/**]\n')
  f.feature('second.md', 'owns: [services/bossd/internal/shared/**]\n', 'second')
  const result = f.run('ownership')
  assert.equal(result.status, 1)
  assert.ok(
    result.stderr.includes(`${file} is owned by sample and second: keep it in one feature's owns:`),
  )
})

test('feature ownership accepts multiple matching globs in one feature and ignores shared test files', (t) => {
  const f = fixture(t)
  f.put('services/boss/api/cli/owned/command.go', 'package owned\n')
  f.put('services/boss/api/cli/owned/command_test.go', 'package owned\n')
  f.feature(
    'sample.md',
    'owns: [services/boss/api/cli/owned/**, services/boss/api/cli/owned/*.go]\n',
  )
  f.feature('second.md', 'owns: [services/boss/api/cli/owned/*_test.go]\n', 'second')
  f.pass(f.run('ownership'))
})

test('feature ownership rejects a new unowned feature folder outside user-facing roots', (t) => {
  const f = fixture(t)
  f.put('services/bossd/internal/newfeature/worker.go', 'package newfeature\n')
  const result = f.run('ownership')
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('services/bossd/internal/newfeature/'))
  assert.ok(!result.stderr.includes('worker.go'))
})

test('feature ownership accepts owned and ignored files and excludes infrastructure folders', (t) => {
  const f = fixture(t)
  f.put('services/boss/api/cli/owned/command.go', 'package owned\n')
  f.put('services/boss/api/cli/ignored/command_test.go', 'package ignored\n')
  for (const name of [
    'db',
    'dbtest',
    'domain',
    'adapter',
    'testharness',
    'fixtures',
    'tuidriver',
    'realstack',
    'plugintest',
  ]) {
    f.put(`services/bossd/internal/${name}/worker.go`, `package ${name}\n`)
  }
  f.pass(f.run('ownership'))
})

test('feature ownership update writes sorted shared-format file and folder entries and rejects stale entries', (t) => {
  const f = fixture(t)
  f.put('services/boss/api/cli/zz/zz.go')
  f.put('services/boss/api/cli/aa/aa.go')
  f.pass(f.run('ownership', '--update'))
  const expected = [
    'services/boss/api/cli/aa/',
    'services/boss/api/cli/aa/aa.go',
    'services/boss/api/cli/zz/',
    'services/boss/api/cli/zz/zz.go',
  ]
  assert.equal(fs.readFileSync(path.join(f.root, BASELINE), 'utf8'), formatBaseline(expected))
  f.pass(f.run('ownership'))
  f.put(BASELINE, formatBaseline([...expected, 'services/retired.go']))
  const result = f.run('ownership')
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('services/retired.go'))
})

test('feature ownership fails when a feature is dropped by the shared loader', (t) => {
  const f = fixture(t)
  for (const [name, text] of [
    ['missing-id', 'owns: [services/**]'],
    ['bad-id', 'id: Invalid\nowns: [services/**]'],
    ['missing-owns', 'id: missing-owns'],
    ['duplicate-id', 'id: sample\nowns: [services/**]'],
  ]) {
    const file = `docs/features/zz-${name}.md`
    f.put(file, `---\n${text}\n---\n`)
    const result = f.run('ownership', '--update')
    assert.equal(result.status, 1)
    assert.ok(result.stderr.includes(file))
    fs.unlinkSync(path.join(f.root, file))
  }
})

test('feature guides reject unresolved paths', (t) => {
  const f = fixture(t)
  f.feature('sample.md', 'guide: services/docs/docs/guides/missing.md\n')
  const result = f.run('guides')
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/sample.md'))
  assert.ok(result.stderr.includes('services/docs/docs/guides/missing.md'))
})

test('feature guides reject tracked paths outside guides and untracked files', (t) => {
  const f = fixture(t)
  f.put('docs/outside.md')
  for (const guide of ['docs/outside.md', 'services/docs/docs/guides/../outside.md']) {
    f.feature('sample.md', `guide: ${guide}\n`)
    assert.equal(f.run('guides').status, 1)
  }
  f.put('services/docs/docs/guides/untracked.md')
  spawnSync('git', ['rm', '--cached', 'services/docs/docs/guides/untracked.md'], { cwd: f.root })
  f.feature('sample.md', 'guide: services/docs/docs/guides/untracked.md\n')
  assert.equal(f.run('guides').status, 1)
})

test('feature guides accept scalar and list forms and require every list entry to resolve', (t) => {
  const f = fixture(t)
  const a = 'services/docs/docs/guides/a.md'
  const b = 'services/docs/docs/guides/b.md'
  f.put(a)
  f.put(b)
  f.feature('sample.md', `guide: ${a}\n`)
  f.pass(f.run('guides'))
  f.feature('sample.md', `guide:\n  - ${a}\n  - ${b}\n`)
  f.pass(f.run('guides'))
  fs.unlinkSync(path.join(f.root, b))
  assert.equal(f.run('guides').status, 1)
})

test('feature index rejects a hand edit', (t) => {
  const f = fixture(t)
  f.pass(f.run('index', '--write'))
  f.pass(f.run('index'))
  const file = path.join(f.root, 'docs/features/README.md')
  fs.writeFileSync(file, fs.readFileSync(file, 'utf8').replace('[Sample]', '[Hand edit]'))
  const result = f.run('index')
  assert.equal(result.status, 1)
  assert.ok(result.stderr.includes('docs/features/README.md'))
})

test('feature index round-trips metadata for every feature, sorted by id, and preserves conventions', (t) => {
  const f = fixture(t)
  f.feature(
    'aaa-file.md',
    'summary: Last feature.\nguide: services/docs/docs/guides/z.md\n',
    'z-last',
  )
  f.pass(f.run('index', '--write'))
  const file = path.join(f.root, 'docs/features/README.md')
  const before = fs.readFileSync(file, 'utf8')
  f.pass(f.run('index'))
  f.pass(f.run('index', '--write'))
  assert.equal(fs.readFileSync(file, 'utf8'), before)
  assert.ok(before.indexOf('(sample.md)') < before.indexOf('(aaa-file.md)'))
  assert.ok(before.includes('Last feature.'))
  assert.ok(before.includes('../../services/docs/docs/guides/z.md'))
  assert.ok(before.includes('Verify conventions live here.'))
  assert.ok(before.includes('cli; 1 criteria'))
})

test('feature index rejects missing, duplicated, and misordered markers', (t) => {
  const f = fixture(t)
  for (const text of [
    readme.replace(BEGIN_MARKER, ''),
    readme.replace(END_MARKER, ''),
    readme + BEGIN_MARKER,
    `${END_MARKER}\n${BEGIN_MARKER}\n`,
  ]) {
    assert.throws(() => replaceIndexRegion(text, 'body\n'))
  }
  f.put('docs/features/README.md', readme.replace(END_MARKER, ''))
  assert.equal(f.run('index', '--write').status, 1)
})

test('feature map CLI rejects unknown flags and incompatible modes', (t) => {
  const f = fixture(t)
  for (const args of [
    [],
    ['ownership'],
    ['guides', '--update'],
    ['ownership', '--write'],
    ['index', '--update'],
    ['index', '--check', '--write'],
    ['guides', '--check', '--unknown'],
    ['guides', '--check', 'extra'],
  ]) {
    const result = spawnSync(process.execPath, [cli, ...args, '--root', f.root], {
      encoding: 'utf8',
    })
    assert.notEqual(result.status, 0)
  }
})

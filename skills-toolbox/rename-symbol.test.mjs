import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { main } from './rename-symbol.mjs'

/** A throwaway git repository whose files are written untracked unless a test commits them. */
function repo(t, files) {
  const root = mkdtempSync(path.join(os.tmpdir(), 'rename-symbol-test-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  const init = spawnSync('git', ['init', '-q'], { cwd: root, encoding: 'utf8' })
  assert.equal(init.status, 0, init.stderr)
  for (const [name, content] of Object.entries(files)) {
    mkdirSync(path.dirname(path.join(root, name)), { recursive: true })
    writeFileSync(path.join(root, name), content)
  }
  return root
}

function read(root, name) {
  return readFileSync(path.join(root, name), 'utf8')
}

function snapshot(root, names) {
  return Object.fromEntries(names.map((name) => [name, read(root, name)]))
}

function run(root, argv) {
  const lines = []
  const errors = []
  const status = main(argv, {
    cwd: root,
    write: (line) => lines.push(JSON.parse(line)),
    writeError: (text) => errors.push(text),
  })
  return { status, report: lines[0], lines, errors }
}

test('renames whole identifiers only, leaving substrings and case variants', (t) => {
  const source = 'OldName(1)\nx.OldName\nOldNameSuffix\nPrefixOldName\noldName\n'
  const root = repo(t, { 'a.go': source })
  const { status, report } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 0)
  assert.equal(read(root, 'a.go'), 'NewName(1)\nx.NewName\nOldNameSuffix\nPrefixOldName\noldName\n')
  assert.deepEqual(report, {
    from: 'OldName',
    to: 'NewName',
    dryRun: false,
    files: [{ path: 'a.go', replacements: 2 }],
    totalReplacements: 2,
  })
})

test('excluded pathspecs are neither rewritten nor reported', (t) => {
  const root = repo(t, { 'gen/a.go': 'OldName\n', 'src/a.go': 'OldName\n' })
  const { status, report } = run(root, [
    '--from',
    'OldName',
    '--to',
    'NewName',
    '--path',
    '.',
    '--exclude',
    'gen',
  ])
  assert.equal(status, 0)
  assert.equal(read(root, 'gen/a.go'), 'OldName\n')
  assert.equal(read(root, 'src/a.go'), 'NewName\n')
  assert.deepEqual(report.files, [{ path: 'src/a.go', replacements: 1 }])
})

test('dry run reports what a real run changes and writes nothing', (t) => {
  const files = { 'a.go': 'OldName OldName\n', 'b/c.go': 'use(OldName)\n', 'd.go': 'other\n' }
  const dry = repo(t, files)
  const real = repo(t, files)
  const before = snapshot(dry, Object.keys(files))
  const dryRun = run(dry, ['--from', 'OldName', '--to', 'NewName', '--dry-run'])
  const realRun = run(real, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(dryRun.status, 0)
  assert.equal(dryRun.report.dryRun, true)
  assert.deepEqual(dryRun.report.files, realRun.report.files)
  assert.equal(dryRun.report.totalReplacements, 3)
  assert.equal(dryRun.report.totalReplacements, realRun.report.totalReplacements)
  assert.deepEqual(snapshot(dry, Object.keys(files)), before)
  assert.notDeepEqual(snapshot(real, Object.keys(files)), before)
})

test('reports only changed files, by exact paths even with spaces or tabs', (t) => {
  const root = repo(t, {
    'with space/my file.go': 'OldName\n',
    'tab\tname.go': 'OldName\n',
    'substring.go': 'OldNameSuffix\n',
    'none.go': 'nothing here\n',
  })
  const { status, report } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 0)
  assert.equal(read(root, 'with space/my file.go'), 'NewName\n')
  assert.equal(read(root, 'substring.go'), 'OldNameSuffix\n')
  assert.equal(read(root, 'tab\tname.go'), 'NewName\n')
  assert.deepEqual(report.files, [
    { path: 'tab\tname.go', replacements: 1 },
    { path: 'with space/my file.go', replacements: 1 },
  ])
})

test('renames tracked and untracked files but not ignored ones', (t) => {
  const root = repo(t, {
    '.gitignore': 'ignored.go\n',
    'tracked.go': 'OldName\n',
    'ignored.go': 'OldName\n',
  })
  for (const args of [
    ['add', 'tracked.go', '.gitignore'],
    [
      '-c',
      'commit.gpgsign=false',
      '-c',
      'user.name=t',
      '-c',
      'user.email=t@example.com',
      'commit',
      '-qm',
      'seed',
    ],
  ]) {
    const result = spawnSync('git', args, { cwd: root, encoding: 'utf8' })
    assert.equal(result.status, 0, result.stderr)
  }
  writeFileSync(path.join(root, 'untracked.go'), 'OldName\n')
  const { status, report } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 0)
  assert.equal(read(root, 'tracked.go'), 'NewName\n')
  assert.equal(read(root, 'untracked.go'), 'NewName\n')
  assert.equal(read(root, 'ignored.go'), 'OldName\n')
  assert.deepEqual(
    report.files.map((file) => file.path),
    ['tracked.go', 'untracked.go'],
  )
})

test('usage errors exit 2 and write nothing', (t) => {
  const root = repo(t, { 'a.go': 'OldName X a\n' })
  for (const argv of [
    ['--from', 'OldName'],
    ['--from', 'a.b', '--to', 'NewName'],
    ['--from', 'X', '--to', 'X'],
    ['--from', 'OldName', '--to', 'NewName', '--fixed'],
    ['--from', 'OldName', '--to', 'NewName', 'positional'],
  ]) {
    const { status, lines, errors } = run(root, argv)
    assert.equal(status, 2, argv.join(' '))
    assert.deepEqual(lines, [])
    assert.equal(errors.length, 1)
    assert.equal(read(root, 'a.go'), 'OldName X a\n')
  }
})

test('no matches exits 0 with an empty file list', (t) => {
  const root = repo(t, { 'a.go': 'nothing\n' })
  const { status, report } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 0)
  assert.deepEqual(report.files, [])
  assert.equal(report.totalReplacements, 0)
})

test('outside a git repository it fails with exit 1 and writes nothing', (t) => {
  const root = mkdtempSync(path.join(os.tmpdir(), 'rename-symbol-nogit-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(path.join(root, 'a.go'), 'OldName\n')
  const { status, lines, errors } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 1)
  assert.deepEqual(lines, [])
  assert.ok(errors.join('').length > 0)
  assert.equal(read(root, 'a.go'), 'OldName\n')
})

test('never writes through a symlink to its target', (t) => {
  const outside = mkdtempSync(path.join(os.tmpdir(), 'rename-symbol-outside-'))
  t.after(() => rmSync(outside, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(path.join(outside, 'target.go'), 'OldName\n')
  const root = repo(t, { 'real.go': 'OldName\n' })
  symlinkSync(path.join(outside, 'target.go'), path.join(root, 'link.go'))
  const { status, report } = run(root, ['--from', 'OldName', '--to', 'NewName'])
  assert.equal(status, 0)
  assert.equal(readFileSync(path.join(outside, 'target.go'), 'utf8'), 'OldName\n')
  assert.deepEqual(report.files, [{ path: 'real.go', replacements: 1 }])
})

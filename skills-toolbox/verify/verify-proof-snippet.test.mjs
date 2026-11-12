import test from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import {
  cutContext,
  cutSnippet,
  gitChangedFiles,
  gitFileDiffs,
  gitMergeBase,
  lineChecker,
  SNIPPET_CONTEXT,
  snippetResolver,
  parseUnifiedDiff,
} from './verify-proof-snippet.mjs'

const git = (cwd, ...args) => {
  const r = spawnSync(
    'git',
    ['-c', 'user.name=t', '-c', 'user.email=t@t', '-c', 'commit.gpgsign=false', ...args],
    {
      cwd,
      encoding: 'utf8',
    },
  )
  assert.equal(r.status, 0, `git ${args.join(' ')}: ${r.stderr}`)
  return r.stdout.trim()
}
const lines = (n, edit = {}) =>
  `${Array.from({ length: n }, (_, i) => edit[i + 1] ?? `line ${i + 1}`).join('\n')}\n`

/** A repo whose head changes line 20 and inserts one line after line 30 of a 40-line file. */
function fixture() {
  const dir = mkdtempSync(join(tmpdir(), 'proof-snippet-'))
  git(dir, 'init', '-q')
  writeFileSync(join(dir, 'app.ts'), lines(40))
  writeFileSync(join(dir, 'same.ts'), lines(5))
  git(dir, 'add', '.')
  git(dir, 'commit', '-qm', 'base')
  const base = git(dir, 'rev-parse', 'HEAD')
  const body = lines(40, { 20: 'const total = 2' }).split('\n')
  body.splice(30, 0, 'inserted()')
  writeFileSync(join(dir, 'app.ts'), body.join('\n'))
  git(dir, 'commit', '-qam', 'head')
  return { dir, base, head: git(dir, 'rev-parse', 'HEAD') }
}

test('a snippet quotes git rows with old and new line numbers, never spec text', () => {
  const { dir, base, head } = fixture()
  try {
    const resolve = snippetResolver(gitFileDiffs({ base, head, cwd: dir }))
    const { snippet, problems } = resolve({ path: 'app.ts', start: 18, end: 22, pinLine: 20 })
    assert.deepEqual(problems, [])
    assert.deepEqual(snippet.rows, [
      { kind: 'ctx', old: 18, new: 18, text: 'line 18' },
      { kind: 'ctx', old: 19, new: 19, text: 'line 19' },
      { kind: 'del', old: 20, new: null, text: 'line 20' },
      { kind: 'add', old: null, new: 20, text: 'const total = 2' },
      { kind: 'ctx', old: 21, new: 21, text: 'line 21' },
      { kind: 'ctx', old: 22, new: 22, text: 'line 22' },
    ])
    assert.equal(snippet.pinLine, 20)
    assert.deepEqual([snippet.adds, snippet.dels], [2, 1])
    const shifted = resolve({ path: 'app.ts', start: 30, end: 33 }).snippet.rows
    assert.deepEqual(
      shifted.map((r) => [r.kind, r.old, r.new]),
      [
        ['ctx', 30, 30],
        ['add', null, 31],
        ['ctx', 31, 32],
        ['ctx', 32, 33],
      ],
    )
    assert.deepEqual(gitChangedFiles({ base, head, cwd: dir }), [
      { path: 'app.ts', adds: 2, dels: 1 },
    ])
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('a snippet outside the diff, on an unchanged file or with no changed line is dropped', () => {
  const { dir, base, head } = fixture()
  try {
    const resolve = snippetResolver(gitFileDiffs({ base, head, cwd: dir }))
    for (const [request, problem] of [
      [{ path: 'app.ts', start: 38, end: 44 }, /line 42 is outside the diff, snippet dropped/],
      [{ path: 'same.ts', start: 1, end: 4 }, /same\.ts is not in the diff, snippet dropped/],
      [{ path: 'app.ts', start: 2, end: 8 }, /shows no changed line, snippet dropped/],
    ]) {
      const result = resolve(request)
      assert.equal(result.snippet, null)
      assert.equal(result.problems.length, 1)
      assert.match(result.problems[0], problem)
    }
    const broken = snippetResolver(gitFileDiffs({ base, head: 'f'.repeat(40), cwd: dir }))
    assert.equal(broken({ path: 'app.ts', start: 18, end: 22 }).snippet, null)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('files and snippets hold only the branch, after the base moves and merges into it', () => {
  const { dir, base: fork, head: branched } = fixture()
  try {
    git(dir, 'checkout', '-qb', 'trunk', fork)
    writeFileSync(join(dir, 'same.ts'), lines(5, { 2: 'main edit' }))
    git(dir, 'commit', '-qam', 'main moves')
    git(dir, 'checkout', '-qb', 'pr', branched)
    git(dir, 'merge', '-q', '--no-edit', 'trunk')
    const merged = git(dir, 'rev-parse', 'trunk')
    git(dir, 'checkout', '-q', 'trunk')
    writeFileSync(join(dir, 'main.ts'), lines(3))
    git(dir, 'add', 'main.ts')
    git(dir, 'commit', '-qm', 'main moves again')
    const range = {
      base: git(dir, 'rev-parse', 'trunk'),
      head: git(dir, 'rev-parse', 'pr'),
      cwd: dir,
    }
    assert.equal(gitMergeBase(range), merged)
    assert.deepEqual(gitChangedFiles(range), [{ path: 'app.ts', adds: 2, dels: 1 }])
    const diffs = gitFileDiffs(range)
    assert.equal(diffs('main.ts'), undefined)
    assert.equal(diffs('same.ts'), undefined)
    assert.equal(snippetResolver(diffs)({ path: 'app.ts', start: 18, end: 22 }).snippet.adds, 2)
    assert.equal(gitMergeBase({ ...range, head: 'f'.repeat(40) }), null)
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('a pin outside its range drops only the pin', () => {
  const diff = parseUnifiedDiff('--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n a\n-b\n+B\n c\n d\n')[0]
  const { snippet, problems } = cutSnippet(diff, { path: 'x', start: 1, end: 4, pinLine: 9 })
  assert.equal(snippet.pinLine, null)
  assert.equal(snippet.rows.length, 5)
  assert.deepEqual(problems, ['x:1-4: pin 9 is outside the snippet, pin dropped'])
})

test('unified diff text parses per file and plain text parses to no files', () => {
  const files = parseUnifiedDiff(
    'diff --git a/a.go b/a.go\nindex 1..2 100644\n--- a/a.go\n+++ b/a.go\n@@ -3,2 +3,2 @@\n-x\n+y\n z\n\\ No newline at end of file\ndiff --git a/b.go b/b.go\n--- /dev/null\n+++ b/b.go\n@@ -0,0 +1 @@\n+new\n',
  )
  assert.deepEqual(
    files.map((f) => [f.path, f.adds, f.dels, f.rows.length]),
    [
      ['a.go', 1, 1, 3],
      ['b.go', 1, 0, 1],
    ],
  )
  assert.deepEqual(files[0].rows[2], { kind: 'ctx', old: 4, new: 4, text: 'z' })
  assert.deepEqual(parseUnifiedDiff('first\nsecond'), [])
})

test('paths are literal and a blank context line stays inside its hunk, whatever git config says', () => {
  const dir = mkdtempSync(join(tmpdir(), 'proof-snippet-literal-'))
  try {
    git(dir, 'init', '-q')
    git(dir, 'config', 'diff.suppressBlankEmpty', 'true')
    mkdirSync(join(dir, 'app', '[A]'), { recursive: true })
    mkdirSync(join(dir, 'app', 'A'), { recursive: true })
    const blank = 'a\nb\n\nc\nd\n'
    writeFileSync(join(dir, 'app', 'A', 'p.ts'), blank)
    writeFileSync(join(dir, 'caf\u00e9.md'), 'x\n')
    git(dir, 'add', '.')
    git(dir, 'commit', '-qm', 'base')
    const base = git(dir, 'rev-parse', 'HEAD')
    writeFileSync(join(dir, 'app', 'A', 'p.ts'), 'a\nb\n\nc\nD\n')
    writeFileSync(join(dir, 'app', '[A]', 'p.ts'), 'new\n')
    writeFileSync(join(dir, 'caf\u00e9.md'), 'y\n')
    git(dir, 'add', '.')
    git(dir, 'commit', '-qm', 'head')
    const head = git(dir, 'rev-parse', 'HEAD')
    const resolve = snippetResolver(gitFileDiffs({ base, head, cwd: dir }))
    const glob = resolve({ path: 'app/[A]/p.ts', start: 1, end: 1 })
    assert.deepEqual(glob.snippet.rows, [{ kind: 'add', old: null, new: 1, text: 'new' }])
    const spanning = resolve({ path: 'app/A/p.ts', start: 2, end: 5 })
    assert.deepEqual(spanning.problems, [])
    assert.equal(spanning.snippet.rows[1].text, '')
    assert.deepEqual(resolve({ path: 'bad\u0000path', start: 1, end: 4 }).snippet, null)
    assert.deepEqual(
      gitChangedFiles({ base, head, cwd: dir })
        .map((f) => f.path)
        .sort(),
      ['app/A/p.ts', 'app/[A]/p.ts', 'caf\u00e9.md'],
    )
  } finally {
    rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
  }
})

test('a line is absent only when the diff shows the whole head file, else it is not checked', (t) => {
  const whole = parseUnifiedDiff(
    '--- a/w.ts\n+++ b/w.ts\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n',
  )[0]
  const late = parseUnifiedDiff('--- a/l.ts\n+++ b/l.ts\n@@ -100,3 +100,3 @@\n a\n-b\n+B\n c\n')[0]
  const tail = Array.from({ length: SNIPPET_CONTEXT }, (_, i) => ` ctx ${i}`).join('\n')
  const long = parseUnifiedDiff(`--- a/t.ts\n+++ b/t.ts\n@@ -1 +1 @@\n-x\n+y\n${tail}\n`)[0]
  const gone = parseUnifiedDiff('--- a/d.go\n+++ /dev/null\n@@ -1,2 +0,0 @@\n-a\n-b\n')[0]
  const headerOnly = { path: 'm.sh', rows: [], adds: 0, dels: 0 }
  const by = { 'w.ts': whole, 'l.ts': late, 't.ts': long, 'd.go': gone, 'm.sh': headerOnly }
  const has = lineChecker((path) => by[path])
  assert.equal(has('w.ts', 2), true)
  assert.equal(has('w.ts', 4), false, 'past the end of a file the diff shows whole')
  assert.equal(has('d.go', 2), true, 'a deleted file is checked against its base lines')
  assert.equal(has('d.go', 3), false)
  assert.equal(has('l.ts', 101), true)
  assert.equal(has('l.ts', 50), null, 'before a hunk that starts after line 1')
  assert.equal(
    has('t.ts', SNIPPET_CONTEXT + 5),
    null,
    'past a context window that may not reach the end',
  )
  assert.equal(has('m.sh', 1), null, 'a change with no diff rows')

  const { dir, base } = fixture()
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 3 }))
  spawnSync('chmod', ['+x', join(dir, 'same.ts')])
  writeFileSync(join(dir, 'blob.bin'), Buffer.from([0, 1, 2, 0]))
  git(dir, 'add', '.')
  git(dir, 'commit', '-qm', 'mode and binary')
  const real = lineChecker(gitFileDiffs({ base, head: git(dir, 'rev-parse', 'HEAD'), cwd: dir }))
  assert.equal(real('same.ts', 1), null, 'a mode-only change is not checked')
  assert.equal(real('blob.bin', 1), null, 'a binary file is not checked')
  assert.equal(real('app.ts', 41), true)
  assert.equal(real('app.ts', 42), false)
})

test('the context around a line keeps the base lines a change replaced, and is capped', () => {
  const removed = Array.from({ length: 20 }, (_, i) => `-gone ${i + 1}`).join('\n')
  const diff = parseUnifiedDiff(
    `--- a/x.go\n+++ b/x.go\n@@ -1,22 +1,3 @@\n keep 1\n${removed}\n+new 2\n keep 3\n`,
  )[0]
  const cut = cutContext(diff, 2)
  assert.deepEqual(cut.focus, { side: 'new', line: 2 })
  assert.equal(cut.rows.length, 12, 'a long deletion is cut to twelve rows')
  assert.ok(
    cut.rows.some((row) => row.kind === 'add' && row.new === 2),
    'the focus line stays',
  )
  assert.equal(cutContext(diff, 40), null, 'a line neither side has quotes nothing')
  assert.equal(cutContext(undefined, 1), null)
})

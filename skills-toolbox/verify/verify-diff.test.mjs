import assert from 'node:assert/strict'
import { test } from 'node:test'

import { readChangedFiles, runTool, scopeChangedFiles } from './verify-diff.mjs'
import { gitFixture } from './verify-git-fixture.mjs'

const body = (n) => Array.from({ length: n }, (_, i) => `line ${i}`).join('\n') + '\n'

test('readChangedFiles reports status, previous path, change counts and patch presence', (t) => {
  const repo = gitFixture(t)
  repo.write({
    'keep.txt': 'one\ntwo\n',
    'gone.txt': 'bye\n',
    'old/name.txt': body(20),
    'logo.bin': Buffer.from([0, 1, 2, 3, 0, 255]),
  })
  const base = repo.commit('base')
  repo.write({ 'keep.txt': 'one\nTWO\n', 'new.txt': 'hello\n', 'gone.txt': null })
  repo.move('old/name.txt', 'fresh/name.txt')
  repo.write({ 'fresh/name.txt': body(20) + 'extra\n', 'logo.bin': Buffer.from([9, 0, 8, 0, 7]) })
  const head = repo.commit('head')

  const files = Object.fromEntries(
    readChangedFiles({ baseSha: base, headSha: head, cwd: repo.dir }).map((f) => [f.path, f]),
  )
  assert.equal(files['keep.txt'].status, 'modified')
  assert.equal(files['keep.txt'].changes, 2)
  assert.match(files['keep.txt'].patch, /^-two$/m)
  assert.equal(files['new.txt'].status, 'added')
  assert.equal(files['new.txt'].changes, 1)
  assert.equal(files['gone.txt'].status, 'removed')
  assert.equal(files['gone.txt'].changes, 1)
  assert.equal(files['fresh/name.txt'].status, 'renamed')
  assert.equal(files['fresh/name.txt'].previousPath, 'old/name.txt')
  assert.equal(files['fresh/name.txt'].changes, 1)
  assert.equal(files['logo.bin'].status, 'modified')
  assert.equal(files['logo.bin'].patch, undefined)
  assert.equal(files['logo.bin'].changes, undefined)
  assert.equal(Object.keys(files).length, 5)
})

test('an identical rename has zero changes and an empty patch', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'a/x.ts': body(5) })
  const base = repo.commit()
  repo.move('a/x.ts', 'b/x.ts')
  const head = repo.commit()
  assert.deepEqual(readChangedFiles({ baseSha: base, headSha: head, cwd: repo.dir }), [
    { path: 'b/x.ts', previousPath: 'a/x.ts', status: 'renamed', changes: 0, patch: '' },
  ])
})

test('the diff runs from the merge base, so base-branch progress is not the PR', (t) => {
  const repo = gitFixture(t)
  repo.write({ 'a.txt': 'a\n' })
  repo.commit()
  repo.git('checkout', '--quiet', '-b', 'feature')
  repo.write({ 'pr.txt': 'pr\n' })
  const head = repo.commit()
  repo.git('checkout', '--quiet', 'main')
  repo.write({ 'main-only.txt': 'm\n' })
  const base = repo.commit()
  assert.deepEqual(
    readChangedFiles({ baseSha: base, headSha: head, cwd: repo.dir }).map((f) => f.path),
    ['pr.txt'],
  )
})

test('a failing git read throws instead of returning a partial list', () => {
  const run = () => ({ status: 128, stdout: '', stderr: 'bad revision' })
  assert.throws(() => readChangedFiles({ baseSha: 'a', headSha: 'b', run }))
})

test('runTool turns a thrown spawn into a failed result', () => {
  const result = runTool(
    () => {
      throw new Error('boom')
    },
    'git',
    [],
  )
  assert.equal(result.ok, false)
})

test('scopeChangedFiles keeps listed files and adds uncovered paths as plain strings', () => {
  const files = [
    { path: 'b/x.ts', previousPath: 'a/x.ts', status: 'renamed', changes: 0, patch: '' },
    { path: 'outside.ts', status: 'modified', changes: 1, patch: '@@ -1 +1 @@\n+y' },
  ]
  assert.deepEqual(scopeChangedFiles(files, ['a/x.ts', 'b/x.ts', 'later.ts']), [
    files[0],
    'later.ts',
  ])
  assert.deepEqual(scopeChangedFiles([], ['p']), ['p'])
})

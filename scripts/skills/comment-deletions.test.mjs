import assert from 'node:assert/strict'
import { execFileSync, spawnSync } from 'node:child_process'
import { existsSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test } from 'node:test'
import { tempDir } from '../test-infra/test-tmpdir.mjs'
import { commentSyntax, judgeFileEdit } from './comment-deletions.mjs'

const helper = new URL('./comment-deletions.mjs', import.meta.url).pathname

/** @param {string} removed @param {string} added */
const hunk = (removed, added = '') =>
  [
    '@@ -1 +1 @@',
    ...removed.split('\n').map((l) => `-${l}`),
    ...(added ? added.split('\n').map((l) => `+${l}`) : []),
  ].join('\n')

test('judgeFileEdit accepts only deletions of comments the branch added', () => {
  const go = commentSyntax('x.go')
  assert.ok(go)
  const branchAdded = new Set(['\t// walk the list', '\tn := 1 // one', '\t//nolint:errcheck'])
  const judge = (diff) => judgeFileEdit({ diff, branchAdded, syntax: go })
  assert.equal(judge(hunk('\t// walk the list')), null)
  assert.equal(judge(hunk('\tn := 1 // one', '\tn := 1')), null)
  assert.equal(judge(hunk('\tn := 1 // one', '\tn := 2')), 'changes code')
  assert.equal(judge(hunk('\t//nolint:errcheck')), 'removes code or a directive')
  assert.equal(judge(hunk('\t// older note')), 'removes a comment the branch did not add')
  assert.equal(judge(hunk('')), 'removes only blank lines')
  assert.equal(judge(''), 'removes no comment')
  const url = '\tu := "http://x" // site'
  assert.equal(
    judgeFileEdit({ diff: hunk(url, '\tu := "http:'), branchAdded: new Set([url]), syntax: go }),
    'changes code',
  )
  const quoted = '\tu := "a //b" // site'
  assert.equal(
    judgeFileEdit({ diff: hunk(quoted, '\tu := "a'), branchAdded: new Set([quoted]), syntax: go }),
    'changes code',
  )
  assert.equal(
    judge(hunk('\tm := 2 // two', '\tm := 2')),
    'removes a comment the branch did not add',
  )
  assert.equal(commentSyntax('notes.md'), undefined)
})

test('checkCommentDeletions keeps in-scope comment deletions and restores everything else', (t) => {
  const repo = tempDir('comment-deletions-', t)
  const git = (...args) => execFileSync('git', ['-C', repo, ...args], { encoding: 'utf8' })
  const write = (name, lines) => writeFileSync(join(repo, name), `${lines.join('\n')}\n`)
  const read = (name) => readFileSync(join(repo, name), 'utf8')
  git('init', '-q', '-b', 'main')
  git('config', 'commit.gpgsign', 'false')
  git('config', 'user.name', 'Fixture')
  git('config', 'user.email', 'fixture@example.test')
  write('app.go', ['package app', '', '// Old explains history.', 'func Old() {}'])
  write('untouched.go', ['package app', '// stays'])
  git('add', '.')
  git('commit', '-qm', 'base')
  const base = git('rev-parse', 'HEAD').trim()
  write('app.go', [
    'package app',
    '',
    '// Old explains history.',
    'func Old() {}',
    '',
    'func New() int {',
    '\t// set n to one',
    '\tn := 1 // the count',
    '\t//nolint:ineffassign',
    '\treturn n',
    '}',
  ])
  write('code.go', ['package app', '', '// Helper narrates.', 'func Helper() {}'])
  write('scratch.sh', ['# scratch notes'])
  git('add', '.')
  git('commit', '-qm', 'branch')
  const from = git('rev-parse', 'HEAD').trim()
  const cleaned = [
    'package app',
    '',
    '// Old explains history.',
    'func Old() {}',
    '',
    'func New() int {',
    '\tn := 1',
    '\t//nolint:ineffassign',
    '\treturn n',
    '}',
  ]
  write('app.go', cleaned)
  write('code.go', ['package app', ''])
  write('untouched.go', ['package app'])
  write('new.go', ['package app'])
  rmSync(join(repo, 'scratch.sh'))
  git('add', 'app.go')
  git('commit', '-qm', 'cleanup committed by mistake')

  const run = spawnSync(process.execPath, [helper, '--base', base, '--from', from], {
    cwd: repo,
    encoding: 'utf8',
  })
  assert.equal(run.status, 0, run.stderr)
  assert.deepEqual(JSON.parse(run.stdout), {
    accepted: ['app.go'],
    rejected: [
      { path: 'new.go', reason: 'creates a file' },
      { path: 'code.go', reason: 'removes code or a directive' },
      { path: 'scratch.sh', reason: 'adds or deletes a file' },
      { path: 'untouched.go', reason: 'outside the branch diff' },
    ],
  })
  assert.equal(git('rev-parse', 'HEAD').trim(), from)
  // guidance-audit: allow, it reads the temp fixture repo the helper rewrote
  assert.equal(read('app.go'), `${cleaned.join('\n')}\n`)
  assert.equal(git('diff', '--name-only', from, '--', 'code.go', 'untouched.go'), '')
  assert.equal(existsSync(join(repo, 'new.go')), false)
  assert.equal(read('scratch.sh'), '# scratch notes\n')
  assert.equal(git('diff', '--cached', '--name-only'), '')
})

// Scenario coverage for post-rebase-audit.mjs.
//
// Every scenario builds a real on-disk git repository and performs a real `git rebase`: the shapes
// this helper detects are produced by git's own rebase behaviour (patch-id skipping, rename-aware
// merging, a conflict resolved to one side), so a mocked porcelain could only restate the
// detector's assumptions back to it.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import {
  REASON_DROPPED_EMPTY,
  REASON_PATCH_UPSTREAM,
  UNEVALUATED,
  VERDICT_CLEAN,
  VERDICT_FINDINGS,
  formatAuditNote,
  identTokens,
  pathTokens,
  postRebaseAudit,
  unquoteGitPath,
} from './post-rebase-audit.mjs'

const HELPER = fileURLToPath(new URL('./post-rebase-audit.mjs', import.meta.url))

const GIT_ENV = {
  ...process.env,
  GIT_AUTHOR_NAME: 'Audit Test',
  GIT_AUTHOR_EMAIL: 'audit@example.invalid',
  GIT_COMMITTER_NAME: 'Audit Test',
  GIT_COMMITTER_EMAIL: 'audit@example.invalid',
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
  GIT_EDITOR: 'true',
}

function git(cwd, args, { allowFail = false } = {}) {
  const res = spawnSync('git', args, { cwd, encoding: 'utf8', env: GIT_ENV })
  if (!allowFail) assert.equal(res.status, 0, `git ${args.join(' ')} failed: ${res.stderr}`)
  return res
}

function writeFiles(dir, files) {
  for (const [file, body] of Object.entries(files)) {
    const full = path.join(dir, file)
    if (body === null) {
      fs.rmSync(full)
      continue
    }
    fs.mkdirSync(path.dirname(full), { recursive: true })
    fs.writeFileSync(full, body)
  }
}

function commit(dir, message) {
  git(dir, ['add', '-A'])
  git(dir, ['commit', '-q', '-m', message])
}

// Twelve distinct lines, so a rename keeps a similarity index far above git's 50% threshold.
const FILLER = Array.from({ length: 12 }, (_, i) => `filler line number ${i + 1}`).join('\n')

/**
 * A repo with `main` (the base) and `work` (the branch) forked from one commit holding `initial`.
 * `branch` and `baseSide` are lists of `{message, files}` commits; a `null` file body deletes it.
 * `baseMoves` renames paths on the base side with `git mv`, ahead of the base commits.
 * Returns the repo and the pre-rebase branch tip; the caller performs the rebase.
 */
function makeRepo({ initial, branch = [], baseSide = [], baseMoves = [] }) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'post-rebase-audit-'))
  git(dir, ['init', '-q', '-b', 'main'])
  git(dir, ['config', 'commit.gpgsign', 'false'])
  writeFiles(dir, initial)
  commit(dir, 'fork point')
  git(dir, ['checkout', '-q', '-b', 'work'])
  for (const c of branch) {
    writeFiles(dir, c.files)
    commit(dir, c.message)
  }
  const preRebaseHead = git(dir, ['rev-parse', 'HEAD']).stdout.trim()
  git(dir, ['checkout', '-q', 'main'])
  for (const [from, to] of baseMoves) {
    fs.mkdirSync(path.dirname(path.join(dir, to)), { recursive: true })
    git(dir, ['mv', from, to])
  }
  if (baseMoves.length > 0) commit(dir, 'base moves files')
  for (const c of baseSide) {
    writeFiles(dir, c.files)
    commit(dir, c.message)
  }
  git(dir, ['checkout', '-q', 'work'])
  return { dir, preRebaseHead }
}

function rebaseClean(dir) {
  git(dir, ['rebase', '-q', 'main'])
}

function audit(dir, preRebaseHead, extra = {}) {
  return postRebaseAudit({ repo: dir, preRebaseHead, base: 'main', ...extra })
}

test('a regeneration commit whose patch the base already carries is a skipped commit', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'src.txt': 'source v1\n', 'gen/out.txt': 'generated from v1\n' },
    branch: [
      { message: 'change the source', files: { 'src.txt': 'source v2\n' } },
      { message: 'regenerate output', files: { 'gen/out.txt': 'generated from v2\n' } },
    ],
    baseSide: [
      { message: 'base regenerates too', files: { 'gen/out.txt': 'generated from v2\n' } },
    ],
  })
  rebaseClean(dir)
  const report = audit(dir, preRebaseHead)
  const regen = report.skippedCommits.find((c) => c.subject === 'regenerate output')
  assert.ok(regen, `expected the regeneration commit, got ${JSON.stringify(report.skippedCommits)}`)
  assert.equal(regen.reason, REASON_PATCH_UPSTREAM)
  assert.deepEqual(regen.paths, ['gen/out.txt'])
  assert.equal(report.verdict, VERDICT_FINDINGS)
})

test('a branch commit the rebase dropped as empty is a skipped commit', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'call.js': 'doThing()\n' },
    branch: [{ message: 'gate the call', files: { 'call.js': 'if (gate) doThing()\n' } }],
    baseSide: [{ message: 'move call into helper', files: { 'call.js': 'helper()\n' } }],
  })
  assert.notEqual(git(dir, ['rebase', 'main'], { allowFail: true }).status, 0)
  git(dir, ['checkout', '--ours', 'call.js'])
  git(dir, ['add', 'call.js'])
  // Resolved to the base side, the replayed commit is empty; skipping it is what an agent does.
  git(dir, ['rebase', '--skip'])
  const report = audit(dir, preRebaseHead)
  const dropped = report.skippedCommits.find((c) => c.subject === 'gate the call')
  assert.ok(dropped, `expected the dropped commit, got ${JSON.stringify(report.skippedCommits)}`)
  assert.equal(dropped.reason, REASON_DROPPED_EMPTY)
})

test('a conflict resolved to the base side loses the branch-added lines', (t) => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'billing/portal.js': 'openPortal(target)\n', 'notes.txt': 'notes v1\n' },
    branch: [
      {
        message: 'gate the portal on the api version',
        files: {
          'billing/portal.js':
            'const supported = apiVersionAtLeast(3)\nif (supported) openPortal(target)\n',
          'notes.txt': 'notes v2\n',
        },
      },
    ],
    baseSide: [
      {
        message: 'move the portal call into a helper',
        files: { 'billing/portal.js': 'openPortalVia(helperTarget(target))\n' },
      },
    ],
  })
  assert.notEqual(git(dir, ['rebase', 'main'], { allowFail: true }).status, 0)
  git(dir, ['checkout', '--ours', 'billing/portal.js'])
  git(dir, ['add', 'billing/portal.js'])
  git(dir, ['rebase', '--continue'])
  const report = audit(dir, preRebaseHead)
  t.diagnostic(JSON.stringify(report))
  const lost = report.lostAdditions.find((e) => e.path === 'billing/portal.js')
  assert.ok(lost, `expected billing/portal.js, got ${JSON.stringify(report.lostAdditions)}`)
  assert.ok(lost.lines.includes('const supported = apiVersionAtLeast(3)'))
  assert.ok(lost.lines.includes('if (supported) openPortal(target)'))
  assert.equal(lost.count, 2)
  assert.equal(report.verdict, VERDICT_FINDINGS)
})

test('a branch line that survives a base-side rename is not a lost addition', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'old/thing.txt': `${FILLER}\n` },
    branch: [
      {
        message: 'add a line',
        files: { 'old/thing.txt': `${FILLER}\nthe branch added this line\n` },
      },
    ],
    baseMoves: [['old/thing.txt', 'new/thing.txt']],
  })
  rebaseClean(dir)
  const moved = git(dir, ['show', 'HEAD:new/thing.txt']).stdout
  assert.match(
    moved,
    /the branch added this line/,
    'fixture: git carried the line across the rename',
  )
  const report = audit(dir, preRebaseHead)
  assert.equal(report.detectors.lostAdditions, 'ok')
  assert.deepEqual(report.lostAdditions, [])
})

test('base-added tests in directories the branch modified are reported', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: {
      'pkg/widget.go': 'package pkg\n',
      'web/app.mjs': 'export const app = 1\n',
      'other/z.go': 'package other\n',
    },
    branch: [
      {
        message: 'extend the widget set',
        files: {
          'pkg/widget.go': 'package pkg\n// gadget\n',
          'web/app.mjs': 'export const app = 2\n',
        },
      },
    ],
    baseSide: [
      {
        message: 'base adds exhaustiveness tests',
        files: {
          'pkg/widget_test.go': 'package pkg\n',
          'web/app.test.mjs': 'test()\n',
          'other/z_test.go': 'package other\n',
        },
      },
    ],
  })
  rebaseClean(dir)
  const report = audit(dir, preRebaseHead)
  assert.deepEqual(report.baseAddedTests, [
    { path: 'pkg/widget_test.go', branchDir: 'pkg' },
    { path: 'web/app.test.mjs', branchDir: 'web' },
  ])
})

// The base moves its scratch convention under a run directory and renames a file in the scratch
// directory; the branch, written against the old convention, adds a reader of the old path and a
// new artifact beside it.
function scratchRelocationRepo() {
  const { dir, preRebaseHead } = makeRepo({
    initial: {
      'skill.md': 'Write the original to .scratch/<ID>.orig.md before editing.\n',
      'scratch/notes.md': `${FILLER}\n`,
    },
    branch: [
      {
        message: 'add a final-copy step',
        files: {
          'guard.md':
            'Compare .scratch/<ID>.orig.md with the edit.\nWrite the result to .scratch/<ID>.final.md.\n',
          'scratch/new-artifact.md': 'artifact\n',
        },
      },
    ],
    baseMoves: [['scratch/notes.md', 'scratch/run/notes.md']],
    baseSide: [
      {
        message: 'move scratch under a run directory',
        files: {
          'skill.md': 'Write the original to .scratch/run-<RUN>/<ID>.orig.md before editing.\n',
        },
      },
    ],
  })
  rebaseClean(dir)
  return audit(dir, preRebaseHead)
}

test('a base rename in a directory the branch wrote into is a base relocation', () => {
  const report = scratchRelocationRepo()
  assert.deepEqual(report.baseRelocations, [
    {
      from: 'scratch/notes.md',
      to: 'scratch/run/notes.md',
      status: 'renamed',
      branchPaths: ['scratch/new-artifact.md'],
    },
  ])
})

test('a base-retired path token the branch still writes is retired by base', () => {
  const report = scratchRelocationRepo()
  assert.deepEqual(report.retiredByBase, [
    { token: '.scratch/<ID>.orig.md', headHits: ['guard.md'] },
  ])
})

test('a branch-added sibling of a retired path token is a convention suspect', () => {
  const report = scratchRelocationRepo()
  assert.deepEqual(report.conventionSuspects, [
    { token: '.scratch/<ID>.final.md', retiredSibling: '.scratch/<ID>.orig.md' },
  ])
})

test('a base-side comment naming an identifier the branch deleted everywhere is reported', (t) => {
  const { dir, preRebaseHead } = makeRepo({
    initial: {
      'limits.go': 'package app\n\nconst MaxWidgetCount = 3\n',
      'use.go': 'package app\n\nfunc cap() int { return MaxWidgetCount }\n',
    },
    branch: [
      {
        message: 'drop the widget cap',
        files: {
          'limits.go': 'package app\n',
          'use.go': 'package app\n\nfunc cap() int { return 0 }\n',
        },
      },
    ],
    baseSide: [
      {
        message: 'document the cap',
        files: {
          'doc.go': 'package app\n\n// MaxWidgetCount bounds how many widgets a page renders.\n',
        },
      },
    ],
  })
  rebaseClean(dir)
  const report = audit(dir, preRebaseHead)
  t.diagnostic(JSON.stringify(report))
  assert.deepEqual(report.deletedByBranch, [{ token: 'MaxWidgetCount', headHits: ['doc.go'] }])
  assert.equal(report.verdict, VERDICT_FINDINGS)
})

test('a non-conflicting, unrelated base advance is clean', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'app/greeting.txt': 'hello world\n', 'lib/util.txt': 'util v1\n' },
    branch: [{ message: 'change the greeting', files: { 'app/greeting.txt': 'hello there\n' } }],
    baseSide: [{ message: 'unrelated base work', files: { 'lib/extra.txt': 'more utilities\n' } }],
  })
  rebaseClean(dir)
  const report = audit(dir, preRebaseHead)
  assert.equal(report.verdict, VERDICT_CLEAN, JSON.stringify(report))
  assert.ok(Object.values(report.detectors).every((s) => s === 'ok'))
  assert.match(formatAuditNote(report), /^Post-rebase audit: clean\./)
})

test('an unresolvable --pre-rebase-head or --base is unevaluated, never clean', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'a.txt': 'a\n' },
    branch: [{ message: 'branch work', files: { 'a.txt': 'b\n' } }],
  })
  rebaseClean(dir)
  for (const opts of [
    { preRebaseHead: 'deadbeef'.repeat(5) },
    { preRebaseHead: 'no-such-ref' },
    { base: 'refs/remotes/origin/missing' },
    { preRebaseHead: '' },
  ]) {
    const report = postRebaseAudit({ repo: dir, preRebaseHead, base: 'main', ...opts })
    assert.equal(report.verdict, UNEVALUATED, JSON.stringify(opts))
    assert.notEqual(report.verdict, VERDICT_CLEAN)
    assert.match(formatAuditNote(report), /UNEVALUATED/)
  }
  // Through the CLI too: the report still lands on stdout, unevaluated.
  const res = spawnSync(
    process.execPath,
    [HELPER, 'check', '--repo', dir, '--pre-rebase-head', 'no-such-ref', '--base', 'main'],
    { encoding: 'utf8' },
  )
  assert.equal(res.status, 0, res.stderr)
  assert.equal(JSON.parse(res.stdout).verdict, UNEVALUATED)
})

test('a failing git call makes its detector unevaluated and the verdict not clean', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'a.txt': 'a\n' },
    branch: [{ message: 'branch work', files: { 'a.txt': 'b\n' } }],
    baseSide: [{ message: 'base work', files: { 'c.txt': 'c\n' } }],
  })
  rebaseClean(dir)
  const real = (repo, args) => {
    const res = spawnSync('git', args, { cwd: repo, encoding: 'utf8', env: GIT_ENV })
    return { status: res.status, stdout: res.stdout, stderr: res.stderr }
  }
  const run = (repo, args) =>
    args[0] === 'cherry'
      ? { status: 128, stdout: '', stderr: 'fatal: simulated' }
      : real(repo, args)
  const report = postRebaseAudit({ repo: dir, preRebaseHead, base: 'main', run })
  assert.equal(report.detectors.skippedCommits, UNEVALUATED)
  assert.equal(report.verdict, UNEVALUATED)
})

test('no rebase (pre-rebase head equals HEAD) is clean with a note', () => {
  const { dir, preRebaseHead } = makeRepo({
    initial: { 'a.txt': 'a\n' },
    branch: [{ message: 'branch work', files: { 'a.txt': 'b\n' } }],
  })
  const report = audit(dir, preRebaseHead)
  assert.equal(report.verdict, VERDICT_CLEAN)
  assert.match(report.notes.join(' '), /no rebase happened/)
})

test('auditing before the rebase (HEAD lacks the base) is unevaluated', () => {
  const { dir } = makeRepo({
    initial: { 'a.txt': 'a\n' },
    branch: [{ message: 'branch work', files: { 'a.txt': 'b\n' } }],
    baseSide: [{ message: 'base work', files: { 'c.txt': 'c\n' } }],
  })
  const report = audit(dir, 'HEAD~1')
  assert.equal(report.verdict, UNEVALUATED)
  assert.match(report.notes.join(' '), /does not contain the base/)
})

test('token extraction and path unquoting', () => {
  assert.deepEqual(pathTokens([{ line: 'see https://x.example/a/b. and // comment and docs/' }]), [
    'docs/',
    'x.example/a/b',
  ])
  assert.deepEqual(identTokens([{ line: 'const MaxWidgetCount = foo(abc)' }]), [
    'MaxWidgetCount',
    'const',
  ])
  assert.equal(unquoteGitPath('"b/caf\\303\\251 x\\tz.txt"'), 'b/café x\tz.txt')
  assert.equal(unquoteGitPath('b/plain.txt'), 'b/plain.txt')
})

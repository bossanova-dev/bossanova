// Scenario coverage for worktree-state.mjs.
//
// Injected-runner scenarios pin the parser and the verdict contract (including the shapes a
// command-rewriting shell hook produces); one integration scenario runs the real git in a temp repo
// so the argv shape is proven against git itself, not against the parser's own assumptions.

import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'

import { CLEAN, DIRTY, UNKNOWN, parsePorcelainZ, runCli, worktreeState } from './worktree-state.mjs'

const HELPER = fileURLToPath(new URL('./worktree-state.mjs', import.meta.url))

function fakeRunner(responses) {
  const calls = []
  const run = (repo, args) => {
    calls.push(args)
    const r = responses[calls.length - 1] ?? responses[responses.length - 1]
    if (r instanceof Error) throw r
    return { status: 0, signal: null, stdout: '', stderr: '', ...r }
  }
  return { run, calls }
}

function cli(argv, responses) {
  const { run, calls } = fakeRunner(responses)
  let text = ''
  const code = runCli(argv, { run, out: (s) => (text += s) })
  return { code, text, lines: text.split('\n'), calls }
}

test('empty stdout with exit 0 is clean, exit 0', () => {
  const r = cli([], [{ stdout: '' }])
  assert.equal(r.code, 0)
  assert.equal(r.lines[0], 'verdict: clean')
})

test('porcelain records are dirty with their paths, exit 1', () => {
  const r = cli([], [{ stdout: ' M a.txt\0?? b/c.txt\0' }])
  assert.equal(r.code, 1)
  assert.equal(r.lines[0], 'verdict: dirty')
  assert.deepEqual(r.lines.slice(1, 3), ['a.txt', 'b/c.txt'])
})

test('a rename record consumes its origin without misaligning later records', () => {
  const parsed = parsePorcelainZ('R  new\0old\0 M z.txt\0')
  assert.deepEqual(parsed, { ok: true, entries: ['new', 'z.txt'] })
  assert.equal(worktreeState({}, fakeRunner([{ stdout: 'R  new\0old\0' }]).run).verdict, DIRTY)
})

test('a rename record missing its origin is unknown', () => {
  assert.equal(worktreeState({}, fakeRunner([{ stdout: 'R  new\0' }]).run).verdict, UNKNOWN)
})

test('paths with spaces and non-ASCII round-trip unquoted', () => {
  const res = worktreeState({}, fakeRunner([{ stdout: '?? dir with space/ünï.txt\0' }]).run)
  assert.deepEqual(res.entries, ['dir with space/ünï.txt'])
})

test('the hook shape "ok\\n" with exit 0 is unknown, exit 2 — never clean or dirty', () => {
  const r = cli([], [{ stdout: 'ok\n' }])
  assert.equal(r.code, 2)
  assert.equal(r.lines[0], 'verdict: unknown')
})

test('"ok" with no newline is unknown', () => {
  assert.equal(worktreeState({}, fakeRunner([{ stdout: 'ok' }]).run).verdict, UNKNOWN)
})

test('a record with a malformed status prefix is unknown', () => {
  for (const stdout of ['XY a.txt\0', ' Ma.txt\0', 'ok\0', ' M \0']) {
    assert.equal(worktreeState({}, fakeRunner([{ stdout }]).run).verdict, UNKNOWN, stdout)
  }
})

test('git exiting 128 (not a repository) is unknown', () => {
  const res = worktreeState(
    {},
    fakeRunner([{ status: 128, stderr: 'fatal: not a git repository' }]).run,
  )
  assert.equal(res.verdict, UNKNOWN)
  assert.match(res.reason, /exited 128/)
})

test('a runner that throws or cannot find git is unknown', () => {
  assert.equal(worktreeState({}, fakeRunner([new Error('spawn git ENOENT')]).run).verdict, UNKNOWN)
  assert.equal(
    worktreeState({}, fakeRunner([{ status: null, error: 'spawnSync git ENOENT' }]).run).verdict,
    UNKNOWN,
  )
})

test('a runner killed by a signal is unknown even with empty stdout', () => {
  assert.equal(
    worktreeState({}, fakeRunner([{ status: null, signal: 'SIGKILL' }]).run).verdict,
    UNKNOWN,
  )
})

test('--exclude "" is a usage error and never calls the runner', () => {
  const r = cli(['--exclude', ''], [{ stdout: '' }])
  assert.equal(r.code, 2)
  assert.equal(r.lines[0], 'verdict: unknown')
  assert.equal(r.calls.length, 0)
})

test('an empty positional pathspec and an unknown flag are usage errors', () => {
  assert.equal(cli(['--', ''], [{}]).code, 2)
  assert.equal(cli(['--bogus'], [{}]).calls.length, 0)
})

test('repeated --exclude reaches git as :(exclude) pathspecs, in order, after positionals', () => {
  const r = cli(['--exclude', 'a', '--exclude', 'b', '--', '.'], [{ stdout: '' }])
  const args = r.calls[0]
  assert.deepEqual(args.slice(-4), ['--', '.', ':(exclude)a', ':(exclude)b'])
})

test('--untracked no passes --untracked-files=no; default is all', () => {
  assert.ok(cli(['--untracked', 'no'], [{}]).calls[0].includes('--untracked-files=no'))
  assert.ok(cli([], [{}]).calls[0].includes('--untracked-files=all'))
  assert.equal(cli(['--untracked', 'normal'], [{}]).code, 2)
})

test('--base with a clean status but a committed diff is dirty with labelled entries', () => {
  const r = cli(['--base', 'origin/main'], [{ stdout: '' }, { stdout: 'x.go\0y/z.md\0' }])
  assert.equal(r.code, 1)
  assert.deepEqual(r.lines.slice(0, 3), ['verdict: dirty', 'committed x.go', 'committed y/z.md'])
  assert.deepEqual(r.calls[1].slice(0, 4), ['diff', '--name-only', '-z', 'origin/main...HEAD'])
})

test('--base whose diff call fails or is not NUL-framed is unknown', () => {
  assert.equal(cli(['--base', 'nope'], [{ stdout: '' }, { status: 128 }]).code, 2)
  assert.equal(cli(['--base', 'origin/main'], [{ stdout: '' }, { stdout: 'ok\n' }]).code, 2)
})

test('the verdict line comes first in every case, and clean only when the parse succeeded', () => {
  for (const [stdout, want] of [
    ['', CLEAN],
    [' M a\0', DIRTY],
    ['ok\n', UNKNOWN],
  ]) {
    assert.equal(cli([], [{ stdout, status: 0 }]).lines[0], `verdict: ${want}`)
  }
})

test('--json emits parseable JSON with a verdict in all three cases', () => {
  for (const [stdout, want] of [
    ['', CLEAN],
    ['?? n\0', DIRTY],
    ['ok\n', UNKNOWN],
  ]) {
    const parsed = JSON.parse(cli(['--json'], [{ stdout }]).text)
    assert.equal(parsed.verdict, want)
    assert.ok(Array.isArray(parsed.entries))
    assert.equal(typeof parsed.reason, 'string')
  }
  assert.equal(JSON.parse(cli(['--json', '--exclude', ''], [{}]).text).verdict, UNKNOWN)
})

test('integration: a real git repo goes clean → dirty → clean through the CLI', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'worktree-state-'))
  const hooks = fs.mkdtempSync(path.join(os.tmpdir(), 'worktree-state-hooks-'))
  const env = {
    ...process.env,
    GIT_AUTHOR_NAME: 'Worktree Test',
    GIT_AUTHOR_EMAIL: 'worktree@example.invalid',
    GIT_COMMITTER_NAME: 'Worktree Test',
    GIT_COMMITTER_EMAIL: 'worktree@example.invalid',
    GIT_CONFIG_NOSYSTEM: '1',
  }
  const git = (...args) => {
    const res = spawnSync(
      'git',
      ['-c', 'commit.gpgsign=false', '-c', `core.hooksPath=${hooks}`, '-C', dir, ...args],
      { encoding: 'utf8', env },
    )
    assert.equal(res.status, 0, res.stderr)
  }
  const verdict = (...args) => {
    const res = spawnSync(process.execPath, [HELPER, '--repo', dir, ...args], {
      encoding: 'utf8',
      env,
    })
    return { code: res.status, first: res.stdout.split('\n')[0], out: res.stdout }
  }
  try {
    git('init', '-q')
    assert.deepEqual([verdict().code, verdict().first], [0, 'verdict: clean'])
    fs.writeFileSync(path.join(dir, 'new file.txt'), 'x\n')
    const dirty = verdict()
    assert.deepEqual([dirty.code, dirty.first], [1, 'verdict: dirty'])
    assert.match(dirty.out, /^new file\.txt$/m)
    assert.equal(verdict('--exclude', 'new file.txt').first, 'verdict: clean')
    assert.equal(verdict('--untracked', 'no').first, 'verdict: clean')
    git('add', '--', 'new file.txt')
    git('commit', '-q', '-m', 'init')
    assert.equal(verdict().first, 'verdict: clean')
    assert.equal(verdict('--', '.').first, 'verdict: clean')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
    fs.rmSync(hooks, { recursive: true, force: true })
  }
})

test('integration: outside a repository the real git yields unknown, exit 2', () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'worktree-state-norepo-'))
  try {
    const res = spawnSync(process.execPath, [HELPER, '--repo', dir], {
      encoding: 'utf8',
      env: { ...process.env, GIT_CEILING_DIRECTORIES: path.dirname(dir) },
    })
    assert.equal(res.status, 2)
    assert.equal(res.stdout.split('\n')[0], 'verdict: unknown')
  } finally {
    fs.rmSync(dir, { recursive: true, force: true })
  }
})

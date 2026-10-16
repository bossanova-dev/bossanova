import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

import {
  DESCRIPTION_LIMIT,
  RECEIPT_CONTEXT,
  STATUS_STATES,
  UsageError,
  capDescription,
  classifyRead,
  main,
  postArgs,
  readArgs,
  readyGateOptions,
  validatePostInput,
} from './commit-status.mjs'
import { PROVENANCE_CONTEXTS } from './pr-check-state.mjs'
import { main as gateMain, readyGateReport } from './ready-gate.mjs'

const SCRIPT_PATH = fileURLToPath(new URL('./commit-status.mjs', import.meta.url))
const SHA = 'a'.repeat(40)

// A fake spawn that records every call and answers with the queued result.
function fakeSpawn(result = { status: 0, stdout: '{}', stderr: '' }) {
  const calls = []
  const spawn = (cmd, args, opts) => {
    calls.push({ cmd, args, opts })
    return typeof result === 'function' ? result(cmd, args) : result
  }
  return { spawn, calls }
}

const status = (context, state, updatedAt = '', extra = {}) => ({
  context,
  state,
  description: `${context} ${state}`,
  target_url: 'https://example.test/run',
  updated_at: updatedAt,
  ...extra,
})

// ---------------------------------------------------------------------------
// Constants

test('RECEIPT_CONTEXT is boss/build and pr-check-state excludes it as provenance', () => {
  assert.equal(RECEIPT_CONTEXT, 'boss/build')
  assert.ok(
    PROVENANCE_CONTEXTS.includes(RECEIPT_CONTEXT),
    'pr-check-state PROVENANCE_CONTEXTS must carry the receipt context, or the receipt becomes a CI gate',
  )
})

// ---------------------------------------------------------------------------
// post argv

test('postArgs — the exact argv for each of the four states, raw -f fields only', () => {
  for (const state of STATUS_STATES) {
    const args = postArgs({ context: 'boss/build', state, sha: SHA })
    assert.deepEqual(args, [
      'api',
      '--method',
      'POST',
      `repos/{owner}/{repo}/statuses/${SHA}`,
      '-f',
      `state=${state}`,
      '-f',
      'context=boss/build',
    ])
    assert.ok(!args.includes('-F'))
    assert.ok(!args.includes('--field'))
  }
})

test('postArgs — description, target URL and repo are added as raw fields and a literal path', () => {
  const args = postArgs({
    context: 'boss/build',
    state: 'success',
    sha: SHA,
    description: '@not-a-file boss-build REVIEW_READY',
    targetUrl: 'https://github.com/o/r/pull/1',
    repo: 'octo-org/some.repo',
  })
  assert.deepEqual(args, [
    'api',
    '--method',
    'POST',
    `repos/octo-org/some.repo/statuses/${SHA}`,
    '-f',
    'state=success',
    '-f',
    'context=boss/build',
    '-f',
    'description=@not-a-file boss-build REVIEW_READY',
    '-f',
    'target_url=https://github.com/o/r/pull/1',
  ])
  assert.ok(!args.includes('-F'))
})

test('postArgs — description only, and target URL only', () => {
  const d = postArgs({ context: 'c', state: 'pending', sha: SHA, description: 'hi' })
  assert.deepEqual(d.slice(-2), ['-f', 'description=hi'])
  assert.ok(!d.some((a) => a.startsWith('target_url=')))
  const u = postArgs({ context: 'c', state: 'error', sha: SHA, targetUrl: 'http://x.test/' })
  assert.deepEqual(u.slice(-2), ['-f', 'target_url=http://x.test/'])
  assert.ok(!u.some((a) => a.startsWith('description=')))
})

// ---------------------------------------------------------------------------
// validation

test('validatePostInput — a 39 or 41 character or non-hex SHA is a usage error', () => {
  for (const sha of ['a'.repeat(39), 'a'.repeat(41), 'g'.repeat(40), '', undefined]) {
    assert.throws(
      () => validatePostInput({ context: 'c', state: 'success', sha }),
      UsageError,
      `sha ${JSON.stringify(sha)}`,
    )
  }
})

test('validatePostInput — an uppercase SHA is normalised to lowercase', () => {
  const v = validatePostInput({ context: 'c', state: 'success', sha: 'ABCDEF'.repeat(6) + 'ABCD' })
  assert.equal(v.sha, 'abcdef'.repeat(6) + 'abcd')
  assert.ok(postArgs({ context: 'c', state: 'success', sha: 'A'.repeat(40) })[3].endsWith(SHA))
})

test('validatePostInput — an empty, whitespace or control-character context is a usage error', () => {
  for (const context of [
    '',
    '   ',
    'boss build',
    'boss/\tbuild',
    'boss/build\n',
    'boss\u0007',
    undefined,
  ]) {
    assert.throws(
      () => validatePostInput({ context, state: 'success', sha: SHA }),
      UsageError,
      `context ${JSON.stringify(context)}`,
    )
  }
})

test('validatePostInput — invalid state, target URL and repo are usage errors', () => {
  assert.throws(() => validatePostInput({ context: 'c', state: 'SUCCESS', sha: SHA }), UsageError)
  assert.throws(() => validatePostInput({ context: 'c', state: 'ok', sha: SHA }), UsageError)
  for (const targetUrl of ['ftp://x.test', 'github.com/o/r', 'https://', 'javascript:alert(1)']) {
    assert.throws(
      () => validatePostInput({ context: 'c', state: 'success', sha: SHA, targetUrl }),
      UsageError,
      `url ${targetUrl}`,
    )
  }
  for (const repo of ['owner', 'owner/name/extra', 'own er/name', '/name']) {
    assert.throws(
      () => validatePostInput({ context: 'c', state: 'success', sha: SHA, repo }),
      UsageError,
      `repo ${repo}`,
    )
  }
})

test('main post — every validation failure throws UsageError without spawning gh', () => {
  const cases = [
    ['--context', 'c', '--state', 'nope', '--sha', SHA],
    ['--context', 'c', '--state', 'success', '--sha', 'abc'],
    ['--context', 'c d', '--state', 'success', '--sha', SHA],
    ['--context', 'c', '--state', 'success', '--sha', SHA, '--target-url', 'nope'],
    ['--context', 'c', '--state', 'success', '--sha', SHA, '--repo', 'nope'],
    ['--context', 'c', '--state', 'success', '--sha', SHA, '--bogus', 'x'],
    ['--context', 'c', '--state', 'success', '--sha'],
  ]
  for (const argv of cases) {
    const { spawn, calls } = fakeSpawn()
    assert.throws(() => main(['post', ...argv], { spawn }), UsageError, argv.join(' '))
    assert.equal(calls.length, 0, `gh must not be spawned for ${argv.join(' ')}`)
  }
})

// ---------------------------------------------------------------------------
// description cap

test('capDescription — exactly 140 code points is unchanged', () => {
  const text = 'x'.repeat(DESCRIPTION_LIMIT)
  assert.equal(DESCRIPTION_LIMIT, 140)
  assert.equal(capDescription(text), text)
  assert.equal(capDescription('short'), 'short')
})

test('capDescription — 141 code points truncates to 140 ending in an ellipsis', () => {
  const out = capDescription('y'.repeat(141))
  assert.equal(Array.from(out).length, 140)
  assert.ok(out.endsWith('…'))
  assert.equal(out, `${'y'.repeat(139)}…`)
})

test('capDescription — an emoji string is counted in code points and never split mid-pair', () => {
  const fits = '😀'.repeat(140)
  assert.equal(capDescription(fits), fits, '140 emoji is 280 UTF-16 units but 140 code points')
  const out = capDescription('😀'.repeat(200))
  assert.equal(Array.from(out).length, 140)
  assert.equal(out, `${'😀'.repeat(139)}…`)
  assert.ok(!/[\uD800-\uDBFF](?![\uDC00-\uDFFF])/.test(out), 'no lone high surrogate')
  const mixed = capDescription(`${'é'.repeat(139)}😀😀`)
  assert.equal(mixed, `${'é'.repeat(139)}…`)
})

test('main post — a long description is capped before it reaches gh', () => {
  const { spawn, calls } = fakeSpawn()
  const { exitCode, result } = main(
    [
      'post',
      '--context',
      'c',
      '--state',
      'success',
      '--sha',
      SHA,
      '--description',
      'z'.repeat(300),
    ],
    { spawn },
  )
  assert.equal(exitCode, 0)
  const field = calls[0].args.find((a) => a.startsWith('description='))
  assert.equal(Array.from(field.slice('description='.length)).length, 140)
  assert.equal(Array.from(result.description).length, 140)
})

// ---------------------------------------------------------------------------
// post exit codes

const READY = () => ({ ready: true, reasons: [] })

test('main post — success is exit 0 with verdict posted', () => {
  const { spawn, calls } = fakeSpawn({ status: 0, stdout: '{"id":1}', stderr: '' })
  const { exitCode, result } = main(
    ['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA.toUpperCase()],
    { spawn, readyGateReport: READY },
  )
  assert.equal(exitCode, 0)
  assert.deepEqual(result, { verdict: 'posted', sha: SHA, context: 'boss/build', state: 'success' })
  assert.equal(calls.length, 1)
  assert.equal(calls[0].cmd, 'gh')
  assert.equal(calls[0].opts.encoding, 'utf8')
  assert.ok(calls[0].opts.timeout > 0)
})

test('main post — gh exiting non-zero is exit 1 with verdict failed and a reason', () => {
  const { spawn } = fakeSpawn({
    status: 1,
    stdout: '',
    stderr: 'HTTP 403: Resource not accessible\nmore',
  })
  const { exitCode, result } = main(
    ['post', '--context', 'c', '--state', 'failure', '--sha', SHA],
    { spawn },
  )
  assert.equal(exitCode, 1)
  assert.equal(result.verdict, 'failed')
  assert.match(result.reason, /gh exited 1: HTTP 403/)
})

test('main post — gh that cannot be spawned is exit 1 with verdict failed', () => {
  const err = Object.assign(new Error('spawn gh ENOENT'), { code: 'ENOENT' })
  const { spawn } = fakeSpawn({ status: null, error: err })
  const { exitCode, result } = main(
    ['post', '--context', 'c', '--state', 'pending', '--sha', SHA],
    {
      spawn,
    },
  )
  assert.equal(exitCode, 1)
  assert.equal(result.verdict, 'failed')
  assert.match(result.reason, /ENOENT/)
})

// ---------------------------------------------------------------------------
// The ready gate on a boss/build success

test('main post — boss/build success asks the ready gate with the head and the override flags', () => {
  const { spawn, calls } = fakeSpawn()
  const asked = []
  const { exitCode } = main(
    [
      'post',
      '--context',
      'boss/build',
      '--state',
      'success',
      '--sha',
      SHA.toUpperCase(),
      '--review-verdict-file',
      '/tmp/v',
      '--reviewed-head-file',
      '/tmp/h',
      '--base',
      'main',
    ],
    { spawn, readyGateReport: (flags) => (asked.push(flags), { ready: true, reasons: [] }) },
  )
  assert.equal(exitCode, 0)
  assert.deepEqual(asked, [
    { head: SHA, reviewVerdictFile: '/tmp/v', reviewedHeadFile: '/tmp/h', base: 'main' },
  ])
  assert.equal(calls.length, 1)
  assert.ok(!calls[0].args.some((a) => /review|base=/.test(a)), 'gate flags never reach gh')
})

test('main post — a not-ready boss/build success is refused with exit 3 and nothing posted', () => {
  const { spawn, calls } = fakeSpawn()
  const { exitCode, result, stderr } = main(
    ['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA],
    {
      spawn,
      readyGateReport: () => ({
        ready: false,
        reasons: ['review-not-clean', 'tree-changed-since-review'],
      }),
    },
  )
  assert.equal(exitCode, 3)
  assert.equal(calls.length, 0, 'gh must not be spawned on a refusal')
  assert.deepEqual(result, {
    verdict: 'refused',
    sha: SHA,
    context: 'boss/build',
    state: 'success',
    reasons: ['review-not-clean', 'tree-changed-since-review'],
  })
  assert.equal(
    stderr,
    'commit-status: refused boss/build success: review-not-clean, tree-changed-since-review',
  )
})

test('main post — a malformed gate answer refuses as ready-gate-error', () => {
  for (const answer of [undefined, null, {}, { ready: 'yes' }, { ready: false, reasons: [] }]) {
    const { spawn, calls } = fakeSpawn()
    const { exitCode, result } = main(
      ['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA],
      { spawn, readyGateReport: () => answer },
    )
    assert.equal(exitCode, 3, JSON.stringify(answer))
    assert.deepEqual(result.reasons, ['ready-gate-error'])
    assert.equal(calls.length, 0)
  }
})

test('main post — a gate that throws refuses as ready-gate-error, naming the error', () => {
  const { spawn, calls } = fakeSpawn()
  const { exitCode, result, stderr } = main(
    ['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA],
    {
      spawn,
      readyGateReport: () => {
        throw new Error('git exploded')
      },
    },
  )
  assert.equal(exitCode, 3)
  assert.deepEqual(result.reasons, ['ready-gate-error'])
  assert.match(stderr, /ready-gate-error \(git exploded\)$/)
  assert.equal(calls.length, 0)
})

test('main post — gate flags on any post but a boss/build success are a usage error', () => {
  for (const [context, state] of [
    ['boss/build', 'failure'],
    ['boss/verify', 'success'],
  ]) {
    for (const flag of ['--review-verdict-file', '--reviewed-head-file', '--base']) {
      const { spawn, calls } = fakeSpawn()
      assert.throws(
        () =>
          main(['post', '--context', context, '--state', state, '--sha', SHA, flag, 'x'], {
            spawn,
            readyGateReport: () => assert.fail('gate asked'),
          }),
        UsageError,
        `${context} ${state} ${flag}`,
      )
      assert.equal(calls.length, 0)
    }
  }
})

test('main post — other contexts and other boss/build states never ask the gate', () => {
  for (const [context, state] of [
    ['boss/build', 'failure'],
    ['boss/build', 'pending'],
    ['boss/build', 'error'],
    ['boss/verify', 'success'],
    ['boss/build-smoke', 'success'],
  ]) {
    const { spawn, calls } = fakeSpawn()
    const { exitCode } = main(['post', '--context', context, '--state', state, '--sha', SHA], {
      spawn,
      readyGateReport: () => assert.fail(`gate asked for ${context} ${state}`),
    })
    assert.equal(exitCode, 0)
    assert.equal(calls.length, 1)
  }
})

test('readyGateOptions maps receipt flags without partial or route opt-ins', () => {
  assert.deepEqual(
    readyGateOptions({
      sha: SHA,
      base: 'main',
      'review-verdict-file': '/tmp/v',
      'reviewed-head-file': '/tmp/h',
      partial: true,
      honourRoute: true,
    }),
    { head: SHA, base: 'main', reviewVerdictFile: '/tmp/v', reviewedHeadFile: '/tmp/h' },
  )
})

test('single seam — a boss/build receipt never takes the PARTIAL route file', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'commit-status-route-'))
  try {
    const git = (...args) => {
      const result = spawnSync('git', args, { cwd: dir, encoding: 'utf8' })
      assert.equal(result.status, 0, result.stderr)
      return result.stdout.trim()
    }
    git('init', '--initial-branch=main')
    const gitDir = git('rev-parse', '--absolute-git-dir')
    writeFileSync(path.join(gitDir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=capped\n')
    writeFileSync(path.join(gitDir, 'boss-build-reviewed-head'), `${SHA}\n`)
    writeFileSync(path.join(gitDir, 'boss-build-ready-route'), 'partial\n')
    const report = (options) => readyGateReport(options, { cwd: dir })
    // The CLI honours the route file in this same repo...
    assert.equal(gateMain(['--head', SHA], { cwd: dir }).admittedFor, 'partial')
    // ...but the receipt's in-process gate does not, so the post is refused.
    const { spawn, calls } = fakeSpawn()
    const refused = main(['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA], {
      spawn,
      readyGateReport: report,
    })
    assert.equal(refused.exitCode, 3)
    assert.deepEqual(refused.result.reasons, ['review-not-clean'])
    assert.equal(calls.length, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('main post — the default gate reads a real repo in-process: refused, then posted', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'commit-status-inproc-'))
  try {
    const verdictFile = path.join(dir, 'verdict')
    const headFile = path.join(dir, 'reviewed')
    const gateArgs = ['--review-verdict-file', verdictFile, '--reviewed-head-file', headFile]
    const post = () =>
      main(['post', '--context', 'boss/build', '--state', 'success', '--sha', SHA, ...gateArgs], {
        spawn: fakeSpawn().spawn,
      })
    writeFileSync(verdictFile, 'REVIEW_VERDICT=capped\n')
    writeFileSync(headFile, `${SHA}\n`)
    const refused = post()
    assert.equal(refused.exitCode, 3)
    assert.deepEqual(refused.result.reasons, ['review-not-clean'])
    writeFileSync(verdictFile, 'REVIEW_VERDICT=clean\n')
    const posted = post()
    assert.equal(posted.exitCode, 0, posted.stderr)
    assert.equal(posted.result.verdict, 'posted')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// ---------------------------------------------------------------------------
// read

test('readArgs — lists every status page, with and without --repo', () => {
  assert.deepEqual(readArgs({ sha: SHA }), [
    'api',
    `repos/{owner}/{repo}/commits/${SHA}/statuses?per_page=100`,
    '--paginate',
    '--slurp',
  ])
  assert.equal(
    readArgs({ sha: SHA, repo: 'o/r' })[1],
    `repos/o/r/commits/${SHA}/statuses?per_page=100`,
  )
  assert.throws(() => readArgs({ sha: 'nope' }), UsageError)
})

test('classifyRead — present with --context, and the latest of two statuses for one context wins', () => {
  // Reverse-chronological slurped pages: the first boss/build is the latest.
  const stdout = JSON.stringify([
    [
      status('boss/build', 'success', '2026-10-06T10:00:00Z'),
      status('ci/other', 'failure', '2026-10-06T09:30:00Z'),
    ],
    [status('boss/build', 'failure', '2026-10-06T09:00:00Z')],
  ])
  const v = classifyRead({ exit: 0, stdout, context: 'boss/build', sha: SHA })
  assert.equal(v.verdict, 'present')
  assert.equal(v.state, 'success')
  assert.equal(v.context, 'boss/build')
  assert.equal(v.sha, SHA)
  assert.deepEqual(Object.keys(v.contexts).sort(), ['boss/build', 'ci/other'])
  assert.deepEqual(v.contexts['boss/build'], {
    state: 'success',
    description: 'boss/build success',
    targetUrl: 'https://example.test/run',
    updatedAt: '2026-10-06T10:00:00Z',
  })
})

test('classifyRead — an out-of-order list still keeps the strictly newer status per context', () => {
  const stdout = JSON.stringify([
    status('boss/build', 'failure', '2026-10-06T09:00:00+00:00'),
    status('boss/build', 'success', '2026-10-06T19:00:00+09:00'),
  ])
  const v = classifyRead({ exit: 0, stdout, context: 'boss/build' })
  assert.equal(v.state, 'success', '19:00+09:00 is 10:00Z, later than 09:00Z')
})

test('classifyRead — present means a status exists, not that it is success', () => {
  const stdout = JSON.stringify([[status('boss/build', 'failure', '2026-10-06T10:00:00Z')]])
  const v = classifyRead({ exit: 0, stdout, context: 'boss/build' })
  assert.equal(v.verdict, 'present')
  assert.equal(v.state, 'failure')
})

test('classifyRead — absent when the commit has no statuses, or none for the context', () => {
  const empty = classifyRead({ exit: 0, stdout: '[[]]', context: 'boss/build' })
  assert.equal(empty.verdict, 'absent')
  assert.equal(empty.state, undefined)
  assert.equal(classifyRead({ exit: 0, stdout: '[]' }).verdict, 'absent')
  const combinedEmpty = classifyRead({ exit: 0, stdout: '{"state":"pending","statuses":[]}' })
  assert.equal(combinedEmpty.verdict, 'absent')
  const other = classifyRead({
    exit: 0,
    stdout: JSON.stringify([[status('ci/other', 'success')]]),
    context: 'boss/build',
  })
  assert.equal(other.verdict, 'absent')
})

test('classifyRead — without --context, present iff any context has a status', () => {
  const v = classifyRead({ exit: 0, stdout: JSON.stringify([[status('ci/other', 'pending')]]) })
  assert.equal(v.verdict, 'present')
  assert.equal(v.context, undefined)
  assert.equal(v.state, undefined)
})

test('classifyRead — a combined-status object is read too', () => {
  const v = classifyRead({
    exit: 0,
    stdout: JSON.stringify({ state: 'success', statuses: [status('boss/build', 'success')] }),
    context: 'boss/build',
  })
  assert.equal(v.verdict, 'present')
  assert.equal(v.state, 'success')
})

test('classifyRead — non-zero exit, malformed JSON, wrong shape and spawn failure are each unknown', () => {
  const enoent = Object.assign(new Error('spawn gh ENOENT'), { code: 'ENOENT' })
  const cases = [
    [{ exit: 1, stdout: '' }, /gh exited 1/],
    [{ exit: 0, stdout: '{not json' }, /unparseable/],
    [{ exit: 0, stdout: '{"message":"Not Found"}' }, /shape/],
    [{ exit: 0, stdout: '"a string"' }, /shape/],
    [{ exit: 0, stdout: '[[{"context":"boss/build"}]]' }, /shape/],
    [{ exit: 0, stdout: '[[null]]' }, /shape/],
    [{ exit: null, stdout: '', error: enoent }, /ENOENT/],
  ]
  for (const [input, reason] of cases) {
    const v = classifyRead({ ...input, context: 'boss/build' })
    assert.equal(v.verdict, 'unknown', JSON.stringify(input))
    assert.match(v.reason, reason)
    assert.notEqual(v.verdict, 'present')
    assert.equal(v.state, undefined)
  }
})

test('main read — always exits 0 once it has a verdict, including unknown', () => {
  const ok = fakeSpawn({ status: 0, stdout: JSON.stringify([[status('boss/build', 'success')]]) })
  const present = main(['read', '--sha', SHA, '--context', 'boss/build'], { spawn: ok.spawn })
  assert.equal(present.exitCode, 0)
  assert.equal(present.result.verdict, 'present')
  assert.deepEqual(ok.calls[0].args, readArgs({ sha: SHA }))

  const bad = fakeSpawn({ status: 1, stdout: '', stderr: 'HTTP 502' })
  const unknown = main(['read', '--sha', SHA, '--repo', 'o/r'], { spawn: bad.spawn })
  assert.equal(unknown.exitCode, 0)
  assert.equal(unknown.result.verdict, 'unknown')
  assert.equal(bad.calls[0].args[1], `repos/o/r/commits/${SHA}/statuses?per_page=100`)
})

test('main read — a usage error throws without spawning gh', () => {
  for (const argv of [
    ['read'],
    ['read', '--sha', 'nope'],
    ['read', '--sha', SHA, '--context', 'a b'],
    ['read', '--sha', SHA, '--state', 'success'],
  ]) {
    const { spawn, calls } = fakeSpawn()
    assert.throws(() => main(argv, { spawn }), UsageError, argv.join(' '))
    assert.equal(calls.length, 0)
  }
  assert.throws(() => main(['nope'], {}), UsageError)
  assert.throws(() => main([], {}), UsageError)
})

// ---------------------------------------------------------------------------
// Real CLI against a fake `gh` first on PATH.

function withFakeGh(body, fn) {
  const dir = mkdtempSync(path.join(tmpdir(), 'commit-status-'))
  try {
    const log = path.join(dir, 'argv.log')
    const gh = path.join(dir, 'gh')
    writeFileSync(
      gh,
      `#!/bin/sh\nfor a in "$@"; do printf '%s\\n' "$a" >> "${log}"; done\n${body}\n`,
    )
    chmodSync(gh, 0o755)
    const run = (args) =>
      spawnSync(process.execPath, [SCRIPT_PATH, ...args], {
        encoding: 'utf8',
        env: { ...process.env, PATH: `${dir}:/usr/bin:/bin` },
      })
    const recorded = () => {
      try {
        return readFileSync(log, 'utf8').split('\n').slice(0, -1)
      } catch {
        return []
      }
    }
    fn({ run, recorded })
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
}

test('CLI post — runs gh with the exact raw-field argv and prints one JSON line', () => {
  withFakeGh(`echo '{"id":1}'`, ({ run, recorded }) => {
    const res = run([
      'post',
      '--context',
      'boss/build-smoke',
      '--state',
      'pending',
      '--sha',
      SHA,
      '--description',
      '@file run 1',
      '--target-url',
      'https://example.test/pr/1',
    ])
    assert.equal(res.status, 0, res.stderr)
    assert.deepEqual(JSON.parse(res.stdout), {
      verdict: 'posted',
      sha: SHA,
      context: 'boss/build-smoke',
      state: 'pending',
      description: '@file run 1',
      targetUrl: 'https://example.test/pr/1',
    })
    assert.equal(res.stdout.trim().split('\n').length, 1)
    assert.deepEqual(recorded(), [
      'api',
      '--method',
      'POST',
      `repos/{owner}/{repo}/statuses/${SHA}`,
      '-f',
      'state=pending',
      '-f',
      'context=boss/build-smoke',
      '-f',
      'description=@file run 1',
      '-f',
      'target_url=https://example.test/pr/1',
    ])
  })
})

test('CLI post — gh failing exits 1; a validation error exits 2 and never runs gh', () => {
  withFakeGh(`echo 'HTTP 422' >&2; exit 1`, ({ run, recorded }) => {
    const failed = run(['post', '--context', 'c', '--state', 'success', '--sha', SHA])
    assert.equal(failed.status, 1)
    assert.equal(JSON.parse(failed.stdout).verdict, 'failed')
    const before = recorded().length
    const usage = run(['post', '--context', 'c', '--state', 'nope', '--sha', SHA])
    assert.equal(usage.status, 2)
    assert.equal(usage.stdout, '')
    assert.match(usage.stderr, /--state/)
    assert.equal(recorded().length, before, 'gh must not run on a usage error')
  })
})

test('CLI read — prints the verdict and exits 0 even when gh fails', () => {
  const payload = JSON.stringify([[status('boss/build', 'success', '2026-10-06T10:00:00Z')]])
  withFakeGh(`printf '%s' '${payload}'`, ({ run, recorded }) => {
    const res = run(['read', '--sha', SHA, '--context', 'boss/build'])
    assert.equal(res.status, 0, res.stderr)
    const v = JSON.parse(res.stdout)
    assert.equal(v.verdict, 'present')
    assert.equal(v.state, 'success')
    assert.deepEqual(recorded(), readArgs({ sha: SHA }))
  })
  withFakeGh('exit 1', ({ run }) => {
    const res = run(['read', '--sha', SHA, '--context', 'boss/build'])
    assert.equal(res.status, 0)
    assert.equal(JSON.parse(res.stdout).verdict, 'unknown')
  })
})

test('CLI post — boss/build success runs the real ready gate: refused (3) or posted (0)', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'commit-status-gate-'))
  try {
    const verdictFile = path.join(dir, 'verdict')
    const missing = path.join(dir, 'absent')
    withFakeGh(`echo '{"id":1}'`, ({ run, recorded }) => {
      const refused = run([
        'post',
        '--context',
        'boss/build',
        '--state',
        'success',
        '--sha',
        SHA,
        '--review-verdict-file',
        missing,
        '--reviewed-head-file',
        missing,
        '--base',
        'main',
      ])
      assert.equal(refused.status, 3, refused.stderr)
      assert.equal(JSON.parse(refused.stdout).verdict, 'refused')
      assert.match(
        refused.stderr,
        /^commit-status: refused boss\/build success: review-verdict-unknown, reviewed-head-unknown$/m,
      )
      assert.equal(recorded().filter((a) => a === 'POST').length, 0, 'nothing posted')
      writeFileSync(verdictFile, 'REVIEW_VERDICT=disabled\n')
      writeFileSync(missing, `${SHA}\n`)
      const posted = run([
        'post',
        '--context',
        'boss/build',
        '--state',
        'success',
        '--sha',
        SHA,
        '--review-verdict-file',
        verdictFile,
        '--reviewed-head-file',
        missing,
        '--base',
        'main',
      ])
      assert.equal(posted.status, 0, posted.stderr)
      assert.equal(JSON.parse(posted.stdout).verdict, 'posted')
      assert.ok(recorded().includes(`repos/{owner}/{repo}/statuses/${SHA}`))
    })
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('boss/build refuses a disabled note after a tree-changing commit', () => {
  const dir = mkdtempSync(path.join(tmpdir(), 'commit-status-disabled-'))
  try {
    const git = (...args) => {
      const r = spawnSync(
        'git',
        [
          '-c',
          'user.name=Test',
          '-c',
          'user.email=test@example.invalid',
          '-c',
          'commit.gpgsign=false',
          ...args,
        ],
        { cwd: dir, encoding: 'utf8' },
      )
      assert.equal(r.status, 0, r.stderr)
      return r.stdout.trim()
    }
    git('init', '--initial-branch=main')
    writeFileSync(path.join(dir, 'file'), 'base')
    git('add', 'file')
    git('commit', '-m', 'base')
    git('update-ref', 'refs/remotes/origin/main', 'HEAD')
    const reviewed = git('rev-parse', 'HEAD')
    const gitDir = git('rev-parse', '--absolute-git-dir')
    writeFileSync(path.join(gitDir, 'boss-build-review-verdict'), 'REVIEW_VERDICT=disabled\n')
    writeFileSync(path.join(gitDir, 'boss-build-reviewed-head'), reviewed)
    const { spawn, calls } = fakeSpawn()
    const post = (head) =>
      main(
        ['post', '--context', 'boss/build', '--state', 'success', '--sha', head, '--base', 'main'],
        { spawn, readyGateReport: (options) => readyGateReport(options, { cwd: dir }) },
      )
    assert.equal(post(reviewed).exitCode, 0)
    writeFileSync(path.join(dir, 'file'), 'repair')
    git('add', 'file')
    git('commit', '-m', 'repair')
    const refused = post(git('rev-parse', 'HEAD'))
    assert.equal(refused.exitCode, 3)
    assert.deepEqual(refused.result.reasons, ['tree-changed-since-review'])
    assert.equal(calls.length, 1, 'changed head never posts to GitHub')
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// skills-toolbox/finalize/boss-finalize.test.mjs
import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  createBossFinalizeAdapter,
  createDefaultRun,
  finalizeOperationMap,
  resolveTagInject,
  runnerFailureMessage,
  stderrTail,
} from './boss-finalize.mjs'

test('injectPrTag shells the tag-injection command with PR number and BASE_BRANCH', () => {
  const calls = []
  const a = createBossFinalizeAdapter({
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  a.injectPrTag('1234', { baseBranch: 'main' })
  assert.equal(calls.length, 1)
  assert.match(calls[0].cmd, /add-pr-numbers\.sh$/)
  assert.deepEqual(calls[0].args, ['1234'])
  assert.equal(calls[0].opts.env.BASE_BRANCH, 'main')
})

test('injectPrTag coerces a numeric PR number to a string argument', () => {
  const calls = []
  const a = createBossFinalizeAdapter({
    runImpl: (cmd, args, opts) => calls.push({ cmd, args, opts }),
  })
  a.injectPrTag(1234, { baseBranch: 'main' })
  assert.deepEqual(calls[0].args, ['1234'])
})

test('the operation map names the ready and repair ops', () => {
  const a = createBossFinalizeAdapter()
  assert.equal(a.operationMap.readyPr.command, 'gh pr ready')
  assert.equal(a.operationMap.repair.skill, 'boss-repair')
  assert.equal(a.operationMap, finalizeOperationMap)
})

test('resolveTagInject prefers the Claude tree when it ships the helper', () => {
  const cmd = resolveTagInject((p) => p.includes('.claude'))
  assert.match(cmd, /\.claude\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('resolveTagInject falls back to the Codex tree for a Codex-only install', () => {
  const cmd = resolveTagInject((p) => p.includes('.codex'))
  assert.match(cmd, /\.codex\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('resolveTagInject defaults to the Claude path when neither tree exists', () => {
  const cmd = resolveTagInject(() => false)
  assert.match(cmd, /\.claude\/skills\/bossanova\/boss-finalize\/add-pr-numbers\.sh$/)
})

test('policy constants match the current SKILL values', () => {
  const a = createBossFinalizeAdapter()
  assert.equal(a.policy.tagFormat, '[#<PR>]')
  assert.equal(a.policy.repairCap, 5)
  assert.equal(a.policy.settleCap, 3)
})

// --- default runner: capture the child's own reason, and still forward it ---------

// A fake helper that writes a known token to stderr and exits non-zero — the shape the
// previous test could not reach, because it only ever threw a synthetic error with a
// hand-written message, which is exactly why the real message shape was never pinned.
const FAILING_HELPER = [
  '-e',
  "process.stderr.write('HELPER-REASON-TOKEN: refusing to amend a signed commit\\n'); process.exit(3)",
]

test('a failing helper reaches the caller as an error carrying its own stderr', () => {
  let forwarded = ''
  const run = createDefaultRun({ errWrite: (s) => (forwarded += s) })
  let thrown
  try {
    run(process.execPath, FAILING_HELPER, {})
  } catch (err) {
    thrown = err
  }
  assert.ok(thrown, 'a non-zero child must throw')
  assert.equal(thrown.status, 3)
  // Both halves: the wrapper's generic first line is preserved for callers that
  // recognise it, and the helper's own reason is appended.
  assert.match(thrown.message, /^Command failed: /)
  assert.match(thrown.message, /HELPER-REASON-TOKEN: refusing to amend a signed commit/)
  assert.match(thrown.stderr, /HELPER-REASON-TOKEN/)
  // ...and the same bytes still reach the caller's stderr, so capturing did not
  // silently swallow the live output the inherited runner used to produce.
  assert.match(forwarded, /HELPER-REASON-TOKEN: refusing to amend a signed commit/)
})

test('a successful helper forwards its stderr and throws nothing', () => {
  let forwarded = ''
  const run = createDefaultRun({ errWrite: (s) => (forwarded += s) })
  run(process.execPath, ['-e', "process.stderr.write('progress: rebasing\\n')"], {})
  assert.match(forwarded, /progress: rebasing/)
})

test('the default runner passes env through and drops a caller stdio override', () => {
  const calls = []
  const run = createDefaultRun({
    spawnImpl: (cmd, args, opts) => {
      calls.push({ cmd, args, opts })
      return { status: 0, stdout: null, stderr: '' }
    },
    errWrite: () => {},
  })
  run('helper.sh', ['1234'], { env: { BASE_BRANCH: 'main' }, stdio: 'inherit' })
  assert.equal(calls[0].opts.env.BASE_BRANCH, 'main')
  assert.deepEqual(calls[0].opts.stdio, ['inherit', 'inherit', 'pipe'])
})

test('a spawn failure surfaces the system error unchanged', () => {
  const spawnErr = Object.assign(new Error('spawn ENOENT'), { code: 'ENOENT' })
  const run = createDefaultRun({
    spawnImpl: () => ({ error: spawnErr, status: null, stderr: '' }),
    errWrite: () => {},
  })
  assert.throws(() => run('missing.sh', [], {}), /spawn ENOENT/)
})

test('stderrTail keeps the trailing bytes and marks the truncation', () => {
  assert.equal(stderrTail('short', 64), 'short')
  const long = 'x'.repeat(100) + 'THE-REASON'
  const tail = stderrTail(long, 20)
  assert.ok(tail.startsWith('…'))
  assert.ok(tail.endsWith('THE-REASON'))
  assert.equal(Buffer.byteLength(tail.slice(1), 'utf8'), 20)
})

test('runnerFailureMessage degrades to the bare command line on empty stderr', () => {
  assert.equal(runnerFailureMessage('helper.sh', ['9'], ''), 'Command failed: helper.sh 9')
  assert.equal(runnerFailureMessage('helper.sh', ['9'], undefined), 'Command failed: helper.sh 9')
})

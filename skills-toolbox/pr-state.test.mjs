import assert from 'node:assert/strict'
import test from 'node:test'

import {
  FALLBACK_LABELS,
  GH_USAGE_LABEL_ENV,
  PR_WAIT_CAP_MS,
  UsageError,
  bossCall,
  daemonFirstReader,
  main,
  readPRState,
  waitAnswered,
  waitPRState,
} from './pr-state.mjs'

const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)
const BOSS = '/fake/bin/boss'
const ENV = Object.freeze({ PATH: '/usr/bin', HOME: '/home/agent' })

// fakeIo records every spawn; `boss` answers `bossAnswer(args, i)`, `gh` answers `ghAnswer(args)`.
function fakeIo({
  bossAnswer = () => ({ status: 0, stdout: '{}' }),
  ghAnswer,
  binary = true,
} = {}) {
  const spawns = []
  let bossCalls = 0
  return {
    spawns,
    gh: () => spawns.filter((s) => s.cmd === 'gh'),
    deps: {
      env: ENV,
      resolveBoss: () =>
        binary ? { ok: true, path: BOSS } : { ok: false, path: null, reason: 'none' },
      spawn: (cmd, args, opts) => {
        spawns.push({ cmd, args, opts })
        if (cmd === BOSS) return bossAnswer(args, bossCalls++)
        if (cmd === 'gh' && ghAnswer) return ghAnswer(args)
        throw new Error(`unexpected spawn ${cmd} ${args.join(' ')}`)
      },
    },
  }
}
const json = (value, status = 0) => ({ status, stdout: `${JSON.stringify(value)}\n` })
const failure = (code, connectCode, message = 'boom') =>
  json({ error: { code, connect_code: connectCode, message } }, 1)
const envelope = (extra = {}) => ({
  head_sha: HEAD,
  base_branch: 'main',
  checks: [
    {
      name: 'build',
      status: 'completed',
      conclusion: 'success',
      description: '',
      is_unclassified: false,
    },
  ],
  verify_phase: { kind: 'none' },
  workflow_runs: [
    { name: 'CI', status: 'completed', conclusion: 'success', event: 'push', head_sha: HEAD },
  ],
  is_workflow_runs_read: true,
  ...extra,
})

test('bossCall: only a missing daemon, a missing binary or a too-old CLI falls back', () => {
  const cases = [
    ['ok envelope', { answer: json(envelope()) }, 'ok'],
    ['no boss binary', { binary: false }, 'fallback', 'no-boss-binary'],
    ['binary vanished', { answer: { error: { code: 'ENOENT' } } }, 'fallback', 'no-boss-binary'],
    [
      'UNAVAILABLE code',
      { answer: failure('UNAVAILABLE', 'unavailable') },
      'fallback',
      'daemon-unavailable',
    ],
    [
      'unimplemented connect code',
      { answer: failure('UNKNOWN', 'unimplemented') },
      'fallback',
      'daemon-unimplemented',
    ],
    [
      'NOT_FOUND code',
      { answer: failure('NOT_FOUND', 'not_found') },
      'fallback',
      'daemon-not_found',
    ],
    [
      'help screen exiting 0',
      { answer: { status: 0, stdout: 'Usage:\n  boss pr [command]\n' } },
      'fallback',
      'boss-too-old',
    ],
    ['nothing on stdout', { answer: { status: 1, stdout: '' } }, 'fallback', 'boss-too-old'],
    [
      'flag this CLI predates',
      { answer: failure('UNKNOWN', 'unknown', 'unknown flag: --with-workflow-runs') },
      'fallback',
      'boss-too-old',
    ],
    [
      'RESOURCE_EXHAUSTED',
      { answer: failure('RESOURCE_EXHAUSTED', 'resource_exhausted') },
      'error',
    ],
    ['INVALID_ARGUMENT', { answer: failure('INVALID_ARGUMENT', 'invalid_argument') }, 'error'],
    [
      'FAILED_PRECONDITION',
      { answer: failure('FAILED_PRECONDITION', 'failed_precondition') },
      'error',
    ],
    ['spawn timeout', { answer: { error: { code: 'ETIMEDOUT' }, status: null } }, 'error'],
  ]
  for (const [name, { answer, binary = true }, kind, reason] of cases) {
    const io = fakeIo({ binary, bossAnswer: () => answer })
    const res = bossCall({ argv: ['pr', 'state', '7', '--json'], timeoutMs: 5000, deps: io.deps })
    assert.equal(res.kind, kind, name)
    if (reason) assert.equal(res.reason, reason, name)
    assert.equal(io.gh().length, 0, `${name}: bossCall never spawns gh`)
  }
})

test('bossCall: runs the resolved binary with the timeout and SIGKILL', () => {
  const io = fakeIo({ bossAnswer: () => json(envelope()) })
  readPRState({ pr: 7, repo: 'acme/widgets', fresh: true, timeoutMs: 4321, deps: io.deps })
  const [call] = io.spawns
  assert.equal(call.cmd, BOSS)
  assert.deepEqual(call.args, [
    'pr',
    'state',
    '7',
    '--with-workflow-runs',
    '--json',
    '--repo',
    'acme/widgets',
    '--fresh',
  ])
  assert.equal(call.opts.timeout, 4321)
  assert.equal(call.opts.killSignal, 'SIGKILL')
})

test('waitPRState: checks-settled within the cap, with spawn headroom over --timeout', () => {
  const io = fakeIo({ bossAnswer: () => json({ outcome: 'met' }) })
  assert.ok(
    waitAnswered(
      waitPRState({ pr: 7, until: 'checks-settled', timeoutMs: 240_500, deps: io.deps }),
    ),
  )
  waitPRState({ pr: 7, until: 'checks-settled', timeoutMs: PR_WAIT_CAP_MS * 3, deps: io.deps })
  const [first, capped] = io.spawns
  assert.deepEqual(first.args, [
    'pr',
    'wait',
    '7',
    '--until',
    'checks-settled',
    '--timeout',
    '240s',
    '--json',
  ])
  assert.ok(first.opts.timeout > 240_000 && first.opts.timeout <= 250_000)
  assert.equal(capped.args[6], `${PR_WAIT_CAP_MS / 1000}s`)
  assert.equal(waitAnswered({ kind: 'error', code: 'RESOURCE_EXHAUSTED' }), false)
  assert.equal(waitAnswered({ kind: 'fallback', reason: 'daemon-unavailable' }), false)
})

test('daemonFirstReader: an error is unreadable and spawns no gh', async () => {
  const io = fakeIo({ bossAnswer: () => failure('RESOURCE_EXHAUSTED', 'resource_exhausted') })
  const read = daemonFirstReader({ deps: io.deps })
  const view = await read({ what: 'view', pr: 7, timeoutMs: 5000 })
  assert.equal(view.ok, false)
  assert.match(view.error, /RESOURCE_EXHAUSTED/)
  assert.equal((await read({ what: 'runs', pr: 7, headSha: HEAD, timeoutMs: 5000 })).ok, false)
  assert.equal(io.gh().length, 0)
  assert.equal(read.source(), 'daemon')
})

test('daemonFirstReader: a fallback switches to labelled gh for good, keeping the rest of the env', async () => {
  const io = fakeIo({
    bossAnswer: () => failure('UNAVAILABLE', 'unavailable'),
    ghAnswer: (args) =>
      args[1] === 'view'
        ? json({ headRefOid: HEAD, baseRefName: 'main', statusCheckRollup: [] })
        : json([]),
  })
  const read = daemonFirstReader({ label: FALLBACK_LABELS.CI_WAIT, deps: io.deps })
  assert.equal((await read({ what: 'view', pr: 7, timeoutMs: 5000 })).headSha, HEAD)
  await read({ what: 'runs', pr: 7, headSha: HEAD, timeoutMs: 5000 })
  await read({ what: 'view', pr: 7, timeoutMs: 5000 })
  assert.equal(read.source(), 'gh-fallback')
  assert.equal(read.fallbackReason(), 'daemon-unavailable')
  assert.equal(io.spawns.filter((s) => s.cmd === BOSS).length, 1, 'the fallback is sticky')
  assert.equal(io.gh().length, 3)
  for (const { opts } of io.gh()) {
    assert.equal(opts.env[GH_USAGE_LABEL_ENV], 'fallback:ci-wait')
    assert.equal(opts.env.HOME, ENV.HOME)
    assert.equal(opts.env.PATH, ENV.PATH)
  }
})

test('daemonFirstReader: serves view, runs and checks from one envelope; never reads verify statuses', async () => {
  const io = fakeIo({ bossAnswer: () => json(envelope({ verify_phase: { kind: 'waiting' } })) })
  const read = daemonFirstReader({ deps: io.deps })
  const view = await read({ what: 'view', pr: 7, timeoutMs: 5000 })
  assert.deepEqual(view, {
    ok: true,
    fresh: false,
    headSha: HEAD,
    payload: {
      headRefOid: HEAD,
      baseRefName: 'main',
      statusCheckRollup: [
        { name: 'build', status: 'COMPLETED', conclusion: 'SUCCESS', description: '' },
      ],
      verifyPhase: 'waiting',
    },
  })
  const runs = await read({ what: 'runs', pr: 7, headSha: HEAD, timeoutMs: 5000 })
  assert.deepEqual(runs.payload, [
    {
      name: 'CI',
      workflowName: 'CI',
      status: 'COMPLETED',
      conclusion: 'SUCCESS',
      headSha: HEAD,
      event: 'push',
    },
  ])
  assert.equal((await read({ what: 'checks', pr: 7, timeoutMs: 5000 })).ok, true)
  assert.equal(
    (await read({ what: 'verify-status', pr: 7, headSha: HEAD, timeoutMs: 5000 })).ok,
    false,
  )
  assert.equal(io.spawns.length, 1)
})

test('daemonFirstReader: an envelope whose runs were not read serves no runs', async () => {
  const io = fakeIo({
    bossAnswer: () => json(envelope({ workflow_runs: [], is_workflow_runs_read: false })),
  })
  const read = daemonFirstReader({ deps: io.deps })
  await read({ what: 'view', pr: 7, timeoutMs: 5000 })
  const runs = await read({ what: 'runs', pr: 7, headSha: HEAD, timeoutMs: 5000 })
  assert.deepEqual(runs, { ok: false, error: 'daemon read carried no workflow runs' })
})

test('daemonFirstReader: runs for another head are re-read once fresh, then unreadable', async () => {
  const split = envelope({
    workflow_runs: [
      { name: 'CI', status: 'completed', conclusion: 'success', event: 'push', head_sha: OTHER },
    ],
  })
  const healed = fakeIo({ bossAnswer: (_args, i) => json(i === 0 ? split : envelope()) })
  const read = daemonFirstReader({ deps: healed.deps })
  assert.equal((await read({ what: 'view', pr: 7, timeoutMs: 5000 })).ok, true)
  assert.deepEqual(
    healed.spawns.map((s) => s.args.includes('--fresh')),
    [false, true],
  )
  const stuck = fakeIo({ bossAnswer: () => json(split) })
  const stuckRead = daemonFirstReader({ deps: stuck.deps })
  const view = await stuckRead({ what: 'view', pr: 7, timeoutMs: 5000 })
  assert.equal(view.ok, false)
  assert.match(view.error, /two heads/)
  assert.equal(stuck.spawns.length, 2, 'one fresh re-read, no more')
  assert.equal(stuck.gh().length, 0)
})

test('read CLI: the daemon path prints the envelope verbatim', () => {
  const io = fakeIo({ bossAnswer: () => json(envelope()) })
  const { exit, out } = main(['read', '--pr', '7', '--repo', 'acme/widgets'], io.deps)
  assert.equal(exit, 0)
  assert.deepEqual(out, { source: 'daemon', state: envelope() })
  assert.equal(io.gh().length, 0)
})

test('read CLI: the fallback path builds the same shape from labelled gh reads', () => {
  const rollup = [
    { __typename: 'CheckRun', name: 'build', status: 'COMPLETED', conclusion: 'SUCCESS' },
    {
      __typename: 'StatusContext',
      context: 'boss/verify',
      state: 'PENDING',
      description: 'verifying… t',
    },
  ]
  const io = fakeIo({
    binary: false,
    ghAnswer: (args) =>
      args[0] === 'pr'
        ? json({
            headRefOid: HEAD,
            headRefName: 'feat',
            baseRefName: 'main',
            state: 'OPEN',
            isDraft: false,
            mergeable: 'MERGEABLE',
            mergeStateStatus: 'BLOCKED',
            reviewDecision: 'REVIEW_REQUIRED',
            statusCheckRollup: rollup,
          })
        : json([
            {
              name: 'CI',
              workflowName: 'CI',
              status: 'completed',
              conclusion: 'success',
              headSha: HEAD,
              event: 'push',
            },
          ]),
  })
  const { exit, out } = main(['read', '--pr', '7', '--repo', 'Acme/Widgets'], io.deps)
  assert.equal(exit, 0)
  assert.equal(out.source, 'gh-fallback')
  assert.equal(out.fallbackReason, 'no-boss-binary')
  const live = envelope()
  assert.deepEqual(
    Object.keys(out.state).sort(),
    [
      ...Object.keys(live),
      'repo',
      'pr_number',
      'state',
      'is_draft',
      'head_branch',
      'is_mergeable',
      'merge_state_status',
      'review_decision',
      'latest_review_state',
      'check_verdict',
      'has_build_receipt',
      'has_settled_workflow_runs',
      'held_workflow_runs',
      'review_comments',
      'observed_at',
    ].sort(),
  )
  for (const field of ['check_verdict', 'verify_phase', 'has_build_receipt', 'observed_at']) {
    assert.equal(out.state[field], null, field)
  }
  assert.equal(out.state.head_sha, HEAD)
  assert.equal(out.state.review_decision, 'required')
  assert.equal(out.state.is_mergeable, true)
  assert.deepEqual(out.state.checks, [
    {
      name: 'build',
      status: 'completed',
      conclusion: 'success',
      description: '',
      is_unclassified: false,
    },
    {
      name: 'boss/verify',
      status: 'queued',
      conclusion: '',
      description: 'verifying… t',
      is_unclassified: false,
    },
  ])
  assert.deepEqual(out.state.workflow_runs, [
    { name: 'CI', status: 'completed', conclusion: 'success', event: 'push', head_sha: HEAD },
  ])
  assert.equal(io.gh().length, 2)
  for (const { args, opts } of io.gh()) {
    assert.equal(opts.env[GH_USAGE_LABEL_ENV], 'fallback:pr-state')
    assert.equal(opts.env.HOME, ENV.HOME)
    assert.deepEqual(args.slice(-2), ['--repo', 'Acme/Widgets'])
  }
})

test('read CLI: a daemon error exits 1 with no gh spawn; a failed gh read exits 1', () => {
  const refused = fakeIo({
    bossAnswer: () => failure('INVALID_ARGUMENT', 'invalid_argument', 'bad pr'),
  })
  const res = main(['read', '--pr', '7'], refused.deps)
  assert.equal(res.exit, 1)
  assert.equal(res.out.source, 'daemon')
  assert.match(res.out.error.message, /bad pr/)
  assert.equal(refused.gh().length, 0)
  const ghDown = fakeIo({ binary: false, ghAnswer: () => ({ status: 1, stdout: '' }) })
  const down = main(['read', '--pr', '7'], ghDown.deps)
  assert.equal(down.exit, 1)
  assert.equal(down.out.source, 'gh-fallback')
})

test('read CLI: argv is parsed strictly', () => {
  assert.throws(() => main(['read', '--pr', '7', '--bogus']), UsageError)
  assert.throws(() => main(['read']), UsageError)
  assert.throws(() => main(['fetch', '--pr', '7']), UsageError)
  assert.throws(() => main(['read', '--pr', '7', '--timeout-ms', '5']), UsageError)
})

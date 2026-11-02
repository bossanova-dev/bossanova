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
  readPRView,
  readSessionPR,
  viewFromEnvelope,
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
    {
      __typename: 'CheckRun',
      name: 'build',
      status: 'COMPLETED',
      conclusion: 'SUCCESS',
      workflowName: 'CI',
    },
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
      workflow_name: 'CI',
    },
    {
      name: 'boss/verify',
      status: 'queued',
      conclusion: '',
      description: 'verifying… t',
      is_unclassified: false,
      workflow_name: '',
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

// recorded lists every spawn as [command, args, env.BOSS_GH_USAGE_LABEL].
const recorded = (io) =>
  io.spawns.map(({ cmd, args, opts }) => [cmd, args, opts.env?.[GH_USAGE_LABEL_ENV]])
const viewEnvelope = (extra = {}) =>
  envelope({
    pr_number: 7,
    state: 'open',
    is_draft: true,
    head_branch: 'feat',
    is_mergeable: true,
    merge_state_status: 'blocked',
    review_decision: 'changes_requested',
    ...extra,
  })

test('view: the daemon answers in gh pr view shape with no gh spawn and no runs read', () => {
  const io = fakeIo({ bossAnswer: () => json(viewEnvelope()) })
  const { exit, out } = main(['view', '--pr', '7', '--repo', 'acme/widgets'], io.deps)
  assert.equal(exit, 0)
  assert.deepEqual(out, {
    number: 7,
    state: 'OPEN',
    isDraft: true,
    headRefOid: HEAD,
    headRefName: 'feat',
    baseRefName: 'main',
    mergeable: 'MERGEABLE',
    mergeStateStatus: 'BLOCKED',
    reviewDecision: 'CHANGES_REQUESTED',
    statusCheckRollup: [
      { name: 'build', status: 'COMPLETED', conclusion: 'SUCCESS', description: '' },
    ],
    source: 'daemon',
  })
  assert.equal('mergedAt' in out, false, 'a merged time the envelope lacks is never invented')
  assert.deepEqual(recorded(io), [
    [BOSS, ['pr', 'state', '7', '--json', '--repo', 'acme/widgets'], undefined],
  ])
})

test('view: mergeability true/false/unset maps to MERGEABLE/CONFLICTING/UNKNOWN', () => {
  for (const [isMergeable, want] of [
    [true, 'MERGEABLE'],
    [false, 'CONFLICTING'],
    [null, 'UNKNOWN'],
    [undefined, 'UNKNOWN'],
  ]) {
    assert.equal(viewFromEnvelope(viewEnvelope({ is_mergeable: isMergeable })).mergeable, want)
  }
  const merged = viewFromEnvelope(viewEnvelope({ state: 'merged', merge_state_status: '' }))
  assert.equal(merged.state, 'MERGED')
  assert.equal(merged.mergeStateStatus, 'UNKNOWN')
  assert.equal(
    viewFromEnvelope(viewEnvelope({ review_decision: 'required' })).reviewDecision,
    'REVIEW_REQUIRED',
  )
  assert.equal(viewFromEnvelope(viewEnvelope({ review_decision: '' })).reviewDecision, '')
})

test('view: --fresh reaches the daemon read; --field prints the raw value', () => {
  const io = fakeIo({ bossAnswer: () => json(viewEnvelope({ is_draft: false })) })
  const res = main(['view', '--pr', '7', '--fresh', '--field', 'isDraft'], io.deps)
  assert.equal(res.text, 'false')
  assert.ok(io.spawns[0].args.includes('--fresh'))
  const base = main(['view', '--pr', '7', '--field', 'baseRefName'], io.deps)
  assert.equal(base.text, 'main')
  assert.equal(main(['view', '--pr', '7', '--field', 'nope'], io.deps).text, '')
})

// The fallback table: exactly a missing daemon or binary reaches labelled gh; every other daemon
// failure is a failed read that spends no GitHub quota.
const FALLBACK_TABLE = [
  ['UNAVAILABLE', { bossAnswer: () => failure('UNAVAILABLE', 'unavailable') }, 'gh'],
  ['UNIMPLEMENTED', { bossAnswer: () => failure('UNIMPLEMENTED', 'unimplemented') }, 'gh'],
  ['NOT_FOUND', { bossAnswer: () => failure('NOT_FOUND', 'not_found') }, 'gh'],
  ['no boss binary', { binary: false }, 'gh'],
  ['binary vanished', { bossAnswer: () => ({ error: { code: 'ENOENT' } }) }, 'gh'],
  [
    'RESOURCE_EXHAUSTED',
    { bossAnswer: () => failure('RESOURCE_EXHAUSTED', 'resource_exhausted') },
    'error',
  ],
  [
    'INVALID_ARGUMENT',
    { bossAnswer: () => failure('INVALID_ARGUMENT', 'invalid_argument') },
    'error',
  ],
]
const ghViewPayload = {
  number: 7,
  state: 'OPEN',
  isDraft: false,
  headRefOid: HEAD,
  headRefName: 'feat',
  baseRefName: 'main',
  mergeable: 'CONFLICTING',
  mergeStateStatus: 'DIRTY',
  reviewDecision: '',
  statusCheckRollup: [],
}

test('view: falls back to labelled gh exactly for a missing daemon or binary', () => {
  for (const [name, io0, want] of FALLBACK_TABLE) {
    const io = fakeIo({
      ...io0,
      ghAnswer: (args) => (args[1] === 'view' ? json(ghViewPayload) : assert.fail(args.join(' '))),
    })
    const res = main(['view', '--pr', '7'], io.deps)
    const gh = recorded(io).filter(([cmd]) => cmd === 'gh')
    if (want === 'gh') {
      assert.equal(res.exit, 0, name)
      assert.equal(res.out.source, 'gh-fallback', name)
      assert.equal(res.out.mergeable, 'CONFLICTING', name)
      assert.deepEqual(
        gh,
        [
          [
            'gh',
            [
              'pr',
              'view',
              '7',
              '--json',
              'number,headRefOid,headRefName,baseRefName,state,isDraft,mergeable,mergeStateStatus,reviewDecision,statusCheckRollup',
            ],
            FALLBACK_LABELS.PR_STATE,
          ],
        ],
        name,
      )
    } else {
      assert.equal(res.exit, 1, name)
      assert.equal(res.out.source, 'daemon', name)
      assert.equal(res.out.error.code, name, name)
      assert.deepEqual(gh, [], `${name}: no gh spawn`)
    }
  }
})

test('view: an envelope without a head SHA is a failed read, never a gh retry', () => {
  const io = fakeIo({ bossAnswer: () => json({ pr_number: 7 }) })
  const res = readPRView({ pr: 7, deps: io.deps })
  assert.equal(res.exit, 1)
  assert.equal(res.out.error.code, 'MALFORMED')
  assert.equal(io.gh().length, 0)
})

// sessionIo answers `boss env`, `boss show` and `boss pr state` from the given bodies, `git branch`
// with `feat`, and `gh pr list` with one open PR.
function sessionIo({ env, show, binary = true, list = [{ number: 9, url: 'https://gh/pr/9' }] }) {
  const io = fakeIo({
    binary,
    bossAnswer: (args) => {
      if (args[0] === 'env') return env
      if (args[0] === 'show') return show
      return json(viewEnvelope({ pr_number: Number(args[2]) }))
    },
    ghAnswer: (args) =>
      args[1] === 'list' ? json(list) : assert.fail(`unexpected gh ${args.join(' ')}`),
  })
  const spawn = io.deps.spawn
  io.deps.spawn = (cmd, args, opts) => {
    if (cmd !== 'git') return spawn(cmd, args, opts)
    io.spawns.push({ cmd, args, opts })
    return { status: 0, stdout: 'feat\n' }
  }
  return io
}
const SESSION = json({ session: { session_id: 'sess-1', repo_id: 'r1' } })
const shown = (prNumber, prUrl = '') =>
  json({ session: { id: 'sess-1', pr_number: prNumber, pr_url: prUrl } })

test('session-pr: the session record answers with no gh spawn', () => {
  const io = sessionIo({ env: SESSION, show: shown(7, 'https://gh/pr/7') })
  const res = readSessionPR({ deps: io.deps })
  assert.deepEqual(res, { exit: 0, out: { number: 7, url: 'https://gh/pr/7', source: 'daemon' } })
  assert.deepEqual(recorded(io), [
    [BOSS, ['env', '--json'], undefined],
    [BOSS, ['show', 'sess-1', '--json'], undefined],
  ])
  assert.equal(main(['session-pr', '--field', 'number'], io.deps).text, '7')
})

test('session-pr: no session, no recorded PR or no daemon reads labelled gh pr list --head', () => {
  const cases = [
    ['no session', { env: json({ session: { session_id: '' } }) }, 'no-session'],
    ['pr_number null', { env: SESSION, show: shown(null) }, 'no-session-pr'],
    ['daemon down', { env: failure('UNAVAILABLE', 'unavailable') }, 'daemon-unavailable'],
    [
      'old daemon',
      { env: SESSION, show: failure('UNIMPLEMENTED', 'unimplemented') },
      'daemon-unimplemented',
    ],
    [
      'session unknown',
      { env: SESSION, show: failure('NOT_FOUND', 'not_found') },
      'daemon-not_found',
    ],
    ['no boss binary', { binary: false }, 'no-boss-binary'],
  ]
  for (const [name, answers, reason] of cases) {
    const io = sessionIo(answers)
    const res = readSessionPR({ deps: io.deps })
    assert.deepEqual(
      res.out,
      { number: 9, url: 'https://gh/pr/9', source: 'gh-fallback', fallbackReason: reason },
      name,
    )
    const gh = recorded(io).filter(([cmd]) => cmd === 'gh')
    assert.deepEqual(
      gh,
      [
        [
          'gh',
          ['pr', 'list', '--head', 'feat', '--state', 'open', '--json', 'number,url'],
          FALLBACK_LABELS.PR_STATE,
        ],
      ],
      name,
    )
  }
  const none = readSessionPR({
    deps: sessionIo({ env: SESSION, show: shown(null), list: [] }).deps,
  })
  assert.deepEqual([none.exit, none.out.number], [0, null])
})

test('session-pr: a daemon failure that is not a missing daemon spawns no gh', () => {
  for (const code of ['RESOURCE_EXHAUSTED', 'INVALID_ARGUMENT']) {
    const io = sessionIo({ env: SESSION, show: failure(code, code.toLowerCase()) })
    const res = readSessionPR({ deps: io.deps })
    assert.equal(res.exit, 1, code)
    assert.equal(res.out.error.code, code)
    assert.equal(io.gh().length, 0, code)
  }
})

test('view with no --pr reads the session PR; no open PR is a failed read', () => {
  const io = sessionIo({ env: SESSION, show: shown(7) })
  const res = main(['view', '--field', 'number'], io.deps)
  assert.equal(res.text, '7')
  assert.deepEqual(io.spawns.at(-1).args.slice(0, 3), ['pr', 'state', '7'])
  assert.equal(io.gh().length, 0)
  const none = main(['view'], sessionIo({ env: SESSION, show: shown(null), list: [] }).deps)
  assert.equal(none.exit, 1)
  assert.equal(none.out.error.code, 'NO_PR')
  const quiet = main(
    ['view', '--field', 'number'],
    sessionIo({ env: SESSION, show: shown(null), list: [] }).deps,
  )
  assert.deepEqual([quiet.exit, quiet.text, quiet.toStderr], [1, undefined, true])
})

test('view and session-pr CLI: argv is parsed strictly per verb', () => {
  assert.throws(() => main(['session-pr', '--pr', '7']), UsageError)
  assert.throws(() => main(['read', '--pr', '7', '--field', 'state']), UsageError)
  assert.throws(() => main(['view', '--pr', 'x']), UsageError)
  assert.throws(() => main(['view', '--field', '.state']), UsageError)
  assert.throws(() => main(['view', '--repo', 'acme/widgets']), UsageError)
})
